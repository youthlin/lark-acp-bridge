package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

func TestHandleFeishuMessageHelp(t *testing.T) {
	svc := newTestService(config.Default(), nil)
	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		Text: "/help",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if !strings.Contains(reply, "/status") {
		t.Fatalf("reply = %q, want status help", reply)
	}
	if !strings.Contains(reply, "/debug status|on|off") {
		t.Fatalf("reply = %q, want debug help", reply)
	}
	if !strings.Contains(reply, "/at - /at on: 必须at才响应; /at off auto|auto-reaction|every") {
		t.Fatalf("reply = %q, want at auto/auto-reaction/every help", reply)
	}
	if !strings.Contains(reply, "/loop add <补充消息>|status|stop") {
		t.Fatalf("reply = %q, want loop add help", reply)
	}
	if !strings.Contains(reply, "/sid <acp_session_id> <prompt>") {
		t.Fatalf("reply = %q, want sid help", reply)
	}
}

func TestSlashCommandTableIncludesHelpAndHandler(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.Models = &acp.SessionModelState{
		CurrentModelID: "gpt-5.5/high",
		AvailableModels: []acp.SessionModel{
			{ModelID: "gpt-5.5", Name: "GPT-5.5", Meta: traeLoadMeta(10)},
		},
	}
	session.ConfigOptions = []acp.SessionConfigOption{
		{
			ID:           "model",
			Name:         "Model",
			Category:     "model",
			Type:         "select",
			CurrentValue: "gpt-5.5",
			Options: []acp.SessionConfigOptionValue{
				{Value: "gpt-5.5", Name: "GPT-5.5"},
				{Value: "gpt-5.6", Name: "GPT-5.6"},
			},
		},
		{
			ID:           "mode",
			Name:         "Mode",
			Category:     "mode",
			Type:         "select",
			CurrentValue: "default",
			Options: []acp.SessionConfigOptionValue{
				{Value: "default", Name: "Default"},
				{Value: "plan", Name: "Plan"},
			},
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(&fakeRuntime{newSessionID: "acp-session-table"})
	helpReply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		Text: "/help",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/help) error = %v", err)
	}
	newCommandDir := t.TempDir()
	newCommandWorkspace := filepath.Join(t.TempDir(), "workspace")
	newCommandText := "/new " + newCommandDir + " 表项会话"
	for _, tt := range []struct {
		name string
		text string
		msg  feishu.Message
	}{
		{
			name: "/help",
			text: "/help",
			msg:  feishu.Message{Text: "/help"},
		},
		{
			name: "/agent",
			text: "/agent status",
			msg:  feishu.Message{BotID: "bot-a", ChatID: "oc_chat", ChatType: "p2p", Text: "/agent status"},
		},
		{
			name: "/at",
			text: "/at status",
			msg: feishu.Message{
				BotID:    "bot-a",
				ChatID:   "oc_group",
				ChatType: "group",
				Mentions: testBotMentions(),
				Text:     "/at status",
			},
		},
		{
			name: "/status",
			text: "/status",
			msg:  feishu.Message{Text: "/status", Workspace: filepath.Join(t.TempDir(), "workspace")},
		},
		{
			name: "/debug",
			text: "/debug status",
			msg:  feishu.Message{Text: "/debug status"},
		},
		{
			name: "/usage",
			text: "/usage day",
			msg:  feishu.Message{Text: "/usage day"},
		},
		{
			name: "/wiki",
			text: "/wiki status",
			msg:  feishu.Message{BotID: "bot-a", ChatID: "oc_wiki", ChatType: "p2p", Text: "/wiki status"},
		},
		{
			name: "/loop",
			text: "/loop status",
			msg:  feishu.Message{BotID: "bot-a", ChatID: "oc_loop", ChatType: "p2p", Text: "/loop status"},
		},
		{
			name: "/cmds",
			text: "/cmds",
			msg:  feishu.Message{Text: "/cmds"},
		},
		{
			name: "/config",
			text: "/config",
			msg: feishu.Message{
				BotID:            session.Key.BotID,
				ChatID:           sessionKeyMainID(session.Key),
				ThreadID:         session.Key.SubID,
				ChatType:         "topic_group",
				GroupMessageType: "thread",
				Mentions:         testBotMentions(),
				Text:             "/config",
			},
		},
		{
			name: "/model",
			text: "/model",
			msg: feishu.Message{
				BotID:            session.Key.BotID,
				ChatID:           sessionKeyMainID(session.Key),
				ThreadID:         session.Key.SubID,
				ChatType:         "topic_group",
				GroupMessageType: "thread",
				Mentions:         testBotMentions(),
				Text:             "/model",
			},
		},
		{
			name: "/mode",
			text: "/mode",
			msg: feishu.Message{
				BotID:            session.Key.BotID,
				ChatID:           sessionKeyMainID(session.Key),
				ThreadID:         session.Key.SubID,
				ChatType:         "topic_group",
				GroupMessageType: "thread",
				Mentions:         testBotMentions(),
				Text:             "/mode",
			},
		},
		{
			name: "/new",
			text: newCommandText,
			msg: feishu.Message{
				BotID:     "bot-a",
				ChatID:    "oc_new",
				ChatType:  "p2p",
				Text:      newCommandText,
				Workspace: newCommandWorkspace,
			},
		},
		{
			name: "/queue",
			text: "/queue",
			msg:  feishu.Message{Text: "/queue"},
		},
		{
			name: "/sid",
			text: "/sid",
			msg:  feishu.Message{Text: "/sid"},
		},
		{
			name: "/restart",
			text: "/restart",
			msg: feishu.Message{
				BotID:     "bot-a",
				ChatID:    "oc_restart",
				ChatType:  "p2p",
				SenderID:  testOwnerOpenID,
				Text:      "/restart",
				Workspace: filepath.Join(t.TempDir(), "workspace"),
			},
		},
		{
			name: "/schedule",
			text: "/schedule",
			msg: feishu.Message{
				BotID:     "bot-a",
				ChatID:    "oc_schedule",
				ChatType:  "p2p",
				SenderID:  testOwnerOpenID,
				Text:      "/schedule",
				Workspace: filepath.Join(t.TempDir(), "workspace"),
			},
		},
		{
			name: "/session",
			text: "/session list",
			msg: feishu.Message{
				BotID:            session.Key.BotID,
				ChatID:           sessionKeyMainID(session.Key),
				ThreadID:         session.Key.SubID,
				ChatType:         "topic_group",
				GroupMessageType: "thread",
				Mentions:         testBotMentions(),
				Text:             "/session list",
			},
		},
		{
			name: "/show",
			text: "/show status",
			msg: feishu.Message{
				BotID:            session.Key.BotID,
				ChatID:           sessionKeyMainID(session.Key),
				ThreadID:         session.Key.SubID,
				ChatType:         "topic_group",
				GroupMessageType: "thread",
				Mentions:         testBotMentions(),
				Text:             "/show status",
			},
		},
	} {
		command, ok := lookupSlashCommand(tt.name)
		if !ok {
			t.Fatalf("%s command missing from command table", tt.name)
		}
		if len(command.helpLines) == 0 {
			t.Fatalf("%s command help is empty", tt.name)
		}
		for _, helpLine := range command.helpLines {
			if !strings.Contains(helpReply, helpLine) {
				t.Fatalf("help reply = %q, want command table help %q", helpReply, helpLine)
			}
		}
		if strings.TrimSpace(tt.msg.Workspace) != "" {
			if err := os.MkdirAll(tt.msg.Workspace, 0o755); err != nil {
				t.Fatalf("MkdirAll(workspace) error = %v", err)
			}
		}
		got, err := handleFeishuMessage(t, svc, context.Background(), tt.msg)
		if err != nil {
			t.Fatalf("HandleFeishuMessage(%s) error = %v", tt.text, err)
		}
		want := command.run(svc, context.Background(), tt.text, tt.msg)
		if got != want {
			t.Fatalf("%s reply = %q, want command table handler reply %q", tt.name, got, want)
		}
	}
}

func TestHandleFeishuMessageSlashCommandRequiresConfiguredOwner(t *testing.T) {
	cfg := config.Default()
	svc := NewService(cfg, nil)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:  "bot-a",
		ChatID: "oc_chat",
		Text:   "/help",
		// 显式 SenderID 避免测试 helper 为普通命令用例注入默认 owner。
		SenderID: "ou_someone",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/help) error = %v", err)
	}
	if reply != "未配置 bot owner，不能执行斜杠命令。" {
		t.Fatalf("reply = %q, want missing owner warning", reply)
	}
}

func TestHandleFeishuMessageDebugCommandTogglesProgramLevel(t *testing.T) {
	orig := logging.ProgramLevel().Level()
	t.Cleanup(func() {
		logging.ProgramLevel().Set(orig)
	})
	logging.SetDebug(false)

	svc := newTestService(config.Default(), nil)
	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		Text: "/debug on",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/debug on) error = %v", err)
	}
	if !logging.DebugEnabled() {
		t.Fatal("DebugEnabled() = false, want true after /debug on")
	}
	if !strings.Contains(reply, "已开启 bridge debug 日志") || !strings.Contains(reply, "开启") {
		t.Fatalf("reply = %q, want debug enabled reply", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		Text: "/debug status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/debug status) error = %v", err)
	}
	if !strings.Contains(reply, "开启") {
		t.Fatalf("reply = %q, want debug status on", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		Text: "/debug off",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/debug off) error = %v", err)
	}
	if logging.DebugEnabled() {
		t.Fatal("DebugEnabled() = true, want false after /debug off")
	}
	if !strings.Contains(reply, "已关闭 bridge debug 日志") || !strings.Contains(reply, "关闭") {
		t.Fatalf("reply = %q, want debug disabled reply", reply)
	}
}

func TestHandleFeishuMessageAgentCommandSwitchesChatDefault(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	traexDir := t.TempDir()
	claudeDir := t.TempDir()
	cfg := config.Default()
	traex := mustConfigAgent(t, cfg, "traex")
	traex.DefaultCwd = traexDir
	cfg.SetAgent("traex", traex)
	cfg.SetAgent("claude", config.AgentConfig{
		Command:    "claude",
		Args:       []string{"acp", "serve"},
		DefaultCwd: claudeDir,
	})
	cfg.AgentList = []config.NamedAgentConfig{
		{Name: "traex", AgentConfig: mustConfigAgent(t, cfg, "traex")},
		{Name: "claude", AgentConfig: mustConfigAgent(t, cfg, "claude")},
	}
	rt := &fakeRuntime{newSessionIDs: []string{"acp-session-claude"}, promptReply: "ACP 回复"}
	svc := newTestService(cfg, store)
	svc.setRuntime(rt)
	msg := feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    msg.BotID,
		ChatID:   msg.ChatID,
		ChatType: msg.ChatType,
		Text:     "/agent",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/agent) error = %v", err)
	}
	if !strings.Contains(reply, "当前聊天默认 agent：") || !strings.Contains(reply, "traex") || !strings.Contains(reply, "claude") {
		t.Fatalf("reply = %q, want current and available agents", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    msg.BotID,
		ChatID:   msg.ChatID,
		ChatType: msg.ChatType,
		Text:     "/agent claude",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/agent claude) error = %v", err)
	}
	if !strings.Contains(reply, "已设置当前聊天默认 agent：claude") {
		t.Fatalf("reply = %q, want switch confirmation", reply)
	}
	chat, ok := store.GetChat(ChatKey{BotID: msg.BotID, ChatID: msg.ChatID})
	if !ok || chat.AgentName != "claude" {
		t.Fatalf("chat = %+v ok=%v, want agent claude", chat, ok)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    msg.BotID,
		ChatID:   msg.ChatID,
		ChatType: msg.ChatType,
		Text:     "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "agent：claude") || !strings.Contains(reply, "cwd："+claudeDir) {
		t.Fatalf("reply = %q, want claude agent and cwd", reply)
	}
	if len(rt.newCalls) != 1 || rt.newCalls[0].AgentName != "claude" || rt.newCalls[0].Cwd != claudeDir {
		t.Fatalf("newCalls = %+v, want claude session", rt.newCalls)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    msg.BotID,
		ChatID:   msg.ChatID,
		ChatType: msg.ChatType,
		Text:     "/agent missing",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/agent missing) error = %v", err)
	}
	if !strings.Contains(reply, "未知 agent：missing") || !strings.Contains(reply, "当前聊天默认 agent：claude") {
		t.Fatalf("reply = %q, want unknown agent and current status", reply)
	}
}

