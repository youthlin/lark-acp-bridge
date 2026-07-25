package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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

const testBotOpenID = "ou_bot"

func testBotMention(name string) feishu.Mention {
	return feishu.Mention{ID: testBotOpenID, Name: name, Type: "bot"}
}

func handleFeishuMessage(t *testing.T, svc *Service, ctx context.Context, msg feishu.Message) (string, error) {
	t.Helper()
	if strings.TrimSpace(msg.BotOpenID) == "" {
		msg.BotOpenID = testBotOpenID
	}
	if strings.TrimSpace(msg.Workspace) == "" {
		msg.Workspace = filepath.Join(t.TempDir(), "workspace")
		if err := os.MkdirAll(msg.Workspace, 0o755); err != nil {
			t.Fatalf("MkdirAll(workspace) error = %v", err)
		}
		botID := msg.BotID
		if strings.TrimSpace(botID) == "" {
			botID = "bot-a"
		}
		for _, file := range workspaceFiles(botID) {
			path := filepath.Join(msg.Workspace, file.name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(file.name), err)
			}
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatalf("WriteFile(%s) error = %v", file.name, err)
			}
		}
		markWorkspaceBootstrapped(t, msg.Workspace)
	}
	return svc.HandleFeishuMessage(ctx, msg)
}

func markWorkspaceBootstrapped(t *testing.T, workspace string) {
	t.Helper()
	if _, err := ensureWorkspace(workspace, "bot-a"); err != nil {
		t.Fatalf("ensureWorkspace(%s) error = %v", workspace, err)
	}
	if err := os.Remove(filepath.Join(workspace, workspaceBootstrapFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove(%s) error = %v", workspaceBootstrapFile, err)
	}
}

func slogAttrsMap(attrs []slog.Attr) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		out[attr.Key] = attr.Value.String()
	}
	return out
}

func testReadySession(t *testing.T, store *SessionStore) Session {
	t.Helper()
	workspace := filepath.Join(t.TempDir(), "workspace")
	markWorkspaceBootstrapped(t, workspace)
	session := Session{
		Key:          SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"},
		Title:        "test session",
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
	}
	if store != nil {
		if err := store.Upsert(session); err != nil {
			t.Fatalf("Upsert(session) error = %v", err)
		}
	}
	return session
}

func TestEnsureWorkspaceCreatesBootstrapOnlyForNewWorkspace(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	status, err := ensureWorkspace(workspace, "bot-a")
	if err != nil {
		t.Fatalf("ensureWorkspace(new) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, workspaceBootstrapFile)); err != nil {
		t.Fatalf("Bootstrap.md should exist after new workspace: %v", err)
	}

	markWorkspaceBootstrapped(t, workspace)
	status, err = ensureWorkspace(workspace, "bot-a")
	if err != nil {
		t.Fatalf("ensureWorkspace(existing) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, workspaceBootstrapFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Bootstrap.md should stay deleted after bootstrapped workspace, err=%v", err)
	}
	if len(status.CreatedFiles) != 0 {
		t.Fatalf("created files = %+v, want none for existing workspace", status.CreatedFiles)
	}
}

func TestEnsureWorkspaceRejectsEmptyPath(t *testing.T) {
	if _, err := ensureWorkspace("", "bot-a"); err == nil || !strings.Contains(err.Error(), "workspace 为空") {
		t.Fatalf("ensureWorkspace(empty) error = %v, want workspace empty error", err)
	}
}

func TestEnsureWorkspaceDefaultToolsIncludesLarkCLIProfileGuidance(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if _, err := ensureWorkspace(workspace, "default"); err != nil {
		t.Fatalf("ensureWorkspace() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "TOOLS.md"))
	if err != nil {
		t.Fatalf("ReadFile(TOOLS.md) error = %v", err)
	}
	tools := string(data)
	for _, want := range []string{
		"飞书 CLI",
		"lark-cli",
		"https://open.feishu.cn/document/no_class/mcp-archive/feishu-cli-installation-guide.md",
		"lark-acp-default",
		"config.json",
		"app_id",
		"app_secret",
		"stdin",
		"不要写入提示词、回复、日志或命令行参数",
	} {
		if !strings.Contains(tools, want) {
			t.Fatalf("TOOLS.md = %q, want %q", tools, want)
		}
	}
}

func TestWorkspaceContextPromptIgnoresEmptyWorkspace(t *testing.T) {
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, workspaceBootstrapFile), []byte("# Should Not Leak\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(Bootstrap.md) error = %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir(tmp) error = %v", err)
	}
	defer func() {
		if err := os.Chdir(oldWd); err != nil {
			t.Fatalf("Chdir(oldWd) error = %v", err)
		}
	}()

	if got := workspaceContextPrompt(""); got != "" {
		t.Fatalf("workspaceContextPrompt(empty) = %q, want empty", got)
	}
	if got := promptTextWithWorkspaceContext("", feishu.Message{}, "hello"); got != "hello" {
		t.Fatalf("promptTextWithWorkspaceContext(empty) = %q, want user text only", got)
	}
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

func TestApplyACPStateUpdateAllowsEmptyLists(t *testing.T) {
	session := Session{
		AvailableCommands: []acp.AvailableCommand{{Name: "review"}},
		ConfigOptions: []acp.SessionConfigOption{
			{ID: "model", Name: "Model", Type: "select", CurrentValue: "gpt-5.5"},
		},
	}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{SessionUpdate: "available_commands_update"}) {
		t.Fatal("available_commands_update should be treated as changed even when empty")
	}
	if len(session.AvailableCommands) != 0 {
		t.Fatalf("AvailableCommands = %+v, want cleared", session.AvailableCommands)
	}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{SessionUpdate: "config_option_update"}) {
		t.Fatal("config_option_update should be treated as changed even when empty")
	}
	if len(session.ConfigOptions) != 0 {
		t.Fatalf("ConfigOptions = %+v, want cleared", session.ConfigOptions)
	}
}

