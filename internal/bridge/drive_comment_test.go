package bridge

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

type driveCommentReplyRecorder struct {
	comments []feishu.DriveComment
	texts    []string
}

func (r *driveCommentReplyRecorder) send(ctx context.Context, comment feishu.DriveComment, text string) error {
	r.comments = append(r.comments, comment)
	r.texts = append(r.texts, text)
	return nil
}

func TestDriveCommentSessionKeyAndPromptMetadata(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace}},
	}, store)
	rt := &fakeRuntime{promptReply: "agent reply"}
	svc.setRuntime(rt)
	replies := &driveCommentReplyRecorder{}
	ctx := feishu.WithDriveCommentReplySender(context.Background(), replies.send)

	comment := feishu.DriveComment{
		BotID:       "bot-a",
		Workspace:   workspace,
		FileToken:   "doc-token",
		FileType:    "docx",
		CommentID:   "comment-1",
		ReplyID:     "reply-1",
		NoticeType:  "add_reply",
		OperatorID:  "ou_user",
		RecipientID: "ou_bot",
		IsMentioned: true,
		CommentText: "root text",
		ReplyText:   "reply text",
		DocumentURL: "https://example.test/doc",
	}
	if err := svc.HandleDriveComment(ctx, comment); err != nil {
		t.Fatalf("HandleDriveComment() error = %v", err)
	}

	wantKey := SessionKey{BotID: "bot-a", Source: sessionSourceDriveComment, MainID: "docx:doc-token", SubID: "comment-1"}
	if len(rt.newCalls) != 1 || rt.newCalls[0].Key != wantKey || rt.newCalls[0].AgentName != "traex" || rt.newCalls[0].Cwd != cwd || rt.newCalls[0].Workspace != workspace {
		t.Fatalf("new calls = %+v, want drive comment session", rt.newCalls)
	}
	if len(rt.promptCalls) != 1 || rt.promptCalls[0].Session.Key != wantKey {
		t.Fatalf("prompt calls = %+v, want drive comment key", rt.promptCalls)
	}
	prompt := rt.promptCalls[0].Text
	assertPromptContainsSectionMetadata(t, prompt, "## Drive Comment Metadata", map[string]string{
		"source":            sessionSourceDriveComment,
		"file_token":        "doc-token",
		"file_type":         "docx",
		"comment_id":        "comment-1",
		"reply_id":          "reply-1",
		"notice_type":       "add_reply",
		"operator_open_id":  "ou_user",
		"recipient_open_id": "ou_bot",
		"comment_content":   "root text",
		"reply_content":     "reply text",
		"document_url":      "https://example.test/doc",
	})
	if !strings.Contains(prompt, "## User Message") || !strings.Contains(prompt, "reply text") {
		t.Fatalf("prompt = %q, want user message from reply text", prompt)
	}
	if len(replies.texts) != 1 || replies.texts[0] != "agent reply" {
		t.Fatalf("replies = %+v, want final result reply", replies.texts)
	}
	if got := rt.wikiRuntimeCallCount(); got != 0 {
		t.Fatalf("wiki runtime calls = %d, want none for drive comment trigger", got)
	}
}