func TestHandleFeishuMessageStatus(t *testing.T) {
	svc := newTestService(config.Default(), nil)
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

func TestHandleFeishuMessageCommandsListsACPCommands(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.AvailableCommands = []acp.AvailableCommand{
		{Name: "review", Description: "Review my current changes", Input: &acp.AvailableCommandInput{Hint: "optional custom review instructions"}},
		{Name: "compact", Description: "summarize conversation"},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(&fakeRuntime{})

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/cmds",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/cmds) error = %v", err)
	}
	for _, want := range []string{"/review - Review my current changes", "参数：optional custom review instructions", "/compact - summarize conversation", "//review"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
}

func TestHandleFeishuMessageCommandsForwardsACPCommand(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.AvailableCommands = []acp.AvailableCommand{{Name: "review", Description: "Review my current changes"}}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{promptReply: "review done"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/cmds /review 重点看测试",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/cmds /review) error = %v", err)
	}
	if reply != "review done" {
		t.Fatalf("reply = %q, want review output", reply)
	}
	if len(rt.promptCalls) != 1 || rt.promptCalls[0].Text != "/review 重点看测试" {
		t.Fatalf("promptCalls = %+v, want ACP slash command prompt", rt.promptCalls)
	}
}

func TestHandleFeishuMessageDoubleSlashForwardsACPCommand(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.AvailableCommands = []acp.AvailableCommand{{Name: "review"}}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{promptReply: "review done"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	_, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "//review 快速检查",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(//review) error = %v", err)
	}
	if len(rt.promptCalls) != 1 || rt.promptCalls[0].Text != "/review 快速检查" {
		t.Fatalf("promptCalls = %+v, want double slash forwarded as single slash", rt.promptCalls)
	}
}

func TestHandleFeishuMessageDoubleSlashCompactResetsWorkspacePrompted(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.AvailableCommands = []acp.AvailableCommand{{Name: "compact"}}
	session.WorkspacePrompted = true
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{promptResults: []acp.PromptResult{
		{Text: "compacted"},
		{Text: "next done"},
	}}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	_, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "//compact",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(//compact) error = %v", err)
	}
	updated, ok := store.Get(session.Key)
	if !ok || updated.WorkspacePrompted {
		t.Fatalf("updated session = %+v, %v; want workspace prompt reset after compact", updated, ok)
	}

	_, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "继续",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(next prompt) error = %v", err)
	}
	calls := rt.promptCallsSnapshot()
	if len(calls) != 2 {
		t.Fatalf("promptCalls = %+v, want compact and next prompt", calls)
	}
	if !strings.Contains(calls[1].Text, "## Workspace Context") || !strings.Contains(calls[1].Text, "## Workspace Memory Policy") {
		t.Fatalf("next prompt = %q, want workspace context and memory policy after compact", calls[1].Text)
	}
}

func TestHandleFeishuMessageCompactConfig(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.ContextWindow = &acp.ContextWindowUsage{Used: 160000, Size: 200000}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(&fakeRuntime{})

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/compact on 80%",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/compact on) error = %v", err)
	}
	for _, want := range []string{"已开启自动 compact", "阈值：80%", "上下文窗口：160K/200K"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
	updated, ok := store.Get(session.Key)
	if !ok || !updated.AutoCompact || updated.AutoCompactPct != 80 {
		t.Fatalf("updated session = %+v, %v; want auto compact 80%%", updated, ok)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/compact off",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/compact off) error = %v", err)
	}
	if !strings.Contains(reply, "已关闭自动 compact") {
		t.Fatalf("reply = %q, want closed acknowledgement", reply)
	}
	updated, ok = store.Get(session.Key)
	if !ok || updated.AutoCompact || updated.AutoCompactPct != 0 || updated.AutoCompacting {
		t.Fatalf("updated session = %+v, %v; want auto compact disabled", updated, ok)
	}
}

