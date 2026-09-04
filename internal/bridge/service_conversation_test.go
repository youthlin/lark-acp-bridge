package bridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

func TestHandleFeishuMessageOmitsReactionPrompt(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workspace := filepath.Join(t.TempDir(), "workspace")
	markWorkspaceBootstrapped(t, workspace)
	if err := store.Upsert(Session{
		Key:          imSessionKey("bot-a", "oc_chat", ""),
		Title:        "test session",
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
	}); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{promptReply: "ACP 回复"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_user",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		Text:      "看一下这个问题",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if rt.promptCallCount() != 1 {
		t.Fatalf("prompt calls = %d, want one prompt", rt.promptCallCount())
	}
	rt.mu.Lock()
	prompt := rt.promptCalls[0].Text
	rt.mu.Unlock()
	for _, want := range []string{
		"## Message Metadata",
		"message_id",
		"om_user",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want message metadata %q", prompt, want)
		}
	}
	if strings.Contains(prompt, "## Feishu Message Reaction") {
		t.Fatalf("prompt = %q, should not contain standalone per-message reaction section", prompt)
	}
}

func TestHandleFeishuMessageNewUsesFirstAgentListEntryByDefault(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	traexDir := t.TempDir()
	claudeDir := t.TempDir()
	cfg := config.Default()
	cfg.AgentList = []config.NamedAgentConfig{
		{
			Name: "traex",
			AgentConfig: config.AgentConfig{
				Command:    "traex",
				Args:       []string{"acp", "serve"},
				DefaultCwd: traexDir,
			},
		},
		{
			Name: "claude",
			AgentConfig: config.AgentConfig{
				Command:    "claude",
				Args:       []string{"acp", "serve"},
				DefaultCwd: claudeDir,
			},
		},
	}
	svc := newTestService(cfg, store)
	rt := &fakeRuntime{newSessionID: "acp-session-traex"}
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "agent：traex") || !strings.Contains(reply, "cwd："+traexDir) {
		t.Fatalf("reply = %q, want first agent_list entry traex", reply)
	}
	if len(rt.newCalls) != 1 || rt.newCalls[0].AgentName != "traex" || rt.newCalls[0].Cwd != traexDir {
		t.Fatalf("newCalls = %+v, want traex session from first agent_list entry", rt.newCalls)
	}
}

func TestHandleFeishuMessagePromptRecreatesSessionAfterAgentSwitch(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	cfg.SetAgent("claude", config.AgentConfig{
		Command:    "claude",
		Args:       []string{"acp", "serve"},
		DefaultCwd: t.TempDir(),
	})
	cfg.AgentList = []config.NamedAgentConfig{
		{Name: "traex", AgentConfig: mustConfigAgent(t, cfg, "traex")},
		{Name: "claude", AgentConfig: mustConfigAgent(t, cfg, "claude")},
	}
	rt := &fakeRuntime{newSessionID: "acp-session-claude", promptReply: "ACP 回复"}
	svc := newTestService(cfg, store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	if err := store.Upsert(Session{
		Key:          key,
		Title:        "old session",
		AgentName:    "traex",
		ACPSessionID: "acp-session-traex",
		Cwd:          workDir,
	}); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    key.BotID,
		ChatID:   sessionKeyMainID(key),
		ChatType: "p2p",
		Text:     "/agent claude",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/agent claude) error = %v", err)
	}
	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     key.BotID,
		ChatID:    sessionKeyMainID(key),
		ChatType:  "p2p",
		MessageID: "om_msg",
		Text:      "继续",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.newCalls) != 1 || rt.newCalls[0].AgentName != "claude" || rt.newCalls[0].Cwd != workDir {
		t.Fatalf("newCalls = %+v, want recreated claude session with existing cwd", rt.newCalls)
	}
	if len(rt.promptCalls) != 1 || rt.promptCalls[0].Session.AgentName != "claude" {
		t.Fatalf("promptCalls = %+v, want prompt on claude session", rt.promptCalls)
	}
	session, ok := store.Get(key)
	if !ok || session.AgentName != "claude" || session.ACPSessionID != "acp-session-claude" {
		t.Fatalf("session = %+v ok=%v, want current claude session", session, ok)
	}
}

func TestHandleFeishuMessageRejectsEmptyACPCommandName(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{promptReply: "should not run"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/cmds /",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/cmds /) error = %v", err)
	}
	if !strings.Contains(reply, "ACP command 名称不能为空") {
		t.Fatalf("reply = %q, want empty command name error", reply)
	}
	if len(rt.promptCalls) != 0 {
		t.Fatalf("promptCalls = %+v, want no ACP prompt", rt.promptCalls)
	}
}

func TestHandleFeishuMessageContextUsageDropResetsWorkspacePrompted(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.WorkspacePrompted = true
	session.ContextWindow = &acp.ContextWindowUsage{Used: 160000, Size: 200000}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{
		promptResults: []acp.PromptResult{
			{Text: "after compact"},
			{Text: "next done"},
		},
		promptUpdatesByCall: [][]acp.PromptUpdate{
			{
				{
					SessionID: session.ACPSessionID,
					Update: acp.SessionUpdate{
						SessionUpdate: "usage_update",
						Used:          10000,
						Size:          200000,
					},
				},
			},
			nil,
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	_, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "压缩后继续",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt after usage drop) error = %v", err)
	}
	updated, ok := store.Get(session.Key)
	if !ok || updated.WorkspacePrompted || updated.ContextWindow == nil || updated.ContextWindow.Used != 10000 {
		t.Fatalf("updated session = %+v, %v; want usage drop reset workspace prompt", updated, ok)
	}

	_, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "下一条",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(next prompt) error = %v", err)
	}
	calls := rt.promptCallsSnapshot()
	if len(calls) != 2 {
		t.Fatalf("promptCalls = %+v, want two prompts", calls)
	}
	if !strings.Contains(calls[1].Text, "## Workspace Context") || !strings.Contains(calls[1].Text, "## Workspace Memory Policy") {
		t.Fatalf("next prompt = %q, want workspace context and memory policy after usage drop", calls[1].Text)
	}
}

func TestHandleFeishuMessageContextUsageDifferentSizeDoesNotResetWorkspacePrompted(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.WorkspacePrompted = true
	session.ContextWindow = &acp.ContextWindowUsage{Used: 160000, Size: 200000}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{
		promptResult: acp.PromptResult{Text: "model changed"},
		promptUpdatesByCall: [][]acp.PromptUpdate{
			{
				{
					SessionID: session.ACPSessionID,
					Update: acp.SessionUpdate{
						SessionUpdate: "usage_update",
						Used:          10000,
						Size:          100000,
					},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	_, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "模型切换后继续",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt after size change) error = %v", err)
	}
	updated, ok := store.Get(session.Key)
	if !ok || !updated.WorkspacePrompted || updated.ContextWindow == nil || updated.ContextWindow.Used != 10000 || updated.ContextWindow.Size != 100000 {
		t.Fatalf("updated session = %+v, %v; want size change recorded without workspace prompt reset", updated, ok)
	}
}

func TestHandleFeishuMessageNewMigratesLegacySessionShowOptionsToChat(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.Key.SubID = ""
	session.HideStatusBar = true
	session.HideUsageDetail = true
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-new"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ChatType: "group",
		Text:     "/new",
		Mentions: []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "已为当前会话创建 ACP 会话") {
		t.Fatalf("reply = %q, want new session confirmation", reply)
	}
	chat, ok := store.GetChat(chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)}))
	if !ok || !chat.HideStatusBar || !chat.HideUsageDetail {
		t.Fatalf("chat config = %+v, %v; want legacy show options migrated", chat, ok)
	}
	updated, ok := store.Get(session.Key)
	if !ok || updated.HideStatusBar || updated.HideUsageDetail {
		t.Fatalf("new session = %+v, %v; show options should not be stored on new session", updated, ok)
	}
}

func TestHandleFeishuMessageWithoutSessionAutoCreatesSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Text:      "@我的智能助手 你好",
		Mentions: []feishu.Mention{
			testBotMention("我的智能助手"),
		},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.newCalls) != 1 {
		t.Fatalf("newCalls = %+v, want auto-created session", rt.newCalls)
	}
	if rt.newCalls[0].Cwd != workDir {
		t.Fatalf("auto-created cwd = %q, want %q", rt.newCalls[0].Cwd, workDir)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want one prompt", rt.promptCalls)
	}
	assertReadyPromptContainsUserTextAndMemoryPolicy(t, rt.promptCalls[0].Text, "你好")
	if strings.Contains(rt.promptCalls[0].Text, "@我的智能助手") {
		t.Fatalf("prompt text = %q, should strip bot mention", rt.promptCalls[0].Text)
	}
	if _, ok := store.Get(imSessionKey("", "oc_chat", "omt_thread")); !ok {
		t.Fatalf("auto-created session not persisted")
	}
}

func TestHandleFeishuMessagePersistsNewSessionInfoMeta(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{
			SessionID: "acp-session-1",
			Meta: map[string]any{
				"messageCount": 12,
			},
		},
		promptReply: "ACP 回复",
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Text:      "@我的智能助手 你好",
		Mentions: []feishu.Mention{
			testBotMention("我的智能助手"),
		},
	}); err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	session, ok := store.Get(imSessionKey("", "oc_chat", "omt_thread"))
	if !ok {
		t.Fatalf("auto-created session not persisted")
	}
	if got := session.ACPMeta["messageCount"]; got != 12 {
		t.Fatalf("ACPMeta[messageCount] = %v, want 12", got)
	}
}

func TestHandleFeishuMessageIncludesReplyContextInPrompt(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_reply",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		ParentID:  "om_parent",
		Text:      "@我的智能助手 这种情况怎么实现",
		Mentions: []feishu.Mention{
			testBotMention("我的智能助手"),
		},
		Reply: &feishu.ReplyContext{
			MessageID:  "om_parent",
			SenderID:   "ou_parent_sender",
			SenderType: "user",
			MsgType:    "text",
			Text:       "我先发一条消息",
		},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("prompt calls = %+v, want one prompt", rt.promptCalls)
	}
	prompt := rt.promptCalls[0].Text
	for _, want := range []string{"## Replied Message Context", "我先发一条消息", "请结合上面的被回复消息理解下面的用户消息。", "## User Message", "这种情况怎么实现"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
	if strings.LastIndex(prompt, "## User Message") <= strings.Index(prompt, "请结合上面的被回复消息理解下面的用户消息。") {
		t.Fatalf("prompt = %q, want nested user message after reply guidance", prompt)
	}
	assertPromptContainsSectionMetadata(t, prompt, "## Replied Message Metadata", map[string]string{
		"message_id":  "om_parent",
		"sender_id":   "ou_parent_sender",
		"sender_type": "user",
		"msg_type":    "text",
	})
}

