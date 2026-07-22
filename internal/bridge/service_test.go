package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func handleFeishuMessage(t *testing.T, svc *Service, ctx context.Context, msg feishu.Message) (string, error) {
	t.Helper()
	if strings.TrimSpace(msg.Workspace) == "" {
		msg.Workspace = filepath.Join(t.TempDir(), "workspace")
		if err := os.MkdirAll(msg.Workspace, 0o755); err != nil {
			t.Fatalf("MkdirAll(workspace) error = %v", err)
		}
		for _, file := range workspaceFiles() {
			path := filepath.Join(msg.Workspace, file.name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(file.name), err)
			}
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatalf("WriteFile(%s) error = %v", file.name, err)
			}
		}
		if err := markWorkspaceReady(msg.Workspace); err != nil {
			t.Fatalf("markWorkspaceReady() error = %v", err)
		}
	}
	return svc.HandleFeishuMessage(ctx, msg)
}

func TestHandleFeishuMessageHelp(t *testing.T) {
	svc := NewService(config.Default(), nil)
	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		Text: "/help",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if !strings.Contains(reply, "/status") {
		t.Fatalf("reply = %q, want status help", reply)
	}
}

func TestHandleFeishuMessageStatus(t *testing.T) {
	svc := NewService(config.Default(), nil)
	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		Text: "@我的智能助手 /status",
		Mentions: []feishu.Mention{
			{Name: "我的智能助手"},
		},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if !strings.Contains(reply, "traex") {
		t.Fatalf("reply = %q, want configured agent name", reply)
	}
}

func TestHandleFeishuMessageWithoutSessionAutoCreatesSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = workDir
	cfg.Agents["traex"] = agent
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "@我的智能助手 你好",
		Mentions: []feishu.Mention{
			{Name: "我的智能助手"},
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
	if _, ok := store.Get(SessionKey{ChatID: "oc_chat", ThreadID: "omt_thread"}); !ok {
		t.Fatalf("auto-created session not persisted")
	}
}

func TestHandleFeishuPrivateChatReusesChatSessionUntilNew(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = firstDir
	cfg.Agents["traex"] = agent
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
	if len(rt.newCalls) != 1 || rt.newCalls[0].Key.ThreadID != "" {
		t.Fatalf("newCalls = %+v, want one chat-level private session", rt.newCalls)
	}
	if _, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_private"}); !ok {
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
	if len(rt.promptCalls) != 2 || rt.promptCalls[1].Session.Key.ThreadID != "" {
		t.Fatalf("promptCalls = %+v, want second prompt on same private chat session", rt.promptCalls)
	}
	assertReadyPromptContainsUserTextAndMemoryPolicy(t, rt.promptCalls[1].Text, "继续")

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
	session, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_private"})
	if !ok {
		t.Fatalf("private chat session not found after /new")
	}
	if session.Cwd != secondDir {
		t.Fatalf("private session cwd = %q, want %q", session.Cwd, secondDir)
	}
}

func TestHandleFeishuSessionListAndResume(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	rt := &fakeRuntime{newSessionIDs: []string{"acp-session-1", "acp-session-2"}, promptReply: "ACP 回复"}
	svc := NewService(config.Default(), store)
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
		Text:      "/session list",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/session list) error = %v", err)
	}
	if !strings.Contains(reply, "1. acp-session-2 *") || !strings.Contains(reply, "2. acp-session-1") {
		t.Fatalf("reply = %q, want newest current session first", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     base.BotID,
		ChatID:    base.ChatID,
		ChatType:  base.ChatType,
		MessageID: "om_resume",
		Text:      "/session resume 2",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/session resume) error = %v", err)
	}
	if !strings.Contains(reply, "已恢复会话 2") || !strings.Contains(reply, "session：acp-session-1") {
		t.Fatalf("reply = %q, want resumed first session", reply)
	}
	if len(rt.closedKeys) != 1 || rt.closedKeys[0] != (SessionKey{BotID: "bot-a", ChatID: "oc_private"}) {
		t.Fatalf("closedKeys = %+v, want current private chat key closed", rt.closedKeys)
	}
	session, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_private"})
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

func TestHandleFeishuSessionTitleCommands(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	rt := &fakeRuntime{newSessionIDs: []string{"acp-session-1", "acp-session-2"}}
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)
	msg := feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_private",
		ChatType: "p2p",
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		MessageID: "om_new_with_title",
		Text:      "/new " + workDir + " 调试登录失败",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new title) error = %v", err)
	}
	if !strings.Contains(reply, "标题：调试登录失败") {
		t.Fatalf("reply = %q, want explicit title", reply)
	}
	session, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_private"})
	if !ok {
		t.Fatalf("session not found")
	}
	if session.Title != "调试登录失败" {
		t.Fatalf("session title = %q, want explicit title", session.Title)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		MessageID: "om_title",
		Text:      "/session title 新标题",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/session title) error = %v", err)
	}
	if !strings.Contains(reply, "已设置当前会话标题：新标题") {
		t.Fatalf("reply = %q, want title confirmation", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		MessageID: "om_title_only",
		Text:      "/new 只指定标题",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new title only) error = %v", err)
	}
	if !strings.Contains(reply, "标题：只指定标题") || !strings.Contains(reply, "cwd 来源：当前会话已有会话") {
		t.Fatalf("reply = %q, want title-only new session to reuse cwd", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		MessageID: "om_list",
		Text:      "/session list",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/session list) error = %v", err)
	}
	if !strings.Contains(reply, "标题：只指定标题") || !strings.Contains(reply, "标题：新标题") {
		t.Fatalf("reply = %q, want titles in list", reply)
	}
}

