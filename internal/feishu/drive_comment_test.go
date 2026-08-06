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
	enabled  *bool
}

func (h *driveCommentRecordingHandler) HandleFeishuMessage(context.Context, Message) (string, error) {
	return "", nil
}

func (h *driveCommentRecordingHandler) HandleDriveComment(ctx context.Context, comment DriveComment) error {
	return h.HandleDriveCommentWithOutbound(ctx, comment, nil)
}

func (h *driveCommentRecordingHandler) HandleDriveCommentWithOutbound(ctx context.Context, comment DriveComment, outbound Outbound) error {
	h.count++
	h.comment = comment
	replier, _ := outbound.(interface {
		ReplyDriveComment(context.Context, DriveComment, string) error
	})
	if h.reply != "" {
		if replier == nil {
			return errors.New("missing drive comment replier")
		}
		if err := replier.ReplyDriveComment(ctx, comment, h.reply); err != nil {
			return err
		}
	}
	if h.errReply != "" {
		if replier == nil {
			return errors.New("missing drive comment replier")
		}
		if err := replier.ReplyDriveComment(ctx, comment, h.errReply); err != nil {
			return err
		}
	}
	return h.err
}

func (h *driveCommentRecordingHandler) DriveCommentEnabled(string) bool {
	return h.enabled == nil || *h.enabled
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
	if got := handler.comment; got.BotID != "bot-a" || got.Workspace != "/workspace" || got.CommentText != "root text" || got.ReplyText != "reply text" || len(got.Replies) != 2 {
		t.Fatalf("handled comment = %+v, want hydrated comment", got)
	}
	if len(client.getCalls) != 1 || client.getCalls[0] != (fakeDriveCommentGetCall{FileToken: "doc-token", FileType: "docx", CommentID: "comment-1"}) {
		t.Fatalf("get calls = %+v, want detail lookup", client.getCalls)
	}
	if len(client.replyCalls) != 1 || client.replyTexts[0] != "agent reply" {
		t.Fatalf("reply calls = %+v texts = %+v, want handler reply", client.replyCalls, client.replyTexts)
	}
}

func TestHandleDriveCommentAddContinuesWithoutRetryWhenDetailFails(t *testing.T) {
	client := &fakeDriveCommentClient{
		err: errors.New("飞书获取云文档评论接口返回错误: code=1069307 msg=not exist"),
	}
	handler := &driveCommentRecordingHandler{}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a", BotOpenID: "ou_bot"}, handler)
	adapter.driveComments = client
	adapter.deduper = newMessageDeduper(driveCommentDeduperTTL, driveCommentDeduperMax)

	if err := adapter.handleDriveCommentAdd(context.Background(), driveCommentEvent(true, "ou_user", "ou_bot", "reply-1")); err != nil {
		t.Fatalf("handleDriveCommentAdd() error = %v", err)
	}
	if len(client.getCalls) != 1 {
		t.Fatalf("get calls = %+v, want single detail lookup", client.getCalls)
	}
	if handler.count != 1 || handler.comment.ReplyText != "" {
		t.Fatalf("handled comment = %+v count = %d, want unhydrated comment after detail failure", handler.comment, handler.count)
	}
}

func TestHandleDriveCommentAddSkipsBeforeDetailAndDedupeWhenDisabled(t *testing.T) {
	enabled := false
	client := &fakeDriveCommentClient{}
	handler := &driveCommentRecordingHandler{enabled: &enabled}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a", BotOpenID: "ou_bot"}, handler)
	adapter.driveComments = client
	adapter.deduper = newMessageDeduper(driveCommentDeduperTTL, driveCommentDeduperMax)
	event := driveCommentEvent(true, "ou_user", "ou_bot", "reply-1")

	if err := adapter.handleDriveCommentAdd(context.Background(), event); err != nil {
		t.Fatalf("handleDriveCommentAdd(disabled) error = %v", err)
	}
	if handler.count != 0 || len(client.getCalls) != 0 {
		t.Fatalf("disabled handler/get calls = %d/%d, want both zero", handler.count, len(client.getCalls))
	}

	enabled = true
	if err := adapter.handleDriveCommentAdd(context.Background(), event); err != nil {
		t.Fatalf("handleDriveCommentAdd(enabled) error = %v", err)
	}
	if handler.count != 1 || len(client.getCalls) != 1 {
		t.Fatalf("enabled handler/get calls = %d/%d, want one each; disabled event must not consume dedupe state", handler.count, len(client.getCalls))
	}
}