func TestHandleFeishuMessageIncludesImageReplyContextInPrompt(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	imagePath := filepath.Join(t.TempDir(), "cache", "img_test_reply_image.png")
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	_, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_reply",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		ParentID:  "om_parent",
		Text:      "@我的智能助手 看看这张图",
		Mentions: []feishu.Mention{
			testBotMention("我的智能助手"),
		},
		Reply: &feishu.ReplyContext{
			MessageID: "om_parent",
			MsgType:   "image",
			ImageKey:  "img_test_reply_image",
			LocalPath: imagePath,
			Images: []feishu.MessageImage{
				{ImageKey: "img_test_reply_image", LocalPath: imagePath},
			},
		},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("prompt calls = %+v, want one prompt", rt.promptCalls)
	}
	prompt := rt.promptCalls[0].Text
	for _, want := range []string{"## Replied Message Context", "[图片消息]", "img_test_reply_image", imagePath, "看看这张图"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
}

func TestHandleFeishuImageMessagePromptsWithLocalPath(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	imagePath := filepath.Join(t.TempDir(), "cache", "img_v3_direct.png")
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_image",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		MsgType:   "image",
		ImageKey:  "img_v3_direct",
		LocalPath: imagePath,
		Images: []feishu.MessageImage{
			{ImageKey: "img_v3_direct", LocalPath: imagePath},
		},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("prompt calls = %+v, want one prompt", rt.promptCalls)
	}
	prompt := rt.promptCalls[0].Text
	for _, want := range []string{"[图片消息]", "image_key: img_v3_direct", "local_path: " + imagePath} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
}

func TestHandleFeishuUnsupportedEmptyMessageDoesNotPrompt(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		Workspace: filepath.Join(t.TempDir(), "not-ready-workspace"),
		MessageID: "om_unsupported",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		MsgType:   "unsupported",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if reply != "暂不支持的消息类型。" {
		t.Fatalf("reply = %q, want unsupported message type reply", reply)
	}
	if len(rt.newCalls) != 0 || len(rt.promptCalls) != 0 {
		t.Fatalf("runtime calls = new %+v prompt %+v, want no ACP calls", rt.newCalls, rt.promptCalls)
	}
}

func TestHandleFeishuAtAutoUnsupportedMessageStaysSilent(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := ChatKey{BotID: "bot-a", ChatID: "oc_chat"}
	if err := store.UpsertChat(ChatConfig{Key: key, MentionOptional: true, AtMode: atModeAuto}); err != nil {
		t.Fatalf("UpsertChat() error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		Workspace: filepath.Join(t.TempDir(), "workspace"),
		MessageID: "om_unsupported_auto",
		ChatID:    key.ChatID,
		ChatType:  "group",
		MsgType:   "unsupported",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want silent at-auto unsupported message", reply)
	}
	if len(rt.newCalls) != 0 || len(rt.promptCalls) != 0 {
		t.Fatalf("runtime calls = new %+v prompt %+v, want no ACP calls", rt.newCalls, rt.promptCalls)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_unknown_command_auto",
		ChatID:    key.ChatID,
		ChatType:  "group",
		Text:      "/unknown",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/unknown) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want silent at-auto unsupported command", reply)
	}
	if len(rt.newCalls) != 0 || len(rt.promptCalls) != 0 {
		t.Fatalf("runtime calls = new %+v prompt %+v, want still no ACP calls", rt.newCalls, rt.promptCalls)
	}
}

func TestHandleFeishuPrivateChatReusesChatSessionUntilNew(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = firstDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_private_1",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "你好",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(first prompt) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.newCalls) != 1 || rt.newCalls[0].Key.SubID != "" {
		t.Fatalf("newCalls = %+v, want one chat-level private session", rt.newCalls)
	}
	if _, ok := store.Get(imSessionKey("bot-a", "oc_private", "")); !ok {
		t.Fatalf("private chat session not persisted by chat id")
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_private_2",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		ThreadID:  "omt_should_not_split",
		Text:      "继续",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(second prompt) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.newCalls) != 1 {
		t.Fatalf("newCalls = %+v, want private prompt to reuse chat session", rt.newCalls)
	}
	if len(rt.promptCalls) != 2 || rt.promptCalls[1].Session.Key.SubID != "" {
		t.Fatalf("promptCalls = %+v, want second prompt on same private chat session", rt.promptCalls)
	}
	assertPromptContainsUserTextOnly(t, rt.promptCalls[1].Text, "继续")

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_private_3",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "/new " + secondDir,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "已为当前会话创建 ACP 会话") {
		t.Fatalf("reply = %q, want new session confirmation", reply)
	}
	if len(rt.newCalls) != 2 {
		t.Fatalf("newCalls = %+v, want /new to create a new private session", rt.newCalls)
	}
	session, ok := store.Get(imSessionKey("bot-a", "oc_private", ""))
	if !ok {
		t.Fatalf("private chat session not found after /new")
	}
	if session.Cwd != secondDir {
		t.Fatalf("private session cwd = %q, want %q", session.Cwd, secondDir)
	}
}

func TestHandleFeishuGroupChatReusesChatSessionWithoutTopic(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	for _, msg := range []feishu.Message{
		{
			BotID:     "bot-a",
			MessageID: "om_group_1",
			ChatID:    "oc_group",
			ChatType:  "group",
			Text:      "@智能助手 你好",
			Mentions:  []feishu.Mention{testBotMention("智能助手")},
		},
		{
			BotID:     "bot-a",
			MessageID: "om_group_2",
			ChatID:    "oc_group",
			ChatType:  "group",
			Text:      "@智能助手 继续",
			Mentions:  []feishu.Mention{testBotMention("智能助手")},
		},
	} {
		reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
		if err != nil {
			t.Fatalf("HandleFeishuMessage(%s) error = %v", msg.MessageID, err)
		}
		if reply != "ACP 回复" {
			t.Fatalf("reply = %q, want ACP reply", reply)
		}
	}
	if len(rt.newCalls) != 1 {
		t.Fatalf("newCalls = %+v, want ordinary group chat to reuse one session", rt.newCalls)
	}
	if len(rt.promptCalls) != 2 {
		t.Fatalf("promptCalls = %+v, want two prompts", rt.promptCalls)
	}
	if rt.newCalls[0].Key.SubID != "" || rt.promptCalls[1].Session.Key.SubID != "" {
		t.Fatalf("session keys = new %+v prompt %+v, want chat-level group session", rt.newCalls[0].Key, rt.promptCalls[1].Session.Key)
	}
	if _, ok := store.Get(imSessionKey("bot-a", "oc_group", "")); !ok {
		t.Fatalf("ordinary group chat session not persisted by chat id")
	}
}

func TestHandleFeishuOrdinaryGroupThreadIDReusesChatSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	for _, msg := range []feishu.Message{
		{
			BotID:     "bot-a",
			MessageID: "om_group_1",
			ChatID:    "oc_group",
			ChatType:  "group",
			Text:      "@智能助手 你好",
			Mentions:  []feishu.Mention{testBotMention("智能助手")},
		},
		{
			BotID:            "bot-a",
			MessageID:        "om_group_thread_reply",
			ChatID:           "oc_group",
			ChatType:         "group",
			GroupMessageType: "chat",
			ThreadID:         "omt_group_thread",
			RootID:           "om_group_1",
			ParentID:         "om_group_1",
			Text:             "@智能助手 继续",
			Mentions:         []feishu.Mention{testBotMention("智能助手")},
		},
	} {
		reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
		if err != nil {
			t.Fatalf("HandleFeishuMessage(%s) error = %v", msg.MessageID, err)
		}
		if reply != "ACP 回复" {
			t.Fatalf("reply = %q, want ACP reply", reply)
		}
	}
	if len(rt.promptCalls) != 2 {
		t.Fatalf("promptCalls = %+v, want two prompts", rt.promptCalls)
	}
	if len(rt.newCalls) != 1 {
		t.Fatalf("newCalls = %+v, want ordinary group thread id to reuse chat session", rt.newCalls)
	}
	if rt.newCalls[0].Key.SubID != "" || rt.promptCalls[0].Session.Key.SubID != "" || rt.promptCalls[1].Session.Key.SubID != "" {
		t.Fatalf("session keys = new %+v prompts %+v, want chat-level group session", rt.newCalls[0].Key, rt.promptCalls)
	}
	if _, ok := store.Get(imSessionKey("bot-a", "oc_group", "omt_group_thread")); ok {
		t.Fatalf("ordinary group thread id should not create thread session")
	}
}