func TestHandleFeishuAutoSessionUsesFirstPromptAsTitle(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = workDir
	cfg.Agents["traex"] = agent
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
	session, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_private"})
	if !ok {
		t.Fatalf("auto-created session not found")
	}
	if session.Title == "" || !strings.HasPrefix(session.Title, "请帮我分析这个") {
		t.Fatalf("session title = %q, want prompt-derived title", session.Title)
	}
	if len([]rune(strings.TrimSuffix(session.Title, "..."))) > maxSessionTitleRunes {
		t.Fatalf("session title = %q, want truncated title", session.Title)
	}
}

func TestHandleFeishuMessageGuidesWorkspaceSetup(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "bot-a")
	svc := NewService(config.Default(), nil)

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
	if !strings.Contains(reply, "当前 bot workspace 尚未 ready") || !strings.Contains(reply, "发送普通文本或 /new [cwd]") {
		t.Fatalf("reply = %q, want workspace setup guide", reply)
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
		filepath.Join("skills", "wiki", "SKILL.md"),
	} {
		if _, err := os.Stat(filepath.Join(workspace, name)); err != nil {
			t.Fatalf("workspace file %s not created: %v", name, err)
		}
	}
}

func TestHandleFeishuMessageNewWhenWorkspaceNotReadyDefersSetupPrompt(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Default(), store)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "请告诉我你想叫我什么名字，以及我的工作风格和需要长期记住的信息。"}
	svc.setRuntime(rt)
	workspace := filepath.Join(t.TempDir(), "bot-a")
	workDir := t.TempDir()
	msg := feishu.Message{
		BotID:     "bot-a",
		Workspace: workspace,
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		Workspace: msg.Workspace,
		MessageID: msg.MessageID,
		ChatID:    msg.ChatID,
		ThreadID:  msg.ThreadID,
		Text:      "/new " + workDir,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "已为当前会话创建 ACP 会话") || !strings.Contains(reply, "session：acp-session-1") {
		t.Fatalf("reply = %q, want new session confirmation", reply)
	}
	if len(rt.newCalls) != 1 {
		t.Fatalf("newCalls = %+v, want one ACP session", rt.newCalls)
	}
	if rt.newCalls[0].Workspace != workspace {
		t.Fatalf("new call workspace = %q, want %q", rt.newCalls[0].Workspace, workspace)
	}
	if len(rt.promptCalls) != 0 {
		t.Fatalf("promptCalls = %+v, want /new to defer setup prompt", rt.promptCalls)
	}
	session, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"})
	if !ok {
		t.Fatalf("persisted session not found")
	}
	if session.Status != "setup" || session.Workspace != workspace || session.PendingInitialPrompt != "setup" {
		t.Fatalf("session = %+v, want setup session with workspace", session)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		Workspace: msg.Workspace,
		MessageID: "om_next",
		ChatID:    msg.ChatID,
		ThreadID:  msg.ThreadID,
		Text:      "你好",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(next prompt) error = %v", err)
	}
	if !strings.Contains(reply, "请告诉我你想叫我什么名字") {
		t.Fatalf("reply = %q, want ACP setup question", reply)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want setup prompt on next message", rt.promptCalls)
	}
	setupPrompt := rt.promptCalls[0].Text
	for _, want := range []string{"Workspace Setup Required", "L0/L1/L2", "knowledge/core.md", "knowledge/index.md", "knowledge/log.md", "SOUL.md", "MEMORY.md", "AGENTS.md", "TOOLS.md", ".setup.json", "不要写 ready=true", "## User Message", "你好"} {
		if !strings.Contains(setupPrompt, want) {
			t.Fatalf("setup prompt = %q, want %q", setupPrompt, want)
		}
	}
	session, ok = store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"})
	if !ok {
		t.Fatalf("persisted session not found after next prompt")
	}
	if session.PendingInitialPrompt != "" {
		t.Fatalf("session pending prompt = %q, want cleared", session.PendingInitialPrompt)
	}
	ready, err := workspaceReady(workspace)
	if err != nil {
		t.Fatalf("workspaceReady() error = %v", err)
	}
	if ready {
		t.Fatalf("workspace should stay not ready until ACP agent writes .setup.json")
	}
}

