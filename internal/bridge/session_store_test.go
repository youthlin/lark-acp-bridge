package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

func ptrTime(t time.Time) *time.Time {
	return &t
}

func TestSessionStoreLoadsLegacyPathAndWritesLocalPath(t *testing.T) {
	workspace := t.TempDir()
	legacyPath := filepath.Join(workspace, "sessions.json")
	localPath := filepath.Join(workspace, ".local", "sessions.json")
	legacy := NewSessionStore(legacyPath)
	if err := legacy.Upsert(Session{
		Key:          SessionKey{BotID: "bot-a", ChatID: "oc_chat"},
		Title:        "legacy",
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          "/repo",
	}); err != nil {
		t.Fatalf("Upsert(legacy) error = %v", err)
	}

	store := NewSessionStoreWithFallback(localPath, legacyPath)
	if err := store.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if session, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat"}); !ok || session.ACPSessionID != "acp-session-1" {
		t.Fatalf("legacy session = %+v ok=%v, want loaded", session, ok)
	}
	if err := store.Upsert(Session{
		Key:          SessionKey{BotID: "bot-a", ChatID: "oc_chat"},
		Title:        "local",
		AgentName:    "traex",
		ACPSessionID: "acp-session-2",
		Cwd:          "/repo",
	}); err != nil {
		t.Fatalf("Upsert(local) error = %v", err)
	}
	if _, err := os.Stat(localPath); err != nil {
		t.Fatalf("local sessions file err = %v, want created", err)
	}
}

func TestSessionStoreTrimsHistoryPerChat(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	store := NewSessionStore(storePath)

	for i := 0; i < maxSessionHistoryPerChat+2; i++ {
		if err := store.Upsert(Session{
			Key:          SessionKey{BotID: "bot-a", ChatID: "oc_chat"},
			Title:        fmt.Sprintf("session %d", i),
			AgentName:    "traex",
			ACPSessionID: fmt.Sprintf("acp-session-%02d", i),
			Cwd:          "/repo",
		}); err != nil {
			t.Fatalf("Upsert(chat session %d) error = %v", i, err)
		}
	}
	for i := 0; i < maxSessionHistoryPerChat; i++ {
		if err := store.Upsert(Session{
			Key:          SessionKey{BotID: "bot-a", ChatID: "oc_other"},
			Title:        fmt.Sprintf("other %d", i),
			AgentName:    "traex",
			ACPSessionID: fmt.Sprintf("other-session-%02d", i),
			Cwd:          "/repo",
		}); err != nil {
			t.Fatalf("Upsert(other session %d) error = %v", i, err)
		}
	}

	items := store.ListByChat("bot-a", "oc_chat")
	if len(items) != maxSessionHistoryPerChat {
		t.Fatalf("len(chat history) = %d, want %d", len(items), maxSessionHistoryPerChat)
	}
	if !sessionListContains(items, "acp-session-11") || !sessionListContains(items, "acp-session-02") || sessionListContains(items, "acp-session-01") {
		t.Fatalf("chat history = %+v, want newest %d sessions", items, maxSessionHistoryPerChat)
	}
	other := store.ListByChat("bot-a", "oc_other")
	if len(other) != maxSessionHistoryPerChat {
		t.Fatalf("len(other history) = %d, want %d", len(other), maxSessionHistoryPerChat)
	}

	reloaded := NewSessionStore(storePath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	reloadedItems := reloaded.ListByChat("bot-a", "oc_chat")
	if len(reloadedItems) != maxSessionHistoryPerChat {
		t.Fatalf("len(reloaded history) = %d, want %d", len(reloadedItems), maxSessionHistoryPerChat)
	}
	if !sessionListContains(reloadedItems, "acp-session-11") || !sessionListContains(reloadedItems, "acp-session-02") || sessionListContains(reloadedItems, "acp-session-01") {
		t.Fatalf("reloaded history = %+v, want newest %d sessions", reloadedItems, maxSessionHistoryPerChat)
	}
}

func TestSessionStoreUpsertWithDefaultTitlePersistsChatSequence(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	store := NewSessionStore(storePath)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}

	first, err := store.UpsertWithDefaultTitle(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          "/repo",
	})
	if err != nil {
		t.Fatalf("UpsertWithDefaultTitle(first) error = %v", err)
	}
	if first.Title != "session#1" || first.ManualTitle {
		t.Fatalf("first session title = %q manual=%v, want automatic session#1", first.Title, first.ManualTitle)
	}

	second, err := store.UpsertWithDefaultTitle(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-2",
		Cwd:          "/repo",
	})
	if err != nil {
		t.Fatalf("UpsertWithDefaultTitle(second) error = %v", err)
	}
	if second.Title != "session#2" {
		t.Fatalf("second session title = %q, want session#2", second.Title)
	}
	chat, ok := store.GetChat(ChatKey{BotID: key.BotID, ChatID: key.ChatID})
	if !ok || chat.NextSessionSeq != 3 {
		t.Fatalf("chat = %+v ok=%v, want next sequence 3", chat, ok)
	}

	reloaded := NewSessionStore(storePath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	third, err := reloaded.UpsertWithDefaultTitle(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-3",
		Cwd:          "/repo",
	})
	if err != nil {
		t.Fatalf("UpsertWithDefaultTitle(third) error = %v", err)
	}
	if third.Title != "session#3" {
		t.Fatalf("third session title = %q, want session#3", third.Title)
	}
}

