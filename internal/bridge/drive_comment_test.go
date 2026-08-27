package bridge

import (
	"context"
	"encoding/json"
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

func (r *driveCommentReplyRecorder) Outbound() {}

func (r *driveCommentReplyRecorder) ReplyDriveComment(ctx context.Context, comment feishu.DriveComment, text string) error {
	r.comments = append(r.comments, comment)
	r.texts = append(r.texts, text)
	return nil
}

func driveCommentEnabledBotConfig(workspace string) []config.BotConfig {
	return []config.BotConfig{{
		ID:        "bot-a",
		Workspace: workspace,
		DriveComment: config.DriveCommentConfig{
			Enabled: true,
		},
	}}
}

func TestDriveCommentDisabledByDefaultSkipsNewComment(t *testing.T) {
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
	svc.setOutbound("bot-a", replies)

	if err := svc.HandleDriveComment(context.Background(), feishu.DriveComment{
		BotID:       "bot-a",
		Workspace:   workspace,
		FileToken:   "doc-token",
		FileType:    "docx",
		CommentID:   "comment-1",
		IsMentioned: true,
		CommentText: "please handle",
	}); err != nil {
		t.Fatalf("HandleDriveComment() error = %v", err)
	}
	if len(rt.newCalls) != 0 || len(rt.promptCalls) != 0 {
		t.Fatalf("runtime calls = %d/%d, want none when drive_comment disabled", len(rt.newCalls), len(rt.promptCalls))
	}
	if len(replies.texts) != 0 {
		t.Fatalf("replies = %+v, want none when drive_comment disabled", replies.texts)
	}
}

func TestDriveCommentSessionKeyAndPromptMetadata(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
		Bots:      driveCommentEnabledBotConfig(workspace),
	}, store)
	rt := &fakeRuntime{promptReply: "agent reply"}
	svc.setRuntime(rt)
	replies := &driveCommentReplyRecorder{}
	svc.setOutbound("bot-a", replies)
	ctx := context.Background()

	comment := feishu.DriveComment{
		BotID:             "bot-a",
		Workspace:         workspace,
		FileToken:         "doc-token",
		FileType:          "docx",
		CommentID:         "comment-1",
		ReplyID:           "reply-1",
		NoticeType:        "add_reply",
		OperatorID:        "ou_user",
		RecipientID:       "ou_bot",
		IsMentioned:       true,
		DetailLoaded:      true,
		CommentUserID:     "ou_comment_user",
		CommentText:       "root text",
		CommentCreateTime: 111,
		CommentUpdateTime: 222,
		CommentIsSolved:   false,
		CommentIsWhole:    false,
		Quote:             "quote text",
		ReplyUserID:       "ou_reply_user",
		ReplyText:         "reply text",
		ReplyCreateTime:   333,
		ReplyUpdateTime:   444,
		ReplyCount:        6,
		RepliesComplete:   true,
		Replies: []feishu.DriveCommentReply{
			{ReplyID: "comment-root", UserID: "ou_comment_user", Text: "root text", CreateTime: 111, UpdateTime: 222},
			{ReplyID: "reply-0", UserID: "ou_other", Text: "older reply", CreateTime: 300, UpdateTime: 300},
			{ReplyID: "reply-1", UserID: "ou_reply_user", Text: "reply text", CreateTime: 333, UpdateTime: 444},
		},
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
	assertPromptContainsSectionMetadata(t, prompt, "## 云文档评论 Metadata", map[string]string{
		"source":              sessionSourceDriveComment,
		"file_token":          "doc-token",
		"file_type":           "docx",
		"comment_id":          "comment-1",
		"reply_id":            "reply-1",
		"notice_type":         "add_reply",
		"is_mentioned":        "true",
		"operator_open_id":    "ou_user",
		"recipient_open_id":   "ou_bot",
		"comment_user_id":     "ou_comment_user",
		"comment_create_time": "111",
		"comment_update_time": "222",
		"comment_is_solved":   "false",
		"comment_is_whole":    "false",
		"quote":               "quote text",
		"comment_content":     "root text",
		"reply_user_id":       "ou_reply_user",
		"reply_create_time":   "333",
		"reply_update_time":   "444",
		"reply_content":       "reply text",
		"reply_count":         "6",
		"replies_complete":    "true",
		"document_url":        "https://example.test/doc",
	})
	for _, want := range []string{
		"## 云文档评论线程",
		"引用正文：\nquote text",
		"评论根内容：",
		"[user=ou_comment_user, create_time=111, update_time=222] root text",
		"评论回复列表：",
		"[2. reply-0, user=ou_other, create_time=300, update_time=300] older reply",
		"[3. reply-1, user=ou_reply_user, create_time=333, update_time=444, current_event=true] reply text",
		"## User Message",
		"reply text",
		"## 云文档评论处理规则",
		"如需更多文档正文上下文，可以使用 lark-cli 读取当前云文档正文",
		"不要调用 lark-cli、飞书 API 或其它工具读取、回复、修改当前云文档评论",
		"如果要回复某条 reply，请使用 `<at id=\"ou_openid\"></at>回复内容` 格式",
		"bridge 会把你的最终正文写回评论",
		"本次评论事件提及了当前 bot，必须回复",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want drive comment instruction %q", prompt, want)
		}
	}
	if strings.Contains(prompt, "[1. comment-root") {
		t.Fatalf("prompt = %q, want root reply omitted from reply list", prompt)
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
		Bots:      driveCommentEnabledBotConfig(workspace),
	}, store)
	rt := &fakeRuntime{
		newSessionIDs: []string{"acp-comment-1", "acp-comment-2"},
		promptReply:   "ok",
	}
	svc.setRuntime(rt)
	svc.setOutbound("bot-a", &driveCommentReplyRecorder{})
	ctx := context.Background()

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