func TestHandleFeishuMessageNewOnlyConfirmsSessionCreation(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Default(), store)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "请先回答初始化问题。"}
	svc.setRuntime(rt)
	workspace := filepath.Join(t.TempDir(), "bot-a")
	workDir := t.TempDir()
	var immediateReplies []string
	ctx := feishu.WithIntermediateReplySender(context.Background(), func(ctx context.Context, msg feishu.Message, text string) error {
		immediateReplies = append(immediateReplies, text)
		return nil
	})

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
		t.Fatalf("promptCalls = %+v, want /new not to send workspace setup prompt", rt.promptCalls)
	}
}

func TestHandleFeishuMessageForwardsPromptProgress(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "收到。现在开始。\n工具处理完成。",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "收到。现在开始。\n"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "function_call",
					Name:          "exec_command",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "reasoning",
					Message:       "The\nuser\nwants\nan\nEnglish\nparagraph.",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "工具处理完成。"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = t.TempDir()
	cfg.Agents["traex"] = agent
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "初始化",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	card := cards[0]
	if got := card.textUpdatesSnapshot(); len(got) != 2 || got[0] != "收到。现在开始。" || got[1] != "收到。现在开始。\n工具处理完成。" {
		t.Fatalf("textUpdates = %+v, want accumulated stream text", got)
	}
	if got := card.processUpdatesSnapshot(); len(got) != 2 || got[0] != "⏳ exec_command" || got[1] != "⏳ exec_command\nThe user wants an English paragraph." {
		t.Fatalf("processUpdates = %+v, want normalized process updates", got)
	}
	if !card.isClosed() {
		t.Fatalf("stream card should be closed")
	}
}

func TestHandleFeishuMessageStreamsThoughtChunksAsOneProcessBlock(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "你好。",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_thought_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "**Restating the request**\n\nThe"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_thought_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: " user"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_thought_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: " said"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "你好。"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = t.TempDir()
	cfg.Agents["traex"] = agent
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "hello",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	got := cards[0].processUpdatesSnapshot()
	if len(got) == 0 || len(got) > 2 {
		t.Fatalf("processUpdates = %+v, want debounced thought block updates", got)
	}
	if got[len(got)-1] != "**Restating the request**\n\nThe user said" {
		t.Fatalf("last process update = %q, want folded thought chunk stream", got[len(got)-1])
	}
	if strings.Contains(got[len(got)-1], "The\nuser\nsaid") {
		t.Fatalf("last process update = %q, should not render one word per line", got[len(got)-1])
	}
}

func TestHandleFeishuMessageStreamsGenericChunksAsOneProcessBlock(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "完成。",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call_output_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "line"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call_output_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: " one"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call_output_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "\nline two"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = t.TempDir()
	cfg.Agents["traex"] = agent
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "hello",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	got := cards[0].processUpdatesSnapshot()
	if len(got) != 1 {
		t.Fatalf("processUpdates = %+v, want generic chunk stream to update one process block", got)
	}
	if got[0] != "line one line two" {
		t.Fatalf("process update = %q, want accumulated generic chunk stream", got[0])
	}
}

func TestHandleFeishuMessageFormatsToolTitleAndStatus(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "完成。",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call",
					Status:        "in_progress",
					Title:         "Read AGENTS.md",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call_update",
					Status:        "completed",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = t.TempDir()
	cfg.Agents["traex"] = agent
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "读取一下AGENTS.md文件当前的内容",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	got := cards[0].processUpdatesSnapshot()
	if len(got) != 2 {
		t.Fatalf("processUpdates = %+v, want tool start and completion updates", got)
	}
	if got[0] != "⏳ Read AGENTS.md" {
		t.Fatalf("first process update = %q, want tool title", got[0])
	}
	if got[1] != "✅ Read AGENTS.md" {
		t.Fatalf("second process update = %q, want completed status replacing tool row", got[1])
	}
}

func TestPromptCardStreamCreatesCardOnceConcurrently(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var mu sync.Mutex
	var cards []*fakeStreamCard
	ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		started <- struct{}{}
		<-release
		card := &fakeStreamCard{}
		mu.Lock()
		cards = append(cards, card)
		mu.Unlock()
		return card, nil
	})
	stream := newPromptCardStream(ctx, feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
	}, Session{ACPSessionID: "acp-session-1"})

	done := make(chan struct{}, 2)
	go func() {
		stream.updateText("hello")
		done <- struct{}{}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream card starter was not called")
	}
	go func() {
		stream.updateProcess("process")
		done <- struct{}{}
	}()
	select {
	case <-started:
		t.Fatal("stream card starter was called twice while first creation was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("concurrent stream update did not finish")
		}
	}
	mu.Lock()
	gotCards := len(cards)
	if gotCards != 1 {
		mu.Unlock()
		t.Fatalf("cards = %d, want one stream card", gotCards)
	}
	card := cards[0]
	mu.Unlock()
	if got := card.textUpdatesSnapshot(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("textUpdates = %+v, want text update on single card", got)
	}
	if got := card.processUpdatesSnapshot(); len(got) != 1 || got[0] != "process" {
		t.Fatalf("processUpdates = %+v, want process update on single card", got)
	}
}

