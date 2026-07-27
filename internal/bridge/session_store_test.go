package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
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
	if chat.Key != (ChatKey{BotID: "bot-a", ChatID: "oc_chat"}) || !chat.HideTools {
		t.Fatalf("chat = %+v, want normalized key and preserved fields", chat)
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
