package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

func TestStatusShowsRuntimeSnapshot(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	svc := newTestService(cfg, store)
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Workspace:    t.TempDir(),
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	runtime := newRuntimeManager()
	runtime.maxSlots = 4
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	runtime.now = func() time.Time { return base }
	runtime.idleTimeout = 30 * time.Minute
	current := currentRuntimeKey(session.Key)
	wiki := wikiRuntimeKey(session.Key, 1, session.ACPSessionID)
	other := currentRuntimeKey(imSessionKey("bot-a", "oc_other", ""))
	runtime.slots[current] = runtimeClientSlot{
		client:    &acp.Client{},
		sessionID: session.ACPSessionID,
		lastUsed:  base.Add(-31 * time.Minute),
	}
	runtime.slots[wiki] = runtimeClientSlot{
		client:    &acp.Client{},
		sessionID: session.ACPSessionID,
		lastUsed:  base,
		active:    1,
	}
	runtime.slots[other] = runtimeClientSlot{
		sessionID: "marker-only",
		lastUsed:  base,
	}
	svc.setRuntime(runtime)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:  "bot-a",
		ChatID: "oc_chat",
		Text:   "/status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/status) error = %v", err)
	}
	for _, want := range []string{
		"runtime：slots 3，clients 2/4，busy 1，idle 1，markers 1",
		"runtime会话：slots 2，clients 2，busy 1，idle 1，scope current/wiki",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
}

func TestHandleFeishuMessageAutoCompactAfterPromptThreshold(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.AvailableCommands = []acp.AvailableCommand{{Name: "compact"}}
	session.AutoCompact = true
	session.AutoCompactPct = 80
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{
		promptResults: []acp.PromptResult{
			{
				Text: "done",
				Meta: acp.PromptResultMeta{TraeTokenUsage: &acp.TraeTokenUsage{
					ContextWindow: acp.ContextWindowUsage{Used: 160000, Size: 200000},
				}},
			},
			{Text: "compacted"},
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
		Text:     "普通消息",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q, want original prompt reply", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 2 })
	calls := rt.promptCallsSnapshot()
	if calls[1].Text != "/compact" {
		t.Fatalf("prompt calls = %+v, want automatic compact second", calls)
	}
	updated, ok := store.Get(session.Key)
	if !ok || updated.ContextWindow == nil || updated.ContextWindow.Used != 160000 || updated.ContextWindow.Size != 200000 {
		t.Fatalf("updated session = %+v, %v; want context window persisted", updated, ok)
	}
	if updated.AutoCompacting || updated.LastAutoCompactAt == nil {
		t.Fatalf("updated session = %+v, want compact finished timestamp", updated)
	}
}

func TestHandleFeishuMessageAutoCompactResetsWorkspacePrompted(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.AvailableCommands = []acp.AvailableCommand{{Name: "compact"}}
	session.AutoCompact = true
	session.AutoCompactPct = 80
	session.WorkspacePrompted = true
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{
		promptResults: []acp.PromptResult{
			{
				Text: "done",
				Meta: acp.PromptResultMeta{TraeTokenUsage: &acp.TraeTokenUsage{
					ContextWindow: acp.ContextWindowUsage{Used: 160000, Size: 200000},
				}},
			},
			{Text: "compacted"},
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
		Text:     "普通消息",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		updated, ok := store.Get(session.Key)
		return rt.promptCallCount() == 2 && ok && !updated.AutoCompacting && updated.LastAutoCompactAt != nil
	})
	updated, ok := store.Get(session.Key)
	if !ok || updated.WorkspacePrompted {
		t.Fatalf("updated session = %+v, %v; want workspace prompt reset after auto compact", updated, ok)
	}
}

func TestHandleFeishuMessageAutoCompactRunsSilently(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.AvailableCommands = []acp.AvailableCommand{{Name: "compact"}}
	session.AutoCompact = true
	session.AutoCompactPct = 80
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: session.ACPSessionID,
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "正在压缩。"},
				},
			},
		},
		promptResults: []acp.PromptResult{
			{
				Text: "done",
				Meta: acp.PromptResultMeta{TraeTokenUsage: &acp.TraeTokenUsage{
					ContextWindow: acp.ContextWindowUsage{Used: 160000, Size: 200000},
				}},
			},
			{Text: "compacted"},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	ctx := withFakeSentMessageClient(context.Background(), svc, session.Key.BotID, client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "普通消息",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want original prompt streamed and automatic compact silent", reply)
	}
	waitForCondition(t, time.Second, func() bool {
		updated, ok := store.Get(session.Key)
		return rt.promptCallCount() == 2 && ok && !updated.AutoCompacting && updated.LastAutoCompactAt != nil
	})
	calls := rt.promptCallsSnapshot()
	if len(calls) != 2 || calls[1].Text != "/compact" {
		t.Fatalf("prompt calls = %+v, want automatic compact second", calls)
	}
	if !calls[0].HasUpdateHandler || !calls[0].HasPermissionHandler {
		t.Fatalf("first prompt call = %+v, want normal stream and permission handlers", calls[0])
	}
	if calls[1].HasUpdateHandler || calls[1].HasPermissionHandler {
		t.Fatalf("auto compact prompt call = %+v, want silent without stream or permission handlers", calls[1])
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want only original prompt stream card", cards)
	}
	if got := cards[0].textUpdatesSnapshot(); len(got) != 1 || got[0] != "正在压缩。" {
		t.Fatalf("textUpdates = %+v, want only original prompt chunk rendered", got)
	}
	if got := cards[0].finalTextUpdatesSnapshot(); len(got) != 1 || got[0] != "done" {
		t.Fatalf("finalTextUpdates = %+v, want only original prompt final text", got)
	}
	if got := client.sentSnapshot(); len(got) != 0 {
		t.Fatalf("sent messages = %+v, want no extra automatic compact reply", got)
	}
}