func TestDriveCommentSuppressesExplicitEmptyFinalReply(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
		Bots:      driveCommentEnabledBotConfig(workspace),
	}, store)
	rt := &fakeRuntime{
		promptResults: []acp.PromptResult{{Text: "raw result should not be written"}},
		promptUpdates: []acp.PromptUpdate{{
			Update: acp.SessionUpdate{
				SessionUpdate: "tool_call",
				Title:         "Check comment",
			},
		}},
	}
	svc.setRuntime(rt)
	replies := &driveCommentReplyRecorder{}
	svc.setOutbound("bot-a", replies)

	if err := svc.HandleDriveComment(context.Background(), feishu.DriveComment{
		BotID:       "bot-a",
		Workspace:   workspace,
		FileToken:   "doc-token",
		FileType:    "docx",
		CommentID:   "comment-1",
		IsMentioned: true,
		CommentText: "please handle",
	}); err != nil {
		t.Fatalf("HandleDriveComment() error = %v", err)
	}
	if len(replies.texts) != 0 {
		t.Fatalf("replies = %+v, want no default reply for explicit empty final text", replies.texts)
	}
}

func TestDriveCommentMissingBodyRepliesWithoutTriggerPrompt(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
		Bots:      driveCommentEnabledBotConfig(workspace),
	}, store)
	rt := &fakeRuntime{promptReply: "agent reply"}
	svc.setRuntime(rt)
	replies := &driveCommentReplyRecorder{}
	svc.setOutbound("bot-a", replies)
	ctx := context.Background()

	err := svc.HandleDriveComment(ctx, feishu.DriveComment{
		BotID:       "bot-a",
		Workspace:   workspace,
		FileToken:   "doc-token",
		FileType:    "docx",
		CommentID:   "comment-1",
		ReplyID:     "reply-1",
		IsMentioned: true,
	})
	if err != nil {
		t.Fatalf("HandleDriveComment() error = %v", err)
	}
	if len(rt.newCalls) != 0 || len(rt.promptCalls) != 0 {
		t.Fatalf("runtime new/prompt calls = %d/%d, want no ACP trigger for missing body", len(rt.newCalls), len(rt.promptCalls))
	}
	if len(replies.texts) != 1 || replies.texts[0] != driveCommentMissingBodyReply {
		t.Fatalf("replies = %+v, want missing body reply", replies.texts)
	}
	if len(replies.comments) != 1 || replies.comments[0].CommentID != "comment-1" {
		t.Fatalf("reply comments = %+v, want original comment target", replies.comments)
	}
}