func TestSessionStoreListByMainIsolatesSources(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	imKey := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	scheduleKey := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}
	commentKey := SessionKey{BotID: "bot-a", Source: "drive_comment", MainID: "docx:token", SubID: "comment-1"}
	for _, session := range []Session{
		{Key: imKey, AgentName: "traex", ACPSessionID: "im-session", Cwd: "/repo"},
		{Key: scheduleKey, AgentName: "traex", ACPSessionID: "schedule-session", Cwd: "/repo"},
		{Key: commentKey, AgentName: "traex", ACPSessionID: "comment-session", Cwd: "/repo"},
	} {
		if err := store.Upsert(session); err != nil {
			t.Fatalf("Upsert(%s) error = %v", session.ACPSessionID, err)
		}
	}

	scheduleItems := store.ListByMain(SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily"})
	if len(scheduleItems) != 1 || scheduleItems[0].ACPSessionID != "schedule-session" {
		t.Fatalf("schedule history = %+v, want only schedule session", scheduleItems)
	}
	commentItems := store.ListByMain(SessionKey{BotID: "bot-a", Source: "drive_comment", MainID: "docx:token"})
	if len(commentItems) != 1 || commentItems[0].ACPSessionID != "comment-session" {
		t.Fatalf("comment history = %+v, want only comment session", commentItems)
	}
	imItems := store.ListByChat("bot-a", "oc_chat")
	if len(imItems) != 1 || imItems[0].ACPSessionID != "im-session" {
		t.Fatalf("im history = %+v, want only im session", imItems)
	}
}

func TestSessionStoreUpsertWithDefaultTitleSupportsNonIMParent(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	store := NewSessionStore(storePath)
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily"}

	first, err := store.UpsertWithDefaultTitle(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "schedule-session-1",
		Cwd:          "/repo",
	})
	if err != nil {
		t.Fatalf("UpsertWithDefaultTitle(first) error = %v", err)
	}
	if first.Title != "session#1" || first.ManualTitle {
		t.Fatalf("first title = %q manual=%v, want automatic session#1", first.Title, first.ManualTitle)
	}
	if _, ok := store.GetChat(ChatKey{BotID: "bot-a", ChatID: "task:daily"}); ok {
		t.Fatal("non-IM default title should not create chat config")
	}

	second, err := store.UpsertWithDefaultTitle(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "schedule-session-2",
		Cwd:          "/repo",
	})
	if err != nil {
		t.Fatalf("UpsertWithDefaultTitle(second) error = %v", err)
	}
	if second.Title != "session#2" {
		t.Fatalf("second title = %q, want session#2", second.Title)
	}
}

