package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
const testOwnerOpenID = "ou_owner"

func testBotMention(name string) feishu.Mention {
	return feishu.Mention{ID: testBotOpenID, Name: name, Type: "bot"}
}

func newTestService(cfg config.Config, store *SessionStore) *Service {
	if len(cfg.Bots) > 0 && len(cfg.Bots[0].OwnerOpenIDs) == 0 {
		cfg.Bots[0].OwnerOpenIDs = []string{testOwnerOpenID}
	}
	return NewService(cfg, store)
}

func handleFeishuMessage(t *testing.T, svc *Service, ctx context.Context, msg feishu.Message) (string, error) {
	t.Helper()
	if strings.TrimSpace(msg.BotOpenID) == "" {
		msg.BotOpenID = testBotOpenID
	}
	if strings.TrimSpace(msg.SenderID) == "" && strings.HasPrefix(strings.TrimSpace(stripMentionNames(msg.Text, msg.Mentions)), "/") {
		ensureTestOwner(t, svc, msg.BotID)
		if owners := svc.ownerOpenIDs(msg.BotID); len(owners) > 0 {
			msg.SenderID = strings.TrimSpace(owners[0])
		} else {
			msg.SenderID = testOwnerOpenID
		}
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

type fakeSentMessageClient struct {
	mu            sync.Mutex
	nextID        string
	sent          []string
	msgs          []feishu.Message
	updates       []string
	updateIDs     []string
	finishes      []string
	finishIDs     []string
	loopRequests  []feishu.LoopStatusCardRequest
	textUpdates   []string
	textUpdateIDs []string
}

func newFakeSentMessageClient(nextID string) *fakeSentMessageClient {
	return &fakeSentMessageClient{nextID: nextID}
}

func (f *fakeSentMessageClient) send(ctx context.Context, msg feishu.Message, text string) (feishu.SentMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := strings.TrimSpace(f.nextID)
	if id == "" {
		id = "om_loop_start"
	}
	f.sent = append(f.sent, text)
	f.msgs = append(f.msgs, msg)
	return feishu.SentMessage{
		MessageID: id,
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		ThreadID:  msg.ThreadID,
		RootID:    id,
	}, nil
}

func (f *fakeSentMessageClient) update(ctx context.Context, messageID string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.textUpdateIDs = append(f.textUpdateIDs, messageID)
	f.textUpdates = append(f.textUpdates, text)
	return nil
}

func (f *fakeSentMessageClient) sendLoopStatusCard(ctx context.Context, msg feishu.Message, request feishu.LoopStatusCardRequest) (feishu.LoopStatusCard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := strings.TrimSpace(f.nextID)
	if id == "" {
		id = "om_loop_start"
	}
	sent := feishu.SentMessage{
		MessageID: id,
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		ThreadID:  msg.ThreadID,
		RootID:    id,
	}
	f.sent = append(f.sent, request.Text)
	f.msgs = append(f.msgs, msg)
	f.loopRequests = append(f.loopRequests, request)
	return &fakeLoopStatusCard{client: f, message: sent}, nil
}

type fakeLoopStatusCard struct {
	client                *fakeSentMessageClient
	message               feishu.SentMessage
	failOnCanceledContext bool
}

func (f *fakeLoopStatusCard) Message() feishu.SentMessage {
	if f == nil {
		return feishu.SentMessage{}
	}
	return f.message
}

func (f *fakeLoopStatusCard) Update(ctx context.Context, text string) error {
	if f == nil || f.client == nil {
		return nil
	}
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	f.client.updateIDs = append(f.client.updateIDs, f.message.MessageID)
	f.client.updates = append(f.client.updates, text)
	return nil
}

func (f *fakeLoopStatusCard) Finish(ctx context.Context, text string) error {
	if f == nil || f.client == nil {
		return nil
	}
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	f.client.updateIDs = append(f.client.updateIDs, f.message.MessageID)
	f.client.updates = append(f.client.updates, text)
	f.client.finishIDs = append(f.client.finishIDs, f.message.MessageID)
	f.client.finishes = append(f.client.finishes, text)
	return nil
}

func (f *fakeSentMessageClient) sentSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func (f *fakeSentMessageClient) updatesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.updates...)
}

func (f *fakeSentMessageClient) updateIDsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.updateIDs...)
}

func (f *fakeSentMessageClient) finishesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.finishes...)
}

func (f *fakeSentMessageClient) loopRequestsSnapshot() []feishu.LoopStatusCardRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]feishu.LoopStatusCardRequest(nil), f.loopRequests...)
}

func (f *fakeSentMessageClient) messagesSnapshot() []feishu.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]feishu.Message(nil), f.msgs...)
}

func (f *fakeSentMessageClient) textUpdatesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.textUpdates...)
}

func withFakeSentMessageClient(ctx context.Context, client *fakeSentMessageClient) context.Context {
	ctx = feishu.WithSentMessageSender(ctx, client.send)
	ctx = feishu.WithMessageUpdater(ctx, client.update)
	return feishu.WithLoopStatusCardSender(ctx, client.sendLoopStatusCard)
}

func ensureTestOwner(t *testing.T, svc *Service, botID string) {
	t.Helper()
	if svc == nil || len(svc.ownerOpenIDs(botID)) > 0 {
		return
	}
	if len(svc.cfg.Bots) == 0 {
		return
	}
	botID = strings.TrimSpace(botID)
	for i := range svc.cfg.Bots {
		if strings.TrimSpace(svc.cfg.Bots[i].ID) == botID {
			svc.cfg.Bots[i].OwnerOpenIDs = []string{testOwnerOpenID}
			return
		}
	}
	if len(svc.cfg.Bots) == 1 {
		svc.cfg.Bots[0].OwnerOpenIDs = []string{testOwnerOpenID}
	}
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

func TestEnsureWorkspaceRejectsFilePath(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.WriteFile(workspace, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile(workspace) error = %v", err)
	}

	if _, err := ensureWorkspace(workspace, "bot-a"); err == nil || !strings.Contains(err.Error(), "workspace 路径不是目录") {
		t.Fatalf("ensureWorkspace(file) error = %v, want not directory error", err)
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

func TestWorkspaceContextPromptTruncatesLargeFiles(t *testing.T) {
	workspace := t.TempDir()
	largeContent := strings.Repeat("a", int(workspaceContextFileMaxBytes)+32) + "TAIL_SHOULD_NOT_APPEAR"
	if err := os.WriteFile(filepath.Join(workspace, "SOUL.md"), []byte(largeContent), 0o644); err != nil {
		t.Fatalf("WriteFile(SOUL.md) error = %v", err)
	}

	prompt := workspaceContextPrompt(workspace)
	if !strings.Contains(prompt, "Workspace Context") {
		t.Fatalf("prompt = %q, want workspace context", prompt)
	}
	if !strings.Contains(prompt, "已截断") {
		t.Fatalf("prompt = %q, want truncation notice", prompt)
	}
	if strings.Contains(prompt, "TAIL_SHOULD_NOT_APPEAR") {
		t.Fatalf("prompt contains truncated tail")
	}
}

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

func TestHandleFeishuMessageSlashCommandRejectsNonOwner(t *testing.T) {
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].OwnerOpenIDs = []string{"ou_owner"}
	svc := NewService(cfg, nil)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		Text:     "/status",
		SenderID: "ou_someone",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/status) error = %v", err)
	}
	if reply != "只有 bot owner 可以执行斜杠命令。" {
		t.Fatalf("reply = %q, want non-owner warning", reply)
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

func TestApplyACPStateUpdateReplacesConfigOptions(t *testing.T) {
	session := Session{
		ConfigOptions: []acp.SessionConfigOption{
			{ID: "model", Name: "Model", Type: "select", CurrentValue: "gpt-5.5"},
			{ID: "mode", Name: "Mode", Type: "select", CurrentValue: "ask"},
		},
	}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "config_option_update",
		ConfigOptions: []acp.SessionConfigOption{
			{ID: "reasoning", Name: "Reasoning", Type: "select", CurrentValue: "high"},
			{ID: "model", Name: "Model", Type: "select", CurrentValue: "gpt-5.6"},
		},
	}) {
		t.Fatal("config_option_update should be treated as changed")
	}
	if len(session.ConfigOptions) != 2 {
		t.Fatalf("ConfigOptions = %+v, want full replacement with two options", session.ConfigOptions)
	}
	if session.ConfigOptions[0].ID != "reasoning" || session.ConfigOptions[0].CurrentValue != "high" {
		t.Fatalf("first config option = %+v, want reasoning high", session.ConfigOptions[0])
	}
	if session.ConfigOptions[1].ID != "model" || session.ConfigOptions[1].CurrentValue != "gpt-5.6" {
		t.Fatalf("second config option = %+v, want model gpt-5.6", session.ConfigOptions[1])
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

func TestApplyACPStateUpdatePersistsCurrentModeUpdate(t *testing.T) {
	session := Session{
		Mode: &acp.SessionModeState{
			CurrentModeID: "default",
			AvailableModes: []acp.SessionMode{
				{ModeID: "default", Name: "Default"},
				{ModeID: "plan", Name: "Plan"},
			},
		},
	}
	if !isACPStateUpdate(acp.SessionUpdate{SessionUpdate: "current_mode_update", ModeID: "plan"}) {
		t.Fatal("current_mode_update should be treated as state update")
	}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{SessionUpdate: "current_mode_update", ModeID: "plan"}) {
		t.Fatal("current_mode_update should update current mode")
	}
	if session.Mode == nil || session.Mode.CurrentModeID != "plan" {
		t.Fatalf("Mode = %+v, want current mode plan", session.Mode)
	}
	if len(session.Mode.AvailableModes) != 2 {
		t.Fatalf("AvailableModes = %+v, want preserved mode list", session.Mode.AvailableModes)
	}
}

func TestApplyACPStateUpdatePersistsSessionInfoTitle(t *testing.T) {
	session := Session{Title: "旧标题"}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		Title:         "新标题",
		TitleSet:      true,
	}) {
		t.Fatal("session_info_update should update title")
	}
	if session.Title != "新标题" {
		t.Fatalf("Title = %q, want 新标题", session.Title)
	}

	session.ManualTitle = true
	if applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		Title:         "自动标题",
		TitleSet:      true,
	}) {
		t.Fatal("manual title should not be overwritten by session_info_update")
	}
	if session.Title != "新标题" {
		t.Fatalf("Title = %q, want manual title preserved", session.Title)
	}
}

func TestApplyACPStateUpdateClearsSessionInfoTitle(t *testing.T) {
	session := Session{Title: "旧标题"}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		TitleSet:      true,
	}) {
		t.Fatal("session_info_update should clear title when title is null")
	}
	if session.Title != "" {
		t.Fatalf("Title = %q, want cleared", session.Title)
	}

	session = Session{Title: "手动标题", ManualTitle: true}
	if applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		TitleSet:      true,
	}) {
		t.Fatal("manual title should not be cleared by session_info_update")
	}
	if session.Title != "手动标题" {
		t.Fatalf("Title = %q, want manual title preserved", session.Title)
	}
}

func TestApplyACPStateUpdatePersistsACPUpdatedAt(t *testing.T) {
	session := Session{ACPUpdatedAt: "2025-10-29T14:22:15Z"}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		UpdatedAt:     "2025-10-29T15:00:00Z",
		UpdatedAtSet:  true,
	}) {
		t.Fatal("session_info_update should update ACP updatedAt")
	}
	if session.ACPUpdatedAt != "2025-10-29T15:00:00Z" {
		t.Fatalf("ACPUpdatedAt = %q, want updated timestamp", session.ACPUpdatedAt)
	}

	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		UpdatedAtSet:  true,
	}) {
		t.Fatal("session_info_update should clear ACP updatedAt")
	}
	if session.ACPUpdatedAt != "" {
		t.Fatalf("ACPUpdatedAt = %q, want cleared", session.ACPUpdatedAt)
	}
}

