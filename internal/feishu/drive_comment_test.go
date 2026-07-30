package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

type fakeDriveCommentClient struct {
	detail     DriveCommentDetail
	err        error
	replyErr   error
	getCalls   []fakeDriveCommentGetCall
	replyCalls []DriveComment
	replyTexts []string
}

type fakeDriveCommentGetCall struct {
	FileToken string
	FileType  string
	CommentID string
}

func (f *fakeDriveCommentClient) GetComment(ctx context.Context, fileToken, fileType, commentID string) (DriveCommentDetail, error) {
	f.getCalls = append(f.getCalls, fakeDriveCommentGetCall{
		FileToken: fileToken,
		FileType:  fileType,
		CommentID: commentID,
	})
	if f.err != nil {
		return DriveCommentDetail{}, f.err
	}
	return f.detail, nil
}

func (f *fakeDriveCommentClient) ReplyComment(ctx context.Context, comment DriveComment, text string) error {
	f.replyCalls = append(f.replyCalls, comment)
	f.replyTexts = append(f.replyTexts, text)
	return f.replyErr
}

type driveCommentRecordingHandler struct {
	count    int
	comment  DriveComment
	reply    string
	errReply string
	err      error
}

func (h *driveCommentRecordingHandler) HandleFeishuMessage(context.Context, Message) (string, error) {
	return "", nil
}

func (h *driveCommentRecordingHandler) HandleDriveComment(ctx context.Context, comment DriveComment) error {
	h.count++
	h.comment = comment
	if h.reply != "" {
		if _, err := ReplyDriveComment(ctx, comment, h.reply); err != nil {
			return err
		}
	}
	if h.errReply != "" {
		if _, err := ReplyDriveComment(ctx, comment, h.errReply); err != nil {
			return err
		}
	}
	return h.err
}

func TestParseDriveCommentExtractsNoticeFields(t *testing.T) {
	event := driveCommentEvent(true, "ou_user", "ou_bot", "reply-1")

	comment, err := ParseDriveComment(event)
	if err != nil {
		t.Fatalf("ParseDriveComment() error = %v", err)
	}
	if comment.FileToken != "doc-token" || comment.FileType != "docx" || comment.CommentID != "comment-1" || comment.ReplyID != "reply-1" {
		t.Fatalf("comment ids = %+v, want parsed fields", comment)
	}
	if !comment.IsMentioned || comment.OperatorID != "ou_user" || comment.RecipientID != "ou_bot" || comment.NoticeType != "add_reply" {
		t.Fatalf("comment metadata = %+v, want notice fields", comment)
	}
	if comment.DocumentURL == "" {
		t.Fatalf("DocumentURL = empty, want constructed link")
	}
	if strings.Contains(comment.DocumentURL, "open-apis") || comment.DocumentURL != "https://feishu.cn/docx/doc-token" {
		t.Fatalf("DocumentURL = %q, want user-facing feishu docx link", comment.DocumentURL)
	}
}

func TestDriveDocumentURLBuildsUserFacingLinks(t *testing.T) {
	for _, tt := range []struct {
		fileType string
		token    string
		want     string
	}{
		{fileType: "docx", token: "docx-token", want: "https://feishu.cn/docx/docx-token"},
		{fileType: "doc", token: "doc-token", want: "https://feishu.cn/docs/doc-token"},
		{fileType: "sheet", token: "sheet-token", want: "https://feishu.cn/sheets/sheet-token"},
		{fileType: "bitable", token: "base-token", want: "https://feishu.cn/base/base-token"},
	} {
		t.Run(tt.fileType, func(t *testing.T) {
			if got := driveDocumentURL(tt.fileType, tt.token); got != tt.want {
				t.Fatalf("driveDocumentURL(%q, %q) = %q, want %q", tt.fileType, tt.token, got, tt.want)
			}
		})
	}
}