func TestHandleFeishuMessageConfigShowsAndSetsOptions(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.Models = &acp.SessionModelState{
		CurrentModelID: "gpt-5.5/high",
		AvailableModels: []acp.SessionModel{
			{ModelID: "gpt-5.5", Name: "GPT-5.5", Meta: traeLoadMeta(10)},
		},
	}
	session.ConfigOptions = []acp.SessionConfigOption{
		{
			ID:           "model",
			Name:         "Model",
			Category:     "model",
			Description:  "Choose which model TRAE CLI should use",
			Type:         "select",
			CurrentValue: "gpt-5.5",
			Options: []acp.SessionConfigOptionValue{
				{Value: "Doubao-Seed-2.1-Pro", Name: "Doubao-Seed-2.1-Pro", Description: "184K context window, support reasoning."},
				{Value: "gpt-5.5", Name: "GPT-5.5", Description: "support reasoning, beta."},
				{Value: "Doubao_1_6", Name: "Doubao-Seed-Code", Description: "."},
			},
		},
		{
			ID:           "brave_mode",
			Name:         "Brave Mode",
			Description:  "Allow bolder actions",
			Type:         "boolean",
			CurrentValue: false,
		},
		{
			ID:           "reasoning",
			Name:         "Reasoning Effort",
			Type:         "select",
			CurrentValue: "medium",
			Options: []acp.SessionConfigOptionValue{
				{Value: "low", Name: "Low"},
				{Value: "medium", Name: "Medium"},
				{Value: "high", Name: "High"},
			},
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{
		configOptions: []acp.SessionConfigOption{
			{
				ID:           "reasoning",
				Name:         "Reasoning Effort",
				Type:         "select",
				CurrentValue: "high",
				Options: []acp.SessionConfigOptionValue{
					{Value: "low", Name: "Low"},
					{Value: "medium", Name: "Medium"},
					{Value: "high", Name: "High"},
				},
			},
			{
				ID:           "brave_mode",
				Name:         "Brave Mode",
				Type:         "boolean",
				CurrentValue: false,
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/config",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/config) error = %v", err)
	}
	for _, want := range []string{"当前 ACP 配置项", "model - Model [select] = gpt-5.5 (负载 10%)", "brave_mode - Brave Mode [boolean] = false", "reasoning - Reasoning Effort [select] = medium", "/config <id> <value>"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/config model",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/config model) error = %v", err)
	}
	for _, want := range []string{"ACP 配置项：model", "名称：Model", "说明：Choose which model TRAE CLI should use", "当前值：gpt-5.5", "- [ ] Doubao-Seed-2.1-Pro - 184K context window, support reasoning.", "- [x] GPT-5.5（gpt-5.5） - support reasoning, beta. - 负载 10%", "- [ ] Doubao-Seed-Code（Doubao_1_6）", "/config model <value>"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
	if strings.Contains(reply, "：.") {
		t.Fatalf("reply = %q, should not render dot-only description", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/config reasoning High",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/config reasoning High) error = %v", err)
	}
	if reply != "已设置配置项 reasoning：High（high）" {
		t.Fatalf("reply = %q, want reasoning set confirmation", reply)
	}
	if len(rt.configCalls) != 1 || rt.configCalls[0].ConfigID != "reasoning" || rt.configCalls[0].Value != "high" {
		t.Fatalf("configCalls = %+v, want reasoning high", rt.configCalls)
	}
	updated, ok := store.Get(session.Key)
	if !ok {
		t.Fatalf("session not found")
	}
	reasoningOpt, ok := findConfigOption(updated, "reasoning")
	if !ok || configOptionValueString(reasoningOpt.CurrentValue) != "high" {
		t.Fatalf("updated config options = %+v, want reasoning high", updated.ConfigOptions)
	}
}

func TestHandleFeishuMessageConfigSendsDetailCard(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.Models = &acp.SessionModelState{
		CurrentModelID: "gpt-5.5/high",
		AvailableModels: []acp.SessionModel{
			{ModelID: "gpt-5.5", Name: "GPT-5.5", Meta: traeLoadMeta(10)},
		},
	}
	session.ConfigOptions = []acp.SessionConfigOption{
		{
			ID:           "model",
			Name:         "Model",
			Category:     "model",
			Description:  "Choose which model TRAE CLI should use",
			Type:         "select",
			CurrentValue: "gpt-5.5",
			Options: []acp.SessionConfigOptionValue{
				{Value: "Doubao-Seed-2.1-Pro", Name: "Doubao-Seed-2.1-Pro", Description: "184K context window, support reasoning."},
				{Value: "gpt-5.5", Name: "GPT-5.5", Description: "support reasoning, beta."},
				{Value: "Doubao_1_6", Name: "Doubao-Seed-Code", Description: "."},
			},
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	svc := newTestService(config.Default(), store)
	var got feishu.ConfigDetailCard
	client := newFakeSentMessageClient("")
	client.configDetailSender = func(ctx context.Context, msg feishu.Message, card feishu.ConfigDetailCard) error {
		got = card
		return nil
	}
	svc.setOutbound(session.Key.BotID, client)
	ctx := context.Background()

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/config model",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/config model) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after sending config detail card", reply)
	}
	if got.ID != "model" || got.Name != "Model" || got.Category != "model" || got.Type != "select" || got.CurrentValue != "gpt-5.5" {
		t.Fatalf("card = %+v, want model config detail", got)
	}
	if got.Description != "Choose which model TRAE CLI should use" || got.SetCommand != "/config model <value>" {
		t.Fatalf("card = %+v, want description and set command", got)
	}
	if len(got.Options) != 3 {
		t.Fatalf("card options = %+v, want 3 options", got.Options)
	}
	if got.Options[1].Value != "gpt-5.5" || got.Options[1].Name != "GPT-5.5" || !got.Options[1].Current {
		t.Fatalf("current option = %+v, want GPT-5.5 current", got.Options[1])
	}
	if got.Options[1].LoadPercent == nil || *got.Options[1].LoadPercent != 10 {
		t.Fatalf("current option = %+v, want load percent 10", got.Options[1])
	}
	if got.Options[2].Description != "" {
		t.Fatalf("dot-only description = %q, want empty", got.Options[2].Description)
	}
}

func TestHandleFeishuMessageConfigSetsBooleanOption(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.ConfigOptions = []acp.SessionConfigOption{
		{
			ID:           "brave_mode",
			Name:         "Brave Mode",
			Type:         "boolean",
			CurrentValue: false,
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{
		configOptions: []acp.SessionConfigOption{
			{
				ID:           "brave_mode",
				Name:         "Brave Mode",
				Type:         "boolean",
				CurrentValue: true,
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/config brave_mode on",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/config brave_mode on) error = %v", err)
	}
	if reply != "已设置配置项 brave_mode：true" {
		t.Fatalf("reply = %q, want boolean set confirmation", reply)
	}
	if len(rt.configCalls) != 1 || rt.configCalls[0].ConfigID != "brave_mode" || rt.configCalls[0].Value != true {
		t.Fatalf("configCalls = %+v, want brave_mode true", rt.configCalls)
	}
}

func TestHandleFeishuMessageConfigRejectsUnknownOptionOrValue(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.ConfigOptions = []acp.SessionConfigOption{
		{
			ID:           "reasoning",
			Name:         "Reasoning Effort",
			Type:         "select",
			CurrentValue: "medium",
			Options: []acp.SessionConfigOptionValue{
				{Value: "low", Name: "Low"},
				{Value: "medium", Name: "Medium"},
				{Value: "high", Name: "High"},
			},
		},
		{
			ID:           "brave_mode",
			Name:         "Brave Mode",
			Type:         "boolean",
			CurrentValue: false,
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/config missing",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/config missing) error = %v", err)
	}
	if !strings.Contains(reply, "未知配置项：missing") || !strings.Contains(reply, "reasoning - Reasoning Effort") {
		t.Fatalf("reply = %q, want unknown config with status", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/config reasoning extreme",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/config reasoning extreme) error = %v", err)
	}
	if !strings.Contains(reply, "配置项 reasoning 不支持该值：extreme") || !strings.Contains(reply, "可选值") {
		t.Fatalf("reply = %q, want unsupported select value", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/config brave_mode maybe",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/config brave_mode maybe) error = %v", err)
	}
	if !strings.Contains(reply, "配置项 brave_mode 不支持该值：maybe") {
		t.Fatalf("reply = %q, want unsupported boolean value", reply)
	}
	if len(rt.configCalls) != 0 {
		t.Fatalf("configCalls = %+v, want no config call for invalid values", rt.configCalls)
	}
}

func TestHandleFeishuMessageModelShowsAndSetsModel(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.ConfigOptions = []acp.SessionConfigOption{
		{
			ID:           "model",
			Name:         "Model",
			Category:     "model",
			Type:         "select",
			CurrentValue: "gpt-5.5",
			Options: []acp.SessionConfigOptionValue{
				{Value: "gpt-5.5", Name: "GPT-5.5"},
				{Value: "gpt-5.6", Name: "GPT-5.6"},
			},
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{
		configOptions: []acp.SessionConfigOption{
			{
				ID:           "model",
				Name:         "Model",
				Category:     "model",
				Type:         "select",
				CurrentValue: "gpt-5.6",
				Options: []acp.SessionConfigOptionValue{
					{Value: "gpt-5.5", Name: "GPT-5.5"},
					{Value: "gpt-5.6", Name: "GPT-5.6"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/model",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/model) error = %v", err)
	}
	for _, want := range []string{"当前会话模型", "gpt-5.5", "gpt-5.6 - GPT-5.6", "/model <model>"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/model gpt-5.6",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/model gpt-5.6) error = %v", err)
	}
	if reply != "已设置当前会话模型：gpt-5.6" {
		t.Fatalf("reply = %q, want set confirmation", reply)
	}
	if len(rt.configCalls) != 1 || rt.configCalls[0].ConfigID != "model" || rt.configCalls[0].Value != "gpt-5.6" {
		t.Fatalf("configCalls = %+v, want model set_config_option", rt.configCalls)
	}
	updated, ok := store.Get(session.Key)
	if !ok {
		t.Fatalf("session not found")
	}
	modelOpt, ok := findModelConfigOption(updated)
	if !ok || modelValueString(modelOpt.CurrentValue) != "gpt-5.6" {
		t.Fatalf("updated config options = %+v, want gpt-5.6", updated.ConfigOptions)
	}
	chat, ok := store.GetChat(ChatKey{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)})
	agentConfig, hasAgentConfig := chat.AgentConfigs[session.AgentName]
	if !ok || !hasAgentConfig || agentConfig.Model != "gpt-5.6" {
		t.Fatalf("chat config = %+v, %v; want default model gpt-5.6", chat, ok)
	}
}

func TestHandleFeishuMessageModelSendsSelectionCard(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.ConfigOptions = []acp.SessionConfigOption{
		{
			ID:           "model",
			Name:         "Model",
			Category:     "model",
			Type:         "select",
			CurrentValue: "gpt-5.5",
			Options: []acp.SessionConfigOptionValue{
				{Value: "gpt-5.5", Name: "GPT-5.5"},
				{Value: "gpt-5.6", Name: "GPT-5.6"},
			},
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	cfg := config.Default()
	cfg.Bots[0].OwnerOpenIDs = []string{"ou_requester"}
	svc := NewService(cfg, store)
	var got feishu.ModelSelectionCard
	client := newFakeSentMessageClient("")
	client.modelSelectionSender = func(ctx context.Context, msg feishu.Message, card feishu.ModelSelectionCard) error {
		got = card
		return nil
	}
	svc.setOutbound(session.Key.BotID, client)
	ctx := context.Background()

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		SenderID: "ou_requester",
		Text:     "/model",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/model) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after sending card", reply)
	}
	if got.ACPSessionID != session.ACPSessionID || got.CurrentModel != "gpt-5.5" || got.RequesterID != "ou_requester" {
		t.Fatalf("card = %+v, want current session model card", got)
	}
	if len(got.Options) != 2 || got.Options[1].Value != "gpt-5.6" || got.Options[1].Name != "GPT-5.6" {
		t.Fatalf("card options = %+v, want available models", got.Options)
	}
}

func TestHandleFeishuMessageModeCommandShowsAndSetsMode(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.ConfigOptions = []acp.SessionConfigOption{
		{
			ID:           "mode",
			Name:         "Mode",
			Category:     "mode",
			Type:         "select",
			CurrentValue: "default",
			Options: []acp.SessionConfigOptionValue{
				{Value: "default", Name: "Default"},
				{Value: "plan", Name: "Plan"},
				{Value: "bypass_permissions", Name: "Bypass Permissions"},
			},
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{
		configOptions: []acp.SessionConfigOption{
			{
				ID:           "mode",
				Name:         "Mode",
				Category:     "mode",
				Type:         "select",
				CurrentValue: "plan",
				Options: []acp.SessionConfigOptionValue{
					{Value: "default", Name: "Default"},
					{Value: "plan", Name: "Plan"},
					{Value: "bypass_permissions", Name: "Bypass Permissions"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/mode",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/mode) error = %v", err)
	}
	for _, want := range []string{"当前会话模式", "default", "plan - Plan", "bypass_permissions - Bypass Permissions", "/mode <mode>"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/mode plan",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/mode plan) error = %v", err)
	}
	if reply != "已设置当前会话模式：plan" {
		t.Fatalf("reply = %q, want set confirmation", reply)
	}
	if len(rt.configCalls) != 1 || rt.configCalls[0].ConfigID != "mode" || rt.configCalls[0].Value != "plan" {
		t.Fatalf("configCalls = %+v, want mode set_config_option", rt.configCalls)
	}
	updated, ok := store.Get(session.Key)
	if !ok {
		t.Fatalf("session not found")
	}
	modeOpt, ok := findModeConfigOption(updated)
	if !ok || configOptionValueString(modeOpt.CurrentValue) != "plan" {
		t.Fatalf("updated config options = %+v, want plan", updated.ConfigOptions)
	}
	chat, ok := store.GetChat(ChatKey{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)})
	agentConfig, hasAgentConfig := chat.AgentConfigs[session.AgentName]
	if !ok || !hasAgentConfig || agentConfig.Mode != "plan" {
		t.Fatalf("chat config = %+v, %v; want default mode plan", chat, ok)
	}
}

func TestHandleFeishuMessageModeSendsSelectionCard(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.ConfigOptions = []acp.SessionConfigOption{
		{
			ID:           "mode",
			Name:         "Mode",
			Category:     "mode",
			Type:         "select",
			CurrentValue: "default",
			Options: []acp.SessionConfigOptionValue{
				{Value: "default", Name: "Default"},
				{Value: "plan", Name: "Plan"},
			},
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	cfg := config.Default()
	cfg.Bots[0].OwnerOpenIDs = []string{"ou_requester"}
	svc := NewService(cfg, store)
	var got feishu.ModeSelectionCard
	client := newFakeSentMessageClient("")
	client.modeSelectionSender = func(ctx context.Context, msg feishu.Message, card feishu.ModeSelectionCard) error {
		got = card
		return nil
	}
	svc.setOutbound(session.Key.BotID, client)
	ctx := context.Background()

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		SenderID: "ou_requester",
		Text:     "/mode",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/mode) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after sending card", reply)
	}
	if got.ACPSessionID != session.ACPSessionID || got.CurrentMode != "default" || got.RequesterID != "ou_requester" {
		t.Fatalf("card = %+v, want current session mode card", got)
	}
	if len(got.Options) != 2 || got.Options[1].Value != "plan" || got.Options[1].Name != "Plan" {
		t.Fatalf("card options = %+v, want available modes", got.Options)
	}
}

func TestHandleFeishuMessageShowCommandPersistsDisplayOptions(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	svc := newTestService(config.Default(), store)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/show thought off",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/show thought off) error = %v", err)
	}
	for _, want := range []string{"已关闭思考消息展示", "过程消息：开启", "计划：开启", "思考消息：关闭", "工具调用：开启", "状态栏：开启", "用量明细：开启"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
	updated, ok := store.GetChat(chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)}))
	if !ok {
		t.Fatal("chat config not found")
	}
	if !updated.HideThoughts || updated.ShowThoughts || updated.HideStepMessages || updated.HidePlans || updated.HideTools {
		t.Fatalf("chat display flags = step:%v plan:%v showThought:%v thought:%v tool:%v, want only thoughts hidden", updated.HideStepMessages, updated.HidePlans, updated.ShowThoughts, updated.HideThoughts, updated.HideTools)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/show thought on",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/show thought on) error = %v", err)
	}
	if !strings.Contains(reply, "已开启思考消息展示") || !strings.Contains(reply, "思考消息：开启") {
		t.Fatalf("reply = %q, want thought display enabled", reply)
	}
	updated, _ = store.GetChat(chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)}))
	if !updated.ShowThoughts || updated.HideThoughts {
		t.Fatalf("thought flags = show:%v hide:%v, want visible after /show thought on", updated.ShowThoughts, updated.HideThoughts)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/show plan off",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/show plan off) error = %v", err)
	}
	if !strings.Contains(reply, "已关闭计划展示") || !strings.Contains(reply, "计划：关闭") {
		t.Fatalf("reply = %q, want plan display disabled", reply)
	}
	updated, _ = store.GetChat(chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)}))
	if !updated.HidePlans {
		t.Fatalf("HidePlans = false, want true after /show plan off")
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/show STATUS off",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/show status off) error = %v", err)
	}
	if !strings.Contains(reply, "已关闭状态栏展示") || !strings.Contains(reply, "状态栏：关闭") {
		t.Fatalf("reply = %q, want status bar disabled", reply)
	}
	updated, _ = store.GetChat(chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)}))
	if !updated.HideStatusBar {
		t.Fatalf("HideStatusBar = false, want true after /show status off")
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/show used off",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/show used off) error = %v", err)
	}
	if !strings.Contains(reply, "已关闭用量明细展示") || !strings.Contains(reply, "用量明细：关闭") {
		t.Fatalf("reply = %q, want usage detail disabled", reply)
	}
	updated, _ = store.GetChat(chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)}))
	if !updated.HideUsageDetail {
		t.Fatalf("HideUsageDetail = false, want true after /show used off")
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "/show STATUS",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/show STATUS) error = %v", err)
	}
	if !strings.Contains(reply, "当前会话流式卡片展示：") || !strings.Contains(reply, "状态栏：关闭") {
		t.Fatalf("reply = %q, want show status output", reply)
	}
}

