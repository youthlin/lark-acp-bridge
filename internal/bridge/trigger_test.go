package bridge

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func TestTriggerRequestNormalizedFillsBotIDAndClonesMetadata(t *testing.T) {
	metadata := map[string]string{"task_id": "daily"}
	req := TriggerRequest{
		Key: SessionKey{
			BotID:  " bot-a ",
			Source: " schedule ",
			MainID: " task:daily ",
			SubID:  " run:1 ",
		},
		Workspace: " /tmp/workspace ",
		AgentName: " traex ",
		Cwd:       " /tmp/project ",
		Title:     " daily task ",
		Prompt:    " run report ",
		Metadata:  metadata,
	}

	normalized := req.normalized()
	metadata["task_id"] = "changed"

	if normalized.BotID != "bot-a" {
		t.Fatalf("BotID = %q, want bot-a", normalized.BotID)
	}
	wantKey := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}
	if normalized.Key != wantKey {
		t.Fatalf("Key = %+v, want %+v", normalized.Key, wantKey)
	}
	if normalized.Workspace != "/tmp/workspace" || normalized.AgentName != "traex" || normalized.Cwd != "/tmp/project" || normalized.Title != "daily task" || normalized.Prompt != "run report" {
		t.Fatalf("normalized request = %+v, want trimmed string fields", normalized)
	}
	if normalized.Metadata["task_id"] != "daily" {
		t.Fatalf("Metadata = %+v, want cloned original value", normalized.Metadata)
	}
}

func TestTriggerRequestValidRequiresPromptAndExplicitKey(t *testing.T) {
	valid := TriggerRequest{
		BotID:  "bot-a",
		Key:    SessionKey{Source: "drive_comment", MainID: "docx:token", SubID: "comment-1"},
		Prompt: "handle comment",
	}
	if !valid.valid() {
		t.Fatalf("valid() = false, want true")
	}

	missingPrompt := valid
	missingPrompt.Prompt = " "
	if missingPrompt.valid() {
		t.Fatalf("valid() = true for empty prompt, want false")
	}

	missingKey := valid
	missingKey.Key = SessionKey{BotID: "bot-a", Source: "schedule"}
	if missingKey.valid() {
		t.Fatalf("valid() = true for incomplete key, want false")
	}
}

func TestNewTriggerResultClonesSessionMeta(t *testing.T) {
	meta := map[string]any{"mode": "auto"}
	errPrompt := errors.New("failed")
	result := newTriggerResult(
		TriggerRequest{BotID: "bot-a", Key: SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}, Prompt: "run"},
		Session{Key: SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}, ACPSessionID: "acp-1", ACPMeta: meta},
		acp.PromptResult{Text: "done"},
		"",
		false,
		errPrompt,
	)
	meta["mode"] = "manual"

	if result.Text != "done" {
		t.Fatalf("Text = %q, want fallback ACP result text", result.Text)
	}
	if result.Err != errPrompt {
		t.Fatalf("Err = %v, want original error", result.Err)
	}
	if result.ACPSessionID != "acp-1" {
		t.Fatalf("ACPSessionID = %q, want acp-1", result.ACPSessionID)
	}
	if result.ACPSessionMeta["mode"] != "auto" {
		t.Fatalf("ACPSessionMeta = %+v, want cloned original value", result.ACPSessionMeta)
	}
}

func TestPrepareTriggerRequestSelectsBotStore(t *testing.T) {
	storeA := NewSessionStore("")
	storeB := NewSessionStore("")
	svc := NewService(config.Config{}, nil)
	svc.stores = map[string]*SessionStore{
		"bot-a": storeA,
		"bot-b": storeB,
	}

	req, store, err := svc.prepareTriggerRequest(TriggerRequest{
		Key:       SessionKey{BotID: " bot-b ", Source: " schedule ", MainID: " task:daily ", SubID: " run:1 "},
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    " run report ",
	})
	if err != nil {
		t.Fatalf("prepareTriggerRequest() error = %v", err)
	}
	if store != storeB {
		t.Fatalf("store = %p, want bot-b store %p", store, storeB)
	}
	if req.BotID != "bot-b" {
		t.Fatalf("BotID = %q, want bot-b from key", req.BotID)
	}
	wantKey := SessionKey{BotID: "bot-b", Source: "schedule", MainID: "task:daily", SubID: "run:1"}
	if req.Key != wantKey {
		t.Fatalf("Key = %+v, want %+v", req.Key, wantKey)
	}
}