func TestPromptCardStreamTruncatesLongProcessText(t *testing.T) {
	var cards []*fakeStreamCard
	ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})
	stream := newPromptCardStream(ctx, feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
	}, Session{ACPSessionID: "acp-session-1"})

	stream.updateProcess(strings.Repeat("前", maxPromptProcessRunes+20) + "尾部")

	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	got := cards[0].processUpdatesSnapshot()
	if len(got) != 1 {
		t.Fatalf("processUpdates = %+v, want one process update", got)
	}
	if !strings.HasPrefix(got[0], "（前面过程内容已省略）\n") {
		t.Fatalf("process update prefix = %q, want omission marker", got[0])
	}
	if !strings.HasSuffix(got[0], "尾部") {
		t.Fatalf("process update suffix = %q, want tail retained", got[0])
	}
	if len([]rune(got[0])) > maxPromptProcessRunes+20 {
		t.Fatalf("process update length = %d, want bounded text", len([]rune(got[0])))
	}
}

func TestPromptChunkAccumulatorDebouncesShortTextChunks(t *testing.T) {
	var cards []*fakeStreamCard
	ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})
	stream := newPromptCardStream(ctx, feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
	}, Session{ACPSessionID: "acp-session-1"})
	chunks := newPromptChunkAccumulator(stream)
	chunks.add(promptChunk{Target: promptChunkTargetText, Key: "agent_message", Text: "Hel"})
	chunks.add(promptChunk{Target: promptChunkTargetText, Key: "agent_message", Text: "lo"})
	if len(cards) != 0 {
		t.Fatalf("cards = %+v, want debounce to delay card creation", cards)
	}
	time.Sleep(promptCardFlushDelay + 80*time.Millisecond)
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card after debounce flush", cards)
	}
	if got := cards[0].textUpdatesSnapshot(); len(got) != 1 || got[0] != "Hello" {
		t.Fatalf("textUpdates = %+v, want one debounced update", got)
	}
	chunks.close()
}

func TestPromptRuntimeWaitsForInFlightDebouncedCardFlush(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "Hello",
		afterUpdates: func() {
			time.Sleep(promptCardFlushDelay + 80*time.Millisecond)
		},
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "Hello"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = t.TempDir()
	cfg.Agents["traex"] = agent
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	started := make(chan struct{})
	release := make(chan struct{})
	var cards []*fakeStreamCard
	ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		close(started)
		<-release
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})

	result := make(chan struct {
		reply string
		err   error
	}, 1)
	go func() {
		reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
			BotID:     "bot-a",
			MessageID: "om_msg",
			ChatID:    "oc_private",
			ChatType:  "p2p",
			Text:      "hello",
		})
		result <- struct {
			reply string
			err   error
		}{reply: reply, err: err}
	}()

	select {
	case <-started:
	case <-time.After(promptCardFlushDelay + time.Second):
		t.Fatal("stream card starter was not called")
	}
	select {
	case got := <-result:
		t.Fatalf("HandleFeishuMessage returned before in-flight card flush finished: reply=%q err=%v", got.reply, got.err)
	default:
	}
	close(release)

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("HandleFeishuMessage(prompt) error = %v", got.err)
		}
		if got.reply != "" {
			t.Fatalf("reply = %q, want empty final reply because card flush already started", got.reply)
		}
	case <-time.After(time.Second):
		t.Fatal("HandleFeishuMessage did not return after releasing stream card starter")
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].textUpdatesSnapshot(); len(got) != 1 || got[0] != "Hello" {
		t.Fatalf("textUpdates = %+v, want debounced card update", got)
	}
	if !cards[0].isClosed() {
		t.Fatalf("stream card should be closed")
	}
}

func TestNormalizeStreamMarkdownFoldsSoftLineBreaks(t *testing.T) {
	input := strings.Join([]string{
		"hello",
		"world",
		"from",
		"ACP.",
		"下一句",
		"继续",
		"",
		"- item 1",
		"- item 2",
		"",
		"```",
		"line 1",
		"line 2",
		"```",
	}, "\n")
	want := strings.Join([]string{
		"hello world from ACP.",
		"下一句继续",
		"",
		"- item 1",
		"- item 2",
		"",
		"```",
		"line 1",
		"line 2",
		"```",
	}, "\n")
	if got := normalizeStreamMarkdown(input); got != want {
		t.Fatalf("normalizeStreamMarkdown() = %q, want %q", got, want)
	}
}