func TestHandleFeishuMessageShowCommandPersistsWithoutSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		Text:     "/show",
		ChatType: "p2p",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/show) error = %v", err)
	}
	for _, want := range []string{"计划：开启", "思考消息：关闭"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want default %q", reply, want)
		}
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		Text:     "/show step off",
		ChatType: "p2p",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/show step off) error = %v", err)
	}
	if !strings.Contains(reply, "已关闭过程消息展示") || !strings.Contains(reply, "过程消息：关闭") {
		t.Fatalf("reply = %q, want chat-level show option confirmation", reply)
	}
	chat, ok := store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_chat"})
	if !ok || !chat.HideStepMessages {
		t.Fatalf("chat config = %+v, %v; want step messages hidden", chat, ok)
	}
}

func TestHandleFeishuMessageCardCommandSendsOverviewCard(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.ConfigOptions = []acp.SessionConfigOption{
		{
			ID:           "model",
			Name:         "Model",
			Category:     "model",
			Type:         "select",
			CurrentValue: "gpt-5.5",
			Options: []acp.SessionConfigOptionValue{
				{Value: "gpt-5.5", Name: "GPT-5.5"},
				{Value: "gpt-5.6", Name: "GPT-5.6"},
			},
		},
		{
			ID:           "mode",
			Name:         "Mode",
			Category:     "mode",
			Type:         "select",
			CurrentValue: "default",
			Options: []acp.SessionConfigOptionValue{
				{Value: "default", Name: "Default"},
				{Value: "plan", Name: "Plan"},
			},
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session with options) error = %v", err)
	}
	oldSession := session
	oldSession.Title = "old session"
	oldSession.ACPSessionID = "acp-session-old"
	oldSession.Cwd = t.TempDir()
	if err := store.Upsert(oldSession); err != nil {
		t.Fatalf("Upsert(old session) error = %v", err)
	}
	if _, restored, err := store.ResumeSessionIfCurrent(session.Key, oldSession.ACPSessionID, session.ACPSessionID); err != nil || !restored {
		t.Fatalf("ResumeSessionIfCurrent(current) restored=%v err=%v", restored, err)
	}
	svc := newTestService(config.Default(), store)
	client := newFakeSentMessageClient("")
	var gotMsg feishu.Message
	var gotCard feishu.OverviewCard
	client.overviewSender = func(ctx context.Context, msg feishu.Message, card feishu.OverviewCard) error {
		gotMsg = msg
		gotCard = card
		return nil
	}
	svc.setOutbound(session.Key.BotID, client)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:            session.Key.BotID,
		ChatID:           sessionKeyMainID(session.Key),
		ThreadID:         session.Key.SubID,
		GroupMessageType: "thread",
		ChatType:         "topic_group",
		SenderID:         testOwnerOpenID,
		Mentions:         testBotMentions(),
		Text:             "/card",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/card) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after overview card is sent", reply)
	}
	if gotMsg.ChatID != sessionKeyMainID(session.Key) || gotMsg.ThreadID != session.Key.SubID {
		t.Fatalf("overview message = %+v, want current chat/thread", gotMsg)
	}
	if !gotCard.HasSession || gotCard.CurrentACPSessionID != session.ACPSessionID || gotCard.SessionTitle != "test session" {
		t.Fatalf("overview card session = %+v, want current session", gotCard)
	}
	if gotCard.ChatAgentName != "traex" || gotCard.AtStatus == "" || !gotCard.WikiEnabled {
		t.Fatalf("overview card config = %+v, want default chat config", gotCard)
	}
	if !gotCard.Show.Step || !gotCard.Show.Plan || gotCard.Show.Thought || !gotCard.Show.Tool || !gotCard.Show.Status || !gotCard.Show.Used {
		t.Fatalf("overview show = %+v, want default display flags", gotCard.Show)
	}
	if len(gotCard.AgentOptions) == 0 || gotCard.AgentOptions[0].Value != "traex" || !gotCard.AgentOptions[0].Current {
		t.Fatalf("agent options = %+v, want current traex option", gotCard.AgentOptions)
	}
	if len(gotCard.SessionOptions) != 2 || gotCard.SessionOptions[0].ACPSessionID != session.ACPSessionID || gotCard.SessionOptions[1].ACPSessionID != oldSession.ACPSessionID {
		t.Fatalf("session options = %+v, want current and historical sessions", gotCard.SessionOptions)
	}
	if len(gotCard.AtOptions) != 4 || gotCard.AtOptions[0].Value != "on" || !gotCard.AtOptions[0].Current || gotCard.AtOptions[3].Value != atModeAutoReaction {
		t.Fatalf("at options = %+v, want current /at on plus available at modes", gotCard.AtOptions)
	}
	if len(gotCard.ModelOptions) != 2 || gotCard.ModelOptions[1].Value != "gpt-5.6" {
		t.Fatalf("model options = %+v, want available models", gotCard.ModelOptions)
	}
	if len(gotCard.ModeOptions) != 2 || gotCard.ModeOptions[1].Value != "plan" {
		t.Fatalf("mode options = %+v, want available modes", gotCard.ModeOptions)
	}
}

func TestHandleFeishuMessageCardCommandFallsBackToTextWithoutOverviewOutbound(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_card",
		ChatType: "p2p",
		SenderID: testOwnerOpenID,
		Text:     "/card",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/card) error = %v", err)
	}
	for _, want := range []string{"当前聊天全览：", "默认 agent：traex", "当前还没有 ACP session"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
}

func TestHandleOverviewActionUpdatesChatConfig(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	cfg := config.Default()
	cfg.SetAgent("codex", config.AgentConfig{Command: "codex"})
	svc := newTestService(cfg, store)
	action := feishu.OverviewAction{
		BotID:               session.Key.BotID,
		ChatID:              sessionKeyMainID(session.Key),
		ThreadID:            session.Key.SubID,
		GroupMessageType:    "thread",
		RequesterID:         testOwnerOpenID,
		OperatorID:          testOwnerOpenID,
		CurrentACPSessionID: session.ACPSessionID,
	}

	showAction := action
	showAction.Action = overviewActionToggleShow
	showAction.Target = "status"
	showAction.Value = "off"
	result, err := svc.HandleOverviewAction(context.Background(), showAction)
	if err != nil {
		t.Fatalf("HandleOverviewAction(toggle show) error = %v", err)
	}
	if result.Overview == nil || result.Overview.Show.Status {
		t.Fatalf("overview result = %+v, want status hidden", result.Overview)
	}
	chat, ok := store.GetChat(ChatKey{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)})
	if !ok || !chat.HideStatusBar {
		t.Fatalf("chat config = %+v, %v; want status bar hidden", chat, ok)
	}

	wikiAction := action
	wikiAction.Action = overviewActionToggleWiki
	wikiAction.Value = "off"
	result, err = svc.HandleOverviewAction(context.Background(), wikiAction)
	if err != nil {
		t.Fatalf("HandleOverviewAction(toggle wiki) error = %v", err)
	}
	if result.Overview == nil || result.Overview.WikiEnabled {
		t.Fatalf("overview result = %+v, want wiki disabled", result.Overview)
	}
	chat, ok = store.GetChat(ChatKey{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)})
	if !ok || !chat.WikiDisabled {
		t.Fatalf("chat config = %+v, %v; want wiki disabled", chat, ok)
	}

	agentAction := action
	agentAction.Action = overviewActionSetAgent
	agentAction.Value = "codex"
	result, err = svc.HandleOverviewAction(context.Background(), agentAction)
	if err != nil {
		t.Fatalf("HandleOverviewAction(set agent) error = %v", err)
	}
	if result.Overview == nil || result.Overview.ChatAgentName != "codex" {
		t.Fatalf("overview result = %+v, want codex chat agent", result.Overview)
	}
	chat, ok = store.GetChat(ChatKey{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)})
	if !ok || chat.AgentName != "codex" {
		t.Fatalf("chat config = %+v, %v; want codex agent", chat, ok)
	}

	atAction := action
	atAction.ChatType = "topic_group"
	atAction.Action = overviewActionSetAt
	atAction.Value = atModeAutoReaction
	result, err = svc.HandleOverviewAction(context.Background(), atAction)
	if err != nil {
		t.Fatalf("HandleOverviewAction(set at) error = %v", err)
	}
	if result.Overview == nil || result.Overview.AtStatus != "自动判断 + 处理中表情" {
		t.Fatalf("overview result = %+v, want auto-reaction at status", result.Overview)
	}
	chat, ok = store.GetChat(ChatKey{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)})
	if !ok || !chat.MentionOptional || chat.AtMode != atModeAutoReaction {
		t.Fatalf("chat config = %+v, %v; want auto-reaction at mode", chat, ok)
	}
}