func TestApplyACPStateUpdatePersistsACPMeta(t *testing.T) {
	session := Session{ACPMeta: map[string]any{"old": true}}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		Meta: map[string]any{
			"priority": "high",
		},
	}) {
		t.Fatal("session_info_update should update ACP meta")
	}
	if got, ok := session.ACPMeta["priority"]; !ok || got != "high" {
		t.Fatalf("ACPMeta[priority] = %v, want high", got)
	}
	if _, ok := session.ACPMeta["old"]; ok {
		t.Fatalf("ACPMeta retained old key: %+v", session.ACPMeta)
	}

	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		Meta:          map[string]any{},
	}) {
		t.Fatal("session_info_update should clear ACP meta with empty object")
	}
	if len(session.ACPMeta) != 0 {
		t.Fatalf("ACPMeta = %+v, want empty", session.ACPMeta)
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
	svc := newTestService(config.Default(), store)
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
	svc := newTestService(config.Default(), store)
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
		ChatID:   session.Key.ChatID,
		ThreadID: session.Key.ThreadID,
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
	cfg := config.Default()
	cfg.Bots[0].OwnerOpenIDs = []string{"ou_requester"}
	svc := NewService(cfg, store)
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
	svc := newTestService(config.Default(), store)
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
	cfg := config.Default()
	cfg.Bots[0].OwnerOpenIDs = []string{"ou_requester"}
	svc := NewService(cfg, store)
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

func TestHandleFeishuMessageShowCommandPersistsDisplayOptions(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	svc := newTestService(config.Default(), store)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   session.Key.ChatID,
		ThreadID: session.Key.ThreadID,
		Text:     "/show thought off",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/show thought off) error = %v", err)
	}
	for _, want := range []string{"已关闭思考消息展示", "过程消息：开启", "思考消息：关闭", "工具调用：开启", "状态栏：开启", "用量明细：开启"} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
	updated, ok := store.GetChat(chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: session.Key.ChatID}))
	if !ok {
		t.Fatal("chat config not found")
	}
	if !updated.HideThoughts || updated.HideStepMessages || updated.HideTools {
		t.Fatalf("chat display flags = step:%v thought:%v tool:%v, want only thoughts hidden", updated.HideStepMessages, updated.HideThoughts, updated.HideTools)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   session.Key.ChatID,
		ThreadID: session.Key.ThreadID,
		Text:     "/show thought on",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/show thought on) error = %v", err)
	}
	if !strings.Contains(reply, "已开启思考消息展示") || !strings.Contains(reply, "思考消息：开启") {
		t.Fatalf("reply = %q, want thought display enabled", reply)
	}
	updated, _ = store.GetChat(chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: session.Key.ChatID}))
	if updated.HideThoughts {
		t.Fatalf("HideThoughts = true, want false after /show thought on")
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   session.Key.ChatID,
		ThreadID: session.Key.ThreadID,
		Text:     "/show status off",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/show status off) error = %v", err)
	}
	if !strings.Contains(reply, "已关闭状态栏展示") || !strings.Contains(reply, "状态栏：关闭") {
		t.Fatalf("reply = %q, want status bar disabled", reply)
	}
	updated, _ = store.GetChat(chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: session.Key.ChatID}))
	if !updated.HideStatusBar {
		t.Fatalf("HideStatusBar = false, want true after /show status off")
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   session.Key.ChatID,
		ThreadID: session.Key.ThreadID,
		Text:     "/show used off",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/show used off) error = %v", err)
	}
	if !strings.Contains(reply, "已关闭用量明细展示") || !strings.Contains(reply, "用量明细：关闭") {
		t.Fatalf("reply = %q, want usage detail disabled", reply)
	}
	updated, _ = store.GetChat(chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: session.Key.ChatID}))
	if !updated.HideUsageDetail {
		t.Fatalf("HideUsageDetail = false, want true after /show used off")
	}
}

func TestHandleFeishuMessageShowCommandPersistsWithoutSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
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

func TestHandleFeishuMessageShowCommandSurvivesNewSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = t.TempDir()
	cfg.Agents["traex"] = agent
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
	if session, ok := store.Get(SessionKey{BotID: msg.BotID, ChatID: msg.ChatID}); !ok || session.HideStatusBar || session.HideUsageDetail {
		t.Fatalf("session = %+v, %v; show options should not be stored on session", session, ok)
	}
	var statusBarEnabled *bool
	var cards []*fakeStreamCard
	ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		enabled := feishu.StreamCardStatusBarEnabled(ctx)
		statusBarEnabled = &enabled
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})

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

func TestHandleFeishuMessageNewMigratesLegacySessionShowOptionsToChat(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.Key.ThreadID = ""
	session.HideStatusBar = true
	session.HideUsageDetail = true
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = t.TempDir()
	cfg.Agents["traex"] = agent
	rt := &fakeRuntime{newSessionID: "acp-session-new"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   session.Key.ChatID,
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
	chat, ok := store.GetChat(chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: session.Key.ChatID}))
	if !ok || !chat.HideStatusBar || !chat.HideUsageDetail {
		t.Fatalf("chat config = %+v, %v; want legacy show options migrated", chat, ok)
	}
	updated, ok := store.Get(session.Key)
	if !ok || updated.HideStatusBar || updated.HideUsageDetail {
		t.Fatalf("new session = %+v, %v; show options should not be stored on new session", updated, ok)
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
	svc := newTestService(config.Default(), store)
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
	selection.RequesterID = ""
	if _, err := svc.HandleModelSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "缺少发起人或操作者") {
		t.Fatalf("missing requester error = %v, want requester metadata validation", err)
	}

	selection.RequesterID = "ou_requester"
	selection.OperatorID = ""
	if _, err := svc.HandleModelSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "缺少发起人或操作者") {
		t.Fatalf("missing operator error = %v, want operator metadata validation", err)
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
	svc := newTestService(config.Default(), store)
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
	selection.RequesterID = ""
	if _, err := svc.HandleModeSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "缺少发起人或操作者") {
		t.Fatalf("missing requester error = %v, want requester metadata validation", err)
	}

	selection.RequesterID = "ou_requester"
	selection.OperatorID = ""
	if _, err := svc.HandleModeSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "缺少发起人或操作者") {
		t.Fatalf("missing operator error = %v, want operator metadata validation", err)
	}

	selection.OperatorID = selection.RequesterID
	selection.ACPSessionID = "stale-session"
	if _, err := svc.HandleModeSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("stale card error = %v, want expired validation", err)
	}
}