func TestSessionStoreWritesCompactRecoverableSessionState(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	store := NewSessionStore(storePath)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	session := Session{
		Key:               key,
		Title:             "ready session",
		AgentName:         "traex",
		ACPSessionID:      "acp-session-1",
		Cwd:               "/repo",
		ContextWindow:     &acp.ContextWindowUsage{Used: 160000, Size: 200000},
		AutoCompact:       true,
		AutoCompactPct:    80,
		LastAutoCompactAt: ptrTime(time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)),
		AvailableCommands: []acp.AvailableCommand{
			{Name: "review", Description: "Review my current changes and find issues", Input: &acp.AvailableCommandInput{Hint: "optional custom review instructions"}},
		},
		ConfigOptions: []acp.SessionConfigOption{
			{
				ID:           "model",
				Name:         "Model",
				Description:  "Large model selection description",
				Category:     "model",
				Type:         "select",
				CurrentValue: "gpt-5.5",
				Options: []acp.SessionConfigOptionValue{
					{Value: "gpt-5.5", Name: "GPT-5.5", Description: "default model"},
					{Value: "gpt-5.6", Name: "GPT-5.6", Description: "next model"},
				},
			},
		},
		Models: &acp.SessionModelState{
			CurrentModelID: "gpt-5.5",
			AvailableModels: []acp.SessionModel{
				{ModelID: "gpt-5.5", Name: "GPT-5.5", Description: "default model"},
				{ModelID: "gpt-5.6", Name: "GPT-5.6", Description: "next model"},
			},
		},
		Mode: &acp.SessionModeState{
			CurrentModeID: "default",
			AvailableModes: []acp.SessionMode{
				{ModeID: "default", Name: "Workspace Edit", Description: "can edit files"},
				{ModeID: "plan", Name: "Plan", Description: "inspect only"},
			},
		},
	}

	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if got, ok := store.Get(key); !ok {
		t.Fatalf("Get() ok = false, want true")
	} else if len(got.AvailableCommands) != 1 || len(got.ConfigOptions[0].Options) != 2 || len(got.Models.AvailableModels) != 2 || len(got.Mode.AvailableModes) != 2 {
		t.Fatalf("in-memory session was compacted: %+v", got)
	}

	data, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("ReadFile(sessions.json) error = %v", err)
	}
	var file sessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("Unmarshal(sessions.json) error = %v", err)
	}
	if len(file.Sessions) != 1 {
		t.Fatalf("len(file.Sessions) = %d, want 1", len(file.Sessions))
	}
	persisted := file.Sessions[0]
	if persisted.Key.Source != "" || persisted.Key.MainID != "" || persisted.Key.ChatID != "oc_chat" {
		t.Fatalf("persisted IM key = %+v, want legacy chat/thread JSON shape", persisted.Key)
	}
	if len(persisted.AvailableCommands) != 0 {
		t.Fatalf("AvailableCommands persisted = %+v, want omitted", persisted.AvailableCommands)
	}
	if len(persisted.ConfigOptions) != 1 {
		t.Fatalf("ConfigOptions persisted = %+v, want compact current option", persisted.ConfigOptions)
	}
	if len(persisted.ConfigOptions[0].Options) != 0 || persisted.ConfigOptions[0].Description != "" {
		t.Fatalf("ConfigOptions[0] = %+v, want no large descriptions/options", persisted.ConfigOptions[0])
	}
	if persisted.ConfigOptions[0].CurrentValue != "gpt-5.5" {
		t.Fatalf("ConfigOptions[0].CurrentValue = %v, want gpt-5.5", persisted.ConfigOptions[0].CurrentValue)
	}
	if persisted.Models == nil || persisted.Models.CurrentModelID != "gpt-5.5" || len(persisted.Models.AvailableModels) != 0 {
		t.Fatalf("Models persisted = %+v, want current model only", persisted.Models)
	}
	if persisted.Mode == nil || persisted.Mode.CurrentModeID != "default" || len(persisted.Mode.AvailableModes) != 0 {
		t.Fatalf("Mode persisted = %+v, want current mode only", persisted.Mode)
	}
	if persisted.ContextWindow == nil || persisted.ContextWindow.Used != 160000 || persisted.ContextWindow.Size != 200000 || !persisted.AutoCompact || persisted.AutoCompactPct != 80 || persisted.LastAutoCompactAt == nil {
		t.Fatalf("compact state persisted = %+v, want context and compact config", persisted)
	}
	if len(file.History) != 1 || len(file.History[0].AvailableCommands) != 0 {
		t.Fatalf("History persisted = %+v, want compact history entry", file.History)
	}
}