func TestDriveCommentTraceStreamsToConfiguredChatAndBindsMessage(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	bots := driveCommentEnabledBotConfig(workspace)
	bots[0].DriveComment.TraceEnabled = true
	bots[0].DriveComment.TraceChatID = "oc_trace"
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
		Bots:      bots,
	}, store)
	rt := &fakeRuntime{
		promptResults: []acp.PromptResult{{Text: "agent reply", StopReason: "end_turn"}},
		promptUpdates: []acp.PromptUpdate{{
			Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Raw:           json.RawMessage(`{"content":"agent reply"}`),
			},
		}},
	}
	svc.setRuntime(rt)
	replies := &driveCommentReplyRecorder{}
	var streamTargets []feishu.Message
	var streamMetas []feishu.StreamCardMeta
	var initialProcesses []string
	streamCard := &fakeStreamCard{message: feishu.SentMessage{MessageID: "om_trace", ChatID: "oc_trace", RootID: "om_trace"}}
	outbound := &fakeSentMessageClient{}
	outbound.driveCommentReplySender = replies.ReplyDriveComment
	outbound.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		streamTargets = append(streamTargets, msg)
		streamMetas = append(streamMetas, options.Meta)
		initialProcesses = append(initialProcesses, options.InitialProcess)
		return streamCard, nil
	}
	svc.setOutbound("bot-a", outbound)

	comment := feishu.DriveComment{
		BotID:       "bot-a",
		Workspace:   workspace,
		FileToken:   "doc-token",
		FileType:    "docx",
		CommentID:   "comment-1",
		IsMentioned: true,
		CommentText: "please handle",
		Quote:       "quoted document text",
		DocumentURL: "https://feishu.cn/docx/doc-token",
	}
	if err := svc.HandleDriveComment(context.Background(), comment); err != nil {
		t.Fatalf("HandleDriveComment() error = %v", err)
	}
	if len(streamTargets) != 1 || streamTargets[0].ChatID != "oc_trace" || streamTargets[0].MessageID != "" {
		t.Fatalf("stream targets = %+v, want new card in trace chat", streamTargets)
	}
	if len(initialProcesses) != 1 || !strings.Contains(initialProcesses[0], "msg: drive\\_comment\\_docx\\_doc-token\\_comment-1") {
		t.Fatalf("initial processes = %+v, want drive comment trace msg id", initialProcesses)
	}
	wantMetadata := "**引用文本：** quoted document text\n**评论内容：** please handle\n**文档链接：** https://feishu.cn/docx/doc-token"
	if len(streamMetas) != 1 || streamMetas[0].Subtitle != "" || streamMetas[0].Metadata != wantMetadata || streamMetas[0].SourceURL != "" || streamMetas[0].Footer != driveCommentStreamCardFooter || !streamMetas[0].HideHeaderIcon {
		t.Fatalf("stream metas = %+v, want comment metadata %q and expected footer", streamMetas, wantMetadata)
	}
	metaUpdates := streamCard.metaUpdatesSnapshot()
	if len(metaUpdates) != 1 || metaUpdates[0].Subtitle != "" || metaUpdates[0].Metadata != wantMetadata || metaUpdates[0].SourceURL != "" || metaUpdates[0].Footer != driveCommentStreamCardFooter || !metaUpdates[0].HideHeaderIcon {
		t.Fatalf("stream meta updates = %+v, want comment metadata %q and expected footer", metaUpdates, wantMetadata)
	}
	if len(replies.texts) != 1 || replies.texts[0] != "agent reply" {
		t.Fatalf("drive comment replies = %+v, want final reply still written to document comment", replies.texts)
	}
	if !streamCard.isClosed() {
		t.Fatal("trace stream card was not closed")
	}
	if got := streamCard.finalTextUpdatesSnapshot(); len(got) != 1 || got[0] != "agent reply" {
		t.Fatalf("final text updates = %+v, want agent reply", got)
	}
	session, binding, ok := store.SessionForMessage("bot-a", "oc_trace", "om_trace")
	wantKey := driveCommentSessionKey(comment)
	if !ok || binding.SessionKey != wantKey || session.Key != wantKey {
		t.Fatalf("message binding ok=%v binding=%+v session=%+v, want trace message bound to drive comment session", ok, binding, session)
	}
}