func TestApplyACPStateUpdatePersistsModeAndModelState(t *testing.T) {
	session := Session{}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_state_update",
		Models: &acp.SessionModelState{
			CurrentModelID: "gpt-5.5",
			AvailableModels: []acp.SessionModel{
				{ModelID: "gpt-5.5", Name: "GPT-5.5"},
			},
		},
		Mode: &acp.SessionModeState{
			CurrentModeID: "agent",
			AvailableModes: []acp.SessionMode{
				{ModeID: "agent", Name: "Agent"},
			},
		},
	}) {
		t.Fatal("session_state_update should update mode/model state")
	}
	if got := currentModelDisplay(session); got != "gpt-5.5" {
		t.Fatalf("currentModelDisplay = %q, want gpt-5.5", got)
	}
	if got := currentModeDisplay(session); got != "agent" {
		t.Fatalf("currentModeDisplay = %q, want agent", got)
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
	svc := NewService(config.Default(), store)
	svc.setRuntime(&fakeRuntime{})

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   session.Key.ChatID,
		ThreadID: session.Key.ThreadID,
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
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   session.Key.ChatID,
		ThreadID: session.Key.ThreadID,
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
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)

	_, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   session.Key.ChatID,
		ThreadID: session.Key.ThreadID,
		Text:     "//review 快速检查",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(//review) error = %v", err)
	}
	if len(rt.promptCalls) != 1 || rt.promptCalls[0].Text != "/review 快速检查" {
		t.Fatalf("promptCalls = %+v, want double slash forwarded as single slash", rt.promptCalls)
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
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   session.Key.ChatID,
		ThreadID: session.Key.ThreadID,
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
		ChatID:   session.Key.ChatID,
		ThreadID: session.Key.ThreadID,
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
	svc := NewService(config.Default(), store)
	var got feishu.ModelSelectionCard
	ctx := feishu.WithModelSelectionCardSender(context.Background(), func(ctx context.Context, msg feishu.Message, card feishu.ModelSelectionCard) error {
		got = card
		return nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   session.Key.ChatID,
		ThreadID: session.Key.ThreadID,
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
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   session.Key.ChatID,
		ThreadID: session.Key.ThreadID,
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
		ChatID:   session.Key.ChatID,
		ThreadID: session.Key.ThreadID,
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
	svc := NewService(config.Default(), store)
	var got feishu.ModeSelectionCard
	ctx := feishu.WithModeSelectionCardSender(context.Background(), func(ctx context.Context, msg feishu.Message, card feishu.ModeSelectionCard) error {
		got = card
		return nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   session.Key.ChatID,
		ThreadID: session.Key.ThreadID,
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

func TestModelSelectionOptionsFallsBackToModels(t *testing.T) {
	session := Session{
		Models: &acp.SessionModelState{
			CurrentModelID: "gpt-5.5",
			AvailableModels: []acp.SessionModel{
				{ModelID: "gpt-5.5", Name: "GPT-5.5"},
				{ModelID: "gpt-5.6", Name: "GPT-5.6"},
			},
		},
	}
	options := modelSelectionOptions(session, acp.SessionConfigOption{ID: "model", Category: "model"})
	if len(options) != 2 || options[1].Value != "gpt-5.6" || options[1].Name != "GPT-5.6" {
		t.Fatalf("options = %+v, want models fallback", options)
	}
}

func TestModeSelectionOptionsFallsBackToModeState(t *testing.T) {
	session := Session{
		Mode: &acp.SessionModeState{
			CurrentModeID: "default",
			AvailableModes: []acp.SessionMode{
				{ModeID: "default", Name: "Default"},
				{ModeID: "plan", Name: "Plan"},
			},
		},
	}
	options := modeSelectionOptions(session, acp.SessionConfigOption{ID: "mode", Category: "mode"})
	if len(options) != 2 || options[1].Value != "plan" || options[1].Name != "Plan" {
		t.Fatalf("options = %+v, want modes fallback", options)
	}
}

func TestHandleModelSelectionSetsModelAndRejectsStaleOrOtherUser(t *testing.T) {
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
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)
	selection := feishu.ModelSelection{
		BotID:        session.Key.BotID,
		ChatID:       session.Key.ChatID,
		ThreadID:     session.Key.ThreadID,
		ACPSessionID: session.ACPSessionID,
		RequesterID:  "ou_requester",
		OperatorID:   "ou_requester",
		Model:        "gpt-5.6",
	}

	display, err := svc.HandleModelSelection(context.Background(), selection)
	if err != nil {
		t.Fatalf("HandleModelSelection() error = %v", err)
	}
	if display != "GPT-5.6（gpt-5.6）" {
		t.Fatalf("display = %q, want friendly model display", display)
	}
	if len(rt.configCalls) != 1 || rt.configCalls[0].Value != "gpt-5.6" {
		t.Fatalf("configCalls = %+v, want gpt-5.6", rt.configCalls)
	}

	selection.OperatorID = "ou_other"
	if _, err := svc.HandleModelSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "只有发起") {
		t.Fatalf("other user error = %v, want requester validation", err)
	}

	selection.OperatorID = selection.RequesterID
	selection.ACPSessionID = "stale-session"
	if _, err := svc.HandleModelSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("stale card error = %v, want expired validation", err)
	}
}

func TestHandleModeSelectionSetsModeAndRejectsStaleOrOtherUser(t *testing.T) {
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
				},
			},
		},
	}
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)
	selection := feishu.ModeSelection{
		BotID:        session.Key.BotID,
		ChatID:       session.Key.ChatID,
		ThreadID:     session.Key.ThreadID,
		ACPSessionID: session.ACPSessionID,
		RequesterID:  "ou_requester",
		OperatorID:   "ou_requester",
		Mode:         "plan",
	}

	display, err := svc.HandleModeSelection(context.Background(), selection)
	if err != nil {
		t.Fatalf("HandleModeSelection() error = %v", err)
	}
	if display != "Plan（plan）" {
		t.Fatalf("display = %q, want friendly mode display", display)
	}
	if len(rt.configCalls) != 1 || rt.configCalls[0].Value != "plan" {
		t.Fatalf("configCalls = %+v, want plan", rt.configCalls)
	}

	selection.OperatorID = "ou_other"
	if _, err := svc.HandleModeSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "只有发起") {
		t.Fatalf("other user error = %v, want requester validation", err)
	}

	selection.OperatorID = selection.RequesterID
	selection.ACPSessionID = "stale-session"
	if _, err := svc.HandleModeSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("stale card error = %v, want expired validation", err)
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
	if _, ok := store.Get(SessionKey{ChatID: "oc_chat", ThreadID: "omt_thread"}); !ok {
		t.Fatalf("auto-created session not persisted")
	}
}

