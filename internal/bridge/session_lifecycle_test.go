package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestUpdateAutomaticSessionTitleKeepsNewerSessionState(t *testing.T) {
	tests := []struct {
		name   string
		latest func(Session) Session
	}{
		{
			name: "手动标题",
			latest: func(session Session) Session {
				session.Title = "手动标题"
				session.ManualTitle = true
				return session
			},
		},
		{
			name: "新会话映射",
			latest: func(session Session) Session {
				session.Title = "新会话"
				session.ACPSessionID = "acp-session-2"
				return session
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
			stale := testReadySession(t, store)
			latest := tt.latest(stale)
			if err := store.Upsert(latest); err != nil {
				t.Fatalf("Upsert(latest) error = %v", err)
			}
			svc := newTestService(config.Default(), store)

			got := svc.updateAutomaticSessionTitle(context.Background(), feishu.Message{
				BotID:    stale.Key.BotID,
				ChatID:   stale.Key.ChatID,
				ThreadID: stale.Key.ThreadID,
			}, stale, "自动标题")

			if got.Title != latest.Title || got.ManualTitle != latest.ManualTitle || got.ACPSessionID != latest.ACPSessionID {
				t.Fatalf("updated session = %+v, want latest state %+v", got, latest)
			}
			persisted, ok := store.Get(stale.Key)
			if !ok {
				t.Fatalf("persisted session not found")
			}
			if persisted.Title != latest.Title || persisted.ManualTitle != latest.ManualTitle || persisted.ACPSessionID != latest.ACPSessionID {
				t.Fatalf("persisted session = %+v, want latest state %+v", persisted, latest)
			}
		})
	}
}

func TestCreateSessionWriteFailureKeepsCurrentRuntime(t *testing.T) {
	dir := t.TempDir()
	store := NewSessionStore(filepath.Join(dir, "sessions.json"))
	key := SessionKey{BotID: "bot-a", ChatID: "oc_private"}
	current := Session{
		Key:          key,
		Title:        "当前会话",
		AgentName:    "traex",
		ACPSessionID: "acp-session-current",
		Cwd:          t.TempDir(),
	}
	if err := store.Upsert(current); err != nil {
		t.Fatalf("Upsert(current) error = %v", err)
	}
	blockingPath := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blockingPath, []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile(blocking path) error = %v", err)
	}
	store.path = filepath.Join(blockingPath, "sessions.json")

	newDir := t.TempDir()
	rt := &fakeRuntime{
		newSessionID:     "acp-session-candidate",
		activeSessionIDs: map[SessionKey]string{key: current.ACPSessionID},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	_, _, _, errText := svc.createSession(context.Background(), []string{"/new", newDir}, feishu.Message{
		BotID:     key.BotID,
		ChatID:    key.ChatID,
		ChatType:  "p2p",
		Workspace: filepath.Join(t.TempDir(), "workspace"),
	})
	if !strings.Contains(errText, "保存会话映射失败") {
		t.Fatalf("createSession(write failure) error = %q, want persistence failure", errText)
	}
	persisted, ok := store.Get(key)
	if !ok || persisted.ACPSessionID != current.ACPSessionID || persisted.Title != current.Title {
		t.Fatalf("persisted session = %+v, %v; want current session unchanged", persisted, ok)
	}
	if active := rt.activeSessionIDs[key]; active != current.ACPSessionID {
		t.Fatalf("active runtime session = %q, want %q", active, current.ACPSessionID)
	}
	if len(rt.abortedSessionIDs) != 1 || rt.abortedSessionIDs[0] != "acp-session-candidate" {
		t.Fatalf("aborted candidates = %+v, want candidate closed", rt.abortedSessionIDs)
	}
}