func TestDriveCommentTraceUsesFinalTextAfterBoundary(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	bots := driveCommentEnabledBotConfig(workspace)
	bots[0].DriveComment.TraceEnabled = true
	bots[0].DriveComment.TraceChatID = "oc_trace"
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
		Bots:      bots,
	}, store)
	rt := &fakeRuntime{
		promptResults: []acp.PromptResult{{Text: "raw result should not replace final text", StopReason: "end_turn"}},
		promptUpdates: []acp.PromptUpdate{
			{Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "先检查。"},
			}},
			{Update: acp.SessionUpdate{
				SessionUpdate: "tool_call",
				ToolCallID:    "tool-1",
				Title:         "读取评论上下文",
				Status:        "in_progress",
			}},
			{Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "最终回复评论。"},
			}},
		},
	}
	svc.setRuntime(rt)
	replies := &driveCommentReplyRecorder{}
	streamCard := &fakeStreamCard{message: feishu.SentMessage{MessageID: "om_trace", ChatID: "oc_trace", RootID: "om_trace"}}
	outbound := &fakeSentMessageClient{}
	outbound.driveCommentReplySender = replies.ReplyDriveComment
	outbound.streamStarter = func(context.Context, feishu.Message, feishu.StreamCardOptions) (feishu.StreamCard, error) {
		return streamCard, nil
	}
	svc.setOutbound("bot-a", outbound)

	comment := feishu.DriveComment{
		BotID:       "bot-a",
		Workspace:   workspace,
		FileToken:   "doc-token",
		FileType:    "docx",
		CommentID:   "comment-1",
		IsMentioned: true,
		CommentText: "please handle",
	}
	if err := svc.HandleDriveComment(context.Background(), comment); err != nil {
		t.Fatalf("HandleDriveComment() error = %v", err)
	}
	if got := streamCard.textUpdatesSnapshot(); len(got) == 0 || got[len(got)-1] != "最终回复评论。" {
		t.Fatalf("text updates = %+v, want final chunk in card text area", got)
	}
	if got := streamCard.finalTextUpdatesSnapshot(); len(got) != 1 || got[0] != "最终回复评论。" {
		t.Fatalf("final text updates = %+v, want final text after tool boundary", got)
	}
	if len(replies.texts) != 1 || replies.texts[0] != "最终回复评论。" {
		t.Fatalf("drive comment replies = %+v, want final text after tool boundary", replies.texts)
	}
}