func TestHandleFeishuMessageRefreshesUnavailablePersistedACPSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	key := SessionKey{ChatID: "oc_chat", ThreadID: "omt_thread"}
	if err := store.Upsert(Session{
		Key:          key,
		Title:        "old session",
		AgentName:    "traex",
		ACPSessionID: "old-acp-session",
		Cwd:          workDir,
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	rt := &fakeRuntime{
		newSessionID: "new-acp-session",
		promptReply:  "ACP 回复",
		promptErrors: []error{errACPSessionUnavailable},
	}
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "继续",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.newCalls) != 1 || rt.newCalls[0].Cwd != workDir {
		t.Fatalf("newCalls = %+v, want refresh with cwd %q", rt.newCalls, workDir)
	}
	if len(rt.promptCalls) != 2 {
		t.Fatalf("promptCalls = %+v, want retry after refresh", rt.promptCalls)
	}
	if rt.promptCalls[0].Session.ACPSessionID != "old-acp-session" {
		t.Fatalf("first prompt session = %q, want old session", rt.promptCalls[0].Session.ACPSessionID)
	}
	if rt.promptCalls[1].Session.ACPSessionID != "new-acp-session" {
		t.Fatalf("second prompt session = %q, want refreshed session", rt.promptCalls[1].Session.ACPSessionID)
	}
	updated, ok := store.Get(key)
	if !ok {
		t.Fatalf("session not found after refresh")
	}
	if updated.ACPSessionID != "new-acp-session" {
		t.Fatalf("persisted session = %q, want new-acp-session", updated.ACPSessionID)
	}
}

func TestHandleFeishuMessageIncludesReplyContextInPrompt(t *testing.T) {
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
	for _, want := range []string{"## Replied Message Context", "我先发一条消息", "请结合上面的被回复消息理解下面的用户消息。", "这种情况怎么实现"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
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
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = workDir
	cfg.Agents["traex"] = agent
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
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = workDir
	cfg.Agents["traex"] = agent
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
	svc := NewService(config.Default(), store)
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

func TestHandleFeishuGroupChatReusesChatSessionWithoutTopic(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = workDir
	cfg.Agents["traex"] = agent
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
	if rt.newCalls[0].Key.ThreadID != "" || rt.promptCalls[1].Session.Key.ThreadID != "" {
		t.Fatalf("session keys = new %+v prompt %+v, want chat-level group session", rt.newCalls[0].Key, rt.promptCalls[1].Session.Key)
	}
	if _, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_group"}); !ok {
		t.Fatalf("ordinary group chat session not persisted by chat id")
	}
}

func TestHandleFeishuGroupChatRequiresMentionByDefault(t *testing.T) {
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

func TestMessageMentionsBotRequiresCurrentBotOpenID(t *testing.T) {
	msg := feishu.Message{
		BotOpenID: testBotOpenID,
		Mentions: []feishu.Mention{
			{ID: "ou_other_user", Name: "其他用户", Type: "user"},
			{ID: "ou_other_bot", Name: "其他助手", Type: "bot"},
		},
	}
	if messageMentionsBot(msg) {
		t.Fatalf("messageMentionsBot(%+v) = true, want false for mentions that do not target current bot", msg.Mentions)
	}

	msg.Mentions = append(msg.Mentions, testBotMention("智能助手"))
	if !messageMentionsBot(msg) {
		t.Fatalf("messageMentionsBot(%+v) = false, want true for current bot open_id", msg.Mentions)
	}

	msg.BotOpenID = ""
	if messageMentionsBot(msg) {
		t.Fatalf("messageMentionsBot without BotOpenID = true, want false")
	}
}

func TestHandleFeishuGroupChatAtCommandConfiguresMentionRequirement(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = workDir
	cfg.Agents["traex"] = agent
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	msg := feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_group",
		ChatType: "group",
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
	if _, ok := store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_group"}); ok {
		t.Fatalf("chat config should not be created by ignored /at off")
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
	if _, ok := store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_group"}); ok {
		t.Fatalf("chat config should not be created by /at off mentioning another bot")
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_off",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "@智能助手 /at off",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(@bot /at off) error = %v", err)
	}
	if !strings.Contains(reply, "无需 at") {
		t.Fatalf("reply = %q, want mention optional confirmation", reply)
	}
	chat, ok := store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_group"})
	if !ok || !chat.MentionOptional {
		t.Fatalf("chat config = %+v, %v; want mention optional", chat, ok)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     msg.BotID,
		MessageID: "om_prompt",
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		Text:      "无需 at 也处理",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group no mention after off) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want one prompt after /at off", rt.promptCalls)
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
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want no extra prompt after /at on", rt.promptCalls)
	}
}