func TestSessionStoreBindMessageToSessionPersistsAndTracksLogicalSession(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	store := NewSessionStore(storePath)
	key := SessionKey{BotID: "bot-a", Source: sessionSourceSchedule, MainID: "task:daily", SubID: "run:1"}
	session := Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-run-1",
		Cwd:          "/repo",
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	binding, err := store.BindMessageToSession(MessageSessionBinding{
		BotID:      " bot-a ",
		ChatID:     " oc_chat ",
		MessageID:  " om_result ",
		SessionKey: key,
	})
	if err != nil {
		t.Fatalf("BindMessageToSession() error = %v", err)
	}
	if binding.BotID != "bot-a" || binding.ChatID != "oc_chat" || binding.MessageID != "om_result" || binding.SessionKey != key {
		t.Fatalf("binding = %+v, want normalized binding", binding)
	}
	got, gotBinding, ok := store.SessionForMessage("bot-a", "oc_chat", "om_result")
	if !ok {
		t.Fatalf("SessionForMessage() ok=false binding=%+v", gotBinding)
	}
	if got.ACPSessionID != "acp-run-1" || got.Key != key {
		t.Fatalf("session = %+v, want %+v", got, session)
	}

	reloaded := NewSessionStore(storePath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got, gotBinding, ok = reloaded.SessionForMessage("bot-a", "oc_chat", "om_result")
	if !ok {
		t.Fatalf("reloaded SessionForMessage() ok=false binding=%+v", gotBinding)
	}
	if got.ACPSessionID != "acp-run-1" || got.Key != key {
		t.Fatalf("reloaded session = %+v, want %+v", got, session)
	}
	replacement := session
	replacement.ACPSessionID = "acp-run-2"
	if err := reloaded.Upsert(replacement); err != nil {
		t.Fatalf("Upsert(replacement) error = %v", err)
	}
	got, gotBinding, ok = reloaded.SessionForMessage("bot-a", "oc_chat", "om_result")
	if !ok {
		t.Fatalf("SessionForMessage() after ACP refresh ok=false binding=%+v", gotBinding)
	}
	if got.ACPSessionID != "acp-run-2" || got.Key != key {
		t.Fatalf("session after ACP refresh = %+v, want refreshed logical session %+v", got, replacement)
	}
}

func TestSessionStoreFirstMessageForSessionReturnsEarliestMatchingBinding(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	store := NewSessionStore(storePath)
	key := SessionKey{BotID: "bot-a", Source: sessionSourceDriveComment, MainID: "docx:token", SubID: "comment-1"}
	createdAt := time.Now().Add(-time.Minute)
	for _, binding := range []MessageSessionBinding{
		{
			BotID:      "bot-a",
			ChatID:     "oc_trace",
			MessageID:  "om_child",
			SessionKey: key,
			CreatedAt:  createdAt.Add(time.Second),
		},
		{
			BotID:      "bot-a",
			ChatID:     "oc_trace",
			MessageID:  "om_root",
			SessionKey: key,
			CreatedAt:  createdAt,
		},
		{
			BotID:      "bot-a",
			ChatID:     "oc_other",
			MessageID:  "om_other_chat",
			SessionKey: key,
			CreatedAt:  createdAt.Add(-time.Second),
		},
	} {
		if _, err := store.BindMessageToSession(binding); err != nil {
			t.Fatalf("BindMessageToSession(%+v) error = %v", binding, err)
		}
	}

	got, ok := store.FirstMessageForSession("bot-a", "oc_trace", key)
	if !ok || got.MessageID != "om_root" {
		t.Fatalf("FirstMessageForSession() = %+v, %v, want om_root", got, ok)
	}

	reloaded := NewSessionStore(storePath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got, ok = reloaded.FirstMessageForSession("bot-a", "oc_trace", key)
	if !ok || got.MessageID != "om_root" {
		t.Fatalf("reloaded FirstMessageForSession() = %+v, %v, want om_root", got, ok)
	}
}

func TestSessionStoreSessionCopiesDoNotShareMutableState(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	session := Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          "/repo",
		ACPMeta: map[string]any{
			"source": "runtime",
			"nested": map[string]any{
				"items": []any{
					map[string]any{"name": "original"},
				},
			},
		},
		AvailableCommands: []acp.AvailableCommand{
			{Name: "review", Input: &acp.AvailableCommandInput{Hint: "original"}},
		},
		ConfigOptions: []acp.SessionConfigOption{
			{
				ID: "model",
				Options: []acp.SessionConfigOptionValue{
					{Value: "gpt-5.6", Name: "GPT-5.6"},
				},
			},
		},
		Models: &acp.SessionModelState{
			CurrentModelID:  "gpt-5.6",
			AvailableModels: []acp.SessionModel{{ModelID: "gpt-5.6", Name: "GPT-5.6"}},
		},
		Mode: &acp.SessionModeState{
			CurrentModeID:  "default",
			AvailableModes: []acp.SessionMode{{ModeID: "default", Name: "Default"}},
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	session.ACPMeta["source"] = "input changed"
	session.ACPMeta["nested"].(map[string]any)["items"].([]any)[0].(map[string]any)["name"] = "input changed"
	session.AvailableCommands[0].Input.Hint = "input changed"
	session.ConfigOptions[0].Options[0].Name = "input changed"
	session.Models.AvailableModels[0].Name = "input changed"
	session.Mode.CurrentModeID = "input changed"

	got, ok := store.Get(key)
	if !ok {
		t.Fatal("Get() ok = false, want true")
	}
	got.ACPMeta["source"] = "get changed"
	got.ACPMeta["nested"].(map[string]any)["items"].([]any)[0].(map[string]any)["name"] = "get changed"
	got.AvailableCommands[0].Input.Hint = "get changed"
	got.ConfigOptions[0].Options[0].Name = "get changed"
	got.Models.AvailableModels[0].Name = "get changed"
	got.Mode.CurrentModeID = "get changed"
	history := store.ListByChat(key.BotID, key.ChatID)
	if len(history) != 1 {
		t.Fatalf("ListByChat() = %+v, want one session", history)
	}
	history[0].Models.CurrentModelID = "history changed"

	persisted, ok := store.Get(key)
	if !ok {
		t.Fatal("Get(persisted) ok = false, want true")
	}
	if persisted.ACPMeta["source"] != "runtime" ||
		persisted.ACPMeta["nested"].(map[string]any)["items"].([]any)[0].(map[string]any)["name"] != "original" ||
		persisted.AvailableCommands[0].Input.Hint != "original" ||
		persisted.ConfigOptions[0].Options[0].Name != "GPT-5.6" ||
		persisted.Models.CurrentModelID != "gpt-5.6" ||
		persisted.Models.AvailableModels[0].Name != "GPT-5.6" ||
		persisted.Mode.CurrentModeID != "default" {
		t.Fatalf("persisted session shares mutable state: %+v", persisted)
	}
}

func TestSessionStoreResumeSessionIfCurrentRejectsStaleSelection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	for i := 1; i <= 3; i++ {
		if err := store.Upsert(Session{
			Key:          key,
			Title:        fmt.Sprintf("会话%d", i),
			AgentName:    "traex",
			ACPSessionID: fmt.Sprintf("acp-session-%d", i),
			Cwd:          fmt.Sprintf("/repo/%d", i),
		}); err != nil {
			t.Fatalf("Upsert(session %d) error = %v", i, err)
		}
	}

	got, restored, err := store.ResumeSessionIfCurrent(key, "acp-session-2", "acp-session-1")
	if err != nil {
		t.Fatalf("ResumeSessionIfCurrent(stale) error = %v", err)
	}
	if restored {
		t.Fatalf("ResumeSessionIfCurrent(stale) restored %+v, want rejection", got)
	}
	current, ok := store.Get(key)
	if !ok || current.ACPSessionID != "acp-session-3" {
		t.Fatalf("current after stale selection = %+v, %v; want session 3", current, ok)
	}

	got, restored, err = store.ResumeSessionIfCurrent(key, "acp-session-3", "acp-session-1")
	if err != nil {
		t.Fatalf("ResumeSessionIfCurrent(current) error = %v", err)
	}
	if !restored || got.ACPSessionID != "acp-session-1" || got.Cwd != "/repo/1" {
		t.Fatalf("ResumeSessionIfCurrent(current) = %+v, %v; want session 1", got, restored)
	}
}

func TestSessionStoreWriteFailureRestoresMemoryState(t *testing.T) {
	dir := t.TempDir()
	store := NewSessionStore(filepath.Join(dir, "sessions.json"))
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	if _, err := store.UpsertWithDefaultTitle(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          "/repo/1",
	}); err != nil {
		t.Fatalf("UpsertWithDefaultTitle(initial) error = %v", err)
	}
	chatKey := ChatKey{BotID: key.BotID, ChatID: key.ChatID}
	if _, err := store.UpdateChat(ChatConfig{Key: chatKey}, func(chat *ChatConfig) {
		chat.HideTools = true
	}); err != nil {
		t.Fatalf("UpdateChat(initial) error = %v", err)
	}

	blockingPath := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blockingPath, []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile(blocking path) error = %v", err)
	}
	store.path = filepath.Join(blockingPath, "sessions.json")

	if _, err := store.UpsertWithDefaultTitle(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-2",
		Cwd:          "/repo/2",
	}); err == nil {
		t.Fatal("UpsertWithDefaultTitle(write failure) error = nil, want error")
	}
	current, ok := store.Get(key)
	if !ok || current.ACPSessionID != "acp-session-1" || current.Title != "session#1" {
		t.Fatalf("current after write failure = %+v, %v; want initial session", current, ok)
	}
	history := store.ListByChat(key.BotID, key.ChatID)
	if len(history) != 1 || history[0].ACPSessionID != "acp-session-1" {
		t.Fatalf("history after write failure = %+v, want initial session only", history)
	}
	chat, ok := store.GetChat(chatKey)
	if !ok || chat.NextSessionSeq != 2 || !chat.HideTools {
		t.Fatalf("chat after session write failure = %+v, %v; want initial sequence and config", chat, ok)
	}

	if _, err := store.UpdateChat(chat, func(latest *ChatConfig) {
		latest.HideTools = false
		latest.HidePlans = true
	}); err == nil {
		t.Fatal("UpdateChat(write failure) error = nil, want error")
	}
	chat, ok = store.GetChat(chatKey)
	if !ok || chat.NextSessionSeq != 2 || !chat.HideTools || chat.HidePlans {
		t.Fatalf("chat after config write failure = %+v, %v; want initial config", chat, ok)
	}
}