func TestPrepareTriggerRequestRejectsIMSource(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	_, _, err := svc.prepareTriggerRequest(TriggerRequest{
		BotID:  "bot-a",
		Key:    SessionKey{BotID: "bot-a", Source: sessionSourceIM, MainID: "oc_chat"},
		Prompt: "run",
	})
	if err == nil || !strings.Contains(err.Error(), "不能使用 IM") {
		t.Fatalf("prepareTriggerRequest() error = %v, want IM rejection", err)
	}
}

func TestPrepareTriggerRequestRequiresExplicitExecutionFields(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	base := TriggerRequest{
		BotID:     "bot-a",
		Key:       SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"},
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "run",
	}
	for _, tt := range []struct {
		name string
		edit func(*TriggerRequest)
		want string
	}{
		{name: "agent", edit: func(req *TriggerRequest) { req.AgentName = "" }, want: "agent_name"},
		{name: "cwd", edit: func(req *TriggerRequest) { req.Cwd = "" }, want: "cwd"},
		{name: "workspace", edit: func(req *TriggerRequest) { req.Workspace = "" }, want: "workspace"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.edit(&req)
			_, _, err := svc.prepareTriggerRequest(req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("prepareTriggerRequest() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestPrepareTriggerFindsExistingSession(t *testing.T) {
	key := SessionKey{BotID: "bot-a", Source: "drive_comment", MainID: "docx:token", SubID: "comment-1"}
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    " traex ",
		ACPSessionID: " acp-existing ",
		Cwd:          "/repo",
		ACPMeta:      map[string]any{"mode": "auto"},
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc := NewService(config.Config{}, store)

	prepared, err := svc.prepareTrigger(TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "handle comment",
	})
	if err != nil {
		t.Fatalf("prepareTrigger() error = %v", err)
	}
	if !prepared.hasSession {
		t.Fatal("hasSession = false, want true")
	}
	if prepared.session.ACPSessionID != "acp-existing" || prepared.session.AgentName != "traex" {
		t.Fatalf("session = %+v, want normalized existing session", prepared.session)
	}

	prepared.session.ACPMeta["mode"] = "changed"
	stored, ok := store.Get(key)
	if !ok {
		t.Fatal("stored session missing")
	}
	if stored.ACPMeta["mode"] != "auto" {
		t.Fatalf("stored meta = %+v, want original value", stored.ACPMeta)
	}
}

func TestPrepareTriggerReturnsNoSessionForNewKey(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	prepared, err := svc.prepareTrigger(TriggerRequest{
		BotID:     "bot-a",
		Key:       SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"},
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "run",
	})
	if err != nil {
		t.Fatalf("prepareTrigger() error = %v", err)
	}
	if prepared.hasSession {
		t.Fatal("hasSession = true, want false")
	}
	if prepared.session.Key.Valid() || prepared.session.ACPSessionID != "" || prepared.session.AgentName != "" {
		t.Fatalf("session = %+v, want zero value", prepared.session)
	}
}

func TestRunTriggerPromptCreatesSessionFromExplicitRequest(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Config{
		AgentList: []config.NamedAgentConfig{
			{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: "/unused-traex"}},
			{Name: "claude", AgentConfig: config.AgentConfig{Command: "claude", DefaultCwd: "/unused-claude"}},
		},
	}
	svc := NewService(cfg, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{
			SessionID: "acp-trigger",
			Meta:      map[string]any{"mode": "auto"},
		},
		promptReply: "trigger done",
	}
	svc.setRuntime(rt)
	sink := &recordingTriggerSink{}
	workspace := t.TempDir()
	cwd := t.TempDir()
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}

	result, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: workspace,
		AgentName: "claude",
		Cwd:       cwd,
		Title:     " daily trigger ",
		Prompt:    "run report",
		Sink:      sink,
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if len(rt.newCalls) != 1 {
		t.Fatalf("newCalls = %+v, want one trigger session creation", rt.newCalls)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want one trigger prompt", rt.promptCalls)
	}
	if rt.newCalls[0].Key != normalizeSessionKey(key) || rt.newCalls[0].AgentName != "claude" || rt.newCalls[0].Cwd != cwd || rt.newCalls[0].Workspace != workspace {
		t.Fatalf("new call = %+v, want explicit trigger request values", rt.newCalls[0])
	}
	if rt.promptCalls[0].Session.ACPSessionID != "acp-trigger" || rt.promptCalls[0].Text != "run report" {
		t.Fatalf("prompt call = %+v, want created trigger session and prompt text", rt.promptCalls[0])
	}
	if result.Session.ACPSessionID != "acp-trigger" || result.Session.AgentName != "claude" || result.Session.Cwd != cwd || result.Session.Workspace != workspace {
		t.Fatalf("result session = %+v, want created trigger session", result.Session)
	}
	if result.Text != "trigger done" {
		t.Fatalf("result text = %q, want trigger done", result.Text)
	}
	if !result.Session.ManualTitle || result.Session.Title != "daily trigger" {
		t.Fatalf("result session title/manual = %q/%v, want explicit title", result.Session.Title, result.Session.ManualTitle)
	}
	if sink.completes != 1 || sink.errors != 0 {
		t.Fatalf("sink complete/errors = %d/%d, want 1/0", sink.completes, sink.errors)
	}
	stored, ok := store.Get(key)
	if !ok {
		t.Fatal("stored trigger session missing")
	}
	if stored.ACPSessionID != "acp-trigger" || stored.AgentName != "claude" {
		t.Fatalf("stored session = %+v, want created trigger session", stored)
	}
}

func TestRunTriggerPromptUsesTextChunkFallback(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-trigger"},
		promptUpdates: []acp.PromptUpdate{
			{Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "hello "},
			}},
			{Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "trigger"},
			}},
		},
	}
	svc.setRuntime(rt)
	sink := &recordingTriggerSink{}
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}

	result, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "run report",
		Sink:      sink,
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if result.Text != "hello trigger" {
		t.Fatalf("result text = %q, want text chunk fallback", result.Text)
	}
	if sink.lastComplete.Text != "hello trigger" {
		t.Fatalf("sink complete text = %q, want text chunk fallback", sink.lastComplete.Text)
	}
}

