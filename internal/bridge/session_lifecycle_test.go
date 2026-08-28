package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
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
				ChatID:   sessionKeyMainID(stale.Key),
				ThreadID: stale.Key.SubID,
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

func TestUpdateAutomaticSessionTitleInStoreKeepsNonIMKey(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}
	session := Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-trigger",
		Cwd:          t.TempDir(),
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	updated := updateAutomaticSessionTitleInStore(context.Background(), store, session, "生成每日报告")

	if updated.Key != key {
		t.Fatalf("updated key = %+v, want non-IM trigger key %+v", updated.Key, key)
	}
	if updated.Title != "生成每日报告" || updated.ManualTitle {
		t.Fatalf("updated title/manual = %q/%v, want automatic trigger title", updated.Title, updated.ManualTitle)
	}
	persisted, ok := store.Get(key)
	if !ok {
		t.Fatal("persisted trigger session not found")
	}
	if persisted.Key != key || persisted.Title != "生成每日报告" || persisted.ManualTitle {
		t.Fatalf("persisted session = %+v, want non-IM key with automatic title", persisted)
	}
}

func TestSessionWithACPInfoAppliesRuntimeState(t *testing.T) {
	commands := []acp.AvailableCommand{{Name: "review"}}
	options := []acp.SessionConfigOption{{ID: "model", CurrentValue: "gpt-5.6"}}
	session := sessionWithACPInfo(Session{
		Key:          SessionKey{BotID: "bot-a", Source: "drive_comment", MainID: "docx:token", SubID: "comment-1"},
		Title:        "已有标题",
		ManualTitle:  true,
		AgentName:    "traex",
		ACPSessionID: "acp-old",
		Cwd:          "/old",
	}, acp.SessionInfo{
		SessionID:         "acp-new",
		Meta:              map[string]any{"refreshed": true},
		AvailableCommands: commands,
		ConfigOptions:     options,
		Models:            &acp.SessionModelState{CurrentModelID: "gpt-5.6"},
		Mode:              &acp.SessionModeState{CurrentModeID: "plan"},
	}, "/tmp/../tmp/project", " /workspace ")
	commands[0].Name = "changed"
	options[0].ID = "changed"

	if session.ACPSessionID != "acp-new" || session.Cwd != "/tmp/project" || session.Workspace != "/workspace" {
		t.Fatalf("session runtime fields = %+v, want refreshed id/cwd/workspace", session)
	}
	if session.Title != "已有标题" || !session.ManualTitle {
		t.Fatalf("session title/manual = %q/%v, want preserved", session.Title, session.ManualTitle)
	}
	if session.ACPMeta["refreshed"] != true {
		t.Fatalf("session ACPMeta = %+v, want refreshed meta", session.ACPMeta)
	}
	if len(session.AvailableCommands) != 1 || session.AvailableCommands[0].Name != "review" {
		t.Fatalf("session commands = %+v, want cloned commands", session.AvailableCommands)
	}
	if len(session.ConfigOptions) != 1 || session.ConfigOptions[0].ID != "model" {
		t.Fatalf("session config options = %+v, want cloned config options", session.ConfigOptions)
	}
	if session.Models == nil || session.Models.CurrentModelID != "gpt-5.6" {
		t.Fatalf("session models = %+v, want refreshed models", session.Models)
	}
	if session.Mode == nil || session.Mode.CurrentModeID != "plan" {
		t.Fatalf("session mode = %+v, want refreshed mode", session.Mode)
	}
}

func TestCommitCurrentACPSessionReplacement(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := SessionKey{BotID: "bot-a", Source: "drive_comment", MainID: "docx:token", SubID: "comment-1"}
	current := Session{
		Key:          key,
		Title:        "已有标题",
		AgentName:    "traex",
		ACPSessionID: "acp-old",
		Cwd:          t.TempDir(),
	}
	if err := store.Upsert(current); err != nil {
		t.Fatalf("Upsert(current) error = %v", err)
	}
	replacement := current
	replacement.ACPSessionID = "acp-new"
	replacement.ACPMeta = map[string]any{"refreshed": true}
	candidate := &fakeSessionCandidate{runtime: &fakeRuntime{}, key: key, info: acp.SessionInfo{SessionID: "acp-new"}}

	updated, err := commitCurrentACPSessionReplacement(candidate, store, "acp-old", replacement)
	if err != nil {
		t.Fatalf("commitCurrentACPSessionReplacement() error = %v", err)
	}
	if !candidate.committed || candidate.aborted {
		t.Fatalf("candidate committed/aborted = %v/%v, want committed only", candidate.committed, candidate.aborted)
	}
	if updated.ACPSessionID != "acp-new" || updated.ACPMeta["refreshed"] != true {
		t.Fatalf("updated session = %+v, want replacement runtime state", updated)
	}
	persisted, ok := store.Get(key)
	if !ok || persisted.ACPSessionID != "acp-new" {
		t.Fatalf("persisted session = %+v ok=%v, want refreshed session", persisted, ok)
	}

	staleCandidate := &fakeSessionCandidate{runtime: &fakeRuntime{}, key: key, info: acp.SessionInfo{SessionID: "acp-stale"}}
	staleReplacement := replacement
	staleReplacement.ACPSessionID = "acp-stale"
	if _, err := commitCurrentACPSessionReplacement(staleCandidate, store, "acp-old", staleReplacement); !errors.Is(err, errCurrentSessionChanged) {
		t.Fatalf("commitCurrentACPSessionReplacement(stale) error = %v, want errCurrentSessionChanged", err)
	}
	if !staleCandidate.aborted {
		t.Fatal("stale candidate should be aborted after failed commit")
	}
}

func TestCommitCurrentACPSessionReplacementWithoutStore(t *testing.T) {
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}
	candidate := &fakeSessionCandidate{runtime: &fakeRuntime{}, key: key, info: acp.SessionInfo{SessionID: "acp-new"}}
	replacement := Session{Key: key, ACPSessionID: "acp-new"}

	updated, err := commitCurrentACPSessionReplacement(candidate, nil, "acp-old", replacement)
	if err != nil {
		t.Fatalf("commitCurrentACPSessionReplacement(nil store) error = %v", err)
	}
	if !candidate.committed || candidate.aborted {
		t.Fatalf("candidate committed/aborted = %v/%v, want committed only", candidate.committed, candidate.aborted)
	}
	if updated.ACPSessionID != "acp-new" {
		t.Fatalf("updated session = %+v, want replacement session", updated)
	}
}

func TestCreateSessionWriteFailureKeepsCurrentRuntime(t *testing.T) {
	dir := t.TempDir()
	store := NewSessionStore(filepath.Join(dir, "sessions.json"))
	key := imSessionKey("bot-a", "oc_private", "")
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
		ChatID:    sessionKeyMainID(key),
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