func TestHandleModeSelectionFallsBackToLegacySetMode(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.ConfigOptions = nil
	session.Mode = &acp.SessionModeState{
		CurrentModeID: "default",
		AvailableModes: []acp.SessionMode{
			{ModeID: "default", Name: "Default"},
			{ModeID: "plan", Name: "Plan"},
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	display, err := svc.HandleModeSelection(context.Background(), feishu.ModeSelection{
		BotID:        session.Key.BotID,
		ChatID:       session.Key.ChatID,
		ThreadID:     session.Key.ThreadID,
		ACPSessionID: session.ACPSessionID,
		RequesterID:  "ou_requester",
		OperatorID:   "ou_requester",
		Mode:         "Plan",
	})
	if err != nil {
		t.Fatalf("HandleModeSelection() error = %v", err)
	}
	if display != "Plan（plan）" {
		t.Fatalf("display = %q, want friendly legacy mode display", display)
	}
	if len(rt.modeCalls) != 1 || rt.modeCalls[0].ModeID != "plan" {
		t.Fatalf("modeCalls = %+v, want legacy set_mode plan", rt.modeCalls)
	}
	if len(rt.configCalls) != 0 {
		t.Fatalf("configCalls = %+v, want no set_config_option fallback call", rt.configCalls)
	}
	updated, ok := store.Get(session.Key)
	if !ok || updated.Mode == nil || updated.Mode.CurrentModeID != "plan" {
		t.Fatalf("updated session = %+v, want legacy mode plan persisted", updated)
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

func TestHandleFeishuMessagePersistsNewSessionInfoMeta(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = workDir
	cfg.Agents["traex"] = agent
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
		ThreadID:  "omt_thread",
		Text:      "@我的智能助手 你好",
		Mentions: []feishu.Mention{
			testBotMention("我的智能助手"),
		},
	}); err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	session, ok := store.Get(SessionKey{ChatID: "oc_chat", ThreadID: "omt_thread"})
	if !ok {
		t.Fatalf("auto-created session not persisted")
	}
	if got := session.ACPMeta["messageCount"]; got != 12 {
		t.Fatalf("ACPMeta[messageCount] = %v, want 12", got)
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
		newSessionInfo: acp.SessionInfo{
			SessionID: "new-acp-session",
			Meta: map[string]any{
				"refreshed": true,
			},
		},
		promptReply:  "ACP 回复",
		promptErrors: []error{errACPSessionUnavailable},
	}
	svc := newTestService(config.Default(), store)
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
	if got := updated.ACPMeta["refreshed"]; got != true {
		t.Fatalf("ACPMeta[refreshed] = %v, want true", got)
	}
	if updated.Title != "继续" {
		t.Fatalf("persisted title = %q, want final session title", updated.Title)
	}
	items := store.ListByChat("", "oc_chat")
	if len(items) != 2 {
		t.Fatalf("history = %+v, want old and refreshed sessions", items)
	}
	var oldItem, newItem Session
	for _, item := range items {
		switch item.ACPSessionID {
		case "old-acp-session":
			oldItem = item
		case "new-acp-session":
			newItem = item
		}
	}
	if oldItem.ACPSessionID == "" || oldItem.Title != "old session" {
		t.Fatalf("old history item = %+v, want original title retained", oldItem)
	}
	if newItem.ACPSessionID == "" || newItem.Title != "继续" {
		t.Fatalf("new history item = %+v, want prompt title on refreshed session", newItem)
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

func TestHandleFeishuGroupChatStartsReactionOnlyWhenMessageIsProcessed(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = workDir
	cfg.Agents["traex"] = agent
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var started, cleaned int
	ctx := feishu.WithProcessingReactionStarter(context.Background(), func(context.Context, feishu.Message) func() {
		started++
		return func() {
			cleaned++
		}
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_group_ignored",
		ChatID:    "oc_group",
		ChatType:  "group",
		Text:      "没有 at",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group no mention) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want silent ignore", reply)
	}
	if started != 0 || cleaned != 0 {
		t.Fatalf("reaction lifecycle = started %d cleaned %d, want none for ignored message", started, cleaned)
	}

	reply, err = handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_group_processed",
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
	if started != 1 || cleaned != 1 {
		t.Fatalf("reaction lifecycle = started %d cleaned %d, want one processed message", started, cleaned)
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
		Text:      "@智能助手 /at OFF",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(@bot /at OFF) error = %v", err)
	}
	if !strings.Contains(reply, "无需 at") {
		t.Fatalf("reply = %q, want mention optional confirmation", reply)
	}
	chat, ok = store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_group"})
	if !ok || !chat.MentionOptional || chat.WikiIntervalSec != 30 || !chat.HideUsageDetail {
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

func TestHandleFeishuTopicThreadRejectsManualNewAndSessionCommands(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	rt := &fakeRuntime{newSessionID: "acp-session-1"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	base := feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_group",
		ChatType: "group",
		ThreadID: "omt_topic",
		SenderID: testOwnerOpenID,
		Mentions: []feishu.Mention{testBotMention("智能助手")},
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     base.BotID,
		ChatID:    base.ChatID,
		ChatType:  base.ChatType,
		ThreadID:  base.ThreadID,
		SenderID:  base.SenderID,
		Mentions:  base.Mentions,
		MessageID: "om_new",
		Text:      "@智能助手 /new " + workDir,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(topic /new) error = %v", err)
	}
	if !strings.Contains(reply, "话题群内不支持使用 /new") {
		t.Fatalf("reply = %q, want topic /new rejection", reply)
	}
	if len(rt.newCalls) != 0 {
		t.Fatalf("newCalls = %+v, want no manual topic session creation", rt.newCalls)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     base.BotID,
		ChatID:    base.ChatID,
		ChatType:  base.ChatType,
		ThreadID:  base.ThreadID,
		SenderID:  base.SenderID,
		Mentions:  base.Mentions,
		MessageID: "om_session",
		Text:      "@智能助手 /session list",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(topic /session) error = %v", err)
	}
	if !strings.Contains(reply, "话题群内不支持使用 /session") {
		t.Fatalf("reply = %q, want topic /session rejection", reply)
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
	ctx := feishu.WithSessionSelectionCardSender(context.Background(), func(ctx context.Context, msg feishu.Message, card feishu.SessionSelectionCard) error {
		got = card
		return nil
	})
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

func TestHandleFeishuSessionListSelectionOptionsAreLimited(t *testing.T) {
	items := make([]Session, 0, maxSessionHistoryPerChat+2)
	for i := 0; i < maxSessionHistoryPerChat+2; i++ {
		items = append(items, Session{
			Title:        fmt.Sprintf("会话%d", i),
			ACPSessionID: fmt.Sprintf("session-%d", i),
			Cwd:          "/repo",
		})
	}
	options := sessionSelectionOptions(items, maxSessionHistoryPerChat)
	if len(options) != maxSessionHistoryPerChat {
		t.Fatalf("len(options) = %d, want %d", len(options), maxSessionHistoryPerChat)
	}
	if options[0].ACPSessionID != "session-0" || options[len(options)-1].ACPSessionID != fmt.Sprintf("session-%d", maxSessionHistoryPerChat-1) {
		t.Fatalf("options = %+v, want first %d items", options, maxSessionHistoryPerChat)
	}
}

func TestHandleSessionSelectionRestoresSessionForOwnerOnly(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	rt := &fakeRuntime{newSessionIDs: []string{"acp-session-1", "acp-session-2"}}
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
		MessageID: "om_first",
		Text:      "/new " + firstDir + " 第一个",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new first) error = %v", err)
	}
	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     base.BotID,
		ChatID:    base.ChatID,
		ChatType:  base.ChatType,
		MessageID: "om_second",
		Text:      "/new " + secondDir + " 第二个",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new second) error = %v", err)
	}

	display, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:        base.BotID,
		ChatID:       base.ChatID,
		RequesterID:  testOwnerOpenID,
		OperatorID:   testOwnerOpenID,
		ACPSessionID: "acp-session-1",
	})
	if err != nil {
		t.Fatalf("HandleSessionSelection(owner) error = %v", err)
	}
	if display != "第一个" {
		t.Fatalf("display = %q, want restored title", display)
	}
	session, ok := store.Get(SessionKey{BotID: base.BotID, ChatID: base.ChatID})
	if !ok {
		t.Fatalf("current session not found")
	}
	if session.ACPSessionID != "acp-session-1" || session.Cwd != firstDir {
		t.Fatalf("session = %+v, want first session restored", session)
	}
	if len(rt.closedKeys) == 0 {
		t.Fatalf("closedKeys = %+v, want runtime closed before resume", rt.closedKeys)
	}

	if _, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:        base.BotID,
		ChatID:       base.ChatID,
		RequesterID:  testOwnerOpenID,
		OperatorID:   "ou_other",
		ACPSessionID: "acp-session-2",
	}); err == nil || !strings.Contains(err.Error(), "只有发起") {
		t.Fatalf("other requester error = %v, want requester validation", err)
	}

	if _, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:        base.BotID,
		ChatID:       base.ChatID,
		RequesterID:  "ou_other",
		OperatorID:   "ou_other",
		ACPSessionID: "acp-session-2",
	}); err == nil || !strings.Contains(err.Error(), "只有 bot owner") {
		t.Fatalf("non-owner error = %v, want owner validation", err)
	}

	if _, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:        base.BotID,
		ChatID:       base.ChatID,
		OperatorID:   testOwnerOpenID,
		ACPSessionID: "acp-session-2",
	}); err == nil || !strings.Contains(err.Error(), "缺少发起人或操作者") {
		t.Fatalf("missing requester error = %v, want requester metadata validation", err)
	}

	if _, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:        base.BotID,
		ChatID:       base.ChatID,
		RequesterID:  testOwnerOpenID,
		ACPSessionID: "acp-session-2",
	}); err == nil || !strings.Contains(err.Error(), "缺少发起人或操作者") {
		t.Fatalf("missing operator error = %v, want operator metadata validation", err)
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
	session, ok = store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_private"})
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
	session, ok = store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_private"})
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
	key := SessionKey{BotID: "bot-a", ChatID: "oc_private"}

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     key.BotID,
		ChatID:    key.ChatID,
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
		ChatID:    key.ChatID,
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
		ChatID:    key.ChatID,
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
	key := SessionKey{BotID: "bot-a", ChatID: "oc_private"}

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     key.BotID,
		ChatID:    key.ChatID,
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
		ChatID:    key.ChatID,
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
		filepath.Join("skills", "wiki", "SKILL.md"),
	} {
		if _, err := os.Stat(filepath.Join(workspace, name)); err != nil {
			t.Fatalf("workspace file %s not created: %v", name, err)
		}
	}
}

func TestHandleFeishuMessageNewDefersBootstrapContextPrompt(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
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
	svc := newTestService(config.Default(), store)
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
	if got := card.textUpdatesSnapshot(); len(got) != 2 || got[0] != "收到。现在开始。" || got[1] != "工具处理完成。" {
		t.Fatalf("textUpdates = %+v, want pre-tool text kept until final candidate replaces it", got)
	}
	if got := card.processUpdatesSnapshot(); len(got) != 3 ||
		got[0] != "💬 收到。现在开始。" ||
		got[1] != "💬 收到。现在开始。\n⏳ exec_command" ||
		got[2] != "💬 收到。现在开始。\n⏳ exec_command\n🧠 The user wants an English paragraph." {
		t.Fatalf("processUpdates = %+v, want immediate tool update and final normalized process update", got)
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
	wantTextUpdates := []string{"先检查。", "中间说明。", "最终结论。"}
	if !reflect.DeepEqual(textUpdates, wantTextUpdates) {
		t.Fatalf("textUpdates = %+v, want intermediate candidates replaced without clearing as %+v", textUpdates, wantTextUpdates)
	}
	processUpdates := cards[0].processUpdatesSnapshot()
	if len(processUpdates) == 0 {
		t.Fatalf("processUpdates = %+v, want process updates", processUpdates)
	}
	lastProcess := processUpdates[len(processUpdates)-1]
	for _, want := range []string{"💬 先检查。", "⏳ Read config", "💬 中间说明。", "⏳ Run tests"} {
		if !strings.Contains(lastProcess, want) {
			t.Fatalf("last process update = %q, want %q", lastProcess, want)
		}
	}
	if strings.Contains(lastProcess, "最终结论") {
		t.Fatalf("last process update = %q, should not include final agent text", lastProcess)
	}
}

func TestHandleFeishuMessageUpdatesStreamCardStatusBar(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptResult: acp.PromptResult{
			Text:       "完成。",
			StopReason: "end_turn",
			Raw:        json.RawMessage(`{"stopReason":"end_turn","usage":{"inputTokens":511800,"outputTokens":27600,"cachedReadTokens":496446},"_meta":{"trace":"abc"}}`),
			Usage: acp.TokenUsage{
				InputTokens:      511800,
				OutputTokens:     27600,
				CachedReadTokens: 496446,
			},
			Meta: acp.PromptResultMeta{
				TraeTokenUsage: &acp.TraeTokenUsage{
					TurnDisplay: acp.TokenUsage{
						InputTokens:  987,
						OutputTokens: 654,
					},
					ContextWindow: acp.ContextWindowUsage{
						Used: 69200,
						Size: 258400,
					},
				},
			},
		},
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "usage_update",
					Used:          69200,
					Size:          258400,
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
		Text:      "run",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want streamed reply", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	got := cards[0].statusUpdatesSnapshot()
	want := []string{
		"执行中 | 69K/258K",
		"执行中 | 511.8K(97%), 27.6K | 69K/258K",
		"已完成 | 511.8K(97%), 27.6K | 69K/258K",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statusUpdates = %+v, want %+v", got, want)
	}
	usageDetails := cards[0].usageDetailsSnapshot()
	if len(usageDetails) != 1 {
		t.Fatalf("usageDetails = %+v, want one usage detail update", usageDetails)
	}
	for _, want := range []string{"```json", `"stopReason": "end_turn"`, `"cachedReadTokens": 496446`, `"_meta": {`, `"trace": "abc"`} {
		if !strings.Contains(usageDetails[0], want) {
			t.Fatalf("usage detail = %q, want %q", usageDetails[0], want)
		}
	}
}

func TestHandleFeishuMessageCanHideStreamCardStatusBar(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	if err := store.UpsertChat(ChatConfig{
		Key:           chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: session.Key.ChatID}),
		HideStatusBar: true,
	}); err != nil {
		t.Fatalf("UpsertChat(chat) error = %v", err)
	}
	rt := &fakeRuntime{
		promptResult: acp.PromptResult{
			Text:       "完成。",
			StopReason: "end_turn",
			Usage: acp.TokenUsage{
				InputTokens:  1200,
				OutputTokens: 345,
			},
		},
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: session.ACPSessionID,
				Update: acp.SessionUpdate{
					SessionUpdate: "usage_update",
					Used:          53000,
					Size:          200000,
				},
			},
			{
				SessionID: session.ACPSessionID,
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	var statusBarEnabled *bool
	var cards []*fakeStreamCard
	ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		enabled := feishu.StreamCardStatusBarEnabled(ctx)
		statusBarEnabled = &enabled
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     session.Key.BotID,
		MessageID: "om_msg",
		ChatID:    session.Key.ChatID,
		ThreadID:  session.Key.ThreadID,
		ChatType:  "group",
		Text:      "run",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want streamed reply", reply)
	}
	if statusBarEnabled == nil || *statusBarEnabled {
		t.Fatalf("statusBarEnabled = %v, want false", statusBarEnabled)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].statusUpdatesSnapshot(); len(got) != 0 {
		t.Fatalf("statusUpdates = %+v, want none when status bar is hidden", got)
	}
}

func TestHandleFeishuMessageCanHideStreamCardUsageDetail(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	if err := store.UpsertChat(ChatConfig{
		Key:             chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: session.Key.ChatID}),
		HideUsageDetail: true,
	}); err != nil {
		t.Fatalf("UpsertChat(chat) error = %v", err)
	}
	rt := &fakeRuntime{
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
				SessionID: session.ACPSessionID,
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     session.Key.BotID,
		MessageID: "om_msg",
		ChatID:    session.Key.ChatID,
		ThreadID:  session.Key.ThreadID,
		ChatType:  "group",
		Text:      "run",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want streamed reply", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].usageDetailsSnapshot(); len(got) != 0 {
		t.Fatalf("usageDetails = %+v, want none when usage detail is hidden", got)
	}
}