func TestHandleDriveCommentAddFetchesDetailAndDispatches(t *testing.T) {
	client := &fakeDriveCommentClient{detail: DriveCommentDetail{
		CommentID: "comment-1",
		UserID:    "ou_user",
		Replies: []DriveCommentReply{
			{ReplyID: "comment-root", UserID: "ou_user", Text: "root text"},
			{ReplyID: "reply-1", UserID: "ou_user", Text: "reply text"},
		},
	}}
	handler := &driveCommentRecordingHandler{reply: "agent reply"}
	adapter := NewAdapter(config.BotConfig{
		ID:        "bot-a",
		BotOpenID: "ou_bot",
		Workspace: "/workspace",
	}, handler)
	adapter.driveComments = client
	adapter.deduper = newMessageDeduper(driveCommentDeduperTTL, driveCommentDeduperMax)

	if err := adapter.handleDriveCommentAdd(context.Background(), driveCommentEvent(true, "ou_user", "ou_bot", "reply-1")); err != nil {
		t.Fatalf("handleDriveCommentAdd() error = %v", err)
	}
	if handler.count != 1 {
		t.Fatalf("handler count = %d, want 1", handler.count)
	}
	if got := handler.comment; got.BotID != "bot-a" || got.Workspace != "/workspace" || got.CommentText != "root text" || got.ReplyText != "reply text" {
		t.Fatalf("handled comment = %+v, want hydrated comment", got)
	}
	if len(client.getCalls) != 1 || client.getCalls[0] != (fakeDriveCommentGetCall{FileToken: "doc-token", FileType: "docx", CommentID: "comment-1"}) {
		t.Fatalf("get calls = %+v, want detail lookup", client.getCalls)
	}
	if len(client.replyCalls) != 1 || client.replyTexts[0] != "agent reply" {
		t.Fatalf("reply calls = %+v texts = %+v, want handler reply", client.replyCalls, client.replyTexts)
	}
}

func TestEventDispatcherRoutesDriveCommentAdd(t *testing.T) {
	client := &fakeDriveCommentClient{detail: DriveCommentDetail{
		CommentID: "comment-1",
		UserID:    "ou_user",
		Replies: []DriveCommentReply{
			{ReplyID: "comment-root", UserID: "ou_user", Text: "root text"},
			{ReplyID: "reply-1", UserID: "ou_user", Text: "reply text"},
		},
	}}
	handler := &driveCommentRecordingHandler{reply: "agent reply"}
	adapter := NewAdapter(config.BotConfig{
		ID:        "bot-a",
		BotOpenID: "ou_bot",
	}, handler)
	adapter.driveComments = client
	adapter.deduper = newMessageDeduper(driveCommentDeduperTTL, driveCommentDeduperMax)

	payload := driveCommentEventPayload(t, driveCommentEvent(true, "ou_user", "ou_bot", "reply-1"))
	if _, err := adapter.newEventDispatcher().Do(context.Background(), payload); err != nil {
		t.Fatalf("dispatcher Do() error = %v", err)
	}
	if handler.count != 1 {
		t.Fatalf("handler count = %d, want dispatcher to route Drive comment event", handler.count)
	}
	if len(client.getCalls) != 1 {
		t.Fatalf("get calls = %d, want detail lookup through dispatcher", len(client.getCalls))
	}
	if len(client.replyTexts) != 1 || client.replyTexts[0] != "agent reply" {
		t.Fatalf("reply texts = %+v, want handler reply through dispatcher", client.replyTexts)
	}
}

func TestHandleDriveCommentAddSkipsUnmentionedDuplicateAndBotSelf(t *testing.T) {
	client := &fakeDriveCommentClient{detail: DriveCommentDetail{
		CommentID: "comment-1",
		UserID:    "ou_user",
		Replies:   []DriveCommentReply{{ReplyID: "reply-1", UserID: "ou_user", Text: "reply text"}},
	}}
	handler := &driveCommentRecordingHandler{}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a", BotOpenID: "ou_bot"}, handler)
	adapter.driveComments = client
	adapter.deduper = newMessageDeduper(driveCommentDeduperTTL, driveCommentDeduperMax)

	if err := adapter.handleDriveCommentAdd(context.Background(), driveCommentEvent(false, "ou_user", "ou_bot", "reply-1")); err != nil {
		t.Fatalf("handle unmentioned error = %v", err)
	}
	if handler.count != 0 || len(client.getCalls) != 0 {
		t.Fatalf("unmentioned handler/get = %d/%d, want skipped", handler.count, len(client.getCalls))
	}

	event := driveCommentEvent(true, "ou_user", "ou_bot", "reply-1")
	if err := adapter.handleDriveCommentAdd(context.Background(), event); err != nil {
		t.Fatalf("handle first event error = %v", err)
	}
	if err := adapter.handleDriveCommentAdd(context.Background(), event); err != nil {
		t.Fatalf("handle duplicate event error = %v", err)
	}
	if handler.count != 1 {
		t.Fatalf("handler count = %d, want duplicate skipped", handler.count)
	}

	selfClient := &fakeDriveCommentClient{detail: DriveCommentDetail{
		CommentID: "comment-2",
		UserID:    "ou_user",
		Replies:   []DriveCommentReply{{ReplyID: "reply-self", UserID: "ou_bot", Text: "bot reply"}},
	}}
	selfHandler := &driveCommentRecordingHandler{}
	selfAdapter := NewAdapter(config.BotConfig{ID: "bot-a", BotOpenID: "ou_bot"}, selfHandler)
	selfAdapter.driveComments = selfClient
	selfAdapter.deduper = newMessageDeduper(driveCommentDeduperTTL, driveCommentDeduperMax)
	if err := selfAdapter.handleDriveCommentAdd(context.Background(), driveCommentEventWithIDs(true, "ou_user", "ou_bot", "doc-token-2", "comment-2", "reply-self")); err != nil {
		t.Fatalf("handle self reply event error = %v", err)
	}
	if selfHandler.count != 0 {
		t.Fatalf("self handler count = %d, want skipped", selfHandler.count)
	}

	rootSelfClient := &fakeDriveCommentClient{detail: DriveCommentDetail{
		CommentID: "comment-3",
		UserID:    "ou_bot",
		Replies:   []DriveCommentReply{{ReplyID: "comment-root", UserID: "ou_bot", Text: "bot root comment"}},
	}}
	rootSelfHandler := &driveCommentRecordingHandler{}
	rootSelfAdapter := NewAdapter(config.BotConfig{ID: "bot-a", BotOpenID: "ou_bot"}, rootSelfHandler)
	rootSelfAdapter.driveComments = rootSelfClient
	rootSelfAdapter.deduper = newMessageDeduper(driveCommentDeduperTTL, driveCommentDeduperMax)
	if err := rootSelfAdapter.handleDriveCommentAdd(context.Background(), driveCommentEventWithIDs(true, "ou_user", "ou_bot", "doc-token-3", "comment-3", "")); err != nil {
		t.Fatalf("handle self root comment event error = %v", err)
	}
	if rootSelfHandler.count != 0 {
		t.Fatalf("root self handler count = %d, want skipped", rootSelfHandler.count)
	}
}