func TestHandleFeishuPrivateChatIgnoresAtConfigAndAlwaysResponds(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = workDir
	cfg.Agents["traex"] = agent
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

func TestHandleFeishuTopicThreadsUseSeparateSessions(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = workDir
	cfg.Agents["traex"] = agent
	rt := &fakeRuntime{newSessionIDs: []string{"acp-session-1", "acp-session-2"}, promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	for _, msg := range []feishu.Message{
		{
			BotID:     "bot-a",
			MessageID: "om_topic_1",
			ChatID:    "oc_group",
			ChatType:  "group",
			ThreadID:  "omt_topic_1",
			Text:      "@智能助手 topic one",
			Mentions:  []feishu.Mention{testBotMention("智能助手")},
		},
		{
			BotID:     "bot-a",
			MessageID: "om_topic_2",
			ChatID:    "oc_group",
			ChatType:  "group",
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
	if rt.newCalls[0].Key.ThreadID != "omt_topic_1" || rt.newCalls[1].Key.ThreadID != "omt_topic_2" {
		t.Fatalf("newCalls = %+v, want distinct thread keys", rt.newCalls)
	}
	for _, key := range []SessionKey{
		{BotID: "bot-a", ChatID: "oc_group", ThreadID: "omt_topic_1"},
		{BotID: "bot-a", ChatID: "oc_group", ThreadID: "omt_topic_2"},
	} {
		if _, ok := store.Get(key); !ok {
			t.Fatalf("topic session %v not persisted", key)
		}
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

func TestHandleFeishuNewSessionUsesDefaultTitleAndDisplaysModeModel(t *testing.T) {
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
	svc := NewService(config.Default(), store)
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
	svc := NewService(config.Default(), store)
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

func TestHandleFeishuMessageEmptyMessageDoesNotPrompt(t *testing.T) {
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
		filepath.Join("skills", "wiki", "SKILL.md"),
	} {
		if _, err := os.Stat(filepath.Join(workspace, name)); err != nil {
			t.Fatalf("workspace file %s not created: %v", name, err)
		}
	}
}

func TestHandleFeishuMessageNewDefersBootstrapContextPrompt(t *testing.T) {
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
		t.Fatalf("promptCalls = %+v, want /new to defer workspace context prompt", rt.promptCalls)
	}
	session, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"})
	if !ok {
		t.Fatalf("persisted session not found")
	}
	if session.Workspace != workspace {
		t.Fatalf("session workspace = %q, want %q", session.Workspace, workspace)
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
		t.Fatalf("promptCalls = %+v, want workspace context prompt on next message", rt.promptCalls)
	}
	setupPrompt := rt.promptCalls[0].Text
	for _, want := range []string{"Workspace Context", "Workspace Bootstrap", "L0/L1/L2", "knowledge/core.md", "knowledge/index.md", "knowledge/log.md", "SOUL.md", "MEMORY.md", "AGENTS.md", "TOOLS.md", "Bootstrap.md", "## User Message", "你好"} {
		if !strings.Contains(setupPrompt, want) {
			t.Fatalf("workspace context prompt = %q, want %q", setupPrompt, want)
		}
	}
	if strings.Contains(setupPrompt, ".setup.json") {
		t.Fatalf("workspace context prompt = %q, should not mention .setup.json", setupPrompt)
	}
	if _, err := os.Stat(filepath.Join(workspace, workspaceBootstrapFile)); err != nil {
		t.Fatalf("Bootstrap.md should stay until ACP agent deletes it: %v", err)
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
		t.Fatalf("promptCalls = %+v, want /new not to send workspace context prompt", rt.promptCalls)
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
	if got := card.textUpdatesSnapshot(); len(got) != 3 || got[0] != "收到。现在开始。" || got[1] != "" || got[2] != "工具处理完成。" {
		t.Fatalf("textUpdates = %+v, want pre-tool text moved away and final candidate retained", got)
	}
	if got := card.processUpdatesSnapshot(); len(got) != 3 ||
		got[0] != "收到。现在开始。" ||
		got[1] != "收到。现在开始。\n⏳ exec_command" ||
		got[2] != "收到。现在开始。\n⏳ exec_command\n🧠 The user wants an English paragraph." {
		t.Fatalf("processUpdates = %+v, want pre-tool agent text and normalized process updates", got)
	}
	if !card.isClosed() {
		t.Fatalf("stream card should be closed")
	}
}

func TestHandleFeishuMessageKeepsOnlyAgentTextAfterLastToolAsFinal(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "先检查。\n中间说明。\n最终结论。",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "先检查。"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call",
					Title:         "Read config",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "中间说明。"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call",
					Title:         "Run tests",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "最终结论。"},
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
		Text:      "run",
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
	textUpdates := cards[0].textUpdatesSnapshot()
	if len(textUpdates) == 0 || textUpdates[len(textUpdates)-1] != "最终结论。" {
		t.Fatalf("textUpdates = %+v, want only text after last tool as final candidate", textUpdates)
	}
	wantTextUpdates := []string{"先检查。", "", "中间说明。", "", "最终结论。"}
	if !reflect.DeepEqual(textUpdates, wantTextUpdates) {
		t.Fatalf("textUpdates = %+v, want stale intermediate candidate cleared as %+v", textUpdates, wantTextUpdates)
	}
	processUpdates := cards[0].processUpdatesSnapshot()
	if len(processUpdates) == 0 {
		t.Fatalf("processUpdates = %+v, want process updates", processUpdates)
	}
	lastProcess := processUpdates[len(processUpdates)-1]
	for _, want := range []string{"先检查。", "⏳ Read config", "中间说明。", "⏳ Run tests"} {
		if !strings.Contains(lastProcess, want) {
			t.Fatalf("last process update = %q, want %q", lastProcess, want)
		}
	}
	if strings.Contains(lastProcess, "最终结论") {
		t.Fatalf("last process update = %q, should not include final agent text", lastProcess)
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
	if got[len(got)-1] != "🧠 **Restating the request**\n\nThe user said" {
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

func TestHandleFeishuMessagePermissionRequestDefaultsToReject(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	rt := &fakeRuntime{
		promptReply: "done",
		permissionRequest: &acp.PermissionRequest{
			RequestID: "1",
			SessionID: session.ACPSessionID,
			ToolCall:  acp.PermissionToolCallRef{ToolCallID: "call-1"},
			Options: []acp.PermissionOption{
				{OptionID: "allow-once", Kind: "allow_once"},
				{OptionID: "reject-once", Kind: "reject_once"},
			},
		},
	}
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     session.Key.BotID,
		ChatID:    session.Key.ChatID,
		ThreadID:  session.Key.ThreadID,
		Text:      "run tests",
		Workspace: session.Workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q, want done", reply)
	}
	if rt.permissionOutcome.Outcome != "selected" || rt.permissionOutcome.OptionID != "reject-once" {
		t.Fatalf("permission outcome = %+v, want reject-once", rt.permissionOutcome)
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
	var cardsMu sync.Mutex
	var cards []*fakeStreamCard
	ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cardsMu.Lock()
		cards = append(cards, card)
		cardsMu.Unlock()
		return card, nil
	})
	cardsSnapshot := func() []*fakeStreamCard {
		cardsMu.Lock()
		defer cardsMu.Unlock()
		return append([]*fakeStreamCard(nil), cards...)
	}
	stream := newPromptCardStream(ctx, feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
	}, Session{ACPSessionID: "acp-session-1"})
	chunks := newPromptChunkAccumulator(stream)
	chunks.add(promptChunk{Target: promptChunkTargetText, Key: "agent_message", Text: "Hel"})
	chunks.add(promptChunk{Target: promptChunkTargetText, Key: "agent_message", Text: "lo"})
	if got := cardsSnapshot(); len(got) != 0 {
		t.Fatalf("cards = %+v, want debounce to delay card creation", got)
	}
	time.Sleep(promptCardFlushDelay + 80*time.Millisecond)
	gotCards := cardsSnapshot()
	if len(gotCards) != 1 {
		t.Fatalf("cards = %+v, want one stream card after debounce flush", gotCards)
	}
	if got := gotCards[0].textUpdatesSnapshot(); len(got) != 1 || got[0] != "Hello" {
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

func TestHandleFeishuMessageAutoCreatesSessionWithBootstrapContext(t *testing.T) {
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
		t.Fatalf("reply = %q, want bootstrap reply", reply)
	}
	if len(rt.newCalls) != 1 {
		t.Fatalf("newCalls = %+v, want auto-created session", rt.newCalls)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want bootstrap context prompt only", rt.promptCalls)
	}
	if !strings.Contains(rt.promptCalls[0].Text, "Workspace Bootstrap") {
		t.Fatalf("prompt text = %q, want bootstrap context", rt.promptCalls[0].Text)
	}
	if !strings.Contains(rt.promptCalls[0].Text, "## User Message") || !strings.Contains(rt.promptCalls[0].Text, "你好") {
		t.Fatalf("prompt text = %q, want user message with bootstrap context", rt.promptCalls[0].Text)
	}
	session, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"})
	if !ok {
		t.Fatalf("auto-created session not persisted")
	}
	if session.Workspace != workspace {
		t.Fatalf("session workspace = %q, want %q", session.Workspace, workspace)
	}
}

func TestHandleFeishuMessageStatusShowsPersistedSessionWithoutReadyState(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Default(), store)
	rt := &fakeRuntime{}
	svc.setRuntime(rt)
	workspace := filepath.Join(t.TempDir(), "bot-a")
	markWorkspaceBootstrapped(t, workspace)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}
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

func TestHandleFeishuMessageKeepsPersistedACPSessionAfterBootstrapDeleted(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workspace := filepath.Join(t.TempDir(), "bot-a")
	markWorkspaceBootstrapped(t, workspace)
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
	}); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
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
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if len(rt.newCalls) != 0 {
		t.Fatalf("newCalls = %+v, want existing ACP session reused", rt.newCalls)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want one prompt", rt.promptCalls)
	}
	if rt.promptCalls[0].Session.ACPSessionID != "setup-session" {
		t.Fatalf("prompt session = %q, want existing setup-session", rt.promptCalls[0].Session.ACPSessionID)
	}
	if !strings.Contains(rt.promptCalls[0].Text, "## Workspace Context") || !strings.Contains(rt.promptCalls[0].Text, "## User Message") || !strings.Contains(rt.promptCalls[0].Text, "你是谁") {
		t.Fatalf("prompt text = %q, want workspace knowledge and user message", rt.promptCalls[0].Text)
	}
	session, ok := store.Get(key)
	if !ok {
		t.Fatalf("session not found")
	}
	if session.ACPSessionID != "setup-session" {
		t.Fatalf("session = %+v, want existing ACP session retained", session)
	}
}

func TestHandleFeishuMessageRecreatesMissingACPSessionTitleUsesUserText(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workspace := filepath.Join(t.TempDir(), "bot-a")
	markWorkspaceBootstrapped(t, workspace)
	workDir := t.TempDir()
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}
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
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		Workspace: workspace,
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
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

func TestHandleFeishuMessageBootstrappedWorkspaceAllowsNewSession(t *testing.T) {
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

	markWorkspaceBootstrapped(t, workspace)
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
		t.Fatalf("promptCalls = %+v, want /new to defer workspace context prompt", rt.promptCalls)
	}
	if _, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}); !ok {
		t.Fatalf("session not found")
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
		t.Fatalf("promptCalls = %+v, want workspace context prompt on next message", rt.promptCalls)
	}
	contextPrompt := rt.promptCalls[0].Text
	for _, want := range []string{"Workspace Context", "SOUL.md", "名字：小助手", "MEMORY.md", "偏好：中文回复", "knowledge/core.md", "repo-workflow", "skills/wiki/SKILL.md"} {
		if !strings.Contains(contextPrompt, want) {
			t.Fatalf("workspace context prompt = %q, want %q", contextPrompt, want)
		}
	}
	if !strings.Contains(contextPrompt, "## User Message") || !strings.Contains(contextPrompt, "介绍一下") {
		t.Fatalf("workspace context prompt = %q, want user message", contextPrompt)
	}
	if _, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}); !ok {
		t.Fatalf("session not found after next prompt")
	}
}

