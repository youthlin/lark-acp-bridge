package bridge

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestConversationManagerRoutesStoreAndFindsBoundSession(t *testing.T) {
	defaultStore := NewSessionStore(filepath.Join(t.TempDir(), "default-sessions.json"))
	botStore := NewSessionStore(filepath.Join(t.TempDir(), "bot-sessions.json"))
	manager := newConversationManager(acp.NewRegistry(config.Config{}), &fakeRuntime{})
	manager.setStore("", defaultStore)
	manager.setStore("bot-a", botStore)

	key := imSessionKey("bot-a", "oc_chat", "omt_topic")
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-topic", Cwd: t.TempDir()}
	if err := botStore.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if _, err := botStore.BindMessageToSession(MessageSessionBinding{
		BotID:      key.BotID,
		ChatID:     sessionKeyMainID(key),
		MessageID:  "om_root",
		SessionKey: key,
	}); err != nil {
		t.Fatalf("BindMessageToSession() error = %v", err)
	}

	if got := manager.storeForBotID(" bot-a "); got != botStore {
		t.Fatalf("storeForBotID(bot-a) = %p, want %p", got, botStore)
	}
	if got := manager.storeForBotID("unknown"); got != defaultStore {
		t.Fatalf("storeForBotID(unknown) = %p, want fallback %p", got, defaultStore)
	}
	found, ok := manager.findSession(feishu.Message{BotID: "bot-a", ChatID: "oc_chat", RootID: "om_root"})
	if !ok || found.ACPSessionID != session.ACPSessionID || found.Key != key {
		t.Fatalf("findSession() = %+v, %v; want bound session %+v", found, ok, session)
	}
}

func TestConversationManagerCreateSessionRunsLifecycleHooks(t *testing.T) {
	cwd := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "workspace")
	cfg := config.Default()
	registry := acp.NewRegistry(cfg)
	runtime := &fakeRuntime{newSessionID: "acp-created"}
	manager := newConversationManager(registry, runtime)
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	manager.setStore("", store)

	var canceled, subscribed, cleared bool
	manager.hooks = conversationManagerHooks{
		cancelRunningSessionWork: func(context.Context, SessionKey) { canceled = true },
		subscribeACPStateUpdates: func(context.Context, feishu.Message, SessionKey) { subscribed = true },
		clearACPError:            func(Session) { cleared = true },
	}
	msg := feishu.Message{BotID: "bot-a", ChatID: "oc_chat", ChatType: "p2p", Workspace: workspace}
	session, _, _, errText := manager.createSession(context.Background(), []string{"/new", cwd, "测试会话"}, msg)
	if errText != "" {
		t.Fatalf("createSession() error = %q", errText)
	}
	if session.ACPSessionID != "acp-created" || session.Cwd != cwd || session.Title != "测试会话" {
		t.Fatalf("session = %+v, want created session", session)
	}
	if !canceled || !subscribed || !cleared {
		t.Fatalf("hooks canceled/subscribed/cleared = %v/%v/%v, want all true", canceled, subscribed, cleared)
	}
	persisted, ok := store.Get(session.Key)
	if !ok || persisted.ACPSessionID != session.ACPSessionID {
		t.Fatalf("persisted session = %+v, %v; want created session", persisted, ok)
	}
}

func TestConversationManagerResumeSessionByID(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := imSessionKey("bot-a", "oc_chat", "")
	for _, session := range []Session{
		{Key: key, Title: "旧会话", AgentName: "traex", ACPSessionID: "acp-old", Cwd: "/old"},
		{Key: key, Title: "当前会话", AgentName: "traex", ACPSessionID: "acp-current", Cwd: "/current"},
	} {
		if err := store.Upsert(session); err != nil {
			t.Fatalf("Upsert(%s) error = %v", session.ACPSessionID, err)
		}
	}
	runtime := &fakeRuntime{activeSessionIDs: map[SessionKey]string{key: "acp-current"}}
	manager := newConversationManager(acp.NewRegistry(config.Config{}), runtime)
	manager.setStore("bot-a", store)

	resumed, errText := manager.resumeSessionByID(context.Background(), feishu.Message{
		BotID: "bot-a", ChatID: "oc_chat", ChatType: "p2p",
	}, "acp-old", nil)
	if errText != "" {
		t.Fatalf("resumeSessionByID() error = %q", errText)
	}
	if resumed.ACPSessionID != "acp-old" || resumed.Title != "旧会话" {
		t.Fatalf("resumed session = %+v, want old session", resumed)
	}
	if active := runtime.activeSessionIDs[key]; active != "acp-old" {
		t.Fatalf("active runtime session = %q, want acp-old", active)
	}
}