func TestSessionStoreLoadMissingFileClearsState(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	store := NewSessionStore(storePath)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	if _, err := store.UpsertWithDefaultTitle(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          "/repo",
	}); err != nil {
		t.Fatalf("UpsertWithDefaultTitle() error = %v", err)
	}
	if err := store.UpsertChat(ChatConfig{
		Key:       ChatKey{BotID: key.BotID, ChatID: key.ChatID},
		HideTools: true,
	}); err != nil {
		t.Fatalf("UpsertChat() error = %v", err)
	}
	if err := os.Remove(storePath); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	if err := store.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if store.Count() != 0 {
		t.Fatalf("Count() = %d, want empty store after missing file load", store.Count())
	}
	if sessions := store.ListByChat("bot-a", "oc_chat"); len(sessions) != 0 {
		t.Fatalf("ListByChat() = %+v, want empty history", sessions)
	}
	if chat, ok := store.GetChat(ChatKey{BotID: key.BotID, ChatID: key.ChatID}); ok {
		t.Fatalf("GetChat() = %+v, want no chat config", chat)
	}
}

func TestSessionStoreUpdateChatAppliesUpdateToLatestState(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := ChatKey{BotID: "bot-a", ChatID: "oc_chat"}
	stale := ChatConfig{Key: key, HideTools: true}
	if err := store.UpsertChat(ChatConfig{
		Key:             key,
		AgentName:       "traex",
		WikiDisabled:    true,
		WikiIntervalSec: 60,
	}); err != nil {
		t.Fatalf("UpsertChat(latest) error = %v", err)
	}

	updated, err := store.UpdateChat(stale, func(chat *ChatConfig) {
		chat.HideStatusBar = true
	})
	if err != nil {
		t.Fatalf("UpdateChat() error = %v", err)
	}
	if updated.AgentName != "traex" || !updated.WikiDisabled || updated.WikiIntervalSec != 60 {
		t.Fatalf("UpdateChat() = %+v, want latest unrelated fields preserved", updated)
	}
	if !updated.HideStatusBar {
		t.Fatalf("UpdateChat() = %+v, want declared field updated", updated)
	}
	if updated.HideTools {
		t.Fatalf("UpdateChat() = %+v, want stale unrelated field ignored", updated)
	}

	persisted, ok := store.GetChat(key)
	if !ok || persisted != updated {
		t.Fatalf("GetChat() = %+v, %v; want persisted update %+v", persisted, ok, updated)
	}
}