func TestHandleOverviewActionSetsModelAndMode(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.ConfigOptions = []acp.SessionConfigOption{
		{
			ID:           "model",
			Name:         "Model",
			Category:     "model",
			Type:         "select",
			CurrentValue: "gpt-5.5",
			Options: []acp.SessionConfigOptionValue{
				{Value: "gpt-5.5", Name: "GPT-5.5"},
				{Value: "gpt-5.6", Name: "GPT-5.6"},
			},
		},
		{
			ID:           "mode",
			Name:         "Mode",
			Category:     "mode",
			Type:         "select",
			CurrentValue: "default",
			Options: []acp.SessionConfigOptionValue{
				{Value: "default", Name: "Default"},
				{Value: "plan", Name: "Plan"},
			},
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{
		configOptions: []acp.SessionConfigOption{
			{
				ID:           "model",
				Name:         "Model",
				Category:     "model",
				Type:         "select",
				CurrentValue: "gpt-5.6",
				Options: []acp.SessionConfigOptionValue{
					{Value: "gpt-5.5", Name: "GPT-5.5"},
					{Value: "gpt-5.6", Name: "GPT-5.6"},
				},
			},
			{
				ID:           "mode",
				Name:         "Mode",
				Category:     "mode",
				Type:         "select",
				CurrentValue: "plan",
				Options: []acp.SessionConfigOptionValue{
					{Value: "default", Name: "Default"},
					{Value: "plan", Name: "Plan"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	action := feishu.OverviewAction{
		BotID:               session.Key.BotID,
		ChatID:              sessionKeyMainID(session.Key),
		ThreadID:            session.Key.SubID,
		GroupMessageType:    "thread",
		RequesterID:         testOwnerOpenID,
		OperatorID:          testOwnerOpenID,
		CurrentACPSessionID: session.ACPSessionID,
	}

	modelAction := action
	modelAction.Action = overviewActionSetModel
	modelAction.Value = "gpt-5.6"
	result, err := svc.HandleOverviewAction(context.Background(), modelAction)
	if err != nil {
		t.Fatalf("HandleOverviewAction(set model) error = %v", err)
	}
	if result.Overview == nil || result.Overview.Model != "gpt-5.6" || len(result.Overview.ModelOptions) != 2 {
		t.Fatalf("overview result = %+v, want refreshed model options", result.Overview)
	}
	if len(rt.configCalls) != 1 || rt.configCalls[0].ConfigID != "model" || rt.configCalls[0].Value != "gpt-5.6" {
		t.Fatalf("configCalls = %+v, want model set_config_option", rt.configCalls)
	}
	chat, ok := store.GetChat(ChatKey{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)})
	agentConfig, hasAgentConfig := chat.AgentConfigs[session.AgentName]
	if !ok || !hasAgentConfig || agentConfig.Model != "gpt-5.6" {
		t.Fatalf("chat config = %+v, %v; want default model gpt-5.6", chat, ok)
	}

	modeAction := action
	modeAction.Action = overviewActionSetMode
	modeAction.Value = "plan"
	result, err = svc.HandleOverviewAction(context.Background(), modeAction)
	if err != nil {
		t.Fatalf("HandleOverviewAction(set mode) error = %v", err)
	}
	if result.Overview == nil || result.Overview.Mode != "plan" || len(result.Overview.ModeOptions) != 2 {
		t.Fatalf("overview result = %+v, want refreshed mode options", result.Overview)
	}
	if len(rt.configCalls) != 2 || rt.configCalls[1].ConfigID != "mode" || rt.configCalls[1].Value != "plan" {
		t.Fatalf("configCalls = %+v, want mode set_config_option", rt.configCalls)
	}
	chat, ok = store.GetChat(ChatKey{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)})
	agentConfig, hasAgentConfig = chat.AgentConfigs[session.AgentName]
	if !ok || !hasAgentConfig || agentConfig.Mode != "plan" || agentConfig.Model != "gpt-5.6" {
		t.Fatalf("chat config = %+v, %v; want default mode plan and model gpt-5.6", chat, ok)
	}
}

func TestHandleOverviewActionSetSessionRestoresAndRefreshesCard(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	current := testReadySession(t, store)
	old := current
	old.Title = "old session"
	old.ACPSessionID = "acp-session-old"
	old.Cwd = t.TempDir()
	if err := store.Upsert(old); err != nil {
		t.Fatalf("Upsert(old session) error = %v", err)
	}
	if _, restored, err := store.ResumeSessionIfCurrent(current.Key, old.ACPSessionID, current.ACPSessionID); err != nil || !restored {
		t.Fatalf("ResumeSessionIfCurrent(current) restored=%v err=%v", restored, err)
	}
	svc := newTestService(config.Default(), store)

	result, err := svc.HandleOverviewAction(context.Background(), feishu.OverviewAction{
		BotID:               current.Key.BotID,
		ChatID:              sessionKeyMainID(current.Key),
		ChatType:            "topic_group",
		ThreadID:            current.Key.SubID,
		GroupMessageType:    "thread",
		RequesterID:         testOwnerOpenID,
		OperatorID:          testOwnerOpenID,
		CurrentACPSessionID: current.ACPSessionID,
		Action:              overviewActionSetSession,
		Value:               old.ACPSessionID,
	})
	if err != nil {
		t.Fatalf("HandleOverviewAction(set session) error = %v", err)
	}
	if result.Overview == nil || result.Overview.CurrentACPSessionID != old.ACPSessionID || result.Overview.SessionTitle != "old session" {
		t.Fatalf("overview result = %+v, want old session refreshed", result.Overview)
	}
	got, ok := store.Get(current.Key)
	if !ok || got.ACPSessionID != old.ACPSessionID {
		t.Fatalf("current session = %+v, %v; want old session restored", got, ok)
	}
}

func TestHandleOverviewActionShowUsageSendsReportAndRefreshesCard(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.Bots[0].Workspace = workspace
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	svc := newTestService(cfg, store)
	client := newFakeSentMessageClient("")
	var sentText string
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		sentText = text
		return nil
	}
	svc.setOutbound("bot-a", client)

	result, err := svc.HandleOverviewAction(context.Background(), feishu.OverviewAction{
		BotID:       "bot-a",
		ChatID:      "oc_card",
		ChatType:    "p2p",
		RequesterID: testOwnerOpenID,
		OperatorID:  testOwnerOpenID,
		Action:      overviewActionShowUsage,
	})
	if err != nil {
		t.Fatalf("HandleOverviewAction(show usage) error = %v", err)
	}
	if result.Overview == nil || result.Toast != "已发送用量" {
		t.Fatalf("overview result = %+v, want refreshed overview and usage toast", result)
	}
	if !strings.Contains(sentText, "Token 用量报告") {
		t.Fatalf("sent usage text = %q, want token usage report", sentText)
	}
}

func TestHandleOverviewActionPreservesChatTypeWhenRefreshingAtStatus(t *testing.T) {
	for _, tt := range []struct {
		name             string
		chatID           string
		chatType         string
		groupMessageType string
		wantAtStatus     string
	}{
		{
			name:         "private",
			chatID:       "oc_private_card",
			chatType:     "p2p",
			wantAtStatus: "私聊始终响应",
		},
		{
			name:             "ordinary group",
			chatID:           "oc_group_card",
			chatType:         "group",
			groupMessageType: "chat",
			wantAtStatus:     "需要 at",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
			svc := newTestService(config.Default(), store)

			result, err := svc.HandleOverviewAction(context.Background(), feishu.OverviewAction{
				BotID:            "bot-a",
				ChatID:           tt.chatID,
				ChatType:         tt.chatType,
				GroupMessageType: tt.groupMessageType,
				RequesterID:      testOwnerOpenID,
				OperatorID:       testOwnerOpenID,
				Action:           overviewActionToggleShow,
				Target:           "status",
				Value:            "off",
			})
			if err != nil {
				t.Fatalf("HandleOverviewAction(toggle show) error = %v", err)
			}
			if result.Overview == nil {
				t.Fatal("result.Overview = nil, want refreshed overview")
			}
			if result.Overview.ChatType != tt.chatType {
				t.Fatalf("overview ChatType = %q, want %q", result.Overview.ChatType, tt.chatType)
			}
			if result.Overview.AtStatus != tt.wantAtStatus {
				t.Fatalf("overview AtStatus = %q, want %q", result.Overview.AtStatus, tt.wantAtStatus)
			}
			if result.Overview.Show.Status {
				t.Fatalf("overview Show.Status = true, want false after toggle")
			}
		})
	}
}

func TestHandleFeishuMessageShowCommandSurvivesNewSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{
		newSessionID: "acp-session-new",
		promptResult: acp.PromptResult{
			Text:       "完成。",
			StopReason: "end_turn",
			Raw:        json.RawMessage(`{"stopReason":"end_turn","usage":{"inputTokens":1200,"outputTokens":345},"_meta":{"trace":"abc"}}`),
			Usage: acp.TokenUsage{
				InputTokens:  1200,
				OutputTokens: 345,
			},
		},
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-new",
				Update: acp.SessionUpdate{
					SessionUpdate: "usage_update",
					Used:          53000,
					Size:          200000,
				},
			},
			{
				SessionID: "acp-session-new",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
				},
			},
		},
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	msg := feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
	}

	for _, command := range []string{"/show status off", "/show used off"} {
		reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
			BotID:    msg.BotID,
			ChatID:   msg.ChatID,
			ChatType: msg.ChatType,
			Text:     command,
		})
		if err != nil {
			t.Fatalf("HandleFeishuMessage(%s) error = %v", command, err)
		}
		if !strings.Contains(reply, "已关闭") {
			t.Fatalf("reply = %q, want show option confirmation", reply)
		}
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    msg.BotID,
		ChatID:   msg.ChatID,
		ChatType: msg.ChatType,
		Text:     "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "已为当前会话创建 ACP 会话") {
		t.Fatalf("reply = %q, want new session confirmation", reply)
	}
	if session, ok := store.Get(imSessionKey(msg.BotID, msg.ChatID, "")); !ok || session.HideStatusBar || session.HideUsageDetail {
		t.Fatalf("session = %+v, %v; show options should not be stored on session", session, ok)
	}
	var statusBarEnabled *bool
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		enabled := options.StatusBarEnabled
		statusBarEnabled = &enabled
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err = handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_msg",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "run",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want streamed reply", reply)
	}
	if statusBarEnabled == nil || *statusBarEnabled {
		t.Fatalf("statusBarEnabled = %v, want false after /new", statusBarEnabled)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].statusUpdatesSnapshot(); len(got) != 0 {
		t.Fatalf("statusUpdates = %+v, want none after /new", got)
	}
	if got := cards[0].usageDetailsSnapshot(); len(got) != 0 {
		t.Fatalf("usageDetails = %+v, want none after /new", got)
	}
}