func TestHandleFeishuMessageAutoCompactCanBeInterruptedByNewMessage(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.AvailableCommands = []acp.AvailableCommand{{Name: "compact"}}
	session.AutoCompact = true
	session.AutoCompactPct = 80
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	compactBlocked := make(chan struct{})
	rt := &fakeRuntime{
		blockPrompt:   compactBlocked,
		blockPromptAt: 2,
		promptResults: []acp.PromptResult{
			{
				Text: "done",
				Meta: acp.PromptResultMeta{TraeTokenUsage: &acp.TraeTokenUsage{
					ContextWindow: acp.ContextWindowUsage{Used: 160000, Size: 200000},
				}},
			},
			{Text: "compacted"},
			{
				Text: "new reply",
				Meta: acp.PromptResultMeta{TraeTokenUsage: &acp.TraeTokenUsage{
					ContextWindow: acp.ContextWindowUsage{Used: 10000, Size: 200000},
				}},
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
		Text:     "普通消息",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(first prompt) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 2 })

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "新的用户消息",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(second prompt) error = %v", err)
	}
	if reply != "new reply" {
		t.Fatalf("reply = %q, want new prompt reply", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() >= 1 })
	waitForCondition(t, time.Second, func() bool {
		updated, ok := store.Get(session.Key)
		return ok && !updated.AutoCompacting
	})
	calls := rt.promptCallsSnapshot()
	if len(calls) != 3 || calls[1].Text != "/compact" || !strings.Contains(calls[2].Text, "新的用户消息") {
		t.Fatalf("prompt calls = %+v, want compact interrupted by new user prompt", calls)
	}
}

func TestHandleFeishuMessageAutoCompactRequiresCommand(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.AutoCompact = true
	session.AutoCompactPct = 80
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{promptResult: acp.PromptResult{
		Text: "done",
		Meta: acp.PromptResultMeta{TraeTokenUsage: &acp.TraeTokenUsage{
			ContextWindow: acp.ContextWindowUsage{Used: 160000, Size: 200000},
		}},
	}}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	_, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "普通消息",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })
	updated, ok := store.Get(session.Key)
	if !ok || updated.AutoCompacting || updated.LastAutoCompactAt != nil {
		t.Fatalf("updated session = %+v, %v; want no automatic compact", updated, ok)
	}
}