func TestSessionKeysFromMessageUsesThreadKeyForGroupTopics(t *testing.T) {
	tests := []struct {
		name string
		msg  feishu.Message
		want []SessionKey
	}{
		{
			name: "ordinary group with thread id stays chat scoped",
			msg:  feishu.Message{BotID: "bot-a", ChatID: "oc_group", ChatType: "group", GroupMessageType: "chat", ThreadID: "omt_group_thread"},
			want: []SessionKey{imSessionKey("bot-a", "oc_group", "")},
		},
		{
			name: "topic group with thread id uses topic scoped keys",
			msg:  feishu.Message{BotID: "bot-a", ChatID: "oc_group", ChatType: "group", GroupMessageType: "thread", ThreadID: "omt_group_thread"},
			want: []SessionKey{imSessionKey("bot-a", "oc_group", "omt_group_thread")},
		},
		{
			name: "topic chat mode without group message type uses topic scoped keys",
			msg:  feishu.Message{BotID: "bot-a", ChatID: "oc_group", ChatType: "group", ChatMode: "topic", ThreadID: "omt_group_thread"},
			want: []SessionKey{imSessionKey("bot-a", "oc_group", "omt_group_thread")},
		},
		{
			name: "private chat with thread id stays chat scoped",
			msg:  feishu.Message{BotID: "bot-a", ChatID: "oc_private", ChatType: "p2p", ThreadID: "omt_private_thread"},
			want: []SessionKey{imSessionKey("bot-a", "oc_private", "")},
		},
		{
			name: "unknown chat type with thread id stays chat scoped",
			msg:  feishu.Message{BotID: "bot-a", ChatID: "oc_group", ThreadID: "omt_unknown_thread"},
			want: []SessionKey{imSessionKey("bot-a", "oc_group", "")},
		},
		{
			name: "topic group with thread id uses topic scoped keys",
			msg: feishu.Message{
				BotID:            "bot-a",
				ChatID:           "oc_topic",
				ChatType:         "topic_group",
				GroupMessageType: "thread",
				ThreadID:         "omt_topic",
				RootID:           "om_root",
				ParentID:         "om_parent",
				MessageID:        "om_msg",
			},
			want: []SessionKey{imSessionKey("bot-a", "oc_topic", "omt_topic")},
		},
		{
			name: "topic group without thread id falls back to current message id",
			msg: feishu.Message{
				BotID:            "bot-a",
				ChatID:           "oc_topic",
				ChatType:         "topic_group",
				GroupMessageType: "thread",
				RootID:           "om_root",
				ParentID:         "om_parent",
				MessageID:        "om_msg",
			},
			want: []SessionKey{imSessionKey("bot-a", "oc_topic", "om_msg")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionKeysFromMessage(tt.msg); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("sessionKeysFromMessage() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestHandleFeishuGroupChatRequiresMentionByDefault(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_group_1",
		ChatID:    "oc_group",
		ChatType:  "group",
		Text:      "你好",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group no mention) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want silent ignore", reply)
	}
	if len(rt.newCalls) != 0 || len(rt.promptCalls) != 0 {
		t.Fatalf("runtime calls = new %+v prompt %+v, want no ACP calls", rt.newCalls, rt.promptCalls)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_group_2",
		ChatID:    "oc_group",
		ChatType:  "group",
		Text:      "@智能助手 你好",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group mention) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.newCalls) != 1 || len(rt.promptCalls) != 1 {
		t.Fatalf("runtime calls = new %+v prompt %+v, want one ACP prompt", rt.newCalls, rt.promptCalls)
	}
}

func TestHandleFeishuTopicGroupRequiresMentionByDefault(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_topic_ignored",
		ChatID:    "oc_group",
		ChatType:  "topic_group",
		ThreadID:  "omt_topic",
		Text:      "话题群里没有 at",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(topic no mention) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want silent ignore", reply)
	}
	if len(rt.newCalls) != 0 || len(rt.promptCalls) != 0 {
		t.Fatalf("runtime calls = new %+v prompt %+v, want no ACP calls", rt.newCalls, rt.promptCalls)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_topic_status",
		ChatID:    "oc_group",
		ChatType:  "topic_group",
		ThreadID:  "omt_topic",
		Text:      "@智能助手 /at status",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(topic /at status) error = %v", err)
	}
	if !strings.Contains(reply, "需要 at 才响应") {
		t.Fatalf("reply = %q, want topic group to support at status", reply)
	}
}

func TestHandleFeishuGroupChatCachesMessagesUntilNextMention(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	ctx := context.Background()

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_group_first_mention",
		ChatID:    "oc_group",
		ChatType:  "group",
		Text:      "@智能助手 第一轮",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(first mention) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}

	for _, msg := range []feishu.Message{
		{
			BotID:     "bot-a",
			MessageID: "om_group_cached_1",
			ChatID:    "oc_group",
			ChatType:  "group",
			SenderID:  "ou_b",
			Text:      "b 的补充",
		},
		{
			BotID:     "bot-a",
			MessageID: "om_group_cached_2",
			ChatID:    "oc_group",
			ChatType:  "group",
			SenderID:  "ou_c",
			Text:      "c 的补充",
		},
		{
			BotID:     "bot-a",
			MessageID: "om_group_ignored_command",
			ChatID:    "oc_group",
			ChatType:  "group",
			SenderID:  "ou_b",
			Text:      "/at off",
		},
	} {
		reply, err = handleFeishuMessage(t, svc, ctx, msg)
		if err != nil {
			t.Fatalf("HandleFeishuMessage(cached no mention %s) error = %v", msg.MessageID, err)
		}
		if reply != "" {
			t.Fatalf("reply = %q, want silent cache for %s", reply, msg.MessageID)
		}
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want only first mention before cache is consumed", rt.promptCalls)
	}

	reply, err = handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_group_second_mention",
		ChatID:    "oc_group",
		ChatType:  "group",
		SenderID:  "ou_a",
		Text:      "@智能助手 第二轮，你说得对 @用户b,你也看看 @用户c",
		Mentions: []feishu.Mention{
			testBotMention("智能助手"),
			{ID: "ou_b", Name: "用户b", Type: "user"},
			{ID: "ou_c", Name: "用户c", Type: "user"},
		},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(second mention) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.promptCalls) != 2 {
		t.Fatalf("promptCalls = %+v, want second mention to consume cache", rt.promptCalls)
	}
	prompt := rt.promptCalls[1].Text
	for _, want := range []string{
		"## 以下是当前对话历史消息",
		"- （om_group_cached_1）（ou_b）b 的补充",
		"- （om_group_cached_2）（ou_c）c 的补充",
		"## User Message",
		"sender：用户(ou_a)",
		"content：第二轮，你说得对 @用户b(ou_b),你也看看 @用户c(ou_c)",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
	if strings.Index(prompt, "（om_group_cached_1）") > strings.Index(prompt, "content：第二轮") ||
		strings.Index(prompt, "（om_group_cached_2）") > strings.Index(prompt, "content：第二轮") {
		t.Fatalf("prompt = %q, want cached messages before current mention", prompt)
	}
	if strings.Contains(prompt, "/at off") {
		t.Fatalf("prompt = %q, should not cache ignored slash command", prompt)
	}

	reply, err = handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_group_third_mention",
		ChatID:    "oc_group",
		ChatType:  "group",
		Text:      "@智能助手 第三轮",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(third mention) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.promptCalls) != 3 {
		t.Fatalf("promptCalls = %+v, want third prompt after cache was cleared", rt.promptCalls)
	}
	if strings.Contains(rt.promptCalls[2].Text, "b 的补充") || strings.Contains(rt.promptCalls[2].Text, "c 的补充") {
		t.Fatalf("prompt = %q, want pending cache cleared after second mention", rt.promptCalls[2].Text)
	}
}

func TestHandleFeishuTopicGroupPendingMentionCacheIsTopicScoped(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionIDs: []string{"acp-session-topic-2", "acp-session-topic-1"}, promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	ctx := context.Background()

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:            "bot-a",
		MessageID:        "om_topic_1_pending",
		ChatID:           "oc_group",
		ChatType:         "group",
		GroupMessageType: "thread",
		ThreadID:         "omt_topic_1",
		RootID:           "om_topic_1_root",
		ParentID:         "om_topic_1_root",
		SenderID:         "ou_a",
		Text:             "话题1里后续不at的消息",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(topic 1 pending) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want silent cache for topic 1 pending message", reply)
	}

	reply, err = handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:            "bot-a",
		MessageID:        "om_topic_2_mention",
		ChatID:           "oc_group",
		ChatType:         "group",
		GroupMessageType: "thread",
		ThreadID:         "omt_topic_2",
		RootID:           "om_topic_2_root",
		ParentID:         "om_topic_2_root",
		SenderID:         "ou_a",
		Text:             "@智能助手 ping",
		Mentions:         []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(topic 2 mention) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want topic 2 prompt only", rt.promptCalls)
	}
	topic2Prompt := rt.promptCalls[0].Text
	if strings.Contains(topic2Prompt, "话题1里后续不at的消息") || strings.Contains(topic2Prompt, "om_topic_1_pending") {
		t.Fatalf("topic 2 prompt = %q, should not include topic 1 pending message", topic2Prompt)
	}
	if !strings.Contains(topic2Prompt, "ping") {
		t.Fatalf("topic 2 prompt = %q, want current mention content", topic2Prompt)
	}
	if rt.promptCalls[0].Session.Key.SubID != "omt_topic_2" {
		t.Fatalf("topic 2 session key = %+v, want topic scoped session", rt.promptCalls[0].Session.Key)
	}

	reply, err = handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:            "bot-a",
		MessageID:        "om_topic_1_mention",
		ChatID:           "oc_group",
		ChatType:         "group",
		GroupMessageType: "thread",
		ThreadID:         "omt_topic_1",
		RootID:           "om_topic_1_root",
		ParentID:         "om_topic_1_root",
		SenderID:         "ou_a",
		Text:             "@智能助手 总结一下",
		Mentions:         []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(topic 1 mention) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.promptCalls) != 2 {
		t.Fatalf("promptCalls = %+v, want topic 1 prompt after topic 2 prompt", rt.promptCalls)
	}
	topic1Prompt := rt.promptCalls[1].Text
	for _, want := range []string{
		"## 以下是当前对话历史消息",
		"- （om_topic_1_pending）（ou_a）话题1里后续不at的消息",
		"content：总结一下",
	} {
		if !strings.Contains(topic1Prompt, want) {
			t.Fatalf("topic 1 prompt = %q, want %q", topic1Prompt, want)
		}
	}
	if rt.promptCalls[1].Session.Key.SubID != "omt_topic_1" {
		t.Fatalf("topic 1 session key = %+v, want topic scoped session", rt.promptCalls[1].Session.Key)
	}
}

func TestHandleFeishuGroupChatPendingMentionCacheKeepsLastHundredMessages(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	ctx := context.Background()

	for i := 0; i < maxPendingAtMessages+1; i++ {
		reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
			BotID:     "bot-a",
			MessageID: fmt.Sprintf("om_group_cached_%03d", i),
			ChatID:    "oc_group",
			ChatType:  "group",
			SenderID:  fmt.Sprintf("ou_%03d", i),
			Text:      fmt.Sprintf("cached-%03d", i),
		})
		if err != nil {
			t.Fatalf("HandleFeishuMessage(cached %d) error = %v", i, err)
		}
		if reply != "" {
			t.Fatalf("reply = %q, want silent cache for %d", reply, i)
		}
	}
	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_group_mention",
		ChatID:    "oc_group",
		ChatType:  "group",
		SenderID:  "ou_a",
		Text:      "@智能助手 汇总一下",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(mention) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want one prompt after consuming cache", rt.promptCalls)
	}
	prompt := rt.promptCalls[0].Text
	if strings.Contains(prompt, "cached-000") || strings.Contains(prompt, "用户(ou_000)") {
		t.Fatalf("prompt = %q, should drop oldest cached message", prompt)
	}
	for _, want := range []string{
		"- （om_group_cached_001）（ou_001）cached-001",
		"- （om_group_cached_100）（ou_100）cached-100",
		"content：汇总一下",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
}