func TestHandleFeishuGroupChatAtCommandConfiguresMentionRequirement(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	msg := feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_group",
		ChatType: "group",
	}
	if err := store.UpsertChat(ChatConfig{
		Key:             ChatKey{BotID: "bot-a", ChatID: "oc_group"},
		WikiIntervalSec: 30,
		HideUsageDetail: true,
	}); err != nil {
		t.Fatalf("UpsertChat(chat) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_status",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "@智能助手 /at status",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/at status) error = %v", err)
	}
	if !strings.Contains(reply, "需要 at 才响应") {
		t.Fatalf("reply = %q, want default mention-required status", reply)
	}
	if !strings.Contains(reply, "/at off auto") || !strings.Contains(reply, "/at off auto-reaction") || !strings.Contains(reply, "/at off every") {
		t.Fatalf("reply = %q, want status to advertise auto, auto-reaction, and every modes", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_off_without_mention",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "/at off",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/at off without mention) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want silent ignore for /at off without mention while mention is required", reply)
	}
	chat, ok := store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_group"})
	if !ok || chat.MentionOptional || chat.WikiIntervalSec != 30 || !chat.HideUsageDetail {
		t.Fatalf("chat config = %+v, %v; ignored /at off should not change existing chat config", chat, ok)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_off_mention_other",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "@其他助手 /at off",
		Mentions:  []feishu.Mention{{ID: "ou_other_bot", Name: "其他助手", Type: "bot"}},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(@other /at off) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want silent ignore for /at off mentioning another bot", reply)
	}
	chat, ok = store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_group"})
	if !ok || chat.MentionOptional || chat.WikiIntervalSec != 30 || !chat.HideUsageDetail {
		t.Fatalf("chat config = %+v, %v; /at off mentioning another bot should not change existing chat config", chat, ok)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_off",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "@智能助手 /at OFF every",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(@bot /at OFF every) error = %v", err)
	}
	if !strings.Contains(reply, "无需 at") {
		t.Fatalf("reply = %q, want every-message confirmation", reply)
	}
	chat, ok = store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_group"})
	if !ok || !chat.MentionOptional || chat.AtMode != atModeEvery || chat.WikiIntervalSec != 30 || !chat.HideUsageDetail {
		t.Fatalf("chat config = %+v, %v; want mention optional without clearing other chat options", chat, ok)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_status_upper",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "/at STATUS",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/at STATUS) error = %v", err)
	}
	if !strings.Contains(reply, "无需 at") {
		t.Fatalf("reply = %q, want case-insensitive status", reply)
	}
	if !strings.Contains(reply, "每条消息都会响应") {
		t.Fatalf("reply = %q, want every mode status", reply)
	}

	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_every_prompt",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "无需 at 也处理",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group no mention after /at off every) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply in every mode", reply)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want one prompt after /at off every", rt.promptCalls)
	}
	if len(cards) != 0 {
		t.Fatalf("cards = %+v, want no stream card without stream card context", cards)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_auto",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "/at off auto",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/at off auto) error = %v", err)
	}
	if !strings.Contains(reply, "自动判断") {
		t.Fatalf("reply = %q, want auto mode confirmation", reply)
	}
	chat, ok = store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_group"})
	if !ok || !chat.MentionOptional || chat.AtMode != atModeAuto || chat.WikiIntervalSec != 30 || !chat.HideUsageDetail {
		t.Fatalf("chat config = %+v, %v; want auto mode without clearing other chat options", chat, ok)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_auto_status",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "/at status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/at status auto) error = %v", err)
	}
	if !strings.Contains(reply, "自动判断") || !strings.Contains(reply, "/at off auto-reaction") || !strings.Contains(reply, "/at off every") {
		t.Fatalf("reply = %q, want auto mode status with auto-reaction and every switch hints", reply)
	}

	rt.mu.Lock()
	rt.promptCalls = nil
	rt.atAutoRuntimeCalls = nil
	rt.promptResults = []acp.PromptResult{
		{Text: "Context compacted Heads up: Long threads and multiple compactions can cause the model to be less accurate. Start a new thread when possible to keep threads small and targeted.SILENT"},
		{Text: "需要回复"},
	}
	rt.promptUpdates = []acp.PromptUpdate{
		{
			SessionID: "acp-session-1",
			Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "处理中"},
			},
		},
	}
	rt.mu.Unlock()
	svc.setOutbound("bot-a", client)

	reply, err = handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_prompt",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "路过闲聊",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group no mention after auto) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want SILENT suppressed in auto mode", reply)
	}
	rt.mu.Lock()
	autoCalls := append([]fakePromptCall(nil), rt.atAutoRuntimeCalls...)
	promptCalls := append([]fakePromptCall(nil), rt.promptCalls...)
	rt.mu.Unlock()
	if len(promptCalls) != 0 {
		t.Fatalf("promptCalls = %+v, want no main prompt for SILENT auto mode decision", promptCalls)
	}
	if len(autoCalls) != 1 {
		t.Fatalf("atAutoRuntimeCalls = %+v, want one companion decision after /at off auto", autoCalls)
	}
	if len(cards) != 0 {
		t.Fatalf("cards = %+v, want no stream card for SILENT auto mode decision", cards)
	}
	autoPrompt := autoCalls[0].Text
	for _, want := range []string{
		"# 群聊自动响应判断",
		"at-auto 伴生判断会话",
		"最终只输出 SILENT",
		"最终只输出 RESPOND",
		"路过闲聊",
	} {
		if !strings.Contains(autoPrompt, want) {
			t.Fatalf("auto prompt = %q, want %q", autoPrompt, want)
		}
	}
	if autoCalls[0].Runtime.Scope != runtimeScopeAtAuto {
		t.Fatalf("auto runtime = %+v, want at-auto companion scope", autoCalls[0].Runtime)
	}

	rt.mu.Lock()
	rt.promptResults = []acp.PromptResult{{Text: "明确回复"}}
	rt.promptUpdates = nil
	rt.mu.Unlock()
	reply, err = handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_auto_mentioned",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "@智能助手 请处理",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group mention after auto) error = %v", err)
	}
	if reply != "明确回复" {
		t.Fatalf("reply = %q, want normal reply for explicit mention in auto mode", reply)
	}
	rt.mu.Lock()
	mentionPrompt := rt.promptCalls[len(rt.promptCalls)-1].Text
	promptCallCountAfterMention := len(rt.promptCalls)
	rt.mu.Unlock()
	if !strings.Contains(mentionPrompt, "请处理") {
		t.Fatalf("mention prompt = %q, want original user message", mentionPrompt)
	}
	for _, unexpected := range []string{
		"## 群聊明确提及",
		"当前群聊已启用 /at off auto",
		"不能输出 SILENT",
		"请先判断这条未 at bot 的群消息是否需要你回复",
		"如果消息与当前会话、你的职责或正在处理的任务无关，最终只输出 SILENT",
	} {
		if strings.Contains(mentionPrompt, unexpected) {
			t.Fatalf("mention prompt = %q, should not contain auto decision rule %q", mentionPrompt, unexpected)
		}
	}

	rt.mu.Lock()
	rt.promptResults = []acp.PromptResult{{Text: "SILENT"}}
	rt.promptUpdates = nil
	rt.mu.Unlock()
	reply, err = handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_auto_mentioned_silent",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "@智能助手 下班了？",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group mention returns SILENT after auto) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want invalid mention SILENT suppressed", reply)
	}

	rt.mu.Lock()
	rt.promptResults = []acp.PromptResult{{Text: "RESPOND"}, {Text: "需要回复"}}
	rt.mu.Unlock()
	reply, err = handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_auto_reply",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "这个可能需要回复",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group no mention after auto reply) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because delayed auto card was sent", reply)
	}
	rt.mu.Lock()
	autoCalls = append([]fakePromptCall(nil), rt.atAutoRuntimeCalls...)
	promptCalls = append([]fakePromptCall(nil), rt.promptCalls...)
	rt.mu.Unlock()
	if len(autoCalls) != 2 {
		t.Fatalf("atAutoRuntimeCalls = %+v, want two companion decisions", autoCalls)
	}
	if len(promptCalls) != promptCallCountAfterMention+2 {
		t.Fatalf("promptCalls = %+v, want mention prompts plus one main auto response", promptCalls)
	}
	mainAutoPrompt := promptCalls[len(promptCalls)-1].Text
	if strings.Contains(mainAutoPrompt, "群聊自动响应判断") || strings.Contains(mainAutoPrompt, "最终只输出 SILENT") {
		t.Fatalf("main auto prompt = %q, should not contain auto decision rules", mainAutoPrompt)
	}
	if !strings.Contains(mainAutoPrompt, "这个可能需要回复") {
		t.Fatalf("main auto prompt = %q, want original user text", mainAutoPrompt)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one delayed auto stream card when reply is needed", cards)
	}
	if got := cards[0].finalTextUpdatesSnapshot(); len(got) == 0 || got[len(got)-1] != "需要回复" {
		t.Fatalf("finalTextUpdates = %+v, want final auto reply card text", got)
	}
	if got := cards[0].processUpdatesSnapshot(); len(got) != 1 || got[0] != "sid: acp-session-1\nmsg: om\\_auto\\_reply" {
		t.Fatalf("processUpdates = %+v, want sid-only process row when auto reply has no tool boundary", got)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_auto_reaction_mode",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "/at off auto-reaction",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/at off auto-reaction) error = %v", err)
	}
	if !strings.Contains(reply, "自动判断") || !strings.Contains(reply, "处理中表情") {
		t.Fatalf("reply = %q, want auto-reaction mode confirmation", reply)
	}
	chat, ok = store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_group"})
	if !ok || !chat.MentionOptional || chat.AtMode != atModeAutoReaction || chat.WikiIntervalSec != 30 || !chat.HideUsageDetail {
		t.Fatalf("chat config = %+v, %v; want auto-reaction mode without clearing other chat options", chat, ok)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_auto_reaction_status",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "/at status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/at status auto-reaction) error = %v", err)
	}
	if !strings.Contains(reply, "自动判断") || !strings.Contains(reply, "处理中表情") || !strings.Contains(reply, "/at off auto") {
		t.Fatalf("reply = %q, want auto-reaction mode status with auto switch hint", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_on",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "/at on",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/at on) error = %v", err)
	}
	if !strings.Contains(reply, "需要 at") {
		t.Fatalf("reply = %q, want mention required confirmation", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_ignored",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "再次不 at",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group no mention after on) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want silent ignore after /at on", reply)
	}
	if len(rt.promptCalls) != len(promptCalls) {
		t.Fatalf("promptCalls = %+v, want no extra prompt after /at on", rt.promptCalls)
	}
}

func TestHandleFeishuPrivateChatIgnoresAtConfigAndAlwaysResponds(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	msg := feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_private",
		ChatType: "p2p",
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_at",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "/at off",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(private /at) error = %v", err)
	}
	if !strings.Contains(reply, "私聊不支持") {
		t.Fatalf("reply = %q, want private unsupported message", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_prompt",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "不用 at 也响应",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(private prompt) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want private prompt", rt.promptCalls)
	}
}

func TestHandleFeishuTopicThreadAllowsNewAndSessionCommands(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	rt := &fakeRuntime{newSessionIDs: []string{"acp-session-1", "acp-session-2"}}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	base := feishu.Message{
		BotID:            "bot-a",
		ChatID:           "oc_group",
		ChatType:         "topic_group",
		GroupMessageType: "thread",
		ThreadID:         "omt_topic",
		SenderID:         testOwnerOpenID,
		Mentions:         []feishu.Mention{testBotMention("智能助手")},
	}

	for i, command := range []string{
		"@智能助手 /new " + firstDir + " 第一个",
		"@智能助手 /new " + secondDir + " 第二个",
	} {
		msg := base
		msg.MessageID = fmt.Sprintf("om_new_%d", i+1)
		msg.Text = command
		reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
		if err != nil {
			t.Fatalf("HandleFeishuMessage(topic /new %d) error = %v", i+1, err)
		}
		if !strings.Contains(reply, fmt.Sprintf("acp-session-%d", i+1)) {
			t.Fatalf("reply = %q, want created topic session %d", reply, i+1)
		}
	}
	topicKey := imSessionKey(base.BotID, base.ChatID, base.ThreadID)
	if len(rt.newCalls) != 2 || rt.newCalls[0].Key != topicKey || rt.newCalls[1].Key != topicKey {
		t.Fatalf("newCalls = %+v, want both sessions scoped to current topic", rt.newCalls)
	}
	if _, ok := store.Get(imSessionKey(base.BotID, base.ChatID, "")); ok {
		t.Fatal("topic /new unexpectedly created a chat-level session")
	}

	titleMsg := base
	titleMsg.MessageID = "om_title"
	titleMsg.Text = "@智能助手 /session title 当前话题"
	reply, err := handleFeishuMessage(t, svc, context.Background(), titleMsg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(topic /session title) error = %v", err)
	}
	if !strings.Contains(reply, "已设置当前会话标题：当前话题") {
		t.Fatalf("reply = %q, want topic title update", reply)
	}

	var card feishu.SessionSelectionCard
	client := newFakeSentMessageClient("")
	client.sessionSelectionSender = func(ctx context.Context, msg feishu.Message, sent feishu.SessionSelectionCard) error {
		card = sent
		return nil
	}
	svc.setOutbound(base.BotID, client)
	ctx := context.Background()
	listMsg := base
	listMsg.MessageID = "om_list"
	listMsg.Text = "@智能助手 /session list"
	reply, err = handleFeishuMessage(t, svc, ctx, listMsg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(topic /session list) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want topic session card only", reply)
	}
	if card.ThreadID != base.ThreadID || card.GroupMessageType != "thread" ||
		card.CurrentACPSessionID != "acp-session-2" || len(card.Options) != 2 {
		t.Fatalf("card = %+v, want complete topic callback context and two sessions", card)
	}

	resumeMsg := base
	resumeMsg.MessageID = "om_resume"
	resumeMsg.Text = "@智能助手 /session resume 2"
	reply, err = handleFeishuMessage(t, svc, context.Background(), resumeMsg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(topic /session resume) error = %v", err)
	}
	if !strings.Contains(reply, "已恢复会话 2") || !strings.Contains(reply, "session：acp-session-1") {
		t.Fatalf("reply = %q, want first topic session restored", reply)
	}
	current, ok := store.Get(topicKey)
	if !ok || current.ACPSessionID != "acp-session-1" {
		t.Fatalf("current topic session = %+v, %v; want first session", current, ok)
	}
}