func TestDriveCommentTraceUsesTraceChatShowConfig(t *testing.T) {
	tests := []struct {
		name        string
		chat        ChatConfig
		wantThought bool
	}{
		{
			name: "default hides thoughts",
			chat: ChatConfig{Key: ChatKey{BotID: "bot-a", ChatID: "oc_trace"}},
		},
		{
			name: "explicit hide thoughts",
			chat: ChatConfig{
				Key:          ChatKey{BotID: "bot-a", ChatID: "oc_trace"},
				ShowThoughts: false,
				HideThoughts: true,
			},
		},
		{
			name: "explicit show thoughts",
			chat: ChatConfig{
				Key:          ChatKey{BotID: "bot-a", ChatID: "oc_trace"},
				ShowThoughts: true,
			},
			wantThought: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			cwd := t.TempDir()
			store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
			if err := store.UpsertChat(tt.chat); err != nil {
				t.Fatalf("UpsertChat(trace show config) error = %v", err)
			}
			bots := driveCommentEnabledBotConfig(workspace)
			bots[0].DriveComment.TraceEnabled = true
			bots[0].DriveComment.TraceChatID = "oc_trace"
			svc := NewService(config.Config{
				AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
				Bots:      bots,
			}, store)
			rt := &fakeRuntime{
				promptResults: []acp.PromptResult{{Text: "NoReply", StopReason: "end_turn"}},
				promptUpdates: []acp.PromptUpdate{{
					Update: acp.SessionUpdate{
						SessionUpdate: "thought_chunk",
						Content:       &acp.ContentBlock{Type: "text", Text: "分析评论上下文。"},
					},
				}},
			}
			svc.setRuntime(rt)
			replies := &driveCommentReplyRecorder{}
			streamCard := &fakeStreamCard{message: feishu.SentMessage{MessageID: "om_trace", ChatID: "oc_trace", RootID: "om_trace"}}
			outbound := &fakeSentMessageClient{}
			outbound.driveCommentReplySender = replies.ReplyDriveComment
			outbound.streamStarter = func(context.Context, feishu.Message, feishu.StreamCardOptions) (feishu.StreamCard, error) {
				return streamCard, nil
			}
			svc.setOutbound("bot-a", outbound)

			comment := feishu.DriveComment{
				BotID:       "bot-a",
				Workspace:   workspace,
				FileToken:   "doc-token",
				FileType:    "docx",
				CommentID:   "comment-1",
				IsMentioned: true,
				CommentText: "please handle",
			}
			if err := svc.HandleDriveComment(context.Background(), comment); err != nil {
				t.Fatalf("HandleDriveComment() error = %v", err)
			}
			process := strings.Join(streamCard.processUpdatesSnapshot(), "\n")
			if tt.wantThought && !strings.Contains(process, "分析评论上下文") {
				t.Fatalf("process updates = %q, want thought from trace chat show config", process)
			}
			if !tt.wantThought && strings.Contains(process, "分析评论上下文") {
				t.Fatalf("process updates = %q, should hide thought by trace chat show config", process)
			}
		})
	}
}

func TestDriveCommentTraceSuppressesExplicitEmptyFinalText(t *testing.T) {
	comment := feishu.DriveComment{
		BotID:       "bot-a",
		FileToken:   "doc-token",
		FileType:    "docx",
		CommentID:   "comment-1",
		IsMentioned: true,
		CommentText: "please handle",
	}
	card := &fakeStreamCard{message: feishu.SentMessage{MessageID: "om_trace", ChatID: "oc_trace", RootID: "om_trace"}}
	sink := &driveCommentTraceSink{
		message: feishu.Message{BotID: "bot-a", ChatID: "oc_trace"},
		cwd:     t.TempDir(),
		comment: comment,
		starter: func(context.Context, feishu.Message, feishu.StreamCardOptions) (feishu.StreamCard, error) {
			return card, nil
		},
	}

	result := TriggerResult{
		Request: TriggerRequest{BotID: "bot-a", Key: driveCommentSessionKey(comment)},
		Session: Session{Key: driveCommentSessionKey(comment), ACPSessionID: "acp-drive"},
		ACPResult: acp.PromptResult{
			StopReason: "end_turn",
		},
		TextSet: true,
	}
	if err := sink.OnComplete(context.Background(), result); err != nil {
		t.Fatalf("OnComplete() error = %v", err)
	}
	if got := card.finalTextUpdatesSnapshot(); len(got) != 0 {
		t.Fatalf("final text updates = %+v, want no default final text for explicit empty", got)
	}
	if !card.isClosed() {
		t.Fatal("trace stream card was not closed")
	}
}

func TestDriveCommentStreamCardMetadataUsesCurrentReplyAndOmitsUnavailableFields(t *testing.T) {
	comment := feishu.DriveComment{
		CommentText: "root comment",
		ReplyText:   "current reply",
		DocumentURL: "https://feishu.cn/docx/doc-token",
	}

	got := driveCommentStreamCardMetadata(comment)
	want := "**评论内容：** current reply\n**文档链接：** https://feishu.cn/docx/doc-token"
	if got != want {
		t.Fatalf("driveCommentStreamCardMetadata() = %q, want %q", got, want)
	}
	if strings.Contains(got, "引用文本：") || strings.Contains(got, "评论者：") || strings.Contains(got, "[查看原文]") {
		t.Fatalf("driveCommentStreamCardMetadata() = %q, want unavailable fields and markdown source link omitted", got)
	}
}