func TestHandleFeishuMessageSkipsUsageDetailWithoutUsageInfo(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	rt := &fakeRuntime{
		promptResult: acp.PromptResult{
			Text:       "完成。",
			StopReason: "end_turn",
			Raw:        json.RawMessage(`{"stopReason":"end_turn"}`),
		},
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: session.ACPSessionID,
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     session.Key.BotID,
		MessageID: "om_msg",
		ChatID:    session.Key.ChatID,
		ThreadID:  session.Key.ThreadID,
		ChatType:  "group",
		Text:      "run",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want streamed reply", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].usageDetailsSnapshot(); len(got) != 0 {
		t.Fatalf("usageDetails = %+v, want none without usage info", got)
	}
}

func TestPromptResultHasUsageDetail(t *testing.T) {
	tests := []struct {
		name   string
		result acp.PromptResult
		want   bool
	}{
		{
			name: "structured usage",
			result: acp.PromptResult{Usage: acp.TokenUsage{
				InputTokens: 1,
			}},
			want: true,
		},
		{
			name: "trae meta",
			result: acp.PromptResult{Meta: acp.PromptResultMeta{
				TraeTokenUsage: &acp.TraeTokenUsage{},
			}},
			want: true,
		},
		{
			name:   "raw usage",
			result: acp.PromptResult{Raw: json.RawMessage(`{"usage":{"inputTokens":1}}`)},
			want:   true,
		},
		{
			name:   "raw meta",
			result: acp.PromptResult{Raw: json.RawMessage(`{"_meta":{"trace":"abc"}}`)},
			want:   true,
		},
		{
			name:   "no usage",
			result: acp.PromptResult{Raw: json.RawMessage(`{"stopReason":"end_turn"}`)},
			want:   false,
		},
		{
			name:   "empty usage object",
			result: acp.PromptResult{Raw: json.RawMessage(`{"usage":{}}`)},
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := promptResultHasUsageDetail(tt.result); got != tt.want {
				t.Fatalf("promptResultHasUsageDetail() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPromptStatusBarUsesMetaOnlyAsFallback(t *testing.T) {
	status := promptStatusBar{state: promptStatusRunning}
	status.Context = acp.ContextWindowUsage{Used: 69200, Size: 258400}
	status.applyPromptResult(acp.PromptResult{
		Usage: acp.TokenUsage{
			InputTokens:      511800,
			CachedReadTokens: 496446,
		},
		Meta: acp.PromptResultMeta{
			TraeTokenUsage: &acp.TraeTokenUsage{
				TurnDisplay: acp.TokenUsage{
					InputTokens:  987,
					OutputTokens: 356,
				},
				ContextWindow: acp.ContextWindowUsage{Used: 99000, Size: 300000},
			},
		},
	})

	got := status.text()
	want := "执行中 | 511.8K(97%), 356 | 69K/258K"
	if got != want {
		t.Fatalf("status text = %q, want %q", got, want)
	}
}

func TestPromptStatusBarUsesMillionUnit(t *testing.T) {
	status := promptStatusBar{
		state:       promptStatusCompleted,
		input:       2_908_700,
		cachedInput: 2_763_265,
		output:      1_200_000,
		Context:     acp.ContextWindowUsage{Used: 2_908_700, Size: 4_096_000},
	}

	got := status.text()
	want := "已完成 | 2.9M(95%), 1.2M | 2M/4M"
	if got != want {
		t.Fatalf("status text = %q, want %q", got, want)
	}
}

func TestPromptStatusBarOmitsMissingTokenUsage(t *testing.T) {
	tests := []struct {
		name string
		bar  promptStatusBar
		want string
	}{
		{
			name: "output only",
			bar:  promptStatusBar{state: promptStatusCompleted, output: 1000},
			want: "已完成 | 1K",
		},
		{
			name: "input only without cache rate",
			bar:  promptStatusBar{state: promptStatusCompleted, input: 1000},
			want: "已完成 | 1K",
		},
		{
			name: "no usage",
			bar:  promptStatusBar{state: promptStatusCompleted},
			want: "已完成",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.bar.text(); got != tt.want {
				t.Fatalf("status text = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPromptStatusBarCancelledStopReason(t *testing.T) {
	status := promptStatusBar{state: promptStatusRunning}
	status.applyPromptResult(acp.PromptResult{
		StopReason: "cancelled",
		Usage: acp.TokenUsage{
			InputTokens:  1200,
			OutputTokens: 345,
		},
	})
	status.state = promptStatusFromStopReason("cancelled")
	status.stopReason = "cancelled"
	if got, want := status.text(), "已取消 | 1.2K, 345"; got != want {
		t.Fatalf("status text = %q, want %q", got, want)
	}
}

func TestHandleFeishuMessageMarksCancelledStopReasonInStreamCardStatus(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptResult: acp.PromptResult{
			Text:       "已取消。",
			StopReason: "cancelled",
			Usage: acp.TokenUsage{
				InputTokens: 1200,
			},
		},
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "已取消。"},
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
		t.Fatalf("reply = %q, want streamed reply", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	got := cards[0].statusUpdatesSnapshot()
	if len(got) == 0 || got[len(got)-1] != "已取消 | 1.2K" {
		t.Fatalf("statusUpdates = %+v, want final cancelled status", got)
	}
}

func TestFormatPromptResultDetailEscapesCodeFence(t *testing.T) {
	got := formatPromptResultDetail(acp.PromptResult{
		Raw: json.RawMessage("{\"message\":\"contains ``` fence\"}"),
	})
	if !strings.HasPrefix(got, "````json\n") || !strings.HasSuffix(got, "\n````") {
		t.Fatalf("detail = %q, want four-backtick fence", got)
	}
	if !strings.Contains(got, "contains ``` fence") {
		t.Fatalf("detail = %q, want raw content", got)
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
		t.Fatalf("processUpdates = %+v, want generic chunk stream to update once within throttle window", got)
	}
	if got[0] != "line one line two" {
		t.Fatalf("process update = %q, want final accumulated generic chunk stream", got[0])
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

func TestHandleFeishuMessageShowOptionsFilterProcessUpdates(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*ChatConfig)
		want     []string
		unwanted []string
	}{
		{
			name: "hide step",
			mutate: func(chat *ChatConfig) {
				chat.HideStepMessages = true
			},
			want:     []string{"⏳ Run tests", "🧠 Thinking"},
			unwanted: []string{"💬 准备处理", "step chunk"},
		},
		{
			name: "hide thought",
			mutate: func(chat *ChatConfig) {
				chat.HideThoughts = true
			},
			want:     []string{"💬 准备处理", "⏳ Run tests", "💬 step chunk"},
			unwanted: []string{"🧠 Thinking"},
		},
		{
			name: "hide tool",
			mutate: func(chat *ChatConfig) {
				chat.HideTools = true
			},
			want:     []string{"💬 准备处理", "🧠 Thinking", "💬 step chunk"},
			unwanted: []string{"Run tests", "tool output"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
			session := testReadySession(t, store)
			chat := ChatConfig{Key: chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: session.Key.ChatID})}
			tt.mutate(&chat)
			if err := store.UpsertChat(chat); err != nil {
				t.Fatalf("UpsertChat(chat) error = %v", err)
			}
			rt := &fakeRuntime{
				promptReply: "完成。",
				promptUpdates: []acp.PromptUpdate{
					{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
						SessionUpdate: "status",
						Message:       "准备处理",
					}},
					{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
						SessionUpdate: "reasoning",
						Message:       "Thinking",
					}},
					{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
						SessionUpdate: "tool_call",
						Title:         "Run tests",
					}},
					{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
						SessionUpdate: "tool_call_output_chunk",
						Content:       &acp.ContentBlock{Type: "text", Text: "tool output"},
					}},
					{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
						SessionUpdate: "progress_chunk",
						Content:       &acp.ContentBlock{Type: "text", Text: "step chunk"},
					}},
					{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
						SessionUpdate: "agent_message_chunk",
						Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
					}},
				},
			}
			svc := newTestService(config.Default(), store)
			svc.setRuntime(rt)
			var cards []*fakeStreamCard
			ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
				card := &fakeStreamCard{}
				cards = append(cards, card)
				return card, nil
			})

			reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
				BotID:     session.Key.BotID,
				MessageID: "om_msg",
				ChatID:    session.Key.ChatID,
				ThreadID:  session.Key.ThreadID,
				ChatType:  "group",
				Text:      "run",
				Mentions:  []feishu.Mention{testBotMention("智能助手")},
			})
			if err != nil {
				t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
			}
			if reply != "" {
				t.Fatalf("reply = %q, want streamed reply", reply)
			}
			if len(cards) != 1 {
				t.Fatalf("cards = %+v, want one stream card", cards)
			}
			process := strings.Join(cards[0].processUpdatesSnapshot(), "\n")
			for _, want := range tt.want {
				if !strings.Contains(process, want) {
					t.Fatalf("process updates = %q, want %q", process, want)
				}
			}
			for _, unwanted := range tt.unwanted {
				if strings.Contains(process, unwanted) {
					t.Fatalf("process updates = %q, should not contain %q", process, unwanted)
				}
			}
		})
	}
}

func TestHandleFeishuMessageShowOptionsCanHideWholeProcessPanel(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	if err := store.UpsertChat(ChatConfig{
		Key:              chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: session.Key.ChatID}),
		HideStepMessages: true,
		HideThoughts:     true,
		HideTools:        true,
	}); err != nil {
		t.Fatalf("UpsertChat(chat) error = %v", err)
	}
	rt := &fakeRuntime{
		promptReply: "完成。",
		promptUpdates: []acp.PromptUpdate{
			{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
				SessionUpdate: "status",
				Message:       "准备处理",
			}},
			{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
				SessionUpdate: "reasoning",
				Message:       "Thinking",
			}},
			{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
				SessionUpdate: "tool_call",
				Title:         "Run tests",
			}},
			{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
			}},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	var processPanelEnabled *bool
	var cards []*fakeStreamCard
	ctx := feishu.WithStreamCardStarter(context.Background(), func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		enabled := feishu.StreamCardProcessPanelEnabled(ctx)
		processPanelEnabled = &enabled
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     session.Key.BotID,
		MessageID: "om_msg",
		ChatID:    session.Key.ChatID,
		ThreadID:  session.Key.ThreadID,
		ChatType:  "group",
		Text:      "run",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want streamed reply", reply)
	}
	if processPanelEnabled == nil || *processPanelEnabled {
		t.Fatalf("processPanelEnabled = %v, want false when all process classes are hidden", processPanelEnabled)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card for final text", cards)
	}
	if got := cards[0].processUpdatesSnapshot(); len(got) != 0 {
		t.Fatalf("processUpdates = %+v, want none", got)
	}
}

func TestFormatPromptUpdatePrefixesProcessMessageOnly(t *testing.T) {
	tests := []struct {
		name   string
		update acp.PromptUpdate
		want   string
	}{
		{
			name: "process message",
			update: acp.PromptUpdate{Update: acp.SessionUpdate{
				SessionUpdate: "status",
				Message:       "准备处理",
			}},
			want: "💬 准备处理",
		},
		{
			name: "agent message",
			update: acp.PromptUpdate{Update: acp.SessionUpdate{
				SessionUpdate: "agent_message",
				Message:       "先说明一下",
			}},
			want: "💬 先说明一下",
		},
		{
			name: "thought message",
			update: acp.PromptUpdate{Update: acp.SessionUpdate{
				SessionUpdate: "reasoning",
				Message:       "分析用户需求",
			}},
			want: "🧠 分析用户需求",
		},
		{
			name: "agent chunk stays final text candidate",
			update: acp.PromptUpdate{Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "正文"},
			}},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatPromptUpdate(tt.update); got != tt.want {
				t.Fatalf("formatPromptUpdate() = %q, want %q", got, tt.want)
			}
		})
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
	svc := newTestService(config.Default(), store)
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
	}, Session{ACPSessionID: "acp-session-1"}, ChatConfig{})

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
	}, Session{ACPSessionID: "acp-session-1"}, ChatConfig{})

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