func TestSessionStoreInsertChatIfAbsentPreservesExistingConfig(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := ChatKey{BotID: "bot-a", ChatID: "oc_chat"}
	inserted, err := store.InsertChatIfAbsent(ChatConfig{
		Key:             key,
		HideStatusBar:   true,
		HideUsageDetail: true,
		WikiIntervalSec: 60,
	})
	if err != nil {
		t.Fatalf("InsertChatIfAbsent(first) error = %v", err)
	}
	if !inserted {
		t.Fatal("InsertChatIfAbsent(first) inserted = false, want true")
	}

	inserted, err = store.InsertChatIfAbsent(ChatConfig{
		Key:             key,
		AgentName:       "traex",
		WikiDisabled:    true,
		WikiIntervalSec: 300,
	})
	if err != nil {
		t.Fatalf("InsertChatIfAbsent(existing) error = %v", err)
	}
	if inserted {
		t.Fatal("InsertChatIfAbsent(existing) inserted = true, want false")
	}
	chat, ok := store.GetChat(key)
	if !ok || !chat.HideStatusBar || !chat.HideUsageDetail || chat.WikiIntervalSec != 60 {
		t.Fatalf("GetChat() = %+v, %v; want first config preserved", chat, ok)
	}
	if chat.AgentName != "" || chat.WikiDisabled {
		t.Fatalf("GetChat() = %+v, want later candidate ignored", chat)
	}
}