func TestHandleFeishuGroupChatAtAutoQueuesWhileMentionPromptRuns(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{
		newSessionID:  "acp-session-1",
		promptResults: []acp.PromptResult{{Text: "正常回复"}, {Text: "RESPOND"}, {Text: "需要回复一次"}},
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	key := ChatKey{BotID: "bot-a", ChatID: "oc_group"}
	if err := store.UpsertChat(ChatConfig{Key: key, MentionOptional: true, AtMode: atModeAuto}); err != nil {
		t.Fatalf("UpsertChat() error = %v", err)
	}
	var intermediate queueIntermediateReplies
	type sentAutoReply struct {
		msg  feishu.Message
		text string
	}
	sentAutoReplies := make(chan sentAutoReply, 1)
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate.append(text)
		sentAutoReplies <- sentAutoReply{msg: msg, text: text}
		return nil
	}
	svc.setOutbound("bot-a", client)

	mentionDone := make(chan struct {
		reply string
		err   error
	}, 1)
	go func() {
		reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
			BotID:     "bot-a",
			MessageID: "om_mention",
			ChatID:    key.ChatID,
			ChatType:  "group",
			SenderID:  "ou_a",
			Text:      "@智能助手 先处理这个",
			Mentions:  []feishu.Mention{testBotMention("智能助手")},
		})
		mentionDone <- struct {
			reply string
			err   error
		}{reply: reply, err: err}
	}()
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	for _, msg := range []feishu.Message{
		{
			BotID:     "bot-a",
			MessageID: "om_auto_pending_1",
			ChatID:    key.ChatID,
			ChatType:  "group",
			SenderID:  "ou_b",
			Text:      "无 at 补充 1",
		},
		{
			BotID:     "bot-a",
			MessageID: "om_auto_pending_2",
			ChatID:    key.ChatID,
			ChatType:  "group",
			SenderID:  "ou_c",
			Text:      "无 at 补充 2",
		},
	} {
		reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
		if err != nil {
			t.Fatalf("HandleFeishuMessage(%s) error = %v", msg.MessageID, err)
		}
		if reply != "" {
			t.Fatalf("reply = %q, want queued auto message to stay silent", reply)
		}
	}
	if got := rt.promptCallCount(); got != 1 {
		t.Fatalf("prompt calls = %d, want queued auto messages not to interrupt running mention prompt", got)
	}
	if got := rt.cancelCallCount(); got != 0 {
		t.Fatalf("cancel calls = %d, want queued auto messages not to cancel running mention prompt", got)
	}

	close(rt.blockPrompt)
	select {
	case got := <-mentionDone:
		if got.err != nil {
			t.Fatalf("mention HandleFeishuMessage() error = %v", got.err)
		}
		if got.reply != "正常回复" {
			t.Fatalf("mention reply = %q, want normal reply", got.reply)
		}
	case <-time.After(time.Second):
		t.Fatal("mention prompt did not finish")
	}
	waitForCondition(t, time.Second, func() bool { return rt.atAutoRuntimeCallCount() == 1 && rt.promptCallCount() == 2 })
	rt.mu.Lock()
	calls := append([]fakePromptCall(nil), rt.promptCalls...)
	autoCalls := append([]fakePromptCall(nil), rt.atAutoRuntimeCalls...)
	rt.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("promptCalls = %+v, want mention prompt and one batched auto response prompt", calls)
	}
	if len(autoCalls) != 1 {
		t.Fatalf("atAutoRuntimeCalls = %+v, want one batched auto companion decision", autoCalls)
	}
	if !intermediate.equal([]string{"需要回复一次"}) {
		t.Fatal("intermediate replies should contain one batched auto reply")
	}
	select {
	case sent := <-sentAutoReplies:
		if sent.text != "需要回复一次" {
			t.Fatalf("sent auto reply = %q, want batched reply", sent.text)
		}
		if sent.msg.MessageID != "om_auto_pending_2" || sent.msg.ForceReplyInThread {
			t.Fatalf("sent auto reply target = %+v, want ordinary reply to last pending message", sent.msg)
		}
	default:
		t.Fatal("missing sent auto reply target")
	}
	autoDecisionPrompt := autoCalls[0].Text
	for _, want := range []string{
		"# 群聊自动响应判断",
		"## 以下是待判断是否需要响应的群消息",
		"- （om_auto_pending_1）（ou_b）无 at 补充 1",
		"- （om_auto_pending_2）（ou_c）无 at 补充 2",
		"多条消息中只要任意一条需要主会话响应，就输出 RESPOND",
	} {
		if !strings.Contains(autoDecisionPrompt, want) {
			t.Fatalf("auto decision prompt = %q, want %q", autoDecisionPrompt, want)
		}
	}
	autoResponsePrompt := calls[1].Text
	for _, want := range []string{
		"## User Message",
		"下面是需要处理的群消息：",
		"- （om_auto_pending_1）（ou_b）无 at 补充 1",
		"- （om_auto_pending_2）（ou_c）无 at 补充 2",
		"请结合上下文综合处理，并只回复一次。",
	} {
		if !strings.Contains(autoResponsePrompt, want) {
			t.Fatalf("auto response prompt = %q, want %q", autoResponsePrompt, want)
		}
	}
}

func TestHandleFeishuGroupChatAtAutoQueuesBehindAutoPrompt(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{
		newSessionID:     "acp-session-1",
		promptResults:    []acp.PromptResult{{Text: "SILENT"}, {Text: "RESPOND"}, {Text: "第二轮回复"}},
		blockPrompt:      make(chan struct{}),
		blockPromptAt:    1,
		blockPromptScope: runtimeScopeAtAuto,
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	key := ChatKey{BotID: "bot-a", ChatID: "oc_group"}
	if err := store.UpsertChat(ChatConfig{Key: key, MentionOptional: true, AtMode: atModeAuto}); err != nil {
		t.Fatalf("UpsertChat() error = %v", err)
	}

	firstDone := make(chan struct {
		reply string
		err   error
	}, 1)
	go func() {
		reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
			BotID:     "bot-a",
			MessageID: "om_auto_1",
			ChatID:    key.ChatID,
			ChatType:  "group",
			SenderID:  "ou_a",
			Text:      "无 at 第一条",
		})
		firstDone <- struct {
			reply string
			err   error
		}{reply: reply, err: err}
	}()
	waitForCondition(t, time.Second, func() bool { return rt.atAutoRuntimeCallCount() == 1 })

	for _, msg := range []feishu.Message{
		{
			BotID:     "bot-a",
			MessageID: "om_auto_2",
			ChatID:    key.ChatID,
			ChatType:  "group",
			SenderID:  "ou_b",
			Text:      "无 at 第二条",
		},
		{
			BotID:     "bot-a",
			MessageID: "om_auto_3",
			ChatID:    key.ChatID,
			ChatType:  "group",
			SenderID:  "ou_c",
			Text:      "无 at 第三条",
		},
	} {
		reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
		if err != nil {
			t.Fatalf("HandleFeishuMessage(%s) error = %v", msg.MessageID, err)
		}
		if reply != "" {
			t.Fatalf("reply = %q, want queued auto prompt to stay silent", reply)
		}
	}
	if got := rt.promptCallCount(); got != 0 {
		t.Fatalf("prompt calls = %d, want no main prompts while first auto decision runs", got)
	}
	if got := rt.atAutoRuntimeCallCount(); got != 1 {
		t.Fatalf("at-auto calls = %d, want later auto messages queued while first auto decision runs", got)
	}
	if got := rt.cancelCallCount(); got != 0 {
		t.Fatalf("cancel calls = %d, want queued auto messages not to cancel running auto prompt", got)
	}
	close(rt.blockPrompt)
	select {
	case got := <-firstDone:
		if got.err != nil {
			t.Fatalf("first HandleFeishuMessage() error = %v", got.err)
		}
		if got.reply != "" {
			t.Fatalf("first reply = %q, want cancelled or SILENT-suppressed auto prompt", got.reply)
		}
	case <-time.After(time.Second):
		t.Fatal("first auto prompt did not finish")
	}
	waitForCondition(t, time.Second, func() bool { return rt.atAutoRuntimeCallCount() == 2 && rt.promptCallCount() == 1 })
	rt.mu.Lock()
	calls := append([]fakePromptCall(nil), rt.promptCalls...)
	autoCalls := append([]fakePromptCall(nil), rt.atAutoRuntimeCalls...)
	rt.mu.Unlock()
	if len(autoCalls) != 2 {
		t.Fatalf("atAutoRuntimeCalls = %+v, want original and batched auto companion decisions", autoCalls)
	}
	if len(calls) != 1 {
		t.Fatalf("promptCalls = %+v, want one batched main auto response prompt", calls)
	}
	autoPrompt := autoCalls[1].Text
	for _, want := range []string{
		"# 群聊自动响应判断",
		"## 以下是待判断是否需要响应的群消息",
		"- （om_auto_2）（ou_b）无 at 第二条",
		"- （om_auto_3）（ou_c）无 at 第三条",
		"多条消息中只要任意一条需要主会话响应，就输出 RESPOND",
	} {
		if !strings.Contains(autoPrompt, want) {
			t.Fatalf("auto prompt = %q, want %q", autoPrompt, want)
		}
	}
	mainPrompt := calls[0].Text
	if strings.Contains(mainPrompt, "群聊自动响应判断") || strings.Contains(mainPrompt, "最终只输出 SILENT") {
		t.Fatalf("main prompt = %q, should not contain auto decision rules", mainPrompt)
	}
	for _, want := range []string{
		"- （om_auto_2）（ou_b）无 at 第二条",
		"- （om_auto_3）（ou_c）无 at 第三条",
		"请结合上下文综合处理，并只回复一次。",
	} {
		if !strings.Contains(mainPrompt, want) {
			t.Fatalf("main prompt = %q, want %q", mainPrompt, want)
		}
	}
}