func TestHandleFeishuMessageRefreshesUnavailablePersistedACPSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	key := imSessionKey("", "oc_chat", "omt_thread")
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
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
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

func TestHandleFeishuMessageRefreshesBrokenPipeACPSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	key := imSessionKey("", "oc_chat", "omt_thread")
	if err := store.Upsert(Session{
		Key:          key,
		Title:        "old session",
		AgentName:    "traex",
		ACPSessionID: "old-acp-session",
		Cwd:          workDir,
		ConfigOptions: []acp.SessionConfigOption{
			{
				ID:           "mode",
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
				Category:     "model",
				Type:         "select",
				CurrentValue: "gpt-5.6",
				Options: []acp.SessionConfigOptionValue{
					{Value: "gpt-5.5", Name: "GPT-5.5"},
					{Value: "gpt-5.6", Name: "GPT-5.6"},
				},
			},
		},
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{
			SessionID: "new-acp-session",
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
		},
		promptReply: "ACP 回复",
		promptErrors: []error{
			fmt.Errorf("%w: session/prompt: write |1: broken pipe", errACPSessionUnavailable),
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
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
	if len(rt.promptCalls) != 2 || rt.promptCalls[1].Session.ACPSessionID != "new-acp-session" {
		t.Fatalf("promptCalls = %+v, want retry on refreshed session", rt.promptCalls)
	}
	if currentModeDisplay(rt.promptCalls[1].Session) != "plan" || currentModelDisplay(rt.promptCalls[1].Session) != "gpt-5.6" {
		t.Fatalf("retry session config = %+v, want inherited mode/model", rt.promptCalls[1].Session.ConfigOptions)
	}
	if len(rt.configCalls) != 2 ||
		rt.configCalls[0].ConfigID != "mode" || rt.configCalls[0].Value != "plan" ||
		rt.configCalls[1].ConfigID != "model" || rt.configCalls[1].Value != "gpt-5.6" {
		t.Fatalf("configCalls = %+v, want mode/model restored before retry", rt.configCalls)
	}
	updated, ok := store.Get(key)
	if !ok || updated.ACPSessionID != "new-acp-session" {
		t.Fatalf("updated session = %+v, ok=%v; want refreshed session", updated, ok)
	}
	if currentModeDisplay(updated) != "plan" || currentModelDisplay(updated) != "gpt-5.6" {
		t.Fatalf("updated session config = %+v, want inherited mode/model", updated.ConfigOptions)
	}
}

func TestRefreshACPSessionDoesNotReplaceNewCurrentSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	oldSession := Session{
		Key:          key,
		Title:        "旧会话",
		AgentName:    "traex",
		ACPSessionID: "acp-session-old",
		Cwd:          t.TempDir(),
	}
	if err := store.Upsert(oldSession); err != nil {
		t.Fatalf("Upsert(oldSession) error = %v", err)
	}
	newCurrent := oldSession
	newCurrent.Title = "新会话"
	newCurrent.ACPSessionID = "acp-session-current"
	rt := &fakeRuntime{
		newSessionID: "acp-session-refreshed",
		afterNewSession: func(SessionKey, string) {
			if err := store.Upsert(newCurrent); err != nil {
				t.Errorf("Upsert(newCurrent) error = %v", err)
			}
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	_, err := svc.refreshACPSession(context.Background(), feishu.Message{
		BotID:    key.BotID,
		ChatID:   sessionKeyMainID(key),
		ChatType: "p2p",
	}, oldSession, mustConfigAgent(t, config.Default(), "traex"))
	if err == nil || !strings.Contains(err.Error(), "当前会话已变化") {
		t.Fatalf("refreshACPSession() error = %v, want changed session error", err)
	}
	persisted, ok := store.Get(key)
	if !ok || persisted.ACPSessionID != newCurrent.ACPSessionID || persisted.Title != newCurrent.Title {
		t.Fatalf("persisted session = %+v, %v; want new current session unchanged", persisted, ok)
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
	session, ok := store.Get(imSessionKey("bot-a", "oc_chat", "omt_thread"))
	if !ok {
		session, ok = store.Get(imSessionKey("bot-a", "oc_chat", ""))
	}
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

func TestHandleFeishuMessageAutoCreatesSessionWithBootstrapContext(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
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

func TestHandleFeishuMessageKeepsPersistedACPSessionAfterBootstrapDeleted(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workspace := filepath.Join(t.TempDir(), "bot-a")
	markWorkspaceBootstrapped(t, workspace)
	if err := os.WriteFile(filepath.Join(workspace, "SOUL.md"), []byte("# SOUL\n\n名字：小助手\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(SOUL.md) error = %v", err)
	}
	workDir := t.TempDir()
	key := imSessionKey("bot-a", "oc_chat", "omt_thread")
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
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
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
	if _, ok := store.Get(imSessionKey("bot-a", "oc_chat", "")); !ok {
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
	if _, ok := store.Get(imSessionKey("bot-a", "oc_chat", "")); !ok {
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
		ChatID:    sessionKeyMainID(session.Key),
		ThreadID:  session.Key.SubID,
		ChatType:  "topic_group",
		Mentions:  testBotMentions(),
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

func TestHandleSIDCommandCancelsRunningTaskBeforeSessionTransition(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = t.TempDir()
	cfg.Bots[0].OwnerOpenIDs = []string{testOwnerOpenID}
	svc := NewService(cfg, store)
	workDir := t.TempDir()
	sourceKey := normalizeSessionKey(imSessionKey("bot-a", "oc_source", "omt_source"))
	targetSession := Session{
		Key:          sourceKey,
		AgentName:    "traex",
		ACPSessionID: "acp-source",
		Cwd:          workDir,
		Workspace:    workDir,
	}
	if err := store.Upsert(targetSession); err != nil {
		t.Fatalf("Upsert(target session) error = %v", err)
	}
	currentSession := targetSession
	currentSession.ACPSessionID = "acp-running"
	if err := store.Upsert(currentSession); err != nil {
		t.Fatalf("Upsert(current session) error = %v", err)
	}
	rt := &fakeRuntime{
		promptReply: "插入完成",
		activeSessionIDs: map[SessionKey]string{
			sourceKey: currentSession.ACPSessionID,
		},
	}
	svc.setRuntime(rt)
	agent := mustConfigAgent(t, cfg, "traex")
	taskStarted := make(chan struct{})
	taskDone := make(chan error, 1)
	go func() {
		_, err := runUserTask(svc, context.Background(), currentSession, agent, runningTaskOptions{}, func(ctx context.Context) (struct{}, error) {
			close(taskStarted)
			<-ctx.Done()
			return struct{}{}, ctx.Err()
		})
		taskDone <- err
	}()
	<-taskStarted
	transitionChecked := make(chan struct{}, 1)
	rt.transitionBefore = func() {
		rt.mu.Lock()
		cancelCalls := append([]fakeCancelCall(nil), rt.cancelCalls...)
		promptCalls := append([]fakePromptCall(nil), rt.promptCalls...)
		rt.mu.Unlock()
		if len(cancelCalls) != 1 || cancelCalls[0].Session.ACPSessionID != "acp-running" {
			t.Errorf("cancelCalls before transition = %+v, want one cancel for acp-running", cancelCalls)
		}
		if len(promptCalls) != 0 {
			t.Errorf("promptCalls before transition = %+v, want none", promptCalls)
		}
		transitionChecked <- struct{}{}
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_sid",
		ChatID:    "oc_trace",
		ChatType:  "group",
		SenderID:  testOwnerOpenID,
		Text:      "@智能助手 /sid acp-source 插入处理",
		Mentions:  testBotMentions(),
		Workspace: workDir,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/sid) error = %v", err)
	}
	if reply != "插入完成" {
		t.Fatalf("reply = %q, want sid prompt reply", reply)
	}
	if err := <-taskDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("running task error = %v, want context.Canceled", err)
	}
	select {
	case <-transitionChecked:
	default:
		t.Fatal("transitionBefore was not called")
	}
	rt.mu.Lock()
	cancels := append([]fakeCancelCall(nil), rt.cancelCalls...)
	rt.mu.Unlock()
	calls := rt.promptCallsSnapshot()
	if len(cancels) != 1 || len(calls) != 1 {
		t.Fatalf("cancelCalls=%+v promptCalls=%+v, want one cancel then one prompt", cancels, calls)
	}
	if cancels[0].Seq >= calls[0].Seq {
		t.Fatalf("call order cancel seq=%d prompt seq=%d, want cancel before prompt", cancels[0].Seq, calls[0].Seq)
	}
	if calls[0].Session.ACPSessionID != "acp-source" {
		t.Fatalf("prompt session = %+v, want target session acp-source", calls[0].Session)
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
		blockPrompt:            make(chan struct{}),
		blockPromptAt:          1,
		blockAfterPromptCancel: true,
		blockCancel:            make(chan struct{}),
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "omt_thread"))
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	ctx := context.Background()
	var cards fakeStreamCardCollector
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards.add(card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)
	firstDone := make(chan struct {
		reply string
		err   error
	}, 1)
	go func() {
		reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
			BotID:     "bot-a",
			MessageID: "om_first",
			ChatID:    "oc_chat",
			ChatType:  "topic_group",
			ThreadID:  "omt_thread",
			Mentions:  testBotMentions(),
			Text:      "先做这个长任务",
		})
		firstDone <- struct {
			reply string
			err   error
		}{reply: reply, err: err}
	}()
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	secondCtx := logging.CtxAddAttr(ctx, slog.String("message_id", "om_second"))
	secondDone := make(chan struct {
		reply string
		err   error
	}, 1)
	go func() {
		reply, err := handleFeishuMessage(t, svc, secondCtx, feishu.Message{
			BotID:     "bot-a",
			MessageID: "om_second",
			ChatID:    "oc_chat",
			ChatType:  "topic_group",
			ThreadID:  "omt_thread",
			Mentions:  testBotMentions(),
			Text:      "改成做这个",
		})
		secondDone <- struct {
			reply string
			err   error
		}{reply: reply, err: err}
	}()
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() >= 1 })
	if got := rt.promptCallCount(); got != 1 {
		t.Fatalf("prompt calls = %d, want replacement prompt to wait for runtime cancel", got)
	}
	waitForCondition(t, time.Second, func() bool { return len(cards.snapshot()) >= 2 })
	select {
	case got := <-secondDone:
		t.Fatalf("second prompt finished before runtime cancel returned: %+v", got)
	default:
	}
	close(rt.blockCancel)
	close(rt.blockPrompt)
	var secondResult struct {
		reply string
		err   error
	}
	select {
	case secondResult = <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second prompt did not finish")
	}
	if secondResult.err != nil {
		t.Fatalf("HandleFeishuMessage(second) error = %v", secondResult.err)
	}
	if secondResult.reply != "" && secondResult.reply != "ACP 回复" {
		t.Fatalf("reply = %q, want empty streamed reply or final text", secondResult.reply)
	}
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
	if len(rt.promptCalls) != 2 || len(rt.cancelCalls) != 1 || rt.cancelCalls[0].Seq > rt.promptCalls[1].Seq {
		t.Fatalf("prompt/cancel calls = %+v / %+v, want runtime cancel before replacement prompt", rt.promptCalls, rt.cancelCalls)
	}
	rt.mu.Unlock()
	cardSnapshot := cards.snapshot()
	if len(cardSnapshot) == 0 {
		t.Fatal("old prompt should create a stream card before cancellation")
	}
	cancelled := false
	for _, update := range cardSnapshot[0].processUpdatesSnapshot() {
		if strings.Contains(update, "已取消") {
			cancelled = true
			break
		}
	}
	if !cancelled {
		t.Fatalf("process updates = %+v, want cancellation marker", cardSnapshot[0].processUpdatesSnapshot())
	}
	statusCancelled := false
	for _, update := range cardSnapshot[0].statusUpdatesSnapshot() {
		if strings.Contains(update, "🚫") {
			statusCancelled = true
			break
		}
	}
	if !statusCancelled {
		t.Fatalf("status updates = %+v, want cancelled status", cardSnapshot[0].statusUpdatesSnapshot())
	}
	if !cardSnapshot[0].isClosed() {
		t.Fatal("cancelled old card should be closed")
	}
	if len(cardSnapshot) < 2 {
		t.Fatalf("cards = %+v, want replacement stream card", cardSnapshot)
	}
	replacementStatus := cardSnapshot[1].statusUpdatesSnapshot()
	if !hasSubstring(replacementStatus, "等待中断") {
		t.Fatalf("replacement status updates = %+v, want waiting status", replacementStatus)
	}
	if !hasStatusWithoutSubstring(replacementStatus, "等待中断") {
		t.Fatalf("replacement status updates = %+v, want normal running status after old task finishes", replacementStatus)
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
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "omt_thread"))
	session := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-1",
		Cwd:             t.TempDir(),
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		WikiIntervalSec: 60,
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc.scheduleWikiAfterUserPrompt(session, mustConfigAgent(t, config.Default(), "traex"))
	t.Cleanup(func() { svc.cancelWikiTimer(key) })

	statusBeforePrompt, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_status_before",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "/status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/status) before prompt error = %v", err)
	}
	if !strings.Contains(statusBeforePrompt, "wiki：等待定时触发") {
		t.Fatalf("reply = %q, want pending wiki timer status", statusBeforePrompt)
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
			ChatType:  "topic_group",
			ThreadID:  "omt_thread",
			Mentions:  testBotMentions(),
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
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
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

func TestHandleFeishuMessageStatusShowsRuntimeDiagnostics(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptReply:   "ACP 回复",
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "omt_thread"))
	session := Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	loopStarted := time.Now()
	svc.markLoopStarted(key, loopStarted, loopRequest{
		Prompt:    "包含敏感正文的 loop 提示词",
		MaxRounds: 3,
		Interval:  time.Second,
	})
	svc.markLoopRound(key, loopStarted, 2)
	if !svc.addLoopPendingMessage(key, "包含敏感正文的 loop 补充消息") {
		t.Fatal("addLoopPendingMessage() = false, want running loop")
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
			ChatType:  "topic_group",
			ThreadID:  "omt_thread",
			Mentions:  testBotMentions(),
			Text:      "先做这个长任务",
		})
		firstDone <- struct {
			reply string
			err   error
		}{reply: reply, err: err}
	}()
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	queueReply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_queue",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "/queue 包含敏感正文的排队任务",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/queue) error = %v", err)
	}
	if !strings.Contains(queueReply, "第 1 条") {
		t.Fatalf("queue reply = %q, want queued index", queueReply)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_status",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "/status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/status) error = %v", err)
	}
	for _, want := range []string{
		"session：acp-session-1",
		"运行态：忙碌（user）",
		"队列：待执行 1 条",
		"wiki：尚未触发",
		"loop：运行中，第 2 轮，有待处理补充消息",
		"ACP错误：无",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
	for _, forbidden := range []string{
		"包含敏感正文的排队任务",
		"包含敏感正文的 loop 提示词",
		"包含敏感正文的 loop 补充消息",
	} {
		if strings.Contains(reply, forbidden) {
			t.Fatalf("reply = %q, should not include sensitive diagnostic text %q", reply, forbidden)
		}
	}
	if got := rt.cancelCallCount(); got != 0 {
		t.Fatalf("cancel calls = %d, want status and queue not to cancel running prompt", got)
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
	// 第一个 prompt 结束后会异步 drain /queue 暂存的任务，它会写 sessions.json。
	// 等待队列 drain 完成，避免后台 goroutine 与 t.TempDir() 清理竞态。
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 2 })
	svc.taskMu.Lock()
	busy := svc.tasks[key] != nil
	svc.taskMu.Unlock()
	if busy {
		t.Fatal("queued prompt still running after drain")
	}
}