func TestRunTriggerPromptUsesFinalTextAfterToolBoundary(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-trigger"},
		promptReply:    "先检查。\n中间过程。\n最终正文。",
		promptUpdates: []acp.PromptUpdate{
			{Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "先检查。"},
			}},
			{Update: acp.SessionUpdate{
				SessionUpdate: "tool_call",
				Title:         "Read comments",
			}},
			{Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "最终正文。"},
			}},
		},
	}
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", Source: "drive_comment", MainID: "docx:token", SubID: "comment-1"}

	result, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "reply comment",
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if result.Text != "最终正文。" {
		t.Fatalf("result text = %q, want final text after tool boundary", result.Text)
	}
	if result.ACPResult.Text != "先检查。\n中间过程。\n最终正文。" {
		t.Fatalf("raw result text = %q, want preserved ACP result", result.ACPResult.Text)
	}
}

func TestRunTriggerPromptSendsReadableUpdateResults(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-trigger"},
		promptUpdates: []acp.PromptUpdate{
			{Update: acp.SessionUpdate{
				SessionUpdate: "status",
				Message:       "preparing",
			}},
			{Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "answer"},
			}},
		},
		promptReply: "done",
	}
	svc.setRuntime(rt)
	sink := &recordingTriggerSink{}
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}

	_, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "run report",
		Sink:      sink,
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if len(sink.updateResults) != 2 {
		t.Fatalf("sink updates = %d, want 2", len(sink.updateResults))
	}
	if got := sink.updateResults[0].Text; !strings.Contains(got, "preparing") {
		t.Fatalf("first update text = %q, want readable status text", got)
	}
	if sink.updateResults[0].Update.Update.SessionUpdate != "status" {
		t.Fatalf("first update raw = %+v, want original status update", sink.updateResults[0].Update)
	}
	if got := sink.updateResults[1].Text; got != "answer" {
		t.Fatalf("second update text = %q, want chunk text", got)
	}
	if sink.updateResults[1].Update.Update.SessionUpdate != "agent_message_chunk" {
		t.Fatalf("second update raw = %+v, want original chunk update", sink.updateResults[1].Update)
	}
}