func TestHandleFeishuGroupChatAtAutoRespondsViaMainSessionAfterCompanionDecision(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{
		newSessionID:  "acp-session-1",
		promptResults: []acp.PromptResult{{Text: "RESPOND"}, {Text: "主会话回复"}},
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	key := ChatKey{BotID: "bot-a", ChatID: "oc_group"}
	if err := store.UpsertChat(ChatConfig{Key: key, MentionOptional: true, AtMode: atModeAuto}); err != nil {
		t.Fatalf("UpsertChat() error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_auto_respond",
		ChatID:    key.ChatID,
		ChatType:  "group",
		SenderID:  "ou_a",
		Text:      "这条需要你处理",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if reply != "主会话回复" {
		t.Fatalf("reply = %q, want main session reply", reply)
	}
	autoCalls := rt.atAutoRuntimeCallsSnapshot()
	if len(autoCalls) != 1 || autoCalls[0].Runtime.Scope != runtimeScopeAtAuto {
		t.Fatalf("atAutoRuntimeCalls = %+v, want one companion decision", autoCalls)
	}
	mainCalls := rt.promptCallsSnapshot()
	if len(mainCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want one main session prompt", mainCalls)
	}
	if !strings.Contains(autoCalls[0].Text, "最终只输出 RESPOND") || !strings.Contains(autoCalls[0].Text, "这条需要你处理") {
		t.Fatalf("companion prompt = %q, want decision rules and user text", autoCalls[0].Text)
	}
	if !strings.Contains(mainCalls[0].Text, "这条需要你处理") {
		t.Fatalf("main prompt = %q, want original user text", mainCalls[0].Text)
	}
	for _, unexpected := range []string{"群聊自动响应判断", "最终只输出 SILENT", "最终只输出 RESPOND"} {
		if strings.Contains(mainCalls[0].Text, unexpected) {
			t.Fatalf("main prompt = %q, should not contain %q", mainCalls[0].Text, unexpected)
		}
	}
}

func TestHandleFeishuGroupChatAtAutoKeepsReplacementWaitDelayedWhenSilent(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{
		newSessionID:   "acp-session-1",
		promptResults:  []acp.PromptResult{{Text: "SILENT"}},
		promptUpdates:  []acp.PromptUpdate{{Update: acp.SessionUpdate{SessionUpdate: "agent_message_chunk", Content: &acp.ContentBlock{Type: "text", Text: "SILENT"}}}},
		blockCancel:    make(chan struct{}),
		promptReply:    "SILENT",
		noDefaultState: true,
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_group", ""))
	chatKey := ChatKey{BotID: "bot-a", ChatID: sessionKeyMainID(key)}
	if err := store.UpsertChat(ChatConfig{Key: chatKey, MentionOptional: true, AtMode: atModeAuto}); err != nil {
		t.Fatalf("UpsertChat() error = %v", err)
	}
	session := Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          workDir,
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	var previousDoneOnce sync.Once
	previousDone := make(chan struct{})
	svc.taskMu.Lock()
	svc.tasks[key] = &runningTask{
		kind:                taskKindWiki,
		runtime:             currentRuntimeKey(key),
		cancel:              func() { previousDoneOnce.Do(func() { close(previousDone) }) },
		done:                previousDone,
		predecessorDetached: make(chan struct{}),
		session:             session,
		agent:               agent,
	}
	svc.taskMu.Unlock()

	var cards []*fakeStreamCard
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	done := make(chan struct {
		reply string
		err   error
	}, 1)
	go func() {
		reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
			BotID:     "bot-a",
			MessageID: "om_auto_silent_wait",
			ChatID:    sessionKeyMainID(key),
			ChatType:  "group",
			SenderID:  "ou_a",
			Text:      "路过闲聊",
		})
		done <- struct {
			reply string
			err   error
		}{reply: reply, err: err}
	}()
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() >= 1 })
	if len(cards) != 0 {
		t.Fatalf("cards = %+v, want replacement wait to stay delayed before auto decision", cards)
	}
	close(rt.blockCancel)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("HandleFeishuMessage() error = %v", got.err)
		}
		if got.reply != "" {
			t.Fatalf("reply = %q, want SILENT suppressed", got.reply)
		}
	case <-time.After(time.Second):
		t.Fatal("HandleFeishuMessage did not finish")
	}
	if len(cards) != 0 {
		t.Fatalf("cards = %+v, want no stream card for SILENT auto prompt after replacement wait", cards)
	}
}

func TestHandleFeishuTopicThreadsUseSeparateSessions(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionIDs: []string{"acp-session-1", "acp-session-2"}, promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	for _, msg := range []feishu.Message{
		{
			BotID:     "bot-a",
			MessageID: "om_topic_1",
			ChatID:    "oc_group",
			ChatType:  "topic_group",
			ThreadID:  "omt_topic_1",
			Text:      "@智能助手 topic one",
			Mentions:  []feishu.Mention{testBotMention("智能助手")},
		},
		{
			BotID:     "bot-a",
			MessageID: "om_topic_2",
			ChatID:    "oc_group",
			ChatType:  "topic_group",
			ThreadID:  "omt_topic_2",
			Text:      "@智能助手 topic two",
			Mentions:  []feishu.Mention{testBotMention("智能助手")},
		},
	} {
		reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
		if err != nil {
			t.Fatalf("HandleFeishuMessage(%s) error = %v", msg.MessageID, err)
		}
		if reply != "ACP 回复" {
			t.Fatalf("reply = %q, want ACP reply", reply)
		}
	}
	if len(rt.newCalls) != 2 {
		t.Fatalf("newCalls = %+v, want one session per topic thread", rt.newCalls)
	}
	if rt.newCalls[0].Key.SubID != "omt_topic_1" || rt.newCalls[1].Key.SubID != "omt_topic_2" {
		t.Fatalf("newCalls = %+v, want distinct thread keys", rt.newCalls)
	}
	for _, key := range []SessionKey{
		imSessionKey("bot-a", "oc_group", "omt_topic_1"),
		imSessionKey("bot-a", "oc_group", "omt_topic_2"),
	} {
		if _, ok := store.Get(key); !ok {
			t.Fatalf("topic session %v not persisted", key)
		}
	}
}

func TestHandleFeishuGroupNewTopicUsesThreadSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-topic", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:            "bot-a",
		MessageID:        "om_group_topic",
		ChatID:           "oc_group",
		ChatType:         "group",
		GroupMessageType: "thread",
		ThreadID:         "omt_group_topic",
		Text:             "@智能助手 新发一条话题消息",
		Mentions:         []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group topic root) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.newCalls) != 1 || rt.newCalls[0].Key.SubID != "omt_group_topic" {
		t.Fatalf("newCalls = %+v, want group topic session keyed by thread id", rt.newCalls)
	}
	if len(rt.promptCalls) != 1 || rt.promptCalls[0].Session.Key.SubID != "omt_group_topic" {
		t.Fatalf("promptCalls = %+v, want prompt on group topic session", rt.promptCalls)
	}
	if _, ok := store.Get(imSessionKey("bot-a", "oc_group", "omt_group_topic")); !ok {
		t.Fatalf("group topic session not persisted")
	}
}

func TestHandleFeishuTopicRepliesReuseSameSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-topic", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	first := feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_topic_root",
		ChatID:    "oc_group",
		ChatType:  "topic_group",
		ThreadID:  "omt_topic",
		Text:      "@智能助手 话题根消息",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	}
	reply, err := handleFeishuMessage(t, svc, context.Background(), first)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(topic root) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_topic_reply",
		ChatID:    "oc_group",
		ChatType:  "topic_group",
		ThreadID:  first.ThreadID,
		RootID:    first.MessageID,
		ParentID:  first.MessageID,
		Text:      "@智能助手 话题内回复",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
		Reply: &feishu.ReplyContext{
			MessageID: first.MessageID,
			MsgType:   "text",
			Text:      "话题根消息",
		},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(topic reply) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.newCalls) != 1 || rt.newCalls[0].Key.SubID != first.ThreadID {
		t.Fatalf("newCalls = %+v, want one session for same topic", rt.newCalls)
	}
	if len(rt.promptCalls) != 2 {
		t.Fatalf("promptCalls = %+v, want root and reply prompts", rt.promptCalls)
	}
	for _, call := range rt.promptCalls {
		if call.Session.ACPSessionID != "acp-session-topic" || call.Session.Key.SubID != first.ThreadID {
			t.Fatalf("prompt call = %+v, want same topic ACP session", call)
		}
	}
}

func TestHandleFeishuNewTopicWithoutThreadDoesNotReusePreviousTopicSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	oldKey := imSessionKey("bot-a", "oc_group", "om_previous_topic")
	if err := store.Upsert(Session{
		Key:          oldKey,
		Title:        "旧话题",
		AgentName:    "traex",
		ACPSessionID: "acp-session-old",
		Cwd:          workDir,
	}); err != nil {
		t.Fatalf("Upsert(old session) error = %v", err)
	}
	rt := &fakeRuntime{newSessionID: "acp-session-new", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_new_topic",
		ChatID:    "oc_group",
		ChatType:  "topic_group",
		RootID:    oldKey.SubID,
		ParentID:  oldKey.SubID,
		Text:      "@智能助手 新话题",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(new topic without thread) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.newCalls) != 1 || rt.newCalls[0].Key.SubID != "om_new_topic" {
		t.Fatalf("newCalls = %+v, want new topic session keyed by current message", rt.newCalls)
	}
	if len(rt.promptCalls) != 1 || rt.promptCalls[0].Session.ACPSessionID != "acp-session-new" {
		t.Fatalf("promptCalls = %+v, want new ACP session", rt.promptCalls)
	}
	if session, ok := store.Get(imSessionKey("bot-a", "oc_group", "om_new_topic")); !ok || session.ACPSessionID != "acp-session-new" {
		t.Fatalf("new topic session = %+v, %v; want persisted new session", session, ok)
	}
}