func TestHandleFeishuMessagePreservesACPStateUpdates(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	rt := &fakeRuntime{promptReply: "ACP 回复"}
	rt.afterUpdates = func() {
		latest, ok := store.Get(session.Key)
		if !ok {
			t.Errorf("session not found during afterUpdates")
			return
		}
		latest.AvailableCommands = []acp.AvailableCommand{{Name: "review", Description: "Review changes"}}
		if err := store.Upsert(latest); err != nil {
			t.Errorf("Upsert(latest) error = %v", err)
		}
	}
	svc := NewService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     session.Key.BotID,
		Workspace: session.Workspace,
		MessageID: "om_msg",
		ChatID:    session.Key.ChatID,
		ThreadID:  session.Key.ThreadID,
		Text:      "介绍一下",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	updated, ok := store.Get(session.Key)
	if !ok {
		t.Fatalf("session not found after prompt")
	}
	if !sessionHasCommand(updated, "review") {
		t.Fatalf("available commands = %+v, want review preserved", updated.AvailableCommands)
	}
}

func TestHandleFeishuMessageAutoCreatesSessionWithKnowledge(t *testing.T) {
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
	session, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"})
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
	session, ok := reloaded.Get(SessionKey{ChatID: "oc_chat", ThreadID: "omt_thread"})
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
	svc := NewService(config.Default(), store)
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
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/new " + workDir,
	}
	if _, err := handleFeishuMessage(t, svc, context.Background(), msg); err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
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
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/new " + workDir,
	}
	if _, err := handleFeishuMessage(t, svc, context.Background(), msg); err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:      "bot-a",
		MessageID:  "om_prompt",
		ChatID:     "oc_chat",
		ChatType:   "group",
		ThreadID:   "omt_thread",
		RootID:     "om_root",
		ParentID:   "om_parent",
		SenderID:   "ou_sender",
		SenderType: "user",
		MsgType:    "text",
		Text:       "@我的智能助手 介绍一下这个仓库",
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