func TestHandleFeishuMessageAutoCreatesSetupSessionWhenWorkspaceNotReady(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = workDir
	cfg.Agents["traex"] = agent
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "请先告诉我基础设置。"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	workspace := filepath.Join(t.TempDir(), "bot-a")

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		Workspace: workspace,
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "你好",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "请先告诉我基础设置。" {
		t.Fatalf("reply = %q, want setup reply", reply)
	}
	if len(rt.newCalls) != 1 {
		t.Fatalf("newCalls = %+v, want auto-created session", rt.newCalls)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want setup prompt only", rt.promptCalls)
	}
	if !strings.Contains(rt.promptCalls[0].Text, "Workspace Setup Required") {
		t.Fatalf("prompt text = %q, want setup prompt", rt.promptCalls[0].Text)
	}
	if strings.Contains(rt.promptCalls[0].Text, "你好") {
		t.Fatalf("setup prompt should not include user prompt before workspace is ready: %q", rt.promptCalls[0].Text)
	}
	session, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"})
	if !ok {
		t.Fatalf("auto-created setup session not persisted")
	}
	if session.Status != "setup" {
		t.Fatalf("session status = %q, want setup", session.Status)
	}
}

func TestHandleFeishuMessageStatusRefreshesReadySetupSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Default(), store)
	rt := &fakeRuntime{}
	svc.setRuntime(rt)
	workspace := filepath.Join(t.TempDir(), "bot-a")
	if err := markWorkspaceReady(workspace); err != nil {
		t.Fatalf("markWorkspaceReady() error = %v", err)
	}
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}
	if err := store.Upsert(Session{
		Key:          key,
		Title:        "setup session",
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
		Status:       "setup",
	}); err != nil {
		t.Fatalf("Upsert(setup session) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		Workspace: workspace,
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/status) error = %v", err)
	}
	if !strings.Contains(reply, "状态：ready") {
		t.Fatalf("reply = %q, want ready status", reply)
	}
	session, ok := store.Get(key)
	if !ok {
		t.Fatalf("session not found")
	}
	if session.Status != "ready" {
		t.Fatalf("persisted session status = %q, want ready", session.Status)
	}
	if session.ACPSessionID != "" {
		t.Fatalf("persisted session id = %q, want cleared setup ACP session", session.ACPSessionID)
	}
	if len(rt.closedKeys) != 1 || rt.closedKeys[0] != key {
		t.Fatalf("closedKeys = %+v, want old setup runtime closed", rt.closedKeys)
	}
}

func TestHandleFeishuMessageRecreatesSessionAfterSetupWorkspaceReady(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workspace := filepath.Join(t.TempDir(), "bot-a")
	if err := markWorkspaceReady(workspace); err != nil {
		t.Fatalf("markWorkspaceReady() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "SOUL.md"), []byte("# SOUL\n\n名字：小助手\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(SOUL.md) error = %v", err)
	}
	workDir := t.TempDir()
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "setup-session",
		Cwd:          workDir,
		Workspace:    workspace,
		Status:       "setup",
	}); err != nil {
		t.Fatalf("Upsert(setup session) error = %v", err)
	}
	rt := &fakeRuntime{newSessionID: "ready-session", promptReply: "我是小助手。"}
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		Workspace: workspace,
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "你是谁",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "我是小助手。" {
		t.Fatalf("reply = %q, want ready prompt reply", reply)
	}
	if len(rt.newCalls) != 1 {
		t.Fatalf("newCalls = %+v, want recreated ready session", rt.newCalls)
	}
	if rt.newCalls[0].Cwd != workDir {
		t.Fatalf("new session cwd = %q, want %q", rt.newCalls[0].Cwd, workDir)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want one prompt", rt.promptCalls)
	}
	if rt.promptCalls[0].Session.ACPSessionID != "ready-session" {
		t.Fatalf("prompt session = %q, want ready-session", rt.promptCalls[0].Session.ACPSessionID)
	}
	if !strings.Contains(rt.promptCalls[0].Text, "## Workspace Knowledge") || !strings.Contains(rt.promptCalls[0].Text, "## User Message") || !strings.Contains(rt.promptCalls[0].Text, "你是谁") {
		t.Fatalf("prompt text = %q, want workspace knowledge and user message", rt.promptCalls[0].Text)
	}
	session, ok := store.Get(key)
	if !ok {
		t.Fatalf("session not found")
	}
	if session.Status != "ready" || session.ACPSessionID != "ready-session" {
		t.Fatalf("session = %+v, want recreated ready session", session)
	}
}