func TestDriveCommentReusesSameCommentSessionAndIsolatesDifferentComments(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace}},
	}, store)
	rt := &fakeRuntime{
		newSessionIDs: []string{"acp-comment-1", "acp-comment-2"},
		promptReply:   "ok",
	}
	svc.setRuntime(rt)
	ctx := feishu.WithDriveCommentReplySender(context.Background(), (&driveCommentReplyRecorder{}).send)

	base := feishu.DriveComment{
		BotID:       "bot-a",
		Workspace:   workspace,
		FileToken:   "doc-token",
		FileType:    "docx",
		CommentID:   "comment-1",
		IsMentioned: true,
		CommentText: "first",
	}
	if err := svc.HandleDriveComment(ctx, base); err != nil {
		t.Fatalf("HandleDriveComment(first) error = %v", err)
	}
	base.ReplyID = "reply-2"
	base.ReplyText = "second"
	if err := svc.HandleDriveComment(ctx, base); err != nil {
		t.Fatalf("HandleDriveComment(second same comment) error = %v", err)
	}
	other := base
	other.CommentID = "comment-2"
	other.ReplyID = "reply-3"
	if err := svc.HandleDriveComment(ctx, other); err != nil {
		t.Fatalf("HandleDriveComment(other comment) error = %v", err)
	}

	if len(rt.newCalls) != 2 {
		t.Fatalf("new calls = %+v, want one session for comment-1 and one for comment-2", rt.newCalls)
	}
	if rt.newCalls[0].Key.SubID != "comment-1" || rt.newCalls[1].Key.SubID != "comment-2" {
		t.Fatalf("new call keys = %+v, want isolated comments", rt.newCalls)
	}
	if len(rt.promptCalls) != 3 {
		t.Fatalf("prompt calls = %+v, want all comments prompted", rt.promptCalls)
	}
	if rt.promptCalls[0].Session.ACPSessionID != rt.promptCalls[1].Session.ACPSessionID {
		t.Fatalf("same comment sessions = %q/%q, want reused ACP session", rt.promptCalls[0].Session.ACPSessionID, rt.promptCalls[1].Session.ACPSessionID)
	}
	if rt.promptCalls[1].Session.ACPSessionID == rt.promptCalls[2].Session.ACPSessionID {
		t.Fatalf("different comment sessions both %q, want isolated ACP sessions", rt.promptCalls[1].Session.ACPSessionID)
	}
}

func TestDriveCommentSkipsUnmentionedAndRequiresDefaultCwd(t *testing.T) {
	workspace := t.TempDir()
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace}},
	}, NewSessionStore(filepath.Join(workspace, "sessions.json")))
	rt := &fakeRuntime{promptReply: "ok"}
	svc.setRuntime(rt)

	if err := svc.HandleDriveComment(context.Background(), feishu.DriveComment{
		BotID:       "bot-a",
		Workspace:   workspace,
		FileToken:   "doc-token",
		FileType:    "docx",
		CommentID:   "comment-1",
		IsMentioned: false,
	}); err != nil {
		t.Fatalf("HandleDriveComment(unmentioned) error = %v", err)
	}
	if len(rt.promptCalls) != 0 {
		t.Fatalf("prompt calls = %+v, want unmentioned skipped", rt.promptCalls)
	}

	err := svc.HandleDriveComment(context.Background(), feishu.DriveComment{
		BotID:       "bot-a",
		Workspace:   workspace,
		FileToken:   "doc-token",
		FileType:    "docx",
		CommentID:   "comment-1",
		IsMentioned: true,
	})
	if err == nil || !strings.Contains(err.Error(), "default_cwd") {
		t.Fatalf("HandleDriveComment(missing default cwd) error = %v, want default_cwd error", err)
	}
}

func TestDriveCommentRepliesErrorWhenTriggerFails(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace}},
	}, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-comment-1"},
		promptErrors:   []error{errors.New("boom")},
	}
	svc.setRuntime(rt)
	replies := &driveCommentReplyRecorder{}
	ctx := feishu.WithDriveCommentReplySender(context.Background(), replies.send)

	err := svc.HandleDriveComment(ctx, feishu.DriveComment{
		BotID:       "bot-a",
		Workspace:   workspace,
		FileToken:   "doc-token",
		FileType:    "docx",
		CommentID:   "comment-1",
		IsMentioned: true,
		CommentText: "please handle",
	})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("HandleDriveComment() error = %v, want boom", err)
	}
	if len(replies.texts) != 1 || replies.texts[0] != "处理评论失败：boom" {
		t.Fatalf("replies = %+v, want trigger failure reply", replies.texts)
	}
	if len(replies.comments) != 1 || replies.comments[0].CommentID != "comment-1" {
		t.Fatalf("reply comments = %+v, want original comment target", replies.comments)
	}
}