func TestHandleFeishuSessionListAndResume(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	rt := &fakeRuntime{newSessionIDs: []string{"acp-session-1", "acp-session-2"}, promptReply: "ACP 回复"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	base := feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_private",
		ChatType: "p2p",
	}

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     base.BotID,
		ChatID:    base.ChatID,
		ChatType:  base.ChatType,
		MessageID: "om_1",
		Text:      "/new " + firstDir,
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new first) error = %v", err)
	}
	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     base.BotID,
		ChatID:    base.ChatID,
		ChatType:  base.ChatType,
		MessageID: "om_2",
		Text:      "/new " + secondDir,
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new second) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     base.BotID,
		ChatID:    base.ChatID,
		ChatType:  base.ChatType,
		MessageID: "om_list",
		Text:      "/session LIST",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/session LIST) error = %v", err)
	}
	if !strings.Contains(reply, "1. acp-session-2 *") || !strings.Contains(reply, "2. acp-session-1") {
		t.Fatalf("reply = %q, want newest current session first", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     base.BotID,
		ChatID:    base.ChatID,
		ChatType:  base.ChatType,
		MessageID: "om_resume",
		Text:      "/session RESUME 2",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/session RESUME) error = %v", err)
	}
	if !strings.Contains(reply, "已恢复会话 2") || !strings.Contains(reply, "session：acp-session-1") {
		t.Fatalf("reply = %q, want resumed first session", reply)
	}
	if len(rt.closedKeys) != 1 || rt.closedKeys[0] != imSessionKey("bot-a", "oc_private", "") {
		t.Fatalf("closedKeys = %+v, want current private chat key closed", rt.closedKeys)
	}
	session, ok := store.Get(imSessionKey("bot-a", "oc_private", ""))
	if !ok {
		t.Fatalf("current private session not found")
	}
	if session.ACPSessionID != "acp-session-1" || session.Cwd != firstDir {
		t.Fatalf("session = %+v, want first session restored", session)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     base.BotID,
		ChatID:    base.ChatID,
		ChatType:  base.ChatType,
		MessageID: "om_prompt",
		Text:      "继续",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if got := rt.promptCalls[len(rt.promptCalls)-1].Session.ACPSessionID; got != "acp-session-1" {
		t.Fatalf("prompt session = %q, want resumed acp-session-1", got)
	}
}

func TestHandleFeishuSessionListSendsSelectionCard(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{newSessionIDs: []string{"acp-session-1", "acp-session-2", "acp-session-3"}}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	base := feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_private",
		ChatType: "p2p",
	}

	for i := 0; i < 3; i++ {
		if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
			BotID:     base.BotID,
			ChatID:    base.ChatID,
			ChatType:  base.ChatType,
			MessageID: fmt.Sprintf("om_new_%d", i),
			Text:      "/new " + t.TempDir() + fmt.Sprintf(" 会话%d", i+1),
		}); err != nil {
			t.Fatalf("HandleFeishuMessage(/new %d) error = %v", i, err)
		}
	}

	var got feishu.SessionSelectionCard
	client := newFakeSentMessageClient("")
	client.sessionSelectionSender = func(ctx context.Context, msg feishu.Message, card feishu.SessionSelectionCard) error {
		got = card
		return nil
	}
	svc.setOutbound(base.BotID, client)
	ctx := context.Background()
	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     base.BotID,
		ChatID:    base.ChatID,
		ChatType:  base.ChatType,
		MessageID: "om_list",
		Text:      "/session list",
		SenderID:  testOwnerOpenID,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/session list) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want card-only response", reply)
	}
	if got.ChatID != base.ChatID || got.RequesterID != testOwnerOpenID {
		t.Fatalf("card = %+v, want chat and requester", got)
	}
	if len(got.Options) != 3 {
		t.Fatalf("options = %+v, want 3 sessions", got.Options)
	}
	if got.Options[0].ACPSessionID != "acp-session-3" || got.CurrentACPSessionID != "acp-session-3" {
		t.Fatalf("card = %+v, want newest current first", got)
	}
}

func TestHandleFeishuNewSessionDefaultTitleUsesChatSequenceAfterHistoryTrim(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	sessionIDs := make([]string, 0, maxSessionHistoryPerChat+2)
	for i := 1; i <= maxSessionHistoryPerChat+2; i++ {
		sessionIDs = append(sessionIDs, fmt.Sprintf("acp-session-%d", i))
	}
	rt := &fakeRuntime{newSessionIDs: sessionIDs}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	msg := feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_private",
		ChatType: "p2p",
	}

	var reply string
	var err error
	for i := 1; i <= maxSessionHistoryPerChat+2; i++ {
		text := "/new"
		if i == 1 {
			text += " " + workDir
		}
		reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
			BotID:     msg.BotID,
			ChatID:    msg.ChatID,
			ChatType:  msg.ChatType,
			MessageID: fmt.Sprintf("om_new_%d", i),
			Text:      text,
		})
		if err != nil {
			t.Fatalf("HandleFeishuMessage(/new %d) error = %v", i, err)
		}
		want := fmt.Sprintf("标题：session#%d", i)
		if !strings.Contains(reply, want) {
			t.Fatalf("reply %d = %q, want %q", i, reply, want)
		}
	}

	items := store.ListByChat(msg.BotID, msg.ChatID)
	if len(items) != maxSessionHistoryPerChat {
		t.Fatalf("len(history) = %d, want %d", len(items), maxSessionHistoryPerChat)
	}
	chat, ok := store.GetChat(ChatKey{BotID: msg.BotID, ChatID: msg.ChatID})
	if !ok || chat.NextSessionSeq != maxSessionHistoryPerChat+3 {
		t.Fatalf("chat = %+v ok=%v, want next sequence %d", chat, ok, maxSessionHistoryPerChat+3)
	}
	if !strings.Contains(reply, fmt.Sprintf("标题：session#%d", maxSessionHistoryPerChat+2)) {
		t.Fatalf("final reply = %q, want latest title after history trim", reply)
	}
}

func TestHandleFeishuNewSessionWaitsForSessionStateUpdate(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	rt := &fakeRuntime{
		newSessionID:   "acp-session-1",
		newSessionInfo: acp.SessionInfo{SessionID: "acp-session-1"},
		noDefaultState: true,
	}
	rt.afterNewSession = func(key SessionKey, sessionID string) {
		go func() {
			time.Sleep(10 * time.Millisecond)
			rt.dispatchUpdate(key, sessionID, acp.SessionUpdate{
				SessionUpdate: "session_state_update",
				Models: &acp.SessionModelState{
					CurrentModelID: "gpt-5.6",
				},
				Mode: &acp.SessionModeState{
					CurrentModeID: "plan",
				},
			})
		}()
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		MessageID: "om_new",
		Text:      "/new " + workDir,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	for _, want := range []string{"标题：session#1", "mode：plan", "model：gpt-5.6"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
}

func TestHandleFeishuNewSessionWaitsAfterPartialSessionState(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{
			SessionID: "acp-session-1",
			Models: &acp.SessionModelState{
				CurrentModelID: "gpt-5.6",
			},
		},
		noDefaultState: true,
	}
	rt.afterNewSession = func(key SessionKey, sessionID string) {
		go func() {
			time.Sleep(10 * time.Millisecond)
			rt.dispatchUpdate(key, sessionID, acp.SessionUpdate{
				SessionUpdate: "session_state_update",
				Mode: &acp.SessionModeState{
					CurrentModeID: "plan",
				},
			})
		}()
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		MessageID: "om_new",
		Text:      "/new " + workDir,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	for _, want := range []string{"标题：session#1", "mode：plan", "model：gpt-5.6"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
}

func TestHandleFeishuAutoSessionUsesFirstPromptAsTitle(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	longPrompt := "请帮我分析这个特别长特别长特别长特别长特别长特别长的登录失败问题"
	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		MessageID: "om_prompt",
		Text:      longPrompt,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	session, ok := store.Get(imSessionKey("bot-a", "oc_private", ""))
	if !ok {
		t.Fatalf("auto-created session not found")
	}
	if session.Title == "" || !strings.HasPrefix(session.Title, "请帮我分析这个") {
		t.Fatalf("session title = %q, want prompt-derived title", session.Title)
	}
	if session.ManualTitle {
		t.Fatalf("auto-created prompt title should not be marked manual")
	}
	if len([]rune(strings.TrimSuffix(session.Title, "..."))) > maxSessionTitleRunes {
		t.Fatalf("session title = %q, want truncated title", session.Title)
	}
}

func TestHandleFeishuMessageRefreshesAutomaticSessionTitle(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := imSessionKey("bot-a", "oc_private", "")

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     key.BotID,
		ChatID:    sessionKeyMainID(key),
		ChatType:  "p2p",
		MessageID: "om_new",
		Text:      "/new " + workDir,
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	session, ok := store.Get(key)
	if !ok {
		t.Fatalf("session not found")
	}
	if session.Title != "session#1" || session.ManualTitle {
		t.Fatalf("new session title = %q manual=%v, want automatic session#1", session.Title, session.ManualTitle)
	}

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     key.BotID,
		ChatID:    sessionKeyMainID(key),
		ChatType:  "p2p",
		MessageID: "om_prompt_1",
		Text:      "第一次问题",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(first prompt) error = %v", err)
	}
	session, ok = store.Get(key)
	if !ok {
		t.Fatalf("session not found after first prompt")
	}
	if session.Title != "第一次问题" || session.ManualTitle {
		t.Fatalf("session title after first prompt = %q manual=%v, want automatic prompt title", session.Title, session.ManualTitle)
	}

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     key.BotID,
		ChatID:    sessionKeyMainID(key),
		ChatType:  "p2p",
		MessageID: "om_prompt_2",
		Text:      "第二次问题",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(second prompt) error = %v", err)
	}
	session, ok = store.Get(key)
	if !ok {
		t.Fatalf("session not found after second prompt")
	}
	if session.Title != "第二次问题" || session.ManualTitle {
		t.Fatalf("session title after second prompt = %q manual=%v, want refreshed automatic title", session.Title, session.ManualTitle)
	}
}

func TestHandleFeishuMessageKeepsManualSessionTitle(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := imSessionKey("bot-a", "oc_private", "")

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     key.BotID,
		ChatID:    sessionKeyMainID(key),
		ChatType:  "p2p",
		MessageID: "om_new",
		Text:      "/new " + workDir + " 手动标题",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new title) error = %v", err)
	}
	session, ok := store.Get(key)
	if !ok {
		t.Fatalf("session not found")
	}
	if session.Title != "手动标题" || !session.ManualTitle {
		t.Fatalf("session title = %q manual=%v, want manual title", session.Title, session.ManualTitle)
	}

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     key.BotID,
		ChatID:    sessionKeyMainID(key),
		ChatType:  "p2p",
		MessageID: "om_prompt",
		Text:      "普通消息不应覆盖手动标题",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	session, ok = store.Get(key)
	if !ok {
		t.Fatalf("session not found after prompt")
	}
	if session.Title != "手动标题" || !session.ManualTitle {
		t.Fatalf("session title after prompt = %q manual=%v, want manual title preserved", session.Title, session.ManualTitle)
	}
}