func TestSessionStoreReplaceCurrentACPSessionPreservesLatestState(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	current := Session{
		Key:          key,
		Title:        "手动标题",
		ManualTitle:  true,
		AgentName:    "traex",
		ACPSessionID: "acp-session-old",
		Cwd:          "/repo",
	}
	if err := store.Upsert(current); err != nil {
		t.Fatalf("Upsert(current) error = %v", err)
	}
	replacement := current
	replacement.Title = "旧快照标题"
	replacement.ManualTitle = false
	replacement.ACPSessionID = "acp-session-refreshed"
	replacement.ACPMeta = map[string]any{"refreshed": true}
	replacement.AvailableCommands = []acp.AvailableCommand{{Name: "review"}}

	updated, replaced, err := store.ReplaceCurrentACPSession("acp-session-old", replacement)
	if err != nil {
		t.Fatalf("ReplaceCurrentACPSession() error = %v", err)
	}
	if !replaced {
		t.Fatal("ReplaceCurrentACPSession() replaced = false, want true")
	}
	if updated.Title != current.Title || !updated.ManualTitle {
		t.Fatalf("updated session = %+v, want latest manual title preserved", updated)
	}
	if updated.ACPSessionID != replacement.ACPSessionID || !sessionHasCommand(updated, "review") {
		t.Fatalf("updated session = %+v, want replacement runtime state", updated)
	}

	newCurrent := current
	newCurrent.ACPSessionID = "acp-session-new"
	newCurrent.Title = "新会话"
	newCurrent.ManualTitle = false
	if err := store.Upsert(newCurrent); err != nil {
		t.Fatalf("Upsert(newCurrent) error = %v", err)
	}
	got, replaced, err := store.ReplaceCurrentACPSession("acp-session-old", replacement)
	if err != nil {
		t.Fatalf("ReplaceCurrentACPSession(stale) error = %v", err)
	}
	if replaced {
		t.Fatal("ReplaceCurrentACPSession(stale) replaced = true, want false")
	}
	if got.ACPSessionID != newCurrent.ACPSessionID || got.Title != newCurrent.Title {
		t.Fatalf("session after stale replacement = %+v, want new current session", got)
	}
}

func TestSessionStoreLoadNormalizesIdentityFields(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	file := sessionFile{
		Version: 1,
		Sessions: []Session{
			{
				Key:          SessionKey{BotID: " bot-a ", ChatID: " oc_chat ", SubID: " omt_thread "},
				AgentName:    " traex ",
				ACPSessionID: " acp-session-current ",
				Cwd:          "/repo",
			},
		},
		History: []Session{
			{
				Key:          SessionKey{BotID: " bot-a ", ChatID: " oc_chat ", SubID: " old_thread "},
				AgentName:    " traex ",
				ACPSessionID: " acp-session-old ",
				Cwd:          "/repo",
			},
		},
		Chats: []ChatConfig{
			{
				Key:       ChatKey{BotID: " bot-a ", ChatID: " oc_chat "},
				AgentName: " claude ",
				HideTools: true,
			},
		},
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("Marshal(session file) error = %v", err)
	}
	if err := os.WriteFile(storePath, data, 0o600); err != nil {
		t.Fatalf("WriteFile(sessions.json) error = %v", err)
	}

	store := NewSessionStore(storePath)
	if err := store.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	session, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "omt_thread"})
	if !ok {
		t.Fatalf("Get(normalized session key) ok = false, want true")
	}
	if session.Key != normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "omt_thread"}) {
		t.Fatalf("session key = %+v, want normalized key", session.Key)
	}
	if session.AgentName != "traex" || session.ACPSessionID != "acp-session-current" {
		t.Fatalf("session agent/session = %q/%q, want normalized values", session.AgentName, session.ACPSessionID)
	}
	if _, ok := store.Get(SessionKey{BotID: " bot-a ", ChatID: " oc_chat ", SubID: " omt_thread "}); !ok {
		t.Fatalf("Get(spaced session key) ok = false, want lookup to normalize key")
	}

	chat, ok := store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_chat"})
	if !ok {
		t.Fatalf("GetChat(normalized key) ok = false, want true")
	}
	if chat.Key != (ChatKey{BotID: "bot-a", ChatID: "oc_chat"}) || chat.AgentName != "claude" || !chat.HideTools {
		t.Fatalf("chat = %+v, want normalized key/agent and preserved fields", chat)
	}

	history := store.ListByChat(" bot-a ", " oc_chat ")
	if len(history) != 2 {
		t.Fatalf("len(history) = %d, want current and historical sessions", len(history))
	}
	if !sessionListContains(history, "acp-session-current") || !sessionListContains(history, "acp-session-old") {
		t.Fatalf("history = %+v, want normalized session IDs", history)
	}
}