func TestDriveCommentStreamCardMetadataTruncatesQuoteAndCommentByRunes(t *testing.T) {
	comment := feishu.DriveComment{
		Quote:       strings.Repeat("引", driveCommentQuoteMaxRunes+1),
		CommentText: strings.Repeat("评", driveCommentTextMaxRunes+1),
		DocumentURL: "https://feishu.cn/docx/doc-token",
	}

	got := driveCommentStreamCardMetadata(comment)
	want := "**引用文本：** " + strings.Repeat("引", driveCommentQuoteMaxRunes) + "...\n" +
		"**评论内容：** " + strings.Repeat("评", driveCommentTextMaxRunes) + "...\n" +
		"**文档链接：** https://feishu.cn/docx/doc-token"
	if got != want {
		t.Fatalf("driveCommentStreamCardMetadata() = %q, want %q", got, want)
	}
}

func TestDriveCommentTraceReusesFirstCardTopicForSameComment(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	storePath := filepath.Join(workspace, "sessions.json")
	bots := driveCommentEnabledBotConfig(workspace)
	bots[0].DriveComment.TraceEnabled = true
	bots[0].DriveComment.TraceChatID = "oc_trace"
	cfg := config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
		Bots:      bots,
	}
	comment := feishu.DriveComment{
		BotID:       "bot-a",
		Workspace:   workspace,
		FileToken:   "doc-token",
		FileType:    "docx",
		CommentID:   "comment-1",
		IsMentioned: true,
		CommentText: "first comment",
	}
	replies := &driveCommentReplyRecorder{}
	var streamTargets []feishu.Message
	var streamCards []*fakeStreamCard
	newOutbound := func() *fakeSentMessageClient {
		outbound := &fakeSentMessageClient{}
		outbound.driveCommentReplySender = replies.ReplyDriveComment
		outbound.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
			streamTargets = append(streamTargets, msg)
			messageID := "om_trace_root"
			threadID := ""
			rootID := messageID
			if len(streamTargets) > 1 {
				messageID = "om_trace_child"
				threadID = "omt_trace"
				rootID = "om_trace_root"
			}
			card := &fakeStreamCard{message: feishu.SentMessage{
				MessageID: messageID,
				ChatID:    msg.ChatID,
				ThreadID:  threadID,
				RootID:    rootID,
			}}
			streamCards = append(streamCards, card)
			return card, nil
		}
		return outbound
	}

	store := NewSessionStore(storePath)
	svc := NewService(cfg, store)
	svc.setRuntime(&fakeRuntime{promptResults: []acp.PromptResult{{Text: "first reply", StopReason: "end_turn"}}})
	svc.setOutbound("bot-a", newOutbound())
	if err := svc.HandleDriveComment(context.Background(), comment); err != nil {
		t.Fatalf("HandleDriveComment(first) error = %v", err)
	}

	reloaded := NewSessionStore(storePath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	svc = NewService(cfg, reloaded)
	svc.setRuntime(&fakeRuntime{promptResults: []acp.PromptResult{{Text: "second reply", StopReason: "end_turn"}}})
	svc.setOutbound("bot-a", newOutbound())
	comment.ReplyID = "reply-2"
	comment.ReplyText = "second comment"
	if err := svc.HandleDriveComment(context.Background(), comment); err != nil {
		t.Fatalf("HandleDriveComment(second) error = %v", err)
	}

	if len(streamTargets) != 2 {
		t.Fatalf("stream targets = %+v, want two cards", streamTargets)
	}
	if got := streamTargets[0]; got.ChatID != "oc_trace" || got.MessageID != "" || got.ForceReplyInThread {
		t.Fatalf("first stream target = %+v, want new root card", got)
	}
	if got := streamTargets[1]; got.ChatID != "oc_trace" || got.MessageID != "om_trace_root" || !got.ForceReplyInThread {
		t.Fatalf("second stream target = %+v, want reply to first card in its topic", got)
	}
	if len(streamCards) != 2 || !streamCards[0].isClosed() || !streamCards[1].isClosed() {
		t.Fatalf("stream cards = %+v, want both closed", streamCards)
	}
	if len(replies.texts) != 2 || replies.texts[0] != "first reply" || replies.texts[1] != "second reply" {
		t.Fatalf("drive comment replies = %+v, want both final replies", replies.texts)
	}
	first, ok := reloaded.FirstMessageForSession("bot-a", "oc_trace", driveCommentSessionKey(comment))
	if !ok || first.MessageID != "om_trace_root" {
		t.Fatalf("first trace binding = %+v, %v, want om_trace_root", first, ok)
	}
}