func TestHandleFeishuMessageEmptyMessageDoesNotPrompt(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "bot-a")
	svc := newTestService(config.Default(), nil)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		Workspace: workspace,
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if reply != "暂不支持的消息类型。" {
		t.Fatalf("reply = %q, want unsupported message type reply", reply)
	}
	for _, name := range []string{
		"SOUL.md",
		"MEMORY.md",
		"AGENTS.md",
		"TOOLS.md",
		filepath.Join("knowledge", "AGENTS.md"),
		filepath.Join("knowledge", "core.md"),
		filepath.Join("knowledge", "index.md"),
		filepath.Join("knowledge", "log.md"),
		filepath.Join("knowledge", "lint.md"),
		filepath.Join("skills", "AGENTS.md"),
		filepath.Join("skills", "core.md"),
		filepath.Join("skills", "acp-trace", "SKILL.md"),
		filepath.Join("skills", "wiki", "SKILL.md"),
	} {
		if _, err := os.Stat(filepath.Join(workspace, name)); err != nil {
			t.Fatalf("workspace file %s not created: %v", name, err)
		}
	}
}

func TestHandleFeishuMessageMentionOnlyPromptsWithContextInstruction(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptReply: "ACP 回复"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	if err := store.Upsert(Session{
		Key:          key,
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		AgentName:    "traex",
		Title:        "已有标题",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_mention_only",
		ChatID:    "oc_chat",
		ChatType:  "group",
		Text:      "@智能助手",
		Mentions:  testBotMentions(),
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(mention only) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want one prompt", rt.promptCalls)
	}
	assertReadyPromptContainsUserTextAndMemoryPolicy(t, rt.promptCalls[0].Text, mentionOnlyPromptText)
	if strings.Contains(rt.promptCalls[0].Text, "@智能助手") {
		t.Fatalf("prompt text = %q, should strip bot mention name", rt.promptCalls[0].Text)
	}
	session, ok := store.Get(key)
	if !ok {
		t.Fatalf("session not found")
	}
	if session.Title != "已有标题" {
		t.Fatalf("session title = %q, want existing title preserved for mention-only prompt", session.Title)
	}
}

func TestHandleFeishuMessageResetsWorkspacePromptedAfterBuiltinSkillUpgrade(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workspace := filepath.Join(t.TempDir(), "bot-a")
	for _, file := range []struct {
		name    string
		content string
	}{
		{name: "SOUL.md", content: "# SOUL\n\n名字：小助手\n"},
		{name: filepath.Join("knowledge", "index.md"), content: "# Index\n\n## L2 Skills\n\n| 文件 | 用途 |\n| --- | --- |\n| `skills/AGENTS.md` | L2 写入规范 |\n| `skills/core.md` | 技能索引入口 |\n| `skills/wiki/SKILL.md` | 知识库维护流程 |\n"},
		{name: filepath.Join("knowledge", "log.md"), content: "# Log\n"},
		{name: filepath.Join("skills", "AGENTS.md"), content: "# Skills 层规范 (L2)\n"},
		{name: filepath.Join("skills", "core.md"), content: "# Skills\n\n- [[wiki]]：维护 workspace 知识库和技能库。\n"},
		{name: filepath.Join("skills", "wiki", "SKILL.md"), content: "# Wiki\n"},
	} {
		path := filepath.Join(workspace, file.name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(file.name), err)
		}
		if err := os.WriteFile(path, []byte(file.content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", file.name, err)
		}
	}
	workDir := t.TempDir()
	key := imSessionKey("bot-a", "oc_chat", "")
	if err := store.Upsert(Session{
		Key:               key,
		AgentName:         "traex",
		ACPSessionID:      "acp-session-1",
		Cwd:               workDir,
		Workspace:         workspace,
		WorkspacePrompted: true,
	}); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{promptReply: "已读取 trace。"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		Workspace: workspace,
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		Text:      "看一下 sid acp-session-1 的执行轨迹",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "已读取 trace。" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want one prompt", rt.promptCalls)
	}
	prompt := rt.promptCalls[0].Text
	for _, want := range []string{"## Workspace Context", "skills/core.md", "[[acp-trace]]", "## Workspace Memory Policy", "## User Message"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt text = %q, want %q", prompt, want)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, workspaceACPTraceSkillFileName())); err != nil {
		t.Fatalf("acp-trace skill should be created: %v", err)
	}
	session, ok := store.Get(key)
	if !ok {
		t.Fatalf("session not found")
	}
	if !session.WorkspacePrompted {
		t.Fatalf("session workspace_prompted = false, want true after refreshed prompt sent")
	}
}

func TestHandleFeishuMessageNewOnlyConfirmsSessionCreation(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "请先回答初始化问题。"}
	svc.setRuntime(rt)
	workspace := filepath.Join(t.TempDir(), "bot-a")
	workDir := t.TempDir()
	var immediateReplies []string
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		immediateReplies = append(immediateReplies, text)
		return nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		Workspace: workspace,
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/new " + workDir,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if len(immediateReplies) != 0 {
		t.Fatalf("immediateReplies = %+v, want no intermediate replies", immediateReplies)
	}
	if !strings.Contains(reply, "已为当前会话创建 ACP 会话") || !strings.Contains(reply, "session：acp-session-1") {
		t.Fatalf("reply = %q, want new session confirmation", reply)
	}
	if len(rt.promptCalls) != 0 {
		t.Fatalf("promptCalls = %+v, want /new not to send workspace context prompt", rt.promptCalls)
	}
}

func TestHandleFeishuMessageRecreatesMissingACPSessionTitleUsesUserText(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workspace := filepath.Join(t.TempDir(), "bot-a")
	markWorkspaceBootstrapped(t, workspace)
	workDir := t.TempDir()
	key := imSessionKey("bot-a", "oc_chat", "omt_thread")
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		Cwd:          workDir,
		Workspace:    workspace,
		ACPSessionID: "",
	}); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{newSessionID: "ready-session", promptReply: "ACP 回复"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		Workspace: workspace,
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "请继续处理",
		Reply: &feishu.ReplyContext{
			MessageID: "om_parent",
			Text:      "这是一段非常长的被回复消息，不应该成为新 ACP session 的标题",
		},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	session, ok := store.Get(key)
	if !ok {
		t.Fatalf("session not found")
	}
	if session.Title != "请继续处理" {
		t.Fatalf("session title = %q, want user text title", session.Title)
	}
	if len(rt.promptCalls) != 1 || !strings.Contains(rt.promptCalls[0].Text, "## Replied Message Context") {
		t.Fatalf("prompt calls = %+v, want reply context still included", rt.promptCalls)
	}
}

func TestHandleFeishuMessageAutoCreatesSessionWithKnowledge(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	workspace := filepath.Join(t.TempDir(), "bot-a")
	markWorkspaceBootstrapped(t, workspace)
	if err := os.WriteFile(filepath.Join(workspace, "SOUL.md"), []byte("# SOUL\n\n名字：小助手\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(SOUL.md) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		Workspace: workspace,
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "@我的智能助手 介绍一下这个仓库",
		Mentions: []feishu.Mention{
			testBotMention("我的智能助手"),
		},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.newCalls) != 1 {
		t.Fatalf("newCalls = %+v, want auto-created session", rt.newCalls)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want one prompt", rt.promptCalls)
	}
	prompt := rt.promptCalls[0].Text
	for _, want := range []string{"Workspace Context", "SOUL.md", "名字：小助手", "## User Message", "介绍一下这个仓库"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
	session, ok := store.Get(imSessionKey("bot-a", "oc_chat", "omt_thread"))
	if !ok {
		session, ok = store.Get(imSessionKey("bot-a", "oc_chat", ""))
	}
	if !ok {
		t.Fatalf("auto-created session not persisted")
	}
	if session.Workspace != workspace {
		t.Fatalf("session workspace = %q, want %q", session.Workspace, workspace)
	}
}

func TestHandleFeishuMessageNewPersistsSession(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	store := NewSessionStore(storePath)
	svc := newTestService(config.Default(), store)
	rt := &fakeRuntime{newSessionID: "acp-session-1"}
	svc.setRuntime(rt)
	home := t.TempDir()
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	msg := feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "@我的智能助手 /new ~/repo",
		Mentions: []feishu.Mention{
			testBotMention("我的智能助手"),
		},
	}
	t.Setenv("HOME", home)

	reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if !strings.Contains(reply, "已为当前会话创建 ACP 会话") {
		t.Fatalf("reply = %q, want new session confirmation", reply)
	}

	reloaded := NewSessionStore(storePath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	session, ok := reloaded.Get(imSessionKey("", "oc_chat", "omt_thread"))
	if !ok {
		session, ok = reloaded.Get(imSessionKey("", "oc_chat", ""))
	}
	if !ok {
		t.Fatalf("persisted session not found")
	}
	if session.Cwd != repo {
		t.Fatalf("Cwd = %q, want expanded cwd", session.Cwd)
	}
	if session.ACPSessionID != "acp-session-1" {
		t.Fatalf("session = %+v, want acp session persisted", session)
	}
}

func TestHandleFeishuMessagePersistsSessionByBotID(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	rt := &fakeRuntime{newSessionID: "acp-session-1"}
	svc.setRuntime(rt)
	workDir := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "bot-a")
	markWorkspaceBootstrapped(t, workspace)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		Workspace: workspace,
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/new " + workDir,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "已为当前会话创建 ACP 会话") {
		t.Fatalf("reply = %q, want new session confirmation", reply)
	}
	if _, ok := store.Get(imSessionKey("bot-a", "oc_chat", "")); !ok {
		t.Fatalf("session with bot id not found")
	}
	if _, ok := store.Get(imSessionKey("", "oc_chat", "omt_thread")); ok {
		t.Fatalf("session without bot id should not be written for new messages")
	}
}