func TestRunTriggerPromptUpdatesAutomaticTitle(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}
	svc := NewService(cfg, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-trigger"},
		promptReply:    "done",
	}
	svc.setRuntime(rt)
	sink := &recordingTriggerSink{}
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}

	result, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "  generate   daily   report  ",
		Sink:      sink,
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if result.Session.Title != "generate daily report" || result.Session.ManualTitle {
		t.Fatalf("result title/manual = %q/%v, want automatic title", result.Session.Title, result.Session.ManualTitle)
	}
	stored, ok := store.Get(key)
	if !ok {
		t.Fatal("stored trigger session missing")
	}
	if stored.Title != "generate daily report" || stored.ManualTitle {
		t.Fatalf("stored title/manual = %q/%v, want automatic title", stored.Title, stored.ManualTitle)
	}
	if sink.lastComplete.Session.Title != "generate daily report" {
		t.Fatalf("sink complete title = %q, want updated title", sink.lastComplete.Session.Title)
	}
}

func TestRunTriggerPromptRecordsTokenUsage(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	usageStore := NewTokenUsageStore(filepath.Join(workspace, "token_usage.json"))
	cfg := config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}
	svc := NewService(cfg, store)
	svc.usageStores["bot-a"] = usageStore
	svc.setRuntime(&fakeRuntime{
		newSessionInfo: acp.SessionInfo{
			SessionID: "acp-trigger",
			ConfigOptions: []acp.SessionConfigOption{
				{ID: "model", Category: "model", CurrentValue: "gpt-5.5"},
			},
		},
		promptResult: acp.PromptResult{
			Text: "done",
			Usage: acp.TokenUsage{
				InputTokens:  10,
				OutputTokens: 5,
			},
		},
	})
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}

	_, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: workspace,
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "generate daily report",
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	usageStore.mu.Lock()
	records := append([]TokenUsageRecord(nil), usageStore.records...)
	usageStore.mu.Unlock()
	if len(records) != 1 {
		t.Fatalf("usage records = %+v, want one trigger usage record", records)
	}
	record := records[0]
	if record.BotID != "bot-a" || record.Source != "schedule" || record.MainID != "task:daily" || record.SubID != "run:1" {
		t.Fatalf("usage record key = %+v, want trigger key fields", record)
	}
	if record.SessionID != "acp-trigger" || record.AgentName != "traex" || record.Model != "gpt-5.5" || record.Usage.TotalTokens != 15 {
		t.Fatalf("usage record = %+v, want trigger session token usage", record)
	}
}

func TestRunTriggerPromptRecordsSanitizedACPError(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-existing", Cwd: t.TempDir()}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	cfg := config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}
	svc := NewService(cfg, store)
	svc.setRuntime(&fakeRuntime{
		promptErrors: []error{errors.New("trigger failed token secret-token")},
	})

	_, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "generate daily report",
	})
	if err == nil {
		t.Fatal("runTriggerPrompt() error = nil, want trigger failure")
	}
	snapshot, ok := svc.acpErrorSnapshot(session)
	if !ok {
		t.Fatal("acpErrorSnapshot() missing after trigger failure")
	}
	if snapshot.operation != "trigger prompt" || !strings.Contains(snapshot.message, "trigger failed") || !strings.Contains(snapshot.message, "token [已隐藏]") {
		t.Fatalf("snapshot = %+v, want sanitized trigger prompt error", snapshot)
	}
	if strings.Contains(snapshot.message, "secret-token") {
		t.Fatalf("snapshot = %+v, should not include raw token", snapshot)
	}
}