func TestHandleWikiCommandPersistsConfig(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Default(), store)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
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

func TestWikiStatusDoesNotCancelScheduledReflection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Default(), store)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}
	session := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-1",
		Cwd:             t.TempDir(),
		WikiIntervalSec: 60,
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc.scheduleWikiAfterUserPrompt(session, config.Default().Agents["traex"])
	t.Cleanup(func() { svc.cancelWikiTimer(key) })
	svc.taskMu.Lock()
	beforeGeneration := svc.wikiGenerations[key]
	_, beforeTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if !beforeTimer {
		t.Fatal("wiki timer should be scheduled before status command")
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_status",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/wiki status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki status) error = %v", err)
	}
	if !strings.Contains(reply, "等待定时触发") {
		t.Fatalf("reply = %q, want scheduled timer status", reply)
	}
	svc.taskMu.Lock()
	afterGeneration := svc.wikiGenerations[key]
	_, afterTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if !afterTimer {
		t.Fatal("/wiki status should not cancel scheduled wiki timer")
	}
	if afterGeneration != beforeGeneration {
		t.Fatalf("wiki generation = %d, want unchanged %d", afterGeneration, beforeGeneration)
	}
}

func TestWikiIntervalReschedulesScheduledReflection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Default(), store)
	svc.setRuntime(&fakeRuntime{promptReply: "NoReply"})
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}
	session := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-1",
		Cwd:             t.TempDir(),
		WikiIntervalSec: 60,
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc.scheduleWikiAfterUserPrompt(session, config.Default().Agents["traex"])
	t.Cleanup(func() { svc.cancelWikiTimer(key) })
	svc.taskMu.Lock()
	beforeGeneration := svc.wikiGenerations[key]
	_, beforeTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if !beforeTimer {
		t.Fatal("wiki timer should be scheduled before interval command")
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_interval",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/wiki interval 1s",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki interval) error = %v", err)
	}
	if !strings.Contains(reply, "1s") {
		t.Fatalf("reply = %q, want interval confirmation", reply)
	}
	svc.taskMu.Lock()
	afterGeneration := svc.wikiGenerations[key]
	_, afterTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if !afterTimer {
		t.Fatal("/wiki interval should keep wiki timer scheduled")
	}
	if afterGeneration <= beforeGeneration {
		t.Fatalf("wiki generation = %d, want greater than %d after reschedule", afterGeneration, beforeGeneration)
	}
	session, ok := store.Get(key)
	if !ok || session.WikiIntervalSec != 1 {
		t.Fatalf("session = %+v, want wiki interval persisted", session)
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

	secondCtx := logging.CtxAddAttr(ctx, slog.String("message_id", "om_second"))
	reply, err := handleFeishuMessage(t, svc, secondCtx, feishu.Message{
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
	rt.mu.Lock()
	if len(rt.cancelCalls) == 0 || rt.cancelCalls[0].Attrs["message_id"] != "om_second" {
		t.Fatalf("cancel calls = %+v, want cancellation ctx from second message", rt.cancelCalls)
	}
	rt.mu.Unlock()
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

func TestHandleFeishuMessageReadOnlyCommandDoesNotCancelInFlightPrompt(t *testing.T) {
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
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	firstDone := make(chan struct {
		reply string
		err   error
	}, 1)
	go func() {
		reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
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

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_status",
		ChatID:    "oc_chat",
		ThreadID:  "omt_thread",
		Text:      "/status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/status) error = %v", err)
	}
	if !strings.Contains(reply, "session：acp-session-1") {
		t.Fatalf("reply = %q, want status without cancelling running prompt", reply)
	}
	if got := rt.cancelCallCount(); got != 0 {
		t.Fatalf("cancel calls = %d, want read-only command not to cancel running prompt", got)
	}
	select {
	case got := <-firstDone:
		t.Fatalf("first prompt finished before unblock: %+v", got)
	default:
	}

	close(rt.blockPrompt)
	select {
	case got := <-firstDone:
		if got.err != nil {
			t.Fatalf("first HandleFeishuMessage() error = %v", got.err)
		}
		if got.reply != "ACP 回复" {
			t.Fatalf("first reply = %q, want ACP reply after unblock", got.reply)
		}
	case <-time.After(time.Second):
		t.Fatal("first prompt did not finish after unblock")
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
	for _, want := range []string{"Workspace Memory Policy", "本地文件工具", "MEMORY.md", "knowledge/core.md", "knowledge/index.md", "skills/core.md", "## User Message", userText} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
	for _, notWant := range []string{"fs/read_text_file", "fs/write_text_file"} {
		if strings.Contains(prompt, notWant) {
			t.Fatalf("prompt = %q, should not mention %q", prompt, notWant)
		}
	}
}

func assertPromptContainsMessageMetadata(t *testing.T, prompt string, want map[string]string) {
	t.Helper()
	assertPromptContainsSectionMetadata(t, prompt, "## Message Metadata", want)
	messageIdx := strings.Index(prompt, "## Message Metadata")
	userIdx := strings.Index(prompt, "## User Message")
	if messageIdx < 0 || userIdx < 0 || messageIdx > userIdx {
		t.Fatalf("prompt = %q, want message metadata before user message", prompt)
	}
}

func assertPromptContainsSectionMetadata(t *testing.T, prompt string, section string, want map[string]string) {
	t.Helper()
	metadata := promptSectionJSON(t, prompt, section)
	for key, value := range want {
		if got := metadata[key]; got != value {
			t.Fatalf("%s metadata[%q] = %q, want %q; prompt = %q", section, key, got, value, prompt)
		}
	}
}

func promptSectionJSON(t *testing.T, prompt string, section string) map[string]string {
	t.Helper()
	idx := strings.Index(prompt, section)
	if idx < 0 {
		t.Fatalf("prompt = %q, want section %q", prompt, section)
	}
	rest := prompt[idx+len(section):]
	start := strings.Index(rest, "```json")
	if start < 0 {
		t.Fatalf("section %q in prompt = %q, want json fence", section, prompt)
	}
	rest = rest[start+len("```json"):]
	end := strings.Index(rest, "```")
	if end < 0 {
		t.Fatalf("section %q in prompt = %q, want closing fence", section, prompt)
	}
	var metadata map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest[:end])), &metadata); err != nil {
		t.Fatalf("Unmarshal(%s metadata) error = %v", section, err)
	}
	return metadata
}