func TestDriveCommentUnmentionedUsesSilentAutoJudgement(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
		Bots:      driveCommentEnabledBotConfig(workspace),
	}, store)
	rt := &fakeRuntime{promptResults: []acp.PromptResult{
		{Text: "Context compacted Heads up: Long threads and multiple compactions can cause the model to be less accurate. Start a new thread when possible to keep threads small and targeted.SILENT"},
		{Text: "需要回复"},
	}}
	svc.setRuntime(rt)
	replies := &driveCommentReplyRecorder{}
	svc.setOutbound("bot-a", replies)
	ctx := context.Background()

	comment := feishu.DriveComment{
		BotID:       "bot-a",
		Workspace:   workspace,
		FileToken:   "doc-token",
		FileType:    "docx",
		CommentID:   "comment-1",
		ReplyID:     "reply-1",
		IsMentioned: false,
		CommentText: "root text",
		ReplyText:   "reply text",
		Replies: []feishu.DriveCommentReply{
			{ReplyID: "comment-root", UserID: "ou_user", Text: "root text"},
			{ReplyID: "reply-1", UserID: "ou_user", Text: "reply text"},
		},
	}
	if err := svc.HandleDriveComment(ctx, comment); err != nil {
		t.Fatalf("HandleDriveComment(silent) error = %v", err)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("prompt calls = %+v, want unmentioned comment to be judged by ACP", rt.promptCalls)
	}
	if len(replies.texts) != 0 {
		t.Fatalf("replies = %+v, want SILENT suppressed", replies.texts)
	}
	prompt := rt.promptCalls[0].Text
	for _, want := range []string{
		"本次评论事件没有提及当前 bot，请先判断是否需要回复",
		"最终只输出 SILENT",
		"## 云文档评论线程",
		"reply text",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}

	comment.ReplyID = "reply-2"
	comment.ReplyText = "follow-up"
	comment.Replies = append(comment.Replies, feishu.DriveCommentReply{ReplyID: "reply-2", UserID: "ou_user", Text: "follow-up"})
	if err := svc.HandleDriveComment(ctx, comment); err != nil {
		t.Fatalf("HandleDriveComment(reply) error = %v", err)
	}
	if len(replies.texts) != 1 || replies.texts[0] != "需要回复" {
		t.Fatalf("replies = %+v, want non-SILENT auto reply", replies.texts)
	}
}

func TestDriveCommentRequiresDefaultCwd(t *testing.T) {
	workspace := t.TempDir()
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
		Bots:      driveCommentEnabledBotConfig(workspace),
	}, NewSessionStore(filepath.Join(workspace, "sessions.json")))
	rt := &fakeRuntime{promptReply: "ok"}
	svc.setRuntime(rt)

	err := svc.HandleDriveComment(context.Background(), feishu.DriveComment{
		BotID:       "bot-a",
		Workspace:   workspace,
		FileToken:   "doc-token",
		FileType:    "docx",
		CommentID:   "comment-1",
		IsMentioned: true,
		CommentText: "please handle",
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
		Bots:      driveCommentEnabledBotConfig(workspace),
	}, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-comment-1"},
		promptErrors:   []error{errors.New("boom")},
	}
	svc.setRuntime(rt)
	replies := &driveCommentReplyRecorder{}
	svc.setOutbound("bot-a", replies)
	ctx := context.Background()

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
