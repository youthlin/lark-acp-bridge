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

func sessionListContains(items []Session, acpSessionID string) bool {
	for _, item := range items {
		if item.ACPSessionID == acpSessionID {
			return true
		}
	}
	return false
}