func TestHandleFeishuSessionTitleCommands(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	rt := &fakeRuntime{newSessionIDs: []string{"acp-session-1", "acp-session-2"}}
	svc := newTestService(config.Default(), store)
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
	session, ok := store.Get(imSessionKey("bot-a", "oc_private", ""))
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
		Text:      "/session TITLE 新标题",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/session TITLE) error = %v", err)
	}
	if !strings.Contains(reply, "已设置当前会话标题：新标题") {
		t.Fatalf("reply = %q, want title confirmation", reply)
	}
	session, ok = store.Get(imSessionKey("bot-a", "oc_private", ""))
	if !ok || session.Title != "新标题" || !session.ManualTitle {
		t.Fatalf("session = %+v, %v; want manual title 新标题", session, ok)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		MessageID: "om_title_spaced",
		Text:      "/session   title   多空格标题",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/session title spaced) error = %v", err)
	}
	if !strings.Contains(reply, "已设置当前会话标题：多空格标题") {
		t.Fatalf("reply = %q, want spaced title confirmation", reply)
	}
	session, ok = store.Get(imSessionKey("bot-a", "oc_private", ""))
	if !ok || session.Title != "多空格标题" || !session.ManualTitle {
		t.Fatalf("session = %+v, %v; want manual title 多空格标题", session, ok)
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
		MessageID: "om_slash_title",
		Text:      "/new bug/fix",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new slash title) error = %v", err)
	}
	if !strings.Contains(reply, "标题：bug/fix") || !strings.Contains(reply, "cwd 来源：当前会话已有会话") {
		t.Fatalf("reply = %q, want slash title to reuse cwd instead of being parsed as cwd", reply)
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
	if !strings.Contains(reply, "标题：bug/fix") || !strings.Contains(reply, "标题：多空格标题") {
		t.Fatalf("reply = %q, want titles in list", reply)
	}
}

func TestHandleFeishuNewSessionUsesDefaultTitleAndDisplaysModeModel(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	rt := &fakeRuntime{newSessionIDs: []string{"acp-session-1", "acp-session-2"}}
	svc := newTestService(config.Default(), store)
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
		MessageID: "om_new_1",
		Text:      "/new " + workDir,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new first) error = %v", err)
	}
	for _, want := range []string{"标题：session#1", "mode：未知", "model：gpt-5.5"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		MessageID: "om_new_2",
		Text:      "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new second) error = %v", err)
	}
	if !strings.Contains(reply, "标题：session#2") || !strings.Contains(reply, "cwd 来源：当前会话已有会话") {
		t.Fatalf("reply = %q, want second default title and reused cwd", reply)
	}
}

func TestHandleFeishuNewSessionInheritsModeAndModelForSameAgent(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	previous := Session{
		Key:          imSessionKey("bot-a", "oc_private", ""),
		Title:        "previous",
		AgentName:    "traex",
		ACPSessionID: "acp-session-old",
		Cwd:          workDir,
		ConfigOptions: []acp.SessionConfigOption{
			{
				ID:           "mode",
				Name:         "Mode",
				Category:     "mode",
				Type:         "select",
				CurrentValue: "plan",
				Options: []acp.SessionConfigOptionValue{
					{Value: "default", Name: "Default"},
					{Value: "plan", Name: "Plan"},
				},
			},
			{
				ID:           "model",
				Name:         "Model",
				Category:     "model",
				Type:         "select",
				CurrentValue: "gpt-5.6",
				Options: []acp.SessionConfigOptionValue{
					{Value: "gpt-5.5", Name: "GPT-5.5"},
					{Value: "gpt-5.6", Name: "GPT-5.6"},
				},
			},
		},
	}
	if err := store.Upsert(previous); err != nil {
		t.Fatalf("Upsert(previous) error = %v", err)
	}
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{
			SessionID: "acp-session-new",
			ConfigOptions: []acp.SessionConfigOption{
				{
					ID:           "mode",
					Name:         "Mode",
					Category:     "mode",
					Type:         "select",
					CurrentValue: "default",
					Options: []acp.SessionConfigOptionValue{
						{Value: "default", Name: "Default"},
						{Value: "plan", Name: "Plan"},
					},
				},
				{
					ID:           "model",
					Name:         "Model",
					Category:     "model",
					Type:         "select",
					CurrentValue: "gpt-5.5",
					Options: []acp.SessionConfigOptionValue{
						{Value: "gpt-5.5", Name: "GPT-5.5"},
						{Value: "gpt-5.6", Name: "GPT-5.6"},
					},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     previous.Key.BotID,
		ChatID:    sessionKeyMainID(previous.Key),
		ChatType:  "p2p",
		MessageID: "om_new",
		Text:      "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	for _, want := range []string{"标题：session#2", "mode：plan", "model：gpt-5.6"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
	if len(rt.configCalls) != 2 {
		t.Fatalf("configCalls = %+v, want mode and model inheritance", rt.configCalls)
	}
	if rt.configCalls[0].ConfigID != "mode" || rt.configCalls[0].Value != "plan" {
		t.Fatalf("first config call = %+v, want mode plan", rt.configCalls[0])
	}
	if rt.configCalls[1].ConfigID != "model" || rt.configCalls[1].Value != "gpt-5.6" {
		t.Fatalf("second config call = %+v, want model gpt-5.6", rt.configCalls[1])
	}
	session, ok := store.Get(previous.Key)
	if !ok {
		t.Fatalf("new session not found")
	}
	if session.ACPSessionID != "acp-session-new" {
		t.Fatalf("session id = %q, want acp-session-new", session.ACPSessionID)
	}
	if currentModeDisplay(session) != "plan" || currentModelDisplay(session) != "gpt-5.6" {
		t.Fatalf("session config = %+v, want inherited mode/model", session.ConfigOptions)
	}
}

func TestHandleFeishuNewSessionWaitsForConfigOptionsBeforeInheritance(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	previous := Session{
		Key:          imSessionKey("bot-a", "oc_private", ""),
		Title:        "previous",
		AgentName:    "traex",
		ACPSessionID: "acp-session-old",
		Cwd:          workDir,
		ConfigOptions: []acp.SessionConfigOption{
			{ID: "mode", Category: "mode", Type: "select", CurrentValue: "plan"},
			{ID: "model", Category: "model", Type: "select", CurrentValue: "gpt-5.6"},
		},
	}
	if err := store.Upsert(previous); err != nil {
		t.Fatalf("Upsert(previous) error = %v", err)
	}
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-session-new"},
		noDefaultState: true,
	}
	rt.afterNewSession = func(key SessionKey, sessionID string) {
		go func() {
			time.Sleep(10 * time.Millisecond)
			rt.dispatchUpdate(key, sessionID, acp.SessionUpdate{
				SessionUpdate: "config_option_update",
				ConfigOptions: []acp.SessionConfigOption{
					{
						ID:           "mode",
						Category:     "mode",
						Type:         "select",
						CurrentValue: "default",
						Options: []acp.SessionConfigOptionValue{
							{Value: "default", Name: "Default"},
							{Value: "plan", Name: "Plan"},
						},
					},
					{
						ID:           "model",
						Category:     "model",
						Type:         "select",
						CurrentValue: "gpt-5.5",
						Options: []acp.SessionConfigOptionValue{
							{Value: "gpt-5.5", Name: "GPT-5.5"},
							{Value: "gpt-5.6", Name: "GPT-5.6"},
						},
					},
				},
			})
		}()
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     previous.Key.BotID,
		ChatID:    sessionKeyMainID(previous.Key),
		ChatType:  "p2p",
		MessageID: "om_new",
		Text:      "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	for _, want := range []string{"mode：plan", "model：gpt-5.6"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
	if len(rt.configCalls) != 2 ||
		rt.configCalls[0].ConfigID != "mode" || rt.configCalls[0].Value != "plan" ||
		rt.configCalls[1].ConfigID != "model" || rt.configCalls[1].Value != "gpt-5.6" {
		t.Fatalf("configCalls = %+v, want inheritance after async options", rt.configCalls)
	}
}

func TestHandleFeishuNewSessionDoesNotInheritModeAndModelForDifferentAgent(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	previous := Session{
		Key:          imSessionKey("bot-a", "oc_private", ""),
		Title:        "previous",
		AgentName:    "traex",
		ACPSessionID: "acp-session-old",
		Cwd:          workDir,
		ConfigOptions: []acp.SessionConfigOption{
			{ID: "mode", Category: "mode", Type: "select", CurrentValue: "plan"},
			{ID: "model", Category: "model", Type: "select", CurrentValue: "gpt-5.6"},
		},
	}
	if err := store.Upsert(previous); err != nil {
		t.Fatalf("Upsert(previous) error = %v", err)
	}
	if err := store.UpsertChat(ChatConfig{
		Key:       ChatKey{BotID: previous.Key.BotID, ChatID: sessionKeyMainID(previous.Key)},
		AgentName: "hermes",
	}); err != nil {
		t.Fatalf("UpsertChat() error = %v", err)
	}
	cfg := config.Default()
	cfg.AgentList = append(cfg.AgentList, config.NamedAgentConfig{
		Name: "hermes",
		AgentConfig: config.AgentConfig{
			Command:    "hermes",
			DefaultCwd: workDir,
		},
	})
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{
			SessionID: "acp-session-new",
			ConfigOptions: []acp.SessionConfigOption{
				{ID: "mode", Category: "mode", Type: "select", CurrentValue: "default"},
				{ID: "model", Category: "model", Type: "select", CurrentValue: "gpt-5.5"},
			},
		},
	}
	svc := newTestService(cfg, store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     previous.Key.BotID,
		ChatID:    sessionKeyMainID(previous.Key),
		ChatType:  "p2p",
		MessageID: "om_new",
		Text:      "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	for _, want := range []string{"mode：default", "model：gpt-5.5"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
	if len(rt.configCalls) != 0 {
		t.Fatalf("configCalls = %+v, want no inherited config for different agent", rt.configCalls)
	}
	session, ok := store.Get(previous.Key)
	if !ok {
		t.Fatalf("new session not found")
	}
	if session.AgentName != "hermes" {
		t.Fatalf("session agent = %q, want hermes", session.AgentName)
	}
	if currentModeDisplay(session) != "default" || currentModelDisplay(session) != "gpt-5.5" {
		t.Fatalf("session config = %+v, want new agent defaults", session.ConfigOptions)
	}
}

func TestHandleFeishuNewTopicInheritsChatDefaultModeAndModel(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	cfg.AgentList = append(cfg.AgentList, config.NamedAgentConfig{
		Name: "hermes",
		AgentConfig: config.AgentConfig{
			Command:    "hermes",
			DefaultCwd: workDir,
		},
	})
	rt := &fakeRuntime{
		promptReply: "ACP 回复",
		newSessionInfo: acp.SessionInfo{
			ConfigOptions: []acp.SessionConfigOption{
				{
					ID:           "mode",
					Name:         "Mode",
					Category:     "mode",
					Type:         "select",
					CurrentValue: "default",
					Options: []acp.SessionConfigOptionValue{
						{Value: "default", Name: "Default"},
						{Value: "plan", Name: "Plan"},
					},
				},
				{
					ID:           "model",
					Name:         "Model",
					Category:     "model",
					Type:         "select",
					CurrentValue: "gpt-5.5",
					Options: []acp.SessionConfigOptionValue{
						{Value: "gpt-5.5", Name: "GPT-5.5"},
						{Value: "gpt-5.6", Name: "GPT-5.6"},
					},
				},
			},
		},
	}
	rt.newSessionIDs = []string{"acp-session-topic-1", "acp-session-topic-2", "acp-session-topic-3"}
	svc := newTestService(cfg, store)
	svc.setRuntime(rt)
	first := feishu.Message{
		BotID:            "bot-a",
		MessageID:        "om_topic_1",
		ChatID:           "oc_group",
		ChatType:         "group",
		GroupMessageType: "thread",
		ThreadID:         "omt_topic_1",
		Text:             "@智能助手 话题一",
		Mentions:         []feishu.Mention{testBotMention("智能助手")},
		SenderID:         testOwnerOpenID,
	}
	if _, err := handleFeishuMessage(t, svc, context.Background(), first); err != nil {
		t.Fatalf("HandleFeishuMessage(first topic) error = %v", err)
	}
	firstSession, ok := store.Get(imSessionKey(first.BotID, first.ChatID, first.ThreadID))
	if !ok {
		t.Fatalf("first topic session not found")
	}
	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:            first.BotID,
		MessageID:        "om_topic_1_mode",
		ChatID:           first.ChatID,
		ChatType:         first.ChatType,
		GroupMessageType: first.GroupMessageType,
		ThreadID:         first.ThreadID,
		Text:             "@智能助手 /config mode plan",
		Mentions:         first.Mentions,
		SenderID:         first.SenderID,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/config mode plan) error = %v", err)
	}
	if reply != "已设置配置项 mode：Plan（plan）" {
		t.Fatalf("reply = %q, want mode config confirmation", reply)
	}
	firstSession, ok = store.Get(imSessionKey(first.BotID, first.ChatID, first.ThreadID))
	if !ok {
		t.Fatalf("first topic session not found after setting mode")
	}
	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:            first.BotID,
		MessageID:        "om_topic_1_model",
		ChatID:           first.ChatID,
		ChatType:         first.ChatType,
		GroupMessageType: first.GroupMessageType,
		ThreadID:         first.ThreadID,
		Text:             "@智能助手 /config model gpt-5.6",
		Mentions:         first.Mentions,
		SenderID:         first.SenderID,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/config model gpt-5.6) error = %v", err)
	}
	if reply != "已设置配置项 model：GPT-5.6（gpt-5.6）" {
		t.Fatalf("reply = %q, want model config confirmation", reply)
	}
	chat, ok := store.GetChat(ChatKey{BotID: first.BotID, ChatID: first.ChatID})
	if !ok {
		t.Fatalf("chat config not found after /config model/mode")
	}
	if agentConfig := chat.AgentConfigs[firstSession.AgentName]; agentConfig.Mode != "plan" || agentConfig.Model != "gpt-5.6" {
		t.Fatalf("chat agent config = %+v, want /config persisted mode/model", agentConfig)
	}

	second := first
	second.MessageID = "om_topic_2"
	second.ThreadID = "omt_topic_2"
	second.Text = "@智能助手 话题二"
	reply, err = handleFeishuMessage(t, svc, context.Background(), second)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(second topic) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.configCalls) != 4 {
		t.Fatalf("configCalls = %+v, want first topic manual set and second topic inherited set", rt.configCalls)
	}
	if rt.configCalls[2].Session.ACPSessionID != "acp-session-topic-2" || rt.configCalls[2].ConfigID != "mode" || rt.configCalls[2].Value != "plan" {
		t.Fatalf("third config call = %+v, want second topic mode inheritance", rt.configCalls[2])
	}
	if rt.configCalls[3].Session.ACPSessionID != "acp-session-topic-2" || rt.configCalls[3].ConfigID != "model" || rt.configCalls[3].Value != "gpt-5.6" {
		t.Fatalf("fourth config call = %+v, want second topic model inheritance", rt.configCalls[3])
	}
	session, ok := store.Get(imSessionKey(second.BotID, second.ChatID, second.ThreadID))
	if !ok {
		t.Fatalf("second topic session not found")
	}
	if currentModeDisplay(session) != "plan" || currentModelDisplay(session) != "gpt-5.6" {
		t.Fatalf("second topic session config = %+v, want chat defaults", session.ConfigOptions)
	}

	if err := store.UpsertChat(ChatConfig{
		Key:       ChatKey{BotID: first.BotID, ChatID: first.ChatID},
		AgentName: "hermes",
		AgentConfigs: map[string]ChatAgentConfig{
			"traex": {Mode: "plan", Model: "gpt-5.6"},
		},
	}); err != nil {
		t.Fatalf("UpsertChat(hermes) error = %v", err)
	}
	third := first
	third.MessageID = "om_topic_3"
	third.ThreadID = "omt_topic_3"
	third.Text = "@智能助手 话题三"
	if _, err := handleFeishuMessage(t, svc, context.Background(), third); err != nil {
		t.Fatalf("HandleFeishuMessage(third topic) error = %v", err)
	}
	if len(rt.configCalls) != 4 {
		t.Fatalf("configCalls = %+v, want no traex defaults applied to hermes", rt.configCalls)
	}
	session, ok = store.Get(imSessionKey(third.BotID, third.ChatID, third.ThreadID))
	if !ok {
		t.Fatalf("third topic session not found")
	}
	if session.AgentName != "hermes" {
		t.Fatalf("third topic agent = %q, want hermes", session.AgentName)
	}
	if currentModeDisplay(session) != "default" || currentModelDisplay(session) != "gpt-5.5" {
		t.Fatalf("third topic session config = %+v, want hermes defaults without traex inheritance", session.ConfigOptions)
	}
}