func TestPromptCardStreamThrottlesProcessUpdatesUntilClose(t *testing.T) {
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
	}, Session{ACPSessionID: "acp-session-1"}, ChatConfig{})

	stream.updateProcess("one")
	stream.updateProcess("two")

	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].processUpdatesSnapshot(); len(got) != 1 || got[0] != "one" {
		t.Fatalf("processUpdates = %+v, want second process update throttled", got)
	}
	stream.close()
	if got := cards[0].processUpdatesSnapshot(); len(got) != 2 || got[1] != "one\ntwo" {
		t.Fatalf("processUpdates = %+v, want pending process flushed on close", got)
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
	}, Session{ACPSessionID: "acp-session-1"}, ChatConfig{})
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
	svc := newTestService(config.Default(), store)
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
	svc := newTestService(config.Default(), store)
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
	svc := newTestService(config.Default(), store)
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
	svc := newTestService(config.Default(), store)
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
	svc := newTestService(config.Default(), store)
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
	svc := newTestService(config.Default(), store)
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
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = defaultDir
	cfg.Agents["traex"] = agent
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
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = t.TempDir()
	cfg.Agents["traex"] = agent
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
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = t.TempDir()
	cfg.Agents["traex"] = agent
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
	svc := newTestService(config.Default(), store)
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
	if !ok || session.WikiIntervalSec != 0 {
		t.Fatalf("session = %+v, want wiki interval not stored on session", session)
	}
	chat, ok := store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_chat"})
	if !ok || chat.WikiIntervalSec != 1 {
		t.Fatalf("chat config = %+v, %v; want wiki interval persisted", chat, ok)
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
	if !ok || session.WikiDisabled {
		t.Fatalf("session = %+v, want wiki disabled not stored on session", session)
	}
	chat, ok = store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_chat"})
	if !ok || !chat.WikiDisabled || chat.WikiIntervalSec != 1 {
		t.Fatalf("chat config = %+v, %v; want wiki disabled and interval persisted", chat, ok)
	}
}

func TestHandleWikiCommandRejectsSubSecondInterval(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	msg := feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		Text:      "/wiki interval 1ms",
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki interval 1ms) error = %v", err)
	}
	if !strings.Contains(reply, "不能小于 1s") {
		t.Fatalf("reply = %q, want sub-second rejection", reply)
	}
	if chat, ok := store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_chat"}); ok || chat.WikiIntervalSec != 0 {
		t.Fatalf("chat config = %+v, %v; want interval not persisted", chat, ok)
	}
}

func TestHandleWikiCommandSurvivesNewSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = t.TempDir()
	cfg.Agents["traex"] = agent
	rt := &fakeRuntime{newSessionID: "acp-session-new"}
	svc := NewService(cfg, store)
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
		Text:     "/wiki interval 1s",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki interval) error = %v", err)
	}
	if !strings.Contains(reply, "1s") {
		t.Fatalf("reply = %q, want wiki interval confirmation", reply)
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
	if !strings.Contains(reply, "已为当前会话创建 ACP 会话") {
		t.Fatalf("reply = %q, want new session confirmation", reply)
	}
	session, ok := store.Get(SessionKey{BotID: msg.BotID, ChatID: msg.ChatID})
	if !ok || session.WikiIntervalSec != 0 || session.WikiDisabled {
		t.Fatalf("session = %+v, %v; wiki options should not be stored on session", session, ok)
	}
	chat, ok := store.GetChat(ChatKey{BotID: msg.BotID, ChatID: msg.ChatID})
	if !ok || chat.WikiIntervalSec != 1 || chat.WikiDisabled {
		t.Fatalf("chat config = %+v, %v; want wiki interval to survive /new", chat, ok)
	}
	svc.scheduleWikiAfterUserPrompt(session, config.Default().Agents["traex"])
	t.Cleanup(func() { svc.cancelWikiTimer(session.Key) })
	svc.taskMu.Lock()
	_, hasTimer := svc.wikiTimers[session.Key]
	svc.taskMu.Unlock()
	if !hasTimer {
		t.Fatal("wiki timer should use chat-level interval after /new")
	}
}

func TestNewSessionRunsPendingWikiReflectionWithRuntimeKey(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = t.TempDir()
	cfg.Agents["traex"] = agent
	rt := &fakeRuntime{
		newSessionID:     "acp-session-new",
		promptReply:      "NoReply",
		blockWikiRuntime: make(chan struct{}),
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	oldSession := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-old",
		Cwd:             t.TempDir(),
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		WikiIntervalSec: 60,
	}
	if err := store.Upsert(oldSession); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc.scheduleWikiAfterUserPrompt(oldSession, agent)
	t.Cleanup(func() {
		svc.cancelWikiTimer(key)
		close(rt.blockWikiRuntime)
	})

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "session：acp-session-new") {
		t.Fatalf("reply = %q, want new session reply without waiting for wiki runtime", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.wikiRuntimeCallCount() == 1 })
	rt.mu.Lock()
	if len(rt.wikiRuntimeCalls) != 1 {
		t.Fatalf("wikiRuntimeCalls = %+v, want one old wiki reflection", rt.wikiRuntimeCalls)
	}
	wikiCall := rt.wikiRuntimeCalls[0]
	rt.mu.Unlock()
	if wikiCall.Session.ACPSessionID != "acp-session-old" {
		t.Fatalf("wiki runtime session = %q, want old acp session", wikiCall.Session.ACPSessionID)
	}
	if wikiCall.Runtime.Scope != runtimeScopeWiki {
		t.Fatalf("wiki runtime scope = %q, want wiki", wikiCall.Runtime.Scope)
	}
	if wikiCall.Runtime.SessionKey != key {
		t.Fatalf("wiki runtime session key = %+v, want %+v", wikiCall.Runtime.SessionKey, key)
	}
	if !strings.Contains(wikiCall.Text, "请对刚才的对话进行反思") {
		t.Fatalf("wiki runtime prompt = %q, want wiki reflection prompt", wikiCall.Text)
	}
	if got := rt.promptCallCount(); got != 0 {
		t.Fatalf("prompt calls = %d, want no normal prompt for wiki runtime", got)
	}
	svc.taskMu.Lock()
	_, hasTimer := svc.wikiTimers[key]
	status := svc.wikiStatuses[key]
	svc.taskMu.Unlock()
	if hasTimer {
		t.Fatal("pending wiki timer should be consumed by /new")
	}
	if !status.running {
		t.Fatalf("wiki status = %+v, want wiki runtime reflection running while blocked", status)
	}
}

func TestNewSessionRuntimeFailureRestoresPendingWiki(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = t.TempDir()
	cfg.Agents["traex"] = agent
	rt := &fakeRuntime{
		newSessionError: errors.New("boom"),
		promptReply:     "NoReply",
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	oldSession := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-old",
		Cwd:             t.TempDir(),
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		WikiIntervalSec: 60,
	}
	if err := store.Upsert(oldSession); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc.scheduleWikiAfterUserPrompt(oldSession, agent)
	t.Cleanup(func() { svc.cancelWikiTimer(key) })

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "创建 ACP session 失败") {
		t.Fatalf("reply = %q, want new session error", reply)
	}
	svc.taskMu.Lock()
	_, hasTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if !hasTimer {
		t.Fatal("failed /new should restore pending wiki timer")
	}
	if got := rt.wikiRuntimeCallCount(); got != 0 {
		t.Fatalf("wiki runtime calls = %d, want none when /new fails", got)
	}
}

func TestNewSessionInvalidRequestKeepsPendingWiki(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = ""
	cfg.Agents["traex"] = agent
	rt := &fakeRuntime{newSessionID: "acp-session-new", promptReply: "NoReply"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	oldSession := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-old",
		Cwd:             t.TempDir(),
		WikiIntervalSec: 60,
	}
	svc.scheduleWikiAfterUserPrompt(oldSession, agent)
	t.Cleanup(func() { svc.cancelWikiTimer(key) })

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new invalid) error = %v", err)
	}
	if !strings.Contains(reply, "默认 agent 未配置 default_cwd") {
		t.Fatalf("reply = %q, want missing cwd warning", reply)
	}
	svc.taskMu.Lock()
	_, hasTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if !hasTimer {
		t.Fatal("invalid /new should keep pending wiki timer")
	}
	if got := rt.wikiRuntimeCallCount(); got != 0 {
		t.Fatalf("wiki runtime calls = %d, want none for invalid /new", got)
	}
}

func TestWikiOffCancelsRunningWikiRuntimeReflection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	agent := cfg.Agents["traex"]
	agent.DefaultCwd = t.TempDir()
	cfg.Agents["traex"] = agent
	rt := &fakeRuntime{
		newSessionID:     "acp-session-new",
		promptReply:      "NoReply",
		blockWikiRuntime: make(chan struct{}),
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	oldSession := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-old",
		Cwd:             t.TempDir(),
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		WikiIntervalSec: 60,
	}
	if err := store.Upsert(oldSession); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc.scheduleWikiAfterUserPrompt(oldSession, agent)
	t.Cleanup(func() {
		svc.cancelSessionWork(context.Background(), key)
		close(rt.blockWikiRuntime)
	})

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/new",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool { return rt.wikiRuntimeCallCount() == 1 })
	rt.mu.Lock()
	wikiKey := rt.wikiRuntimeCalls[0].Runtime
	rt.mu.Unlock()

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/wiki off",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki off) error = %v", err)
	}
	if !strings.Contains(reply, "已关闭当前聊天的自动知识沉淀") {
		t.Fatalf("reply = %q, want wiki disabled", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() == 1 })
	rt.mu.Lock()
	cancelCall := rt.cancelCalls[0]
	closedKeys := append([]runtimeKey(nil), rt.closedRuntimeKeys...)
	rt.mu.Unlock()
	if cancelCall.Runtime != wikiKey {
		t.Fatalf("cancel runtime = %+v, want %+v", cancelCall.Runtime, wikiKey)
	}
	if len(closedKeys) == 0 || closedKeys[0] != wikiKey {
		t.Fatalf("closed runtime keys = %+v, want first %+v", closedKeys, wikiKey)
	}
}

func TestWikiStatusDoesNotCancelScheduledReflection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
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
	svc := newTestService(config.Default(), store)
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
	if !ok || session.WikiIntervalSec != 60 {
		t.Fatalf("session = %+v, want legacy session wiki interval unchanged", session)
	}
	chat, ok := store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_chat"})
	if !ok || chat.WikiIntervalSec != 1 {
		t.Fatalf("chat config = %+v, %v; want wiki interval persisted", chat, ok)
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
	svc := newTestService(config.Default(), store)
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
	statusCancelled := false
	for _, update := range cards[0].statusUpdatesSnapshot() {
		if strings.Contains(update, "已取消") {
			statusCancelled = true
			break
		}
	}
	if !statusCancelled {
		t.Fatalf("status updates = %+v, want cancelled status", cards[0].statusUpdatesSnapshot())
	}
	if !cards[0].isClosed() {
		t.Fatal("cancelled old card should be closed")
	}
	session, ok := store.Get(key)
	if !ok {
		t.Fatalf("session not found after cancellation")
	}
	if session.Title != "改成做这个" {
		t.Fatalf("session title = %q, want second prompt title", session.Title)
	}
}