func TestRunTriggerPromptKeepsManualTitle(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}
	svc := NewService(cfg, store)
	svc.setRuntime(&fakeRuntime{newSessionInfo: acp.SessionInfo{SessionID: "acp-trigger"}, promptReply: "done"})
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}

	result, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Title:     "manual title",
		Prompt:    "generate daily report",
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if result.Session.Title != "manual title" || !result.Session.ManualTitle {
		t.Fatalf("result title/manual = %q/%v, want manual title", result.Session.Title, result.Session.ManualTitle)
	}
	stored, ok := store.Get(key)
	if !ok {
		t.Fatal("stored trigger session missing")
	}
	if stored.Title != "manual title" || !stored.ManualTitle {
		t.Fatalf("stored title/manual = %q/%v, want manual title", stored.Title, stored.ManualTitle)
	}
}

func TestRunTriggerPromptTreatsSlashTextAsPrompt(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}
	svc := NewService(cfg, store)
	rt := &fakeRuntime{newSessionInfo: acp.SessionInfo{SessionID: "acp-trigger"}, promptReply: "handled slash text"}
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}

	result, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "/session list",
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if result.Text != "handled slash text" {
		t.Fatalf("result text = %q, want ACP prompt result", result.Text)
	}
	if len(rt.promptCalls) != 1 || rt.promptCalls[0].Text != "/session list" {
		t.Fatalf("prompt calls = %+v, want slash text sent to ACP runtime", rt.promptCalls)
	}
}

func TestRunTriggerPromptDoesNotScheduleWikiByDefault(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}
	svc := NewService(cfg, store)
	svc.setRuntime(&fakeRuntime{newSessionInfo: acp.SessionInfo{SessionID: "acp-trigger"}, promptReply: "done"})
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}

	_, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "generate daily report",
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if svc.hasWikiTimer(key) {
		t.Fatal("wiki timer scheduled by default, want disabled for non-IM trigger")
	}
}

func TestRunTriggerPromptSchedulesWikiWhenEnabled(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}
	svc := NewService(cfg, store)
	svc.setRuntime(&fakeRuntime{newSessionInfo: acp.SessionInfo{SessionID: "acp-trigger"}, promptReply: "done"})
	key := SessionKey{BotID: "bot-a", Source: "drive_comment", MainID: "docx:token", SubID: "comment-1"}

	_, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:                "bot-a",
		Key:                  key,
		Workspace:            t.TempDir(),
		AgentName:            "traex",
		Cwd:                  t.TempDir(),
		Prompt:               "handle comment",
		EnableWikiReflection: true,
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if !svc.hasWikiTimer(key) {
		t.Fatal("wiki timer not scheduled when EnableWikiReflection is true")
	}
}

func TestRunTriggerPromptUsesExistingSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}
	if err := store.Upsert(Session{Key: key, AgentName: "traex", ACPSessionID: "acp-existing", Cwd: "/repo"}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{promptReply: "existing done"}
	svc.setRuntime(rt)
	sink := &recordingTriggerSink{}
	result, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "run",
		Sink:      sink,
		Metadata:  map[string]string{"task_id": "daily"},
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if result.Request.Key != (SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}) {
		t.Fatalf("result request key = %+v, want normalized trigger key", result.Request.Key)
	}
	if result.Session.ACPSessionID != "acp-existing" {
		t.Fatalf("result session = %+v, want existing session", result.Session)
	}
	if result.Text != "existing done" {
		t.Fatalf("result text = %q, want existing done", result.Text)
	}
	if len(rt.newCalls) != 0 {
		t.Fatalf("newCalls = %+v, want none for existing session", rt.newCalls)
	}
	if len(rt.promptCalls) != 1 || rt.promptCalls[0].Session.ACPSessionID != "acp-existing" {
		t.Fatalf("promptCalls = %+v, want existing session prompt", rt.promptCalls)
	}
	if sink.completes != 1 || sink.errors != 0 {
		t.Fatalf("sink complete/errors = %d/%d, want 1/0", sink.completes, sink.errors)
	}
}