func TestDriveCommentDetailFromFileComment(t *testing.T) {
	comment := larkdrive.NewFileCommentBuilder().
		CommentId("comment-1").
		UserId("ou_user").
		CreateTime(111).
		UpdateTime(222).
		IsSolved(false).
		IsWhole(false).
		HasMore(true).
		PageToken("reply-next").
		Quote("quote text").
		ReplyList(larkdrive.NewReplyListBuilder().Replies([]*larkdrive.FileCommentReply{
			larkdrive.NewFileCommentReplyBuilder().
				ReplyId("reply-1").
				UserId("ou_user").
				CreateTime(333).
				UpdateTime(444).
				Content(larkdrive.NewReplyContentBuilder().Elements([]*larkdrive.ReplyElement{
					larkdrive.NewReplyElementBuilder().
						Type("person").
						Person(larkdrive.NewPersonBuilder().UserId("ou_bot").Build()).
						Build(),
					larkdrive.NewReplyElementBuilder().
						Type("text_run").
						TextRun(larkdrive.NewTextRunBuilder().Text(" 测试评论文本").Build()).
						Build(),
				}).Build()).
				Build(),
		}).Build()).
		Build()

	detail := driveCommentDetailFromFileComment(comment)
	if detail.CommentID != "comment-1" || detail.UserID != "ou_user" || detail.Quote != "quote text" {
		t.Fatalf("detail = %+v, want parsed comment metadata", detail)
	}
	if detail.CreateTime != 111 || detail.UpdateTime != 222 || detail.IsSolved || detail.IsWhole || !detail.HasMore || detail.PageToken != "reply-next" || detail.RepliesComplete {
		t.Fatalf("detail = %+v, want parsed status, timestamps and pagination", detail)
	}
	if len(detail.Replies) != 1 {
		t.Fatalf("replies = %+v, want one reply", detail.Replies)
	}
	if got := detail.Replies[0]; got.ReplyID != "reply-1" || got.UserID != "ou_user" || got.Text != "@ou_bot测试评论文本" || got.CreateTime != 333 || got.UpdateTime != 444 {
		t.Fatalf("reply = %+v, want parsed reply text", got)
	}

	hydrated := (DriveComment{ReplyID: "reply-1"}).withDetail(detail).Normalized()
	if !hydrated.DetailLoaded || hydrated.CommentUserID != "ou_user" || hydrated.Quote != "quote text" || hydrated.CommentCreateTime != 111 || hydrated.CommentUpdateTime != 222 || hydrated.CommentIsSolved || hydrated.CommentIsWhole {
		t.Fatalf("hydrated comment = %+v, want comment metadata", hydrated)
	}
	if hydrated.CommentText != "@ou_bot测试评论文本" || hydrated.ReplyUserID != "ou_user" || hydrated.ReplyText != "@ou_bot测试评论文本" || hydrated.ReplyCreateTime != 333 || hydrated.ReplyUpdateTime != 444 || hydrated.ReplyCount != 1 || hydrated.RepliesComplete {
		t.Fatalf("hydrated comment = %+v, want reply metadata", hydrated)
	}
}

func TestDriveReplyContentFromTextParsesAtTags(t *testing.T) {
	content := driveReplyContentFromText(` <at id=ou_user></at> 收到 <at id="ou_other"></at> `)
	if content == nil || len(content.Elements) != 3 {
		t.Fatalf("content = %+v, want 3 reply elements", content)
	}
	if got := content.Elements[0]; got.Type == nil || *got.Type != "person" || got.Person == nil || value(got.Person.UserId) != "ou_user" {
		t.Fatalf("first element = %+v, want person mention ou_user", got)
	}
	if got := content.Elements[1]; got.Type == nil || *got.Type != "text_run" || got.TextRun == nil || strings.TrimSpace(value(got.TextRun.Text)) != "收到" {
		t.Fatalf("second element = %+v, want text", got)
	}
	if got := content.Elements[2]; got.Type == nil || *got.Type != "person" || got.Person == nil || value(got.Person.UserId) != "ou_other" {
		t.Fatalf("third element = %+v, want person mention ou_other", got)
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
		t.Fatalf("handler count = %d, want dispatcher to route cloud document comment event", handler.count)
	}
	if len(client.getCalls) != 1 {
		t.Fatalf("get calls = %d, want detail lookup through dispatcher", len(client.getCalls))
	}
	if len(client.replyTexts) != 1 || client.replyTexts[0] != "agent reply" {
		t.Fatalf("reply texts = %+v, want handler reply through dispatcher", client.replyTexts)
	}
}

func TestHandleDriveCommentAddFetchesUnmentionedAndSkipsDuplicateAndBotSelf(t *testing.T) {
	client := &fakeDriveCommentClient{detail: DriveCommentDetail{
		CommentID: "comment-1",
		UserID:    "ou_user",
		Replies: []DriveCommentReply{
			{ReplyID: "reply-1", UserID: "ou_user", Text: "reply text"},
			{ReplyID: "reply-2", UserID: "ou_user", Text: "mentioned reply"},
		},
	}}
	handler := &driveCommentRecordingHandler{}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a", BotOpenID: "ou_bot"}, handler)
	adapter.driveComments = client
	adapter.deduper = newMessageDeduper(driveCommentDeduperTTL, driveCommentDeduperMax)

	if err := adapter.handleDriveCommentAdd(context.Background(), driveCommentEvent(false, "ou_user", "ou_bot", "reply-1")); err != nil {
		t.Fatalf("handle unmentioned error = %v", err)
	}
	if handler.count != 1 || len(client.getCalls) != 1 || handler.comment.IsMentioned {
		t.Fatalf("unmentioned handler/get/comment = %d/%d/%+v, want fetched and dispatched unmentioned event", handler.count, len(client.getCalls), handler.comment)
	}

	event := driveCommentEvent(true, "ou_user", "ou_bot", "reply-2")
	if err := adapter.handleDriveCommentAdd(context.Background(), event); err != nil {
		t.Fatalf("handle first event error = %v", err)
	}
	if err := adapter.handleDriveCommentAdd(context.Background(), event); err != nil {
		t.Fatalf("handle duplicate event error = %v", err)
	}
	if handler.count != 2 {
		t.Fatalf("handler count = %d, want unmentioned event and first mentioned event with duplicate skipped", handler.count)
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