func TestHandleFeishuMessageReadOnlyCommandDoesNotCancelInFlightPrompt(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptReply:   "ACP 回复",
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := newTestService(config.Default(), store)
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

func TestParseLoopRequest(t *testing.T) {
	req, err := parseLoopRequest("/loop -t 30m -n 3 -i 2s 持续推进")
	if err != nil {
		t.Fatalf("parseLoopRequest() error = %v", err)
	}
	if req.MaxDuration != 30*time.Minute || req.MaxRounds != 3 || req.Interval != 2*time.Second || req.Prompt != "持续推进" {
		t.Fatalf("request = %+v, want parsed loop options", req)
	}

	req, err = parseLoopRequest("/loop -t 0 -n 0 默认间隔")
	if err != nil {
		t.Fatalf("parseLoopRequest(unlimited) error = %v", err)
	}
	if req.MaxDuration != 0 || req.MaxRounds != 0 || req.Interval != defaultLoopInterval || req.Prompt != "默认间隔" {
		t.Fatalf("request = %+v, want unlimited with default interval", req)
	}

	req, err = parseLoopRequest("/loop -n 1 请保留  多空格\n以及换行")
	if err != nil {
		t.Fatalf("parseLoopRequest(spaced prompt) error = %v", err)
	}
	if req.Prompt != "请保留  多空格\n以及换行" {
		t.Fatalf("prompt = %q, want original spacing", req.Prompt)
	}

	if _, err := parseLoopRequest("/loop -i 0 提示词"); err == nil || !strings.Contains(err.Error(), "-i 必须大于 0") {
		t.Fatalf("parseLoopRequest(-i 0) error = %v, want interval validation", err)
	}
	if _, err := parseLoopRequest("/loop -x 提示词"); err == nil || !strings.Contains(err.Error(), "未知 loop 参数") {
		t.Fatalf("parseLoopRequest(-x) error = %v, want unknown option", err)
	}
	if _, err := parseLoopRequest("/loop -n 1"); err == nil || !strings.Contains(err.Error(), "提示词必填") {
		t.Fatalf("parseLoopRequest(no prompt) error = %v, want required prompt", err)
	}
}

func TestLoopDone(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "plain", text: "DONE", want: true},
		{name: "space", text: " \nDONE\t", want: true},
		{name: "inline code not accepted", text: "`DONE`"},
		{name: "plain fenced not accepted", text: "```\nDONE\n```"},
		{name: "typed fenced not accepted", text: "```text\nDONE\n```"},
		{name: "lowercase not accepted", text: "done"},
		{name: "extra text not accepted", text: "DONE\n继续"},
		{name: "sentence not accepted", text: "DONE，已完成"},
		{name: "typed fenced extra text not accepted", text: "```text\nDONE\n继续\n```"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := loopDone(tt.text); got != tt.want {
				t.Fatalf("loopDone(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestLoopCommandRunsUntilDoneAndUpdatesStartCard(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptResults: []acp.PromptResult{{Text: "继续"}, {Text: "DONE"}}}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	client := newFakeSentMessageClient("om_loop_start")
	var intermediate []string
	ctx := withFakeSentMessageClient(context.Background(), client)
	ctx = feishu.WithIntermediateReplySender(ctx, func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate = append(intermediate, text)
		return nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop -n 5 -i 1ms 持续推进这个目标",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after sending loop start message", reply)
	}
	sent := client.sentSnapshot()
	if len(sent) != 1 || !strings.Contains(sent[0], "已启动 loop") || !strings.Contains(sent[0], "最大轮次：5") {
		t.Fatalf("sent = %#v, want loop start confirmation", sent)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 2 })
	waitForCondition(t, time.Second, func() bool {
		updates := client.updatesSnapshot()
		return len(updates) > 0 && strings.Contains(updates[len(updates)-1], "状态：已完成") && strings.Contains(updates[len(updates)-1], "结束原因：agent 返回 DONE")
	})
	if len(intermediate) != 0 {
		t.Fatalf("intermediate = %#v, want no separate finish reply", intermediate)
	}
	updates := client.updatesSnapshot()
	if !containsStringWithAll(updates, "状态：第 1 轮运行中", "更新时间：") {
		t.Fatalf("updates = %#v, want round 1 running update", updates)
	}
	if !containsStringWithAll(updates, "状态：第 2 轮已完成", "更新时间：") {
		t.Fatalf("updates = %#v, want round 2 completed update", updates)
	}
	for _, id := range client.updateIDsSnapshot() {
		if id != "om_loop_start" {
			t.Fatalf("update message id = %q, want loop start card message", id)
		}
	}
	if textUpdates := client.textUpdatesSnapshot(); len(textUpdates) != 0 {
		t.Fatalf("text updates = %#v, want loop status card updates only", textUpdates)
	}
	rt.mu.Lock()
	calls := append([]fakePromptCall(nil), rt.promptCalls...)
	rt.mu.Unlock()
	if !strings.Contains(calls[0].Text, "## Loop Metadata") || !strings.Contains(calls[0].Text, "round: 1") || !strings.Contains(calls[0].Text, "## Loop Stop Rules") {
		t.Fatalf("first loop prompt = %q, want loop metadata and stop rules", calls[0].Text)
	}
	if !strings.Contains(calls[1].Text, "round: 2") {
		t.Fatalf("second loop prompt = %q, want round 2 metadata", calls[1].Text)
	}
	svc.taskMu.Lock()
	status := svc.loopStatuses[key]
	svc.taskMu.Unlock()
	if status.running || status.round != 2 || status.reason != "agent 返回 DONE" {
		t.Fatalf("loop status = %+v, want completed at round 2 by DONE", status)
	}
}

func TestLoopCommandStopsWhenDoneComesFromStreamChunk(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "DONE"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	client := newFakeSentMessageClient("om_loop_start")
	var intermediate []string
	var streamMsgs []feishu.Message
	ctx := withFakeSentMessageClient(context.Background(), client)
	ctx = feishu.WithIntermediateReplySender(ctx, func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate = append(intermediate, text)
		return nil
	})
	ctx = feishu.WithStreamCardStarter(ctx, func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		streamMsgs = append(streamMsgs, msg)
		return &fakeStreamCard{}, nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop -n 3 -i 1ms 等待流式 DONE",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after sending loop start message", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })
	waitForCondition(t, time.Second, func() bool {
		updates := client.updatesSnapshot()
		return len(updates) > 0 && strings.Contains(updates[len(updates)-1], "结束原因：agent 返回 DONE")
	})
	if len(intermediate) != 0 {
		t.Fatalf("intermediate = %#v, want no separate finish reply", intermediate)
	}
	if len(streamMsgs) != 1 || streamMsgs[0].MessageID != "om_loop_start" || !streamMsgs[0].ForceReplyInThread {
		t.Fatalf("stream messages = %+v, want thread reply to loop start message", streamMsgs)
	}
	if textUpdates := client.textUpdatesSnapshot(); len(textUpdates) != 0 {
		t.Fatalf("text updates = %#v, want loop status card updates only", textUpdates)
	}
	svc.taskMu.Lock()
	status := svc.loopStatuses[key]
	svc.taskMu.Unlock()
	if status.running || status.round != 1 || status.reason != "agent 返回 DONE" {
		t.Fatalf("loop status = %+v, want completed at round 1 by streamed DONE", status)
	}
}

func TestLoopCommandStopsWhenFinalCardTextIsDoneAfterProcessMessages(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
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
					Title:         "Run tests",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "DONE"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	client := newFakeSentMessageClient("om_loop_start")
	ctx := withFakeSentMessageClient(context.Background(), client)
	var cards []*fakeStreamCard
	ctx = feishu.WithStreamCardStarter(ctx, func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})

	if reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop -n 3 -i 1ms 等待流式 DONE",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	} else if reply != "" {
		t.Fatalf("reply = %q, want empty after sending loop start message", reply)
	}

	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })
	waitForCondition(t, time.Second, func() bool {
		updates := client.updatesSnapshot()
		return len(updates) > 0 && strings.Contains(updates[len(updates)-1], "结束原因：agent 返回 DONE")
	})
	svc.taskMu.Lock()
	status := svc.loopStatuses[key]
	svc.taskMu.Unlock()
	if status.running || status.round != 1 || status.reason != "agent 返回 DONE" {
		t.Fatalf("loop status = %+v, want completed at round 1 by final card DONE", status)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	textUpdates := cards[0].textUpdatesSnapshot()
	if len(textUpdates) == 0 || textUpdates[len(textUpdates)-1] != "DONE" {
		t.Fatalf("textUpdates = %+v, want final card text DONE", textUpdates)
	}
}

func TestLoopRoundCardsReplyToStartMessageInThread(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "DONE"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	client := newFakeSentMessageClient("om_loop_start")
	var streamMsgs []feishu.Message
	ctx := withFakeSentMessageClient(context.Background(), client)
	ctx = feishu.WithStreamCardStarter(ctx, func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		streamMsgs = append(streamMsgs, msg)
		return &fakeStreamCard{}, nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "group",
		Text:     "@智能助手 /loop -n 3 -i 1ms 等待流式 DONE",
		Mentions: []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after sending loop start message", reply)
	}
	startMsgs := client.messagesSnapshot()
	if len(startMsgs) != 1 {
		t.Fatalf("start messages = %+v, want one loop start card message", startMsgs)
	}
	if startMsgs[0].ForceReplyInThread {
		t.Fatalf("start message = %+v, want loop start card to reply normally to user message", startMsgs[0])
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })
	if len(streamMsgs) != 1 {
		t.Fatalf("stream messages = %+v, want one card", streamMsgs)
	}
	if streamMsgs[0].MessageID != "om_loop_start" || !streamMsgs[0].ForceReplyInThread {
		t.Fatalf("stream message = %+v, want card reply to loop start message in thread", streamMsgs[0])
	}
}

func TestUpdateLoopAnchorIgnoresCanceledParentContext(t *testing.T) {
	svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
	client := newFakeSentMessageClient("om_loop_start")
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	anchor := loopAnchor{
		message: feishu.Message{MessageID: "om_loop_start"},
		request: loopRequest{Prompt: "持续推进", Interval: time.Second},
		card: &fakeLoopStatusCard{
			client:                client,
			message:               feishu.SentMessage{MessageID: "om_loop_start"},
			failOnCanceledContext: true,
		},
	}

	if !svc.updateLoopAnchor(parent, anchor, loopProgressFinished, 0, "agent 返回 DONE") {
		t.Fatal("updateLoopAnchor() = false, want update with detached context")
	}
	finishes := client.finishesSnapshot()
	if len(finishes) != 1 || !strings.Contains(finishes[0], "状态：已完成") || !strings.Contains(finishes[0], "结束原因：agent 返回 DONE") {
		t.Fatalf("finishes = %#v, want completed finish update", finishes)
	}
}

func TestLoopCommandStopsAtMaxRounds(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptReply: "继续"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	client := newFakeSentMessageClient("om_loop_start")
	var intermediate []string
	ctx := withFakeSentMessageClient(context.Background(), client)
	ctx = feishu.WithIntermediateReplySender(ctx, func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate = append(intermediate, text)
		return nil
	})

	if _, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop -n 2 -i 1ms 做两轮",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 2 })
	waitForCondition(t, time.Second, func() bool {
		updates := client.updatesSnapshot()
		return len(updates) > 0 && strings.Contains(updates[len(updates)-1], "结束原因：已达到最大轮次")
	})
	if len(intermediate) != 0 {
		t.Fatalf("intermediate = %#v, want no separate finish reply", intermediate)
	}
	if textUpdates := client.textUpdatesSnapshot(); len(textUpdates) != 0 {
		t.Fatalf("text updates = %#v, want loop status card updates only", textUpdates)
	}
	svc.taskMu.Lock()
	status := svc.loopStatuses[key]
	svc.taskMu.Unlock()
	if status.running || status.round != 2 || status.reason != "已达到最大轮次" {
		t.Fatalf("loop status = %+v, want completed by max rounds", status)
	}
}