func TestRunTriggerPromptPersistsACPStateUpdates(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := SessionKey{BotID: "bot-a", Source: "drive_comment", MainID: "docx:token", SubID: "comment-1"}
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-trigger"},
		noDefaultState: true,
		promptReply:    "done",
	}
	svc.setRuntime(rt)

	_, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "handle comment",
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}

	rt.dispatchUpdate(key, "stale-session", acp.SessionUpdate{
		SessionUpdate:     "available_commands_update",
		AvailableCommands: []acp.AvailableCommand{{Name: "stale"}},
	})
	stored, ok := store.Get(key)
	if !ok {
		t.Fatal("stored trigger session missing")
	}
	if len(stored.AvailableCommands) != 0 {
		t.Fatalf("stale update commands = %+v, want ignored", stored.AvailableCommands)
	}

	rt.dispatchUpdate(key, "acp-trigger", acp.SessionUpdate{
		SessionUpdate:     "available_commands_update",
		AvailableCommands: []acp.AvailableCommand{{Name: "review", Description: "Review changes"}},
	})
	stored, ok = store.Get(key)
	if !ok {
		t.Fatal("stored trigger session missing after update")
	}
	if len(stored.AvailableCommands) != 1 || stored.AvailableCommands[0].Name != "review" {
		t.Fatalf("stored commands = %+v, want trigger state update persisted", stored.AvailableCommands)
	}
}

func TestRunTriggerPromptRefreshesUnavailableSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := SessionKey{BotID: "bot-a", Source: "drive_comment", MainID: "docx:token", SubID: "comment-1"}
	cwd := t.TempDir()
	workspace := t.TempDir()
	if err := store.Upsert(Session{
		Key:          key,
		Title:        "old title",
		ManualTitle:  true,
		AgentName:    "traex",
		ACPSessionID: "acp-old",
		Cwd:          cwd,
		Workspace:    workspace,
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{
			SessionID: "acp-refreshed",
			Meta:      map[string]any{"refreshed": true},
		},
		promptErrors: []error{errACPSessionUnavailable},
		promptReply:  "refreshed done",
	}
	svc.setRuntime(rt)

	result, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: workspace,
		AgentName: "traex",
		Cwd:       cwd,
		Prompt:    "handle comment",
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if len(rt.newCalls) != 1 || rt.newCalls[0].Key != normalizeSessionKey(key) || rt.newCalls[0].Cwd != cwd || rt.newCalls[0].Workspace != workspace {
		t.Fatalf("newCalls = %+v, want refresh using existing trigger session fields", rt.newCalls)
	}
	if len(rt.promptCalls) != 2 {
		t.Fatalf("promptCalls = %+v, want retry after refresh", rt.promptCalls)
	}
	if rt.promptCalls[0].Session.ACPSessionID != "acp-old" || rt.promptCalls[1].Session.ACPSessionID != "acp-refreshed" {
		t.Fatalf("prompt session ids = %+v, want old then refreshed", rt.promptCalls)
	}
	if result.Text != "refreshed done" || result.Session.ACPSessionID != "acp-refreshed" {
		t.Fatalf("result = %+v, want refreshed reply and session", result)
	}
	stored, ok := store.Get(key)
	if !ok {
		t.Fatal("stored trigger session missing")
	}
	if stored.ACPSessionID != "acp-refreshed" || stored.Title != "old title" || !stored.ManualTitle {
		t.Fatalf("stored session = %+v, want refreshed ACP id preserving title", stored)
	}
	if stored.ACPMeta["refreshed"] != true {
		t.Fatalf("stored ACPMeta = %+v, want refreshed meta", stored.ACPMeta)
	}
}

func TestRunTriggerPromptUsesDefaultPermissionOutcome(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}
	if err := store.Upsert(Session{Key: key, AgentName: "traex", ACPSessionID: "acp-existing", Cwd: "/repo"}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{
		promptReply: "done",
		permissionRequest: &acp.PermissionRequest{
			ToolCall: acp.PermissionToolCallRef{ToolCallID: "tool-1", Title: "write file"},
			Options: []acp.PermissionOption{
				{OptionID: "approve", Kind: "allow_once"},
				{OptionID: "reject", Kind: "reject_once"},
			},
		},
	}
	svc.setRuntime(rt)
	_, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "run",
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if rt.permissionOutcome.Outcome != "selected" || rt.permissionOutcome.OptionID != "reject" {
		t.Fatalf("permission outcome = %+v, want reject option selected", rt.permissionOutcome)
	}
	logText := logs.String()
	if !strings.Contains(logText, "trigger 权限请求无法发送：出站不支持私聊权限卡片，已拒绝") || !strings.Contains(logText, "tool_call_id=tool-1") {
		t.Fatalf("logs = %q, want trigger permission rejection details", logText)
	}

	rt.permissionOutcome = acp.PermissionOutcome{}
	rt.permissionRequest = &acp.PermissionRequest{
		Options: []acp.PermissionOption{{OptionID: "approve", Kind: "allow_once"}},
	}
	_, err = svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "run again",
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt(second) error = %v", err)
	}
	if rt.permissionOutcome.Outcome != "cancelled" {
		t.Fatalf("permission outcome = %+v, want cancelled fallback", rt.permissionOutcome)
	}
}