type fakeRuntime struct {
	mu                sync.Mutex
	newSessionID      string
	newSessionIDs     []string
	newSessionInfo    acp.SessionInfo
	noDefaultState    bool
	afterNewSession   func(key SessionKey, sessionID string)
	promptReply       string
	promptErrors      []error
	promptUpdates     []acp.PromptUpdate
	afterUpdates      func()
	permissionRequest *acp.PermissionRequest
	permissionOutcome acp.PermissionOutcome
	blockPrompt       chan struct{}
	blockPromptAt     int
	configOptions     []acp.SessionConfigOption
	configCalls       []fakeConfigCall
	newCalls          []fakeNewCall
	promptCalls       []fakePromptCall
	cancelCalls       []fakeCancelCall
	closedKeys        []SessionKey
	updateHandlers    map[SessionKey][]acp.UpdateHandler
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
	Attrs   map[string]string
}

type fakeConfigCall struct {
	Session  Session
	ConfigID string
	Value    any
}

func (f *fakeRuntime) NewSession(ctx context.Context, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (acp.SessionInfo, error) {
	f.mu.Lock()
	f.newCalls = append(f.newCalls, fakeNewCall{Key: key, AgentName: agentName, Cwd: cwd, Workspace: workspace})
	info := f.newSessionInfo
	afterNewSession := f.afterNewSession
	if len(f.newSessionIDs) > 0 {
		info.SessionID = f.newSessionIDs[0]
		f.newSessionIDs = f.newSessionIDs[1:]
	} else if f.newSessionID != "" {
		info.SessionID = f.newSessionID
	}
	if info.SessionID == "" {
		info.SessionID = "acp-session"
	}
	if !f.noDefaultState && len(info.ConfigOptions) == 0 && info.Models == nil && info.Mode == nil {
		info.ConfigOptions = []acp.SessionConfigOption{
			{ID: "model", Name: "Model", Category: "model", Type: "select", CurrentValue: "gpt-5.5"},
		}
	}
	f.mu.Unlock()
	if afterNewSession != nil {
		afterNewSession(key, info.SessionID)
	}
	return info, nil
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
	var promptErr error
	if len(f.promptErrors) > 0 {
		promptErr = f.promptErrors[0]
		f.promptErrors = f.promptErrors[1:]
	}
	f.mu.Unlock()
	if opts.OnUpdate != nil {
		for _, update := range updates {
			opts.OnUpdate(update)
		}
	}
	if afterUpdates != nil {
		afterUpdates()
	}
	if f.permissionRequest != nil && opts.OnPermissionRequest != nil {
		outcome, _ := opts.OnPermissionRequest(ctx, *f.permissionRequest)
		f.mu.Lock()
		f.permissionOutcome = outcome
		f.mu.Unlock()
	}
	if blockThisPrompt {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-blockPrompt:
		}
	}
	if promptErr != nil {
		return "", promptErr
	}
	return reply, nil
}