func TestSessionStoreWriteUsesStableOrder(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	store := NewSessionStore(storePath)

	sessions := []Session{
		{
			Key:          SessionKey{BotID: "bot-b", ChatID: "oc_1"},
			AgentName:    "traex",
			ACPSessionID: "session-b",
			Cwd:          "/repo",
		},
		{
			Key:          SessionKey{BotID: "bot-a", ChatID: "oc_2", SubID: "thread-b"},
			AgentName:    "traex",
			ACPSessionID: "session-a2b",
			Cwd:          "/repo",
		},
		{
			Key:          SessionKey{BotID: "bot-a", ChatID: "oc_1"},
			AgentName:    "traex",
			ACPSessionID: "session-a1",
			Cwd:          "/repo",
		},
		{
			Key:          SessionKey{BotID: "bot-a", ChatID: "oc_2", SubID: "thread-a"},
			AgentName:    "traex",
			ACPSessionID: "session-a2a",
			Cwd:          "/repo",
		},
	}
	for _, session := range sessions {
		if err := store.Upsert(session); err != nil {
			t.Fatalf("Upsert(%s) error = %v", session.ACPSessionID, err)
		}
	}
	for _, chat := range []ChatConfig{
		{Key: ChatKey{BotID: "bot-b", ChatID: "oc_1"}, MentionOptional: true},
		{Key: ChatKey{BotID: "bot-a", ChatID: "oc_2"}, WikiDisabled: true},
		{Key: ChatKey{BotID: "bot-a", ChatID: "oc_1"}, HideTools: true},
	} {
		if err := store.UpsertChat(chat); err != nil {
			t.Fatalf("UpsertChat(%+v) error = %v", chat.Key, err)
		}
	}
	for _, binding := range []MessageSessionBinding{
		{BotID: "bot-b", ChatID: "oc_1", MessageID: "om_b", SessionKey: sessions[0].Key},
		{BotID: "bot-a", ChatID: "oc_2", MessageID: "om_c", SessionKey: sessions[1].Key},
		{BotID: "bot-a", ChatID: "oc_1", MessageID: "om_a", SessionKey: sessions[2].Key},
	} {
		if _, err := store.BindMessageToSession(binding); err != nil {
			t.Fatalf("BindMessageToSession(%+v) error = %v", binding, err)
		}
	}

	data, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("ReadFile(sessions.json) error = %v", err)
	}
	var file sessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("Unmarshal(sessions.json) error = %v", err)
	}

	gotSessions := make([]SessionKey, 0, len(file.Sessions))
	for _, session := range file.Sessions {
		gotSessions = append(gotSessions, session.Key)
	}
	wantSessions := []SessionKey{
		{BotID: "bot-a", ChatID: "oc_1"},
		{BotID: "bot-a", ChatID: "oc_2", SubID: "thread-a"},
		{BotID: "bot-a", ChatID: "oc_2", SubID: "thread-b"},
		{BotID: "bot-b", ChatID: "oc_1"},
	}
	if !sessionKeysEqual(gotSessions, wantSessions) {
		t.Fatalf("session order = %+v, want %+v", gotSessions, wantSessions)
	}

	gotChats := make([]ChatKey, 0, len(file.Chats))
	for _, chat := range file.Chats {
		gotChats = append(gotChats, chat.Key)
	}
	wantChats := []ChatKey{
		{BotID: "bot-a", ChatID: "oc_1"},
		{BotID: "bot-a", ChatID: "oc_2"},
		{BotID: "bot-b", ChatID: "oc_1"},
	}
	if !chatKeysEqual(gotChats, wantChats) {
		t.Fatalf("chat order = %+v, want %+v", gotChats, wantChats)
	}
	gotMessages := make([]messageBindingKey, 0, len(file.Messages))
	for _, binding := range file.Messages {
		gotMessages = append(gotMessages, messageBindingKeyFromBinding(binding))
	}
	wantMessages := []messageBindingKey{
		{BotID: "bot-a", ChatID: "oc_1", MessageID: "om_a"},
		{BotID: "bot-a", ChatID: "oc_2", MessageID: "om_c"},
		{BotID: "bot-b", ChatID: "oc_1", MessageID: "om_b"},
	}
	if !messageBindingKeysEqual(gotMessages, wantMessages) {
		t.Fatalf("message binding order = %+v, want %+v", gotMessages, wantMessages)
	}
}

func sessionListContains(items []Session, acpSessionID string) bool {
	for _, item := range items {
		if item.ACPSessionID == acpSessionID {
			return true
		}
	}
	return false
}

func sessionKeysEqual(got, want []SessionKey) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func chatKeysEqual(got, want []ChatKey) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func messageBindingKeysEqual(got, want []messageBindingKey) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