func TestRunTriggerPromptLogsSourceAndIdentifiers(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := SessionKey{BotID: "bot-a", Source: sessionSourceSchedule, MainID: "task:daily", SubID: "run:run-1"}
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-run-1"},
		promptReply:    "done",
	}
	svc.setRuntime(rt)

	_, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID:     "bot-a",
		Key:       key,
		Workspace: t.TempDir(),
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "run",
		Metadata: map[string]string{
			"task_id": "daily",
			"run_id":  "run-1",
		},
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	got := logs.String()
	for _, want := range []string{
		"开始执行 trigger prompt",
		"trigger prompt 执行完成",
		"source=schedule",
		"main_id=task:daily",
		"sub_id=run:run-1",
		"task_id=daily",
		"run_id=run-1",
		"acp_session_id=acp-run-1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("logs = %q, want %q", got, want)
		}
	}
}

func TestNoopTriggerSink(t *testing.T) {
	sink := noopTriggerSink{}
	result := TriggerResult{}
	if err := sink.OnUpdate(context.Background(), result); err != nil {
		t.Fatalf("OnUpdate() error = %v", err)
	}
	if err := sink.OnComplete(context.Background(), result); err != nil {
		t.Fatalf("OnComplete() error = %v", err)
	}
	if err := sink.OnError(context.Background(), result); err != nil {
		t.Fatalf("OnError() error = %v", err)
	}
}

type recordingTriggerSink struct {
	updates       int
	completes     int
	errors        int
	updateResults []TriggerResult
	lastComplete  TriggerResult
}

func (s *recordingTriggerSink) OnUpdate(_ context.Context, result TriggerResult) error {
	s.updates++
	s.updateResults = append(s.updateResults, result)
	return nil
}

func (s *recordingTriggerSink) OnComplete(_ context.Context, result TriggerResult) error {
	s.completes++
	s.lastComplete = result
	return nil
}

func (s *recordingTriggerSink) OnError(context.Context, TriggerResult) error {
	s.errors++
	return nil
}

// fakeTriggerPermissionOutbound 实现 triggerPermissionRequester，记录发卡请求并返回预设结果。
type fakeTriggerPermissionOutbound struct {
	targetOpenIDs []string
	sources       []string
	outcomes      []acp.PermissionOutcome
	err           error
	call          int
}

func (*fakeTriggerPermissionOutbound) Outbound() {}

func (f *fakeTriggerPermissionOutbound) RequestPermissionForOpenID(_ context.Context, targetOpenID string, source string, _ acp.PermissionRequest) (acp.PermissionOutcome, error) {
	f.targetOpenIDs = append(f.targetOpenIDs, targetOpenID)
	f.sources = append(f.sources, source)
	if f.err != nil {
		return acp.PermissionOutcome{}, f.err
	}
	idx := f.call
	f.call++
	if idx < len(f.outcomes) {
		return f.outcomes[idx], nil
	}
	return acp.PermissionOutcome{Outcome: "cancelled"}, nil
}