func (f *fakeRuntime) CancelSession(ctx context.Context, session Session, agent config.AgentConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls = append(f.cancelCalls, fakeCancelCall{Session: session, Attrs: slogAttrsMap(logging.CtxAttrs(ctx))})
	return nil
}

func (f *fakeRuntime) SetConfigOption(ctx context.Context, session Session, agent config.AgentConfig, configID string, value any) ([]acp.SessionConfigOption, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configCalls = append(f.configCalls, fakeConfigCall{Session: session, ConfigID: configID, Value: value})
	if len(f.configOptions) > 0 {
		return append([]acp.SessionConfigOption(nil), f.configOptions...), nil
	}
	return []acp.SessionConfigOption{
		{
			ID:           configID,
			Name:         configID,
			Category:     configID,
			Type:         "select",
			CurrentValue: value,
		},
	}, nil
}

func (f *fakeRuntime) SubscribeUpdates(key SessionKey, handler acp.UpdateHandler) func() {
	if handler == nil {
		return func() {}
	}
	f.mu.Lock()
	if f.updateHandlers == nil {
		f.updateHandlers = make(map[SessionKey][]acp.UpdateHandler)
	}
	f.updateHandlers[key] = append(f.updateHandlers[key], handler)
	f.mu.Unlock()
	return func() {}
}

func (f *fakeRuntime) dispatchUpdate(key SessionKey, sessionID string, update acp.SessionUpdate) {
	f.mu.Lock()
	handlers := append([]acp.UpdateHandler(nil), f.updateHandlers[key]...)
	f.mu.Unlock()
	for _, handler := range handlers {
		handler(sessionID, update)
	}
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
