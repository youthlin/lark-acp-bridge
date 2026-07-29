package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

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

func TestSessionStoreWritesCompactRecoverableSessionState(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	store := NewSessionStore(storePath)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	session := Session{
		Key:          key,
		Title:        "ready session",
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          "/repo",
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
	if len(file.History) != 1 || len(file.History[0].AvailableCommands) != 0 {
		t.Fatalf("History persisted = %+v, want compact history entry", file.History)
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

func TestSessionStoreLoadNormalizesIdentityFields(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	file := sessionFile{
		Version: 1,
		Sessions: []Session{
			{
				Key:          SessionKey{BotID: " bot-a ", ChatID: " oc_chat ", ThreadID: " omt_thread "},
				AgentName:    " traex ",
				ACPSessionID: " acp-session-current ",
				Cwd:          "/repo",
			},
		},
		History: []Session{
			{
				Key:          SessionKey{BotID: " bot-a ", ChatID: " oc_chat ", ThreadID: " old_thread "},
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

	session, ok := store.Get(SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"})
	if !ok {
		t.Fatalf("Get(normalized session key) ok = false, want true")
	}
	if session.Key != (SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}) {
		t.Fatalf("session key = %+v, want normalized key", session.Key)
	}
	if session.AgentName != "traex" || session.ACPSessionID != "acp-session-current" {
		t.Fatalf("session agent/session = %q/%q, want normalized values", session.AgentName, session.ACPSessionID)
	}
	if _, ok := store.Get(SessionKey{BotID: " bot-a ", ChatID: " oc_chat ", ThreadID: " omt_thread "}); !ok {
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
			Key:          SessionKey{BotID: "bot-a", ChatID: "oc_2", ThreadID: "thread-b"},
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
			Key:          SessionKey{BotID: "bot-a", ChatID: "oc_2", ThreadID: "thread-a"},
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
		{BotID: "bot-a", ChatID: "oc_2", ThreadID: "thread-a"},
		{BotID: "bot-a", ChatID: "oc_2", ThreadID: "thread-b"},
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
