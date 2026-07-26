package bridge

import (
	"fmt"
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

func sessionListContains(items []Session, acpSessionID string) bool {
	for _, item := range items {
		if item.ACPSessionID == acpSessionID {
			return true
		}
	}
	return false
}