func TestNewMessageCancelsRunningLoop(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptReply:   "ACP 回复",
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := newFakeSentMessageClient("om_loop_start")
	ctx := withFakeSentMessageClient(context.Background(), client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop -n 0 -i 1ms 一直推进",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after sending loop start message", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	secondReply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "改成处理这个",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(user) error = %v", err)
	}
	if secondReply != "ACP 回复" {
		t.Fatalf("second reply = %q, want user prompt reply", secondReply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() >= 1 })
	close(rt.blockPrompt)
	if rt.promptCallCount() != 2 {
		t.Fatalf("prompt calls = %d, want loop prompt and replacement user prompt", rt.promptCallCount())
	}
	updates := client.updatesSnapshot()
	if !containsStringWithAll(updates, "状态：已完成", "结束原因：已取消") {
		t.Fatalf("updates = %#v, want cancelled loop start message update", updates)
	}
	if textUpdates := client.textUpdatesSnapshot(); len(textUpdates) != 0 {
		t.Fatalf("text updates = %#v, want loop status card updates only", textUpdates)
	}
	svc.taskMu.Lock()
	status := svc.loopStatuses[key]
	svc.taskMu.Unlock()
	if status.running || status.reason != "已取消" {
		t.Fatalf("loop status = %+v, want cancelled by new message", status)
	}
}

func TestLoopStopCancelsRunningLoopAndStatusReportsManualStop(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptReply:   "继续",
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := newFakeSentMessageClient("om_loop_start")
	ctx := withFakeSentMessageClient(context.Background(), client)

	if _, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop -n 0 -i 1ms 长循环",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop stop",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop stop) error = %v", err)
	}
	if reply != "已停止当前会话的 loop。" {
		t.Fatalf("reply = %q, want stop confirmation", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() >= 1 })
	updates := client.updatesSnapshot()
	if !containsStringWithAll(updates, "状态：已完成", "结束原因：已手动停止") {
		t.Fatalf("updates = %#v, want manual stop update", updates)
	}
	if textUpdates := client.textUpdatesSnapshot(); len(textUpdates) != 0 {
		t.Fatalf("text updates = %#v, want loop status card updates only", textUpdates)
	}
	close(rt.blockPrompt)

	statusReply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop status) error = %v", err)
	}
	if !strings.Contains(statusReply, "状态：已结束") || !strings.Contains(statusReply, "原因：已手动停止") {
		t.Fatalf("status reply = %q, want manual stop status", statusReply)
	}
}

func TestHandleLoopCancelAllowsOwnerAndCancelsRunningLoop(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{}
	svc := newTestService(config.Default(), store)
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
	taskCtx, finish := svc.startTask(context.Background(), session, config.AgentConfig{}, taskKindLoop)
	defer finish()
	svc.taskMu.Lock()
	svc.loopStatuses[key] = loopRunStatus{running: true, started: time.Now()}
	svc.taskMu.Unlock()

	display, err := svc.HandleLoopCancel(context.Background(), feishu.LoopCancel{
		BotID:        "bot-a",
		ChatID:       "oc_chat",
		ThreadID:     "omt_thread",
		ACPSessionID: "acp-session-1",
		OperatorID:   testOwnerOpenID,
	})
	if err != nil {
		t.Fatalf("HandleLoopCancel() error = %v", err)
	}
	if !strings.Contains(display, "loop 已结束") || !strings.Contains(display, "结束原因：已通过卡片取消") {
		t.Fatalf("display = %q, want finished cancel text", display)
	}
	select {
	case <-taskCtx.Done():
	default:
		t.Fatal("loop task context was not cancelled")
	}
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() >= 1 })
	svc.taskMu.Lock()
	status := svc.loopStatuses[key]
	_, stillRunning := svc.tasks[key]
	svc.taskMu.Unlock()
	if stillRunning || status.running || status.reason != "已通过卡片取消" {
		t.Fatalf("task stillRunning=%v status=%+v, want cancelled loop", stillRunning, status)
	}
}

func TestHandleLoopCancelUpdatesRunningRoundCardWithDetachedContext(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "正在执行\n"},
				},
			},
		},
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := newTestService(config.Default(), store)
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

	client := newFakeSentMessageClient("om_loop_start")
	var cards []*fakeStreamCard
	ctx := withFakeSentMessageClient(context.Background(), client)
	ctx = feishu.WithStreamCardStarter(ctx, func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{failOnCanceledContext: true}
		cards = append(cards, card)
		return card, nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_user",
		ChatID:    "oc_chat",
		ChatType:  "group",
		ThreadID:  "omt_thread",
		Text:      "@智能助手 /loop -n 0 -i 1ms 长循环",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after sending loop start message", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 && len(cards) == 1 })

	display, err := svc.HandleLoopCancel(context.Background(), feishu.LoopCancel{
		BotID:        "bot-a",
		ChatID:       "oc_chat",
		ThreadID:     "omt_thread",
		ACPSessionID: "acp-session-1",
		OperatorID:   testOwnerOpenID,
	})
	if err != nil {
		t.Fatalf("HandleLoopCancel() error = %v", err)
	}
	if !strings.Contains(display, "结束原因：已通过卡片取消") {
		t.Fatalf("display = %q, want card cancel reason", display)
	}
	waitForCondition(t, time.Second, func() bool {
		statusCancelled := false
		for _, update := range cards[0].statusUpdatesSnapshot() {
			if strings.Contains(update, "已取消") {
				statusCancelled = true
				break
			}
		}
		return statusCancelled && cards[0].isClosed()
	})

	processCancelled := false
	for _, update := range cards[0].processUpdatesSnapshot() {
		if strings.Contains(update, "已取消") {
			processCancelled = true
			break
		}
	}
	if !processCancelled {
		t.Fatalf("process updates = %+v, want cancellation marker", cards[0].processUpdatesSnapshot())
	}
}

func TestHandleLoopCancelRejectsNonOwner(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	_, err := svc.HandleLoopCancel(context.Background(), feishu.LoopCancel{
		BotID:        "bot-a",
		ChatID:       "oc_chat",
		ACPSessionID: "acp-session-1",
		OperatorID:   "ou_other",
	})
	if err == nil || !strings.Contains(err.Error(), "只有 bot owner 可以取消 loop") {
		t.Fatalf("HandleLoopCancel(non-owner) error = %v, want owner-only error", err)
	}
}

func TestHandleLoopCancelRejectsExpiredCard(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-current",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	_, err := svc.HandleLoopCancel(context.Background(), feishu.LoopCancel{
		BotID:        "bot-a",
		ChatID:       "oc_chat",
		ACPSessionID: "acp-session-old",
		OperatorID:   testOwnerOpenID,
	})
	if err == nil || !strings.Contains(err.Error(), "该 loop 卡片已过期") {
		t.Fatalf("HandleLoopCancel(expired) error = %v, want expired-card error", err)
	}
}

func TestHandleRestartCommandWritesAckSendsPreparingReplyAndRunsCommand(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].OwnerOpenIDs = []string{"ou_owner"}
	svc := NewService(cfg, store)
	restartCalled := make(chan struct{}, 1)
	svc.setRestartCommand(func(ctx context.Context) error {
		restartCalled <- struct{}{}
		return nil
	})
	var intermediate []string
	ctx := feishu.WithIntermediateReplySender(context.Background(), func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate = append(intermediate, text)
		return nil
	})
	workspace := t.TempDir()

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		SenderID:  "ou_owner",
		Text:      "/restart",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/restart) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after intermediate preparing reply", reply)
	}
	if got, want := intermediate, []string{"收到，准备重启 bridge 服务。"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("intermediate replies = %#v, want %#v", got, want)
	}
	select {
	case <-restartCalled:
	case <-time.After(time.Second):
		t.Fatal("restart command was not called")
	}
	data, err := os.ReadFile(restartAckPath(workspace))
	if err != nil {
		t.Fatalf("ReadFile(restart ack) error = %v", err)
	}
	var ack restartAck
	if err := json.Unmarshal(data, &ack); err != nil {
		t.Fatalf("Unmarshal(restart ack) error = %v", err)
	}
	if ack.BotID != "bot-a" || ack.Message.MessageID != "om_restart" || ack.Message.ChatID != "oc_chat" || ack.RequestedBy != "ou_owner" {
		t.Fatalf("restart ack = %+v, want original message target and requester", ack)
	}
}

func TestHandleRestartCommandRequiresOwner(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].OwnerOpenIDs = []string{"ou_owner"}
	svc := NewService(cfg, store)
	svc.setRestartCommand(func(ctx context.Context) error {
		t.Fatal("restart command should not run for non-owner")
		return nil
	})

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		SenderID:  "ou_other",
		Text:      "/restart",
		Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/restart) error = %v", err)
	}
	if reply != "只有 bot owner 可以执行斜杠命令。" {
		t.Fatalf("reply = %q, want owner warning", reply)
	}
}

func TestHandleRestartCommandRejectsDefaultRestartOutsideDaemon(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].OwnerOpenIDs = []string{"ou_owner"}
	svc := NewService(cfg, store)
	ctx := feishu.WithIntermediateReplySender(context.Background(), func(ctx context.Context, msg feishu.Message, text string) error {
		t.Fatal("intermediate reply should not be sent when restart command is unavailable")
		return nil
	})
	workspace := t.TempDir()

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		SenderID:  "ou_owner",
		Text:      "/restart",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/restart) error = %v", err)
	}
	if !strings.Contains(reply, "未配置 restart_command") || !strings.Contains(reply, "systemd") {
		t.Fatalf("reply = %q, want restart_command guidance", reply)
	}
	if _, err := os.Stat(restartAckPath(workspace)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart ack file err = %v, want no ack when restart is rejected", err)
	}
}

func TestExecuteRestartCommandAllowsConfiguredCommandOutsideDaemon(t *testing.T) {
	cfg := config.Default()
	cfg.RestartCommand = []string{"/bin/echo", "restart-ok"}
	svc := NewService(cfg, NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))

	if err := svc.executeRestartCommand(context.Background()); err != nil {
		t.Fatalf("executeRestartCommand() error = %v", err)
	}
}

func TestRestartCommandAllowsAdapterResolvedOwner(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	svc := NewService(cfg, store)
	adapter := feishu.NewAdapter(config.BotConfig{
		ID:           "bot-a",
		BotOpenID:    " ou_bot_resolved ",
		OwnerOpenIDs: []string{" ou_owner ", "ou_owner"},
	}, svc)
	if !svc.syncResolvedBotConfig(0, adapter) {
		t.Fatal("syncResolvedBotConfig() = false, want resolved fields copied")
	}
	if got, want := svc.botOpenID("bot-a"), "ou_bot_resolved"; got != want {
		t.Fatalf("botOpenID() = %q, want %q", got, want)
	}
	if got, want := svc.ownerOpenIDs("bot-a"), []string{"ou_owner"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ownerOpenIDs() = %#v, want %#v", got, want)
	}

	restartCalled := make(chan struct{}, 1)
	svc.setRestartCommand(func(ctx context.Context) error {
		restartCalled <- struct{}{}
		return nil
	})
	ctx := feishu.WithIntermediateReplySender(context.Background(), func(ctx context.Context, msg feishu.Message, text string) error {
		return nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		SenderID:  "ou_owner",
		Text:      "/restart",
		Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/restart) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after accepted restart command", reply)
	}
	select {
	case <-restartCalled:
	case <-time.After(time.Second):
		t.Fatal("restart command was not called for adapter-resolved owner")
	}
}