func TestHandleFeishuMessageWorkspaceReadyAllowsNewSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Default(), store)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc.setRuntime(rt)
	workspace := filepath.Join(t.TempDir(), "bot-a")
	workDir := t.TempDir()
	msg := feishu.Message{
		BotID:     "bot-a",
		Workspace: workspace,
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
	}

	if err := markWorkspaceReady(workspace); err != nil {
		t.Fatalf("markWorkspaceReady() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "SOUL.md"), []byte("# SOUL\n\n名字：小助手\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(SOUL.md) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "MEMORY.md"), []byte("# MEMORY\n\n偏好：中文回复\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(MEMORY.md) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "knowledge", "core.md"), []byte("---\ntitle: core knowledge\ntype: knowledge\n---\n\n# Core Knowledge\n\n- [[repo-workflow]]：仓库开发流程。\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(knowledge/core.md) error = %v", err)
	}
	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		Workspace: msg.Workspace,
		MessageID: msg.MessageID,
		ChatID:    msg.ChatID,
		ThreadID:  msg.ThreadID,
		Text:      "/new " + workDir,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "已为当前会话创建 ACP 会话") {
		t.Fatalf("reply = %q, want new session confirmation", reply)
	}
	if len(rt.promptCalls) != 0 {
		t.Fatalf("promptCalls = %+v, want /new to defer ready prompt", rt.promptCalls)
	}
	session, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"})
	if !ok {
		t.Fatalf("session not found")
	}
	if session.PendingInitialPrompt != "ready" {
		t.Fatalf("pending prompt = %q, want ready", session.PendingInitialPrompt)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		Workspace: msg.Workspace,
		MessageID: "om_next",
		ChatID:    msg.ChatID,
		ThreadID:  msg.ThreadID,
		Text:      "介绍一下",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(next prompt) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want ready prompt on next message", rt.promptCalls)
	}
	readyPrompt := rt.promptCalls[0].Text
	for _, want := range []string{"Workspace Knowledge", "SOUL.md", "名字：小助手", "MEMORY.md", "偏好：中文回复", "knowledge/core.md", "repo-workflow", "skills/wiki/SKILL.md"} {
		if !strings.Contains(readyPrompt, want) {
			t.Fatalf("ready prompt = %q, want %q", readyPrompt, want)
		}
	}
	if !strings.Contains(readyPrompt, "## User Message") || !strings.Contains(readyPrompt, "介绍一下") {
		t.Fatalf("ready prompt = %q, want user message", readyPrompt)
	}
	session, ok = store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"})
	if !ok {
		t.Fatalf("session not found after next prompt")
	}
	if session.PendingInitialPrompt != "" {
		t.Fatalf("pending prompt = %q, want cleared", session.PendingInitialPrompt)
	}
}

func TestHandleFeishuMessageAutoCreatesReadySessionWithKnowledge(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = workDir
	cfg.Agents["traex"] = agent
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	workspace := filepath.Join(t.TempDir(), "bot-a")
	if err := markWorkspaceReady(workspace); err != nil {
		t.Fatalf("markWorkspaceReady() error = %v", err)
	}
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
			{Name: "我的智能助手"},
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
	for _, want := range []string{"Workspace Knowledge", "SOUL.md", "名字：小助手", "## User Message", "介绍一下这个仓库"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
	session, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"})
	if !ok {
		t.Fatalf("auto-created ready session not persisted")
	}
	if session.Status != "ready" {
		t.Fatalf("session status = %q, want ready", session.Status)
	}
}

func TestHandleFeishuMessageNewPersistsSession(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	store := NewSessionStore(storePath)
	svc := NewService(config.Default(), store)
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
			{Name: "我的智能助手"},
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
	session, ok := reloaded.Get(SessionKey{ChatID: "oc_chat", ThreadID: "omt_thread"})
	if !ok {
		t.Fatalf("persisted session not found")
	}
	if session.Cwd != repo {
		t.Fatalf("Cwd = %q, want expanded cwd", session.Cwd)
	}
	if session.ACPSessionID != "acp-session-1" || session.Status != "ready" {
		t.Fatalf("session = %+v, want ready acp session", session)
	}
}

func TestHandleFeishuMessagePersistsSessionByBotID(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Default(), store)
	rt := &fakeRuntime{newSessionID: "acp-session-1"}
	svc.setRuntime(rt)
	workDir := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "bot-a")
	if err := markWorkspaceReady(workspace); err != nil {
		t.Fatalf("markWorkspaceReady() error = %v", err)
	}

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
	if _, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}); !ok {
		t.Fatalf("session with bot id not found")
	}
	if _, ok := store.Get(SessionKey{ChatID: "oc_chat", ThreadID: "omt_thread"}); ok {
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
			AppSecret: "secret",
			Workspace: workspace,
		},
	}
	if err := markWorkspaceReady(workspace); err != nil {
		t.Fatalf("markWorkspaceReady() error = %v", err)
	}
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
	storePath := filepath.Join(workspace, "sessions.json")
	reloaded := NewSessionStore(storePath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, ok := reloaded.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}); !ok {
		t.Fatalf("persisted session not found in bot workspace store")
	}
}

func TestHandleFeishuMessageStatusShowsPersistedSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Default(), store)
	svc.setRuntime(&fakeRuntime{newSessionID: "acp-session-1"})
	workDir := t.TempDir()
	msg := feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/new " + workDir,
	}
	if _, err := handleFeishuMessage(t, svc, context.Background(), msg); err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		MessageID: "om_status",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/status) error = %v", err)
	}
	if !strings.Contains(reply, "cwd："+workDir) {
		t.Fatalf("reply = %q, want persisted cwd", reply)
	}
	if !strings.Contains(reply, "session：acp-session-1") {
		t.Fatalf("reply = %q, want acp session id", reply)
	}
}