func TestHandleFeishuMessageUsesBotWorkspaceSessionStore(t *testing.T) {
	workDir := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "bot-a")
	cfg := config.Default()
	cfg.Bots = []config.BotConfig{
		{
			ID:        "bot-a",
			AppID:     "cli_xxx",
			AppSecret: config.FileSecret("bot-a.appsecret"),
			Workspace: workspace,
		},
	}
	markWorkspaceBootstrapped(t, workspace)
	svc := NewService(cfg, nil)
	svc.setRuntime(&fakeRuntime{newSessionID: "acp-session-1"})

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		Workspace: workspace,
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/new " + workDir,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "已为当前会话创建 ACP 会话") {
		t.Fatalf("reply = %q, want new session confirmation", reply)
	}
	storePath := filepath.Join(workspace, ".local", "sessions.json")
	reloaded := NewSessionStore(storePath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, ok := reloaded.Get(imSessionKey("bot-a", "oc_chat", "")); !ok {
		t.Fatalf("persisted session not found in bot workspace store")
	}
}

func TestHandleFeishuMessageNewWithoutCwdUsesDefaultCwd(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(&fakeRuntime{newSessionID: "acp-session-1"})

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "cwd："+workDir) || !strings.Contains(reply, "cwd 来源：默认配置") {
		t.Fatalf("reply = %q, want default cwd", reply)
	}
}

func TestHandleFeishuMessageNewWithoutCwdReusesCurrentSessionCwd(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	firstDir := t.TempDir()
	defaultDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = defaultDir
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(&fakeRuntime{newSessionID: "acp-session-1"})
	msg := feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
	}

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		MessageID: msg.MessageID,
		ChatID:    msg.ChatID,
		ThreadID:  msg.ThreadID,
		Text:      "/new " + firstDir,
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new first) error = %v", err)
	}
	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		MessageID: "om_msg_2",
		ChatID:    msg.ChatID,
		ThreadID:  msg.ThreadID,
		Text:      "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new reuse) error = %v", err)
	}
	if !strings.Contains(reply, "cwd："+firstDir) || !strings.Contains(reply, "cwd 来源：当前会话已有会话") {
		t.Fatalf("reply = %q, want reused cwd", reply)
	}
	if strings.Contains(reply, defaultDir) {
		t.Fatalf("reply = %q, should not use default cwd when current session exists", reply)
	}
}

func TestHandleFeishuMessageNewRelativeCwd(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rootDir := t.TempDir()
	currentDir := filepath.Join(rootDir, "current")
	siblingDir := filepath.Join(rootDir, "sibling")
	defaultDir := filepath.Join(rootDir, "default")
	for _, dir := range []string{currentDir, siblingDir, defaultDir} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("Mkdir(%s) error = %v", dir, err)
		}
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = defaultDir
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(&fakeRuntime{newSessionIDs: []string{"acp-session-1", "acp-session-2", "acp-session-3"}})
	msg := feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
	}

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_current",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "/new " + currentDir,
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new current) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_relative_current",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "/new ../sibling 相对路径标题",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new relative current) error = %v", err)
	}
	wantSibling := filepath.Clean(siblingDir)
	if !strings.Contains(reply, "cwd："+wantSibling) || !strings.Contains(reply, "cwd 来源：命令参数") || !strings.Contains(reply, "标题：相对路径标题") {
		t.Fatalf("reply = %q, want relative cwd resolved from current session", reply)
	}

	otherReply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_relative_default",
		ChatID:    "oc_other",
		ChatType:  msg.ChatType,
		Text:      "/new ../default",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new relative default) error = %v", err)
	}
	if !strings.Contains(otherReply, "cwd："+defaultDir) || !strings.Contains(otherReply, "cwd 来源：命令参数") {
		t.Fatalf("reply = %q, want relative cwd resolved from default cwd", otherReply)
	}
}

func TestHandleFeishuMessageNewRejectsMissingExplicitCwd(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	missing := filepath.Join(t.TempDir(), "missing")

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_missing",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/new " + missing,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new missing) error = %v", err)
	}
	if !strings.Contains(reply, "工作目录不可访问") {
		t.Fatalf("reply = %q, want missing cwd rejection", reply)
	}
	if len(rt.newCalls) != 0 {
		t.Fatalf("newCalls = %+v, want no ACP session", rt.newCalls)
	}
}

func TestHandleFeishuMessageNewRejectsExplicitCwdFile(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	cwd := filepath.Join(t.TempDir(), "repo.txt")
	if err := os.WriteFile(cwd, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(cwd) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_file",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/new " + cwd,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new file) error = %v", err)
	}
	if !strings.Contains(reply, "工作目录不是目录") {
		t.Fatalf("reply = %q, want file cwd rejection", reply)
	}
	if len(rt.newCalls) != 0 {
		t.Fatalf("newCalls = %+v, want no ACP session", rt.newCalls)
	}
}

func TestHandleFeishuMessagePromptUsesPersistedSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	workDir := t.TempDir()
	if err := store.Upsert(Session{
		Key:          imSessionKey("bot-a", "oc_chat", "omt_thread"),
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          workDir,
	}); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:            "bot-a",
		MessageID:        "om_prompt",
		ChatID:           "oc_chat",
		ChatType:         "group",
		GroupMessageType: "thread",
		ThreadID:         "omt_thread",
		RootID:           "om_root",
		ParentID:         "om_parent",
		SenderID:         "ou_sender",
		SenderType:       "user",
		MsgType:          "text",
		Text:             "@我的智能助手 介绍一下这个仓库",
		Mentions: []feishu.Mention{
			testBotMention("我的智能助手"),
		},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want one prompt", rt.promptCalls)
	}
	assertReadyPromptContainsUserTextAndMemoryPolicy(t, rt.promptCalls[0].Text, "介绍一下这个仓库")
	assertPromptContainsMessageMetadata(t, rt.promptCalls[0].Text, map[string]string{
		"bot_id":      "bot-a",
		"message_id":  "om_prompt",
		"chat_id":     "oc_chat",
		"chat_type":   "group",
		"thread_id":   "omt_thread",
		"root_id":     "om_root",
		"parent_id":   "om_parent",
		"sender_id":   "ou_sender",
		"sender_type": "user",
		"msg_type":    "text",
	})
	if strings.Contains(rt.promptCalls[0].Text, "@我的智能助手") {
		t.Fatalf("prompt text = %q, should strip bot mention", rt.promptCalls[0].Text)
	}
	if rt.promptCalls[0].Session.ACPSessionID != "acp-session-1" {
		t.Fatalf("prompt session = %+v, want persisted acp session id", rt.promptCalls[0].Session)
	}
}

func TestHandleFeishuMessageInjectsTraceAttrsIntoPromptContext(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptReply: "ACP 回复"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "omt_thread")),
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Workspace:    t.TempDir(),
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}

	_, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:            "bot-a",
		MessageID:        "om_trace_prompt",
		ChatID:           "oc_chat",
		ChatType:         "group",
		GroupMessageType: "thread",
		ThreadID:         "omt_thread",
		SenderID:         "ou_sender",
		Text:             "@智能助手 帮我处理 secret-token",
		Mentions:         testBotMentions(),
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("prompt calls = %+v, want one call", rt.promptCalls)
	}
	attrs := rt.promptCalls[0].Attrs
	traceID := attrs["trace_id"]
	if traceID == "" {
		t.Fatalf("prompt attrs = %+v, want trace_id", attrs)
	}
	wantTraceID := logging.TraceIDFromParts("feishu_message", "bot-a", "om_trace_prompt", "oc_chat", "omt_thread", "", "", "ou_sender")
	if traceID != wantTraceID {
		t.Fatalf("trace_id = %q, want stable %q", traceID, wantTraceID)
	}
	for key, want := range map[string]string{
		"message_id": "om_trace_prompt",
		"chat_id":    "oc_chat",
		"thread_id":  "omt_thread",
		"sender_id":  "ou_sender",
	} {
		if got := attrs[key]; got != want {
			t.Fatalf("prompt attrs[%s] = %q, want %q; attrs=%+v", key, got, want, attrs)
		}
	}
	for key, value := range attrs {
		if strings.Contains(value, "secret-token") || strings.Contains(value, "帮我处理") {
			t.Fatalf("prompt attr %s=%q should not contain message body or secret", key, value)
		}
	}
}

func TestHandleFeishuMessageReplyToDriveCommentTraceCardUsesBoundSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptReply: "trace 群回复"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	workDir := t.TempDir()
	commentKey := SessionKey{BotID: "bot-a", Source: sessionSourceDriveComment, MainID: "docx:token", SubID: "comment-1"}
	if err := store.Upsert(Session{
		Key:          commentKey,
		AgentName:    "traex",
		ACPSessionID: "acp-comment",
		Cwd:          workDir,
		Workspace:    workDir,
	}); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	if _, err := store.BindMessageToSession(MessageSessionBinding{
		BotID:      "bot-a",
		ChatID:     "oc_trace",
		MessageID:  "om_trace_card",
		SessionKey: commentKey,
	}); err != nil {
		t.Fatalf("BindMessageToSession() error = %v", err)
	}
	client := newFakeSentMessageClient("om_followup_card")
	var driveCommentReplies []string
	client.driveCommentReplySender = func(ctx context.Context, comment feishu.DriveComment, text string) error {
		driveCommentReplies = append(driveCommentReplies, text)
		return nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:      "bot-a",
		MessageID:  "om_followup",
		ChatID:     "oc_trace",
		ChatType:   "group",
		RootID:     "om_trace_card",
		ParentID:   "om_trace_card",
		SenderID:   "ou_sender",
		SenderType: "user",
		MsgType:    "text",
		Text:       "@智能助手 继续处理",
		Mentions:   testBotMentions(),
		Reply:      &feishu.ReplyContext{MessageID: "om_trace_card"},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(trace follow-up) error = %v", err)
	}
	if reply != "trace 群回复" {
		t.Fatalf("reply = %q, want IM follow-up reply", reply)
	}
	if len(driveCommentReplies) != 0 {
		t.Fatalf("drive comment replies = %+v, want none for trace chat follow-up", driveCommentReplies)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want one prompt", rt.promptCalls)
	}
	if rt.promptCalls[0].Session.Key != commentKey || rt.promptCalls[0].Session.ACPSessionID != "acp-comment" {
		t.Fatalf("prompt session = %+v, want bound drive comment session", rt.promptCalls[0].Session)
	}
	if strings.Contains(rt.promptCalls[0].Text, "@智能助手") || !strings.Contains(rt.promptCalls[0].Text, "继续处理") {
		t.Fatalf("prompt text = %q, want stripped follow-up text", rt.promptCalls[0].Text)
	}
}