func TestHandleDriveCommentAddDoesNotDuplicateHandlerErrorReply(t *testing.T) {
	client := &fakeDriveCommentClient{detail: DriveCommentDetail{
		CommentID: "comment-1",
		UserID:    "ou_user",
		Replies:   []DriveCommentReply{{ReplyID: "reply-1", UserID: "ou_user", Text: "reply text"}},
	}}
	handler := &driveCommentRecordingHandler{errReply: "处理评论失败：boom", err: errors.New("boom")}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a", BotOpenID: "ou_bot"}, handler)
	adapter.driveComments = client
	adapter.deduper = newMessageDeduper(driveCommentDeduperTTL, driveCommentDeduperMax)

	if err := adapter.handleDriveCommentAdd(context.Background(), driveCommentEvent(true, "ou_user", "ou_bot", "reply-1")); err != nil {
		t.Fatalf("handleDriveCommentAdd() error = %v", err)
	}
	if len(client.replyTexts) != 1 || client.replyTexts[0] != "处理评论失败：boom" {
		t.Fatalf("reply texts = %+v, want only handler-owned error reply", client.replyTexts)
	}
}

func driveCommentEvent(mentioned bool, fromOpenID, toOpenID, replyID string) *larkdrive.P2NoticeCommentAddV1 {
	return driveCommentEventWithIDs(mentioned, fromOpenID, toOpenID, "doc-token", "comment-1", replyID)
}

func driveCommentEventWithIDs(mentioned bool, fromOpenID, toOpenID, fileToken, commentID, replyID string) *larkdrive.P2NoticeCommentAddV1 {
	return &larkdrive.P2NoticeCommentAddV1{
		EventReq: &larkevent.EventReq{Body: []byte(`{"event_type":"drive.notice.comment_add_v1"}`)},
		Event: &larkdrive.P2NoticeCommentAddV1Data{
			NoticeMeta: larkdrive.NewNoticeBuilder().
				FileType("docx").
				FileToken(fileToken).
				FromUserId(larkdrive.NewUserIdBuilder().OpenId(fromOpenID).Build()).
				ToUserId(larkdrive.NewUserIdBuilder().OpenId(toOpenID).Build()).
				NoticeType("add_reply").
				Build(),
			CommentId:   stringPtr(commentID),
			ReplyId:     stringPtr(replyID),
			IsMentioned: boolPtr(mentioned),
		},
	}
}

func stringPtr(s string) *string {
	return &s
}

func boolPtr(v bool) *bool {
	return &v
}

func driveCommentEventPayload(t *testing.T, event *larkdrive.P2NoticeCommentAddV1) []byte {
	t.Helper()
	body := struct {
		Schema string                              `json:"schema"`
		Header larkevent.EventHeader               `json:"header"`
		Event  *larkdrive.P2NoticeCommentAddV1Data `json:"event"`
	}{
		Schema: "2.0",
		Header: larkevent.EventHeader{
			EventID:    "evt-drive-comment",
			EventType:  "drive.notice.comment_add_v1",
			AppID:      "cli_app",
			TenantKey:  "tenant",
			CreateTime: "1700000000000",
		},
		Event: event.Event,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal drive comment payload: %v", err)
	}
	return payload
}