func TestHandleFeishuMessageStatusFallsBackToRootID(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Default(), store)
	svc.setRuntime(&fakeRuntime{newSessionID: "acp-session-1"})
	workDir := t.TempDir()
	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		MessageID: "om_root",
		ChatID:    "oc_chat",
		Text:      "/new " + workDir,
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		MessageID: "om_reply",
		ChatID:    "oc_chat",
		RootID:    "om_root",
		Text:      "/status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/status) error = %v", err)
	}
	if !strings.Contains(reply, "cwd："+workDir) {
		t.Fatalf("reply = %q, want root session cwd", reply)
	}
}

func TestHandleFeishuMessageNewWithoutCwdUsesDefaultCwd(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = workDir
	cfg.Agents["traex"] = agent
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
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = defaultDir
	cfg.Agents["traex"] = agent
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

func TestHandleFeishuMessagePromptUsesPersistedSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)
	workDir := t.TempDir()
	msg := feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/new " + workDir,
	}
	if _, err := handleFeishuMessage(t, svc, context.Background(), msg); err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		MessageID: "om_prompt",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "@我的智能助手 介绍一下这个仓库",
		Mentions: []feishu.Mention{
			{Name: "我的智能助手"},
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
	if strings.Contains(rt.promptCalls[0].Text, "@我的智能助手") {
		t.Fatalf("prompt text = %q, should strip bot mention", rt.promptCalls[0].Text)
	}
	if rt.promptCalls[0].Session.ACPSessionID != "acp-session-1" {
		t.Fatalf("prompt session = %+v, want persisted acp session id", rt.promptCalls[0].Session)
	}
}

func TestHandleWikiCommandPersistsConfig(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Default(), store)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Status:       "ready",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	msg := feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/wiki interval 1s",
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki interval) error = %v", err)
	}
	if !strings.Contains(reply, "1s") {
		t.Fatalf("reply = %q, want interval confirmation", reply)
	}
	session, ok := store.Get(key)
	if !ok || session.WikiIntervalSec != 1 {
		t.Fatalf("session = %+v, want wiki interval persisted", session)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_off",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/wiki off",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki off) error = %v", err)
	}
	if !strings.Contains(reply, "已关闭") {
		t.Fatalf("reply = %q, want off confirmation", reply)
	}
	session, ok = store.Get(key)
	if !ok || !session.WikiDisabled {
		t.Fatalf("session = %+v, want wiki disabled", session)
	}
}

func TestHandleFeishuMessageCancelsInFlightPromptForNewMessage(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "ACP 回复",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "旧任务输出\n"},
				},
			},
		},
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Status:       "ready",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	ctx := context.Background()
	var cards []*fakeStreamCard
	ctx = feishu.WithStreamCardStarter(ctx, func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})
	firstDone := make(chan struct {
		reply string
		err   error
	}, 1)
	go func() {
		reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
			BotID:     "bot-a",
			MessageID: "om_first",
			ChatID:    "oc_chat",
			ThreadID:  "omt_thread",
			Text:      "先做这个长任务",
		})
		firstDone <- struct {
			reply string
			err   error
		}{reply: reply, err: err}
	}()
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_second",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "改成做这个",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(second) error = %v", err)
	}
	if reply != "" && reply != "ACP 回复" {
		t.Fatalf("reply = %q, want empty streamed reply or final text", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() >= 1 })
	select {
	case got := <-firstDone:
		if got.err != nil {
			t.Fatalf("first HandleFeishuMessage() error = %v", got.err)
		}
		if got.reply != "" {
			t.Fatalf("first reply = %q, want silent cancellation", got.reply)
		}
	case <-time.After(time.Second):
		t.Fatal("first prompt was not cancelled")
	}
	if rt.promptCallCount() != 2 {
		t.Fatalf("prompt calls = %d, want old and new prompt", rt.promptCallCount())
	}
	if len(cards) == 0 {
		t.Fatal("old prompt should create a stream card before cancellation")
	}
	cancelled := false
	for _, update := range cards[0].processUpdatesSnapshot() {
		if strings.Contains(update, "已取消") {
			cancelled = true
			break
		}
	}
	if !cancelled {
		t.Fatalf("process updates = %+v, want cancellation marker", cards[0].processUpdatesSnapshot())
	}
	if !cards[0].isClosed() {
		t.Fatal("cancelled old card should be closed")
	}
}

func TestWikiTimerRunsSilentReflection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptReply: "NoReply"}
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)
	session := Session{
		Key:             SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"},
		AgentName:       "traex",
		ACPSessionID:    "acp-session-1",
		Cwd:             t.TempDir(),
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		Status:          "ready",
		WikiIntervalSec: 1,
	}
	svc.scheduleWikiAfterUserPrompt(session, config.Default().Agents["traex"])

	waitForCondition(t, 2*time.Second, func() bool { return rt.promptCallCount() == 1 })
	if got := rt.promptCalls[0].Text; !strings.Contains(got, "请对刚才的对话进行反思") || !strings.Contains(got, "NoReply") {
		t.Fatalf("wiki prompt = %q, want reflection prompt", got)
	}
	svc.taskMu.Lock()
	status := svc.wikiStatuses[session.Key]
	_, hasTimer := svc.wikiTimers[session.Key]
	svc.taskMu.Unlock()
	if hasTimer {
		t.Fatal("wiki timer should not reschedule itself after reflection")
	}
	if !status.lastSuccess || status.running {
		t.Fatalf("wiki status = %+v, want completed success", status)
	}
}