func TestRunRestartCommandRemovesAckOnFailure(t *testing.T) {
	cfg := config.Default()
	svc := NewService(cfg, NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
	svc.setRestartCommand(func(ctx context.Context) error {
		return fmt.Errorf("restart failed")
	})
	workspace := t.TempDir()
	if err := writeRestartAck(workspace, newRestartAck(feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
	})); err != nil {
		t.Fatalf("writeRestartAck() error = %v", err)
	}

	svc.runRestartCommand(context.Background(), workspace)

	if _, err := os.Stat(restartAckPath(workspace)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart ack file err = %v, want removed after restart command failure", err)
	}
}

func TestConsumeRestartAckSendsConfirmationAndRemovesFile(t *testing.T) {
	workspace := t.TempDir()
	ack := newRestartAck(feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "group",
		ThreadID:  "omt_thread",
		SenderID:  "ou_owner",
	})
	if err := writeRestartAck(workspace, ack); err != nil {
		t.Fatalf("writeRestartAck() error = %v", err)
	}
	sender := &fakeRestartAckSender{}

	if err := consumeRestartAck(context.Background(), workspace, sender, "bot-a"); err != nil {
		t.Fatalf("consumeRestartAck() error = %v", err)
	}
	if _, err := os.Stat(restartAckPath(workspace)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart ack file err = %v, want removed", err)
	}
	if got := sender.messages; len(got) != 1 || got[0].MessageID != "om_restart" || got[0].ThreadID != "omt_thread" || sender.texts[0] != restartAckText() {
		t.Fatalf("sent messages = %+v texts = %+v, want restart confirmation to original message", sender.messages, sender.texts)
	}
}

func TestConsumeRestartAckKeepsFileWhenSendFails(t *testing.T) {
	workspace := t.TempDir()
	if err := writeRestartAck(workspace, newRestartAck(feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
	})); err != nil {
		t.Fatalf("writeRestartAck() error = %v", err)
	}
	sender := &fakeRestartAckSender{err: fmt.Errorf("send failed")}

	err := consumeRestartAck(context.Background(), workspace, sender, "bot-a")
	if err == nil || !strings.Contains(err.Error(), "发送重启确认消息") {
		t.Fatalf("consumeRestartAck() error = %v, want send error", err)
	}
	if _, statErr := os.Stat(restartAckPath(workspace)); statErr != nil {
		t.Fatalf("restart ack file should remain after send failure, stat err = %v", statErr)
	}
}

func TestConsumeRestartAckRemovesInvalidJSONFile(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(restartAckPath(workspace), []byte("{invalid json"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	sender := &fakeRestartAckSender{}

	if err := consumeRestartAck(context.Background(), workspace, sender, "bot-a"); err != nil {
		t.Fatalf("consumeRestartAck() error = %v", err)
	}
	if _, err := os.Stat(restartAckPath(workspace)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart ack file err = %v, want removed", err)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("sent messages = %+v, want none for invalid ack", sender.messages)
	}
}

func TestConsumeRestartAckRemovesMissingTargetFile(t *testing.T) {
	workspace := t.TempDir()
	ack := newRestartAck(feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "group",
	})
	data, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(restartAckPath(workspace), data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	sender := &fakeRestartAckSender{}

	if err := consumeRestartAck(context.Background(), workspace, sender, "bot-a"); err != nil {
		t.Fatalf("consumeRestartAck() error = %v", err)
	}
	if _, err := os.Stat(restartAckPath(workspace)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart ack file err = %v, want removed", err)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("sent messages = %+v, want none for ack without message target", sender.messages)
	}
}

func TestWikiTimerRunsSilentReflection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptReply: "NoReply"}
	svc := newTestService(config.Default(), store)
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
	svc := newTestService(config.Default(), store)
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

func TestShutdownCancelsRuntimeTasksBeforeRuntimeShutdown(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	cfg.Bots[0].Workspace = t.TempDir()
	svc := newTestService(cfg, store)
	rt := &fakeRuntime{blockPrompt: make(chan struct{})}
	svc.setRuntime(rt)
	session := Session{
		Key:          SessionKey{BotID: "bot-a", ChatID: "oc_chat"},
		AgentName:    "traex",
		Cwd:          t.TempDir(),
		ACPSessionID: "acp-session-1",
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := svc.runUserPrompt(context.Background(), feishu.Message{
			BotID:     "bot-a",
			ChatID:    "oc_chat",
			ChatType:  "p2p",
			MessageID: "om_prompt",
			Workspace: cfg.Bots[0].Workspace,
		}, session, cfg.Agents["traex"], "长任务")
		done <- err
	}()
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runUserPrompt() error = %v, want context.Canceled", err)
	}
	rt.mu.Lock()
	cancelCount := len(rt.cancelCalls)
	shutdownCancelCount := rt.shutdownCancelCount
	rt.mu.Unlock()
	if cancelCount != 1 {
		t.Fatalf("cancel calls = %d, want 1", cancelCount)
	}
	if shutdownCancelCount != 1 {
		t.Fatalf("shutdownCancelCount = %d, want runtime cancel completed before shutdown", shutdownCancelCount)
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

func containsStringWithAll(values []string, parts ...string) bool {
	for _, value := range values {
		matched := true
		for _, part := range parts {
			if !strings.Contains(value, part) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
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
	mu                  sync.Mutex
	newSessionID        string
	newSessionIDs       []string
	newSessionInfo      acp.SessionInfo
	newSessionError     error
	noDefaultState      bool
	afterNewSession     func(key SessionKey, sessionID string)
	promptReply         string
	promptErrors        []error
	promptUpdates       []acp.PromptUpdate
	afterUpdates        func()
	permissionRequest   *acp.PermissionRequest
	permissionOutcome   acp.PermissionOutcome
	blockPrompt         chan struct{}
	blockPromptAt       int
	promptResult        acp.PromptResult
	promptResults       []acp.PromptResult
	configOptions       []acp.SessionConfigOption
	configCalls         []fakeConfigCall
	modeCalls           []fakeModeCall
	newCalls            []fakeNewCall
	promptCalls         []fakePromptCall
	wikiRuntimeCalls    []fakePromptCall
	cancelCalls         []fakeCancelCall
	closedRuntimeKeys   []runtimeKey
	closedKeys          []SessionKey
	shutdownCancelCount int
	updateHandlers      map[SessionKey][]acp.UpdateHandler
	blockWikiRuntime    chan struct{}
}

type fakeNewCall struct {
	Key       SessionKey
	AgentName string
	Cwd       string
	Workspace string
}

type fakePromptCall struct {
	Runtime runtimeKey
	Session Session
	Text    string
}

type fakeCancelCall struct {
	Runtime runtimeKey
	Session Session
	Attrs   map[string]string
}

type fakeConfigCall struct {
	Session  Session
	ConfigID string
	Value    any
}

type fakeModeCall struct {
	Session Session
	ModeID  string
}

func (f *fakeRuntime) NewSession(ctx context.Context, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (acp.SessionInfo, error) {
	f.mu.Lock()
	f.newCalls = append(f.newCalls, fakeNewCall{Key: key, AgentName: agentName, Cwd: cwd, Workspace: workspace})
	if f.newSessionError != nil {
		err := f.newSessionError
		f.mu.Unlock()
		return acp.SessionInfo{}, err
	}
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

func (f *fakeRuntime) Prompt(ctx context.Context, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (acp.PromptResult, error) {
	return f.prompt(ctx, currentRuntimeKey(session.Key), session, text, opts, false)
}

func (f *fakeRuntime) PromptWithRuntimeKey(ctx context.Context, key runtimeKey, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (acp.PromptResult, error) {
	return f.prompt(ctx, key, session, text, opts, key.Scope == runtimeScopeWiki)
}

func (f *fakeRuntime) prompt(ctx context.Context, key runtimeKey, session Session, text string, opts acp.PromptOptions, wiki bool) (acp.PromptResult, error) {
	f.mu.Lock()
	call := fakePromptCall{Runtime: key, Session: session, Text: text}
	if wiki {
		f.wikiRuntimeCalls = append(f.wikiRuntimeCalls, call)
	} else {
		f.promptCalls = append(f.promptCalls, call)
	}
	callNumber := len(f.promptCalls)
	updates := append([]acp.PromptUpdate(nil), f.promptUpdates...)
	afterUpdates := f.afterUpdates
	blockPrompt := f.blockPrompt
	blockThisPrompt := blockPrompt != nil && (f.blockPromptAt == 0 || f.blockPromptAt == callNumber)
	blockWikiRuntime := f.blockWikiRuntime
	result := f.promptResult
	if len(f.promptResults) > 0 {
		result = f.promptResults[0]
		f.promptResults = f.promptResults[1:]
	}
	if result.Text == "" {
		result.Text = f.promptReply
	}
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
	if wiki && blockWikiRuntime != nil {
		select {
		case <-ctx.Done():
			return acp.PromptResult{}, ctx.Err()
		case <-blockWikiRuntime:
		}
	}
	if blockThisPrompt {
		select {
		case <-ctx.Done():
			return acp.PromptResult{}, ctx.Err()
		case <-blockPrompt:
		}
	}
	if promptErr != nil {
		return acp.PromptResult{}, promptErr
	}
	return result, nil
}

func (f *fakeRuntime) CancelSession(ctx context.Context, key runtimeKey, session Session, agent config.AgentConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls = append(f.cancelCalls, fakeCancelCall{Runtime: key, Session: session, Attrs: slogAttrsMap(logging.CtxAttrs(ctx))})
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

func (f *fakeRuntime) SetMode(ctx context.Context, session Session, agent config.AgentConfig, modeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modeCalls = append(f.modeCalls, fakeModeCall{Session: session, ModeID: modeID})
	return nil
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

func (f *fakeRuntime) CloseRuntimeKey(key runtimeKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedRuntimeKeys = append(f.closedRuntimeKeys, key)
	return nil
}

func (f *fakeRuntime) CloseSession(key SessionKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedKeys = append(f.closedKeys, key)
	return nil
}

func (f *fakeRuntime) Shutdown(ctx context.Context) error {
	f.mu.Lock()
	f.shutdownCancelCount = len(f.cancelCalls)
	f.mu.Unlock()
	return nil
}

func (f *fakeRuntime) promptCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.promptCalls)
}

func (f *fakeRuntime) wikiRuntimeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.wikiRuntimeCalls)
}

func (f *fakeRuntime) cancelCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cancelCalls)
}

type fakeStreamCard struct {
	mu                    sync.Mutex
	textUpdates           []string
	processUpdates        []string
	statusUpdates         []string
	usageDetails          []string
	closed                bool
	failOnCanceledContext bool
}

type fakeRestartAckSender struct {
	messages []feishu.Message
	texts    []string
	err      error
}

func (f *fakeRestartAckSender) SendText(ctx context.Context, msg feishu.Message, text string) error {
	if f.err != nil {
		return f.err
	}
	f.messages = append(f.messages, msg)
	f.texts = append(f.texts, text)
	return nil
}

func (f *fakeStreamCard) UpdateText(ctx context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.textUpdates = append(f.textUpdates, text)
	return nil
}

func (f *fakeStreamCard) UpdateProcess(ctx context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.processUpdates = append(f.processUpdates, text)
	return nil
}

func (f *fakeStreamCard) UpdateStatus(ctx context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.statusUpdates = append(f.statusUpdates, text)
	return nil
}

func (f *fakeStreamCard) UpdateUsageDetail(ctx context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.usageDetails = append(f.usageDetails, text)
	return nil
}

func (f *fakeStreamCard) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
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

func (f *fakeStreamCard) usageDetailsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.usageDetails...)
}

func (f *fakeStreamCard) statusUpdatesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.statusUpdates...)
}

func (f *fakeStreamCard) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}