func TestHandleFeishuMessageStatusShowsPersistedSessionWithoutReadyState(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	rt := &fakeRuntime{}
	svc.setRuntime(rt)
	workspace := filepath.Join(t.TempDir(), "bot-a")
	markWorkspaceBootstrapped(t, workspace)
	key := imSessionKey("bot-a", "oc_chat", "omt_thread")
	if err := store.Upsert(Session{
		Key:          key,
		Title:        "test session",
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
	}); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		Workspace: workspace,
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Text:      "/status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/status) error = %v", err)
	}
	if strings.Contains(reply, "状态：") {
		t.Fatalf("reply = %q, should not show setup/ready state", reply)
	}
	session, ok := store.Get(key)
	if !ok {
		t.Fatalf("session not found")
	}
	if session.ACPSessionID != "acp-session-1" {
		t.Fatalf("persisted session id = %q, want unchanged", session.ACPSessionID)
	}
	if len(rt.closedKeys) != 0 {
		t.Fatalf("closedKeys = %+v, want no runtime close", rt.closedKeys)
	}
}

func TestHandleFeishuMessageStatusShowsPersistedSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	svc.setRuntime(&fakeRuntime{newSessionID: "acp-session-1"})
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
		MessageID:        "om_status",
		ChatID:           "oc_chat",
		ChatType:         "group",
		GroupMessageType: "thread",
		ThreadID:         "omt_thread",
		Text:             "@智能助手 /status",
		Mentions:         testBotMentions(),
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
	svc := newTestService(config.Default(), store)
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

func TestHandleSIDCommandRoutesPromptToSpecifiedSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptReply: "已停止"}
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = t.TempDir()
	cfg.Bots[0].OwnerOpenIDs = []string{testOwnerOpenID}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	workDir := t.TempDir()
	sourceKey := normalizeSessionKey(imSessionKey("bot-a", "oc_source", "omt_source"))
	sourceSession := Session{
		Key:          sourceKey,
		AgentName:    "traex",
		ACPSessionID: "acp-source",
		Cwd:          workDir,
		Workspace:    workDir,
	}
	if err := store.Upsert(sourceSession); err != nil {
		t.Fatalf("Upsert(source session) error = %v", err)
	}
	otherCurrent := sourceSession
	otherCurrent.ACPSessionID = "acp-other"
	if err := store.Upsert(otherCurrent); err != nil {
		t.Fatalf("Upsert(other current session) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_sid",
		ChatID:    "oc_trace",
		ChatType:  "group",
		SenderID:  testOwnerOpenID,
		Text:      "@智能助手 /sid acp-source 停止执行 你跑偏啦",
		Mentions:  testBotMentions(),
		Workspace: workDir,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/sid) error = %v", err)
	}
	if reply != "已停止" {
		t.Fatalf("reply = %q, want sid prompt reply", reply)
	}
	calls := rt.promptCallsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("promptCalls = %+v, want one prompt", calls)
	}
	if calls[0].Session.Key != sourceKey || calls[0].Session.ACPSessionID != "acp-source" {
		t.Fatalf("prompt session = %+v, want source session", calls[0].Session)
	}
	if strings.Contains(calls[0].Text, "/sid") || strings.Contains(calls[0].Text, "acp-source") || !strings.Contains(calls[0].Text, "停止执行 你跑偏啦") {
		t.Fatalf("prompt text = %q, want stripped user text", calls[0].Text)
	}
	restored, ok := store.Get(sourceKey)
	if !ok || restored.ACPSessionID != "acp-source" {
		t.Fatalf("current source session = %+v ok=%v, want restored acp-source", restored, ok)
	}
}

func TestHandleFeishuMessageStatusShowsSanitizedACPError(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptErrors: []error{errors.New("request failed Authorization: Bearer real-token token abc123 api_key xyz789 app_secret=super-secret prompt=用户隐私正文")},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	_, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "用户隐私正文",
	})
	if err == nil {
		t.Fatal("HandleFeishuMessage(prompt) error = nil, want prompt failure")
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/status) error = %v", err)
	}
	for _, want := range []string{
		"ACP错误：",
		"prompt：request failed",
		"Authorization:[已隐藏] [已隐藏] [已隐藏]",
		"token [已隐藏]",
		"api_key [已隐藏]",
		"app_secret=[已隐藏]",
		"prompt=[已隐藏]",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
	for _, forbidden := range []string{
		"real-token",
		"abc123",
		"xyz789",
		"super-secret",
		"用户隐私正文",
	} {
		if strings.Contains(reply, forbidden) {
			t.Fatalf("reply = %q, should not include sensitive diagnostic text %q", reply, forbidden)
		}
	}
}

func TestHandleFeishuMessageStatusIgnoresStaleACPErrorAfterNewSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-2",
		promptErrors: []error{
			errors.New("request failed token old-token"),
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	oldDir := t.TempDir()
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          oldDir,
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	_, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "触发旧会话错误",
	})
	if err == nil {
		t.Fatal("HandleFeishuMessage(prompt) error = nil, want prompt failure")
	}
	newDir := t.TempDir()
	if reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/new " + newDir,
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	} else if !strings.Contains(reply, "session：acp-session-2") {
		t.Fatalf("reply = %q, want new acp session", reply)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/status) error = %v", err)
	}
	for _, want := range []string{
		"session：acp-session-2",
		"ACP错误：无",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
	if strings.Contains(reply, "old-token") || strings.Contains(reply, "request failed") {
		t.Fatalf("reply = %q, should not include stale acp error", reply)
	}
}