func TestNewMessageCancelsRunningWikiReflection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptReply:   "ACP 回复",
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}
	session := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-1",
		Cwd:             t.TempDir(),
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		Status:          "ready",
		WikiIntervalSec: 1,
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc.taskMu.Lock()
	svc.wikiGenerations[key] = 1
	svc.taskMu.Unlock()
	wikiDone := make(chan struct{})
	go func() {
		svc.runWikiTimer(key, 1, session, config.Default().Agents["traex"])
		close(wikiDone)
	}()
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_user",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "先处理我的新问题",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(user) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want user prompt reply", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() >= 1 })
	select {
	case <-wikiDone:
	case <-time.After(time.Second):
		t.Fatal("wiki reflection was not cancelled")
	}
	if rt.promptCallCount() != 2 {
		t.Fatalf("prompt calls = %d, want wiki then user prompt", rt.promptCallCount())
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ok() {
		return
	}
	t.Fatalf("condition not met within %s", timeout)
}

func assertReadyPromptContainsUserTextAndMemoryPolicy(t *testing.T, prompt, userText string) {
	t.Helper()
	for _, want := range []string{"Workspace Memory Policy", "fs/read_text_file", "fs/write_text_file", "MEMORY.md", "knowledge/core.md", "knowledge/index.md", "skills/core.md", "## User Message", userText} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
}

type fakeRuntime struct {
	mu            sync.Mutex
	newSessionID  string
	newSessionIDs []string
	promptReply   string
	promptUpdates []acp.PromptUpdate
	afterUpdates  func()
	blockPrompt   chan struct{}
	blockPromptAt int
	newCalls      []fakeNewCall
	promptCalls   []fakePromptCall
	cancelCalls   []fakeCancelCall
	closedKeys    []SessionKey
}

type fakeNewCall struct {
	Key       SessionKey
	AgentName string
	Cwd       string
	Workspace string
}

type fakePromptCall struct {
	Session Session
	Text    string
}

type fakeCancelCall struct {
	Session Session
}

func (f *fakeRuntime) NewSession(ctx context.Context, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.newCalls = append(f.newCalls, fakeNewCall{Key: key, AgentName: agentName, Cwd: cwd, Workspace: workspace})
	if len(f.newSessionIDs) > 0 {
		id := f.newSessionIDs[0]
		f.newSessionIDs = f.newSessionIDs[1:]
		return id, nil
	}
	if f.newSessionID != "" {
		return f.newSessionID, nil
	}
	return "acp-session", nil
}

func (f *fakeRuntime) Prompt(ctx context.Context, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (string, error) {
	f.mu.Lock()
	f.promptCalls = append(f.promptCalls, fakePromptCall{Session: session, Text: text})
	callNumber := len(f.promptCalls)
	updates := append([]acp.PromptUpdate(nil), f.promptUpdates...)
	afterUpdates := f.afterUpdates
	blockPrompt := f.blockPrompt
	blockThisPrompt := blockPrompt != nil && (f.blockPromptAt == 0 || f.blockPromptAt == callNumber)
	reply := f.promptReply
	f.mu.Unlock()
	if opts.OnUpdate != nil {
		for _, update := range updates {
			opts.OnUpdate(update)
		}
	}
	if afterUpdates != nil {
		afterUpdates()
	}
	if blockThisPrompt {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-blockPrompt:
		}
	}
	return reply, nil
}

func (f *fakeRuntime) CancelSession(ctx context.Context, session Session, agent config.AgentConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls = append(f.cancelCalls, fakeCancelCall{Session: session})
	return nil
}

func (f *fakeRuntime) CloseSession(key SessionKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedKeys = append(f.closedKeys, key)
	return nil
}

func (f *fakeRuntime) Shutdown(ctx context.Context) error {
	return nil
}

func (f *fakeRuntime) promptCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.promptCalls)
}

func (f *fakeRuntime) cancelCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cancelCalls)
}

type fakeStreamCard struct {
	mu             sync.Mutex
	textUpdates    []string
	processUpdates []string
	closed         bool
}

func (f *fakeStreamCard) UpdateText(ctx context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.textUpdates = append(f.textUpdates, text)
	return nil
}

func (f *fakeStreamCard) UpdateProcess(ctx context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.processUpdates = append(f.processUpdates, text)
	return nil
}

func (f *fakeStreamCard) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeStreamCard) textUpdatesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.textUpdates...)
}

func (f *fakeStreamCard) processUpdatesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.processUpdates...)
}

func (f *fakeStreamCard) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}