func newTriggerPermissionService(t *testing.T, key SessionKey, rt *fakeRuntime, owners []string) *Service {
	t.Helper()
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err := store.Upsert(Session{Key: key, AgentName: "traex", ACPSessionID: "acp-existing", Cwd: "/repo"}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	bot := config.BotConfig{ID: "bot-a"}
	bot.OwnerOpenIDs = owners
	svc := NewService(config.Config{
		Bots:      []config.BotConfig{bot},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	svc.setRuntime(rt)
	return svc
}

func TestRunTriggerPromptSendsPermissionCardToOwnerAndAllows(t *testing.T) {
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}
	rt := &fakeRuntime{
		promptReply: "done",
		permissionRequest: &acp.PermissionRequest{
			ToolCall: acp.PermissionToolCallRef{ToolCallID: "tool-1", Title: "write file"},
			Options:  []acp.PermissionOption{{OptionID: "approve", Kind: "allow_once"}},
		},
	}
	svc := newTriggerPermissionService(t, key, rt, []string{"ou_owner"})
	out := &fakeTriggerPermissionOutbound{outcomes: []acp.PermissionOutcome{{Outcome: "selected", OptionID: "approve"}}}
	svc.setOutbound("bot-a", out)

	_, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID: "bot-a", Key: key, Workspace: t.TempDir(), AgentName: "traex",
		Cwd: t.TempDir(), Prompt: "run",
		Metadata: map[string]string{"task_id": "daily", "run_id": "run:1"},
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if len(out.targetOpenIDs) != 1 || out.targetOpenIDs[0] != "ou_owner" {
		t.Fatalf("targetOpenIDs = %v, want [ou_owner]", out.targetOpenIDs)
	}
	if rt.permissionOutcome.Outcome != "selected" || rt.permissionOutcome.OptionID != "approve" {
		t.Fatalf("permission outcome = %+v, want approve", rt.permissionOutcome)
	}
	if len(out.sources) != 1 || !strings.Contains(out.sources[0], "定时任务") || !strings.Contains(out.sources[0], "daily") {
		t.Fatalf("source label = %q, want schedule label containing 定时任务/daily", out.sources[0])
	}
}

func TestRunTriggerPromptOwnerRejectFallsThrough(t *testing.T) {
	key := SessionKey{BotID: "bot-a", Source: "drive_comment", MainID: "docx:token", SubID: "comment-1"}
	rt := &fakeRuntime{
		promptReply: "done",
		permissionRequest: &acp.PermissionRequest{
			ToolCall: acp.PermissionToolCallRef{ToolCallID: "tool-1", Title: "write file"},
			Options:  []acp.PermissionOption{{OptionID: "reject", Kind: "reject_once"}},
		},
	}
	svc := newTriggerPermissionService(t, key, rt, []string{"ou_owner"})
	out := &fakeTriggerPermissionOutbound{outcomes: []acp.PermissionOutcome{{Outcome: "selected", OptionID: "reject"}}}
	svc.setOutbound("bot-a", out)

	_, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID: "bot-a", Key: key, Workspace: t.TempDir(), AgentName: "traex",
		Cwd: t.TempDir(), Prompt: "run",
		Metadata: map[string]string{"file_type": "docx", "file_token": "token", "comment_id": "comment-1"},
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if rt.permissionOutcome.OptionID != "reject" {
		t.Fatalf("permission outcome = %+v, want reject", rt.permissionOutcome)
	}
	if len(out.sources) != 1 || !strings.Contains(out.sources[0], "云文档评论") {
		t.Fatalf("source label = %q, want drive_comment label", out.sources[0])
	}
}

func TestRunTriggerPermissionWithoutOwnerRejects(t *testing.T) {
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}
	rt := &fakeRuntime{
		promptReply: "done",
		permissionRequest: &acp.PermissionRequest{
			Options: []acp.PermissionOption{{OptionID: "reject", Kind: "reject_once"}},
		},
	}
	svc := newTriggerPermissionService(t, key, rt, nil)
	out := &fakeTriggerPermissionOutbound{}
	svc.setOutbound("bot-a", out)

	_, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
		BotID: "bot-a", Key: key, Workspace: t.TempDir(), AgentName: "traex",
		Cwd: t.TempDir(), Prompt: "run",
	})
	if err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if len(out.targetOpenIDs) != 0 {
		t.Fatalf("targetOpenIDs = %v, want no card sent without owner", out.targetOpenIDs)
	}
	if rt.permissionOutcome.OptionID != "reject" {
		t.Fatalf("permission outcome = %+v, want default reject", rt.permissionOutcome)
	}
}
