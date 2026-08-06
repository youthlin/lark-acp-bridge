package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestExecuteRestartCommandAllowsConfiguredCommandOutsideDaemon(t *testing.T) {
	cfg := config.Default()
	cfg.RestartCommand = []string{"/bin/echo", "restart-ok"}
	svc := NewService(cfg, NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))

	if err := svc.executeRestartCommand(context.Background()); err != nil {
		t.Fatalf("executeRestartCommand() error = %v", err)
	}
}

func TestRunRestartCommandRemovesAckOnFailure(t *testing.T) {
	cfg := config.Default()
	svc := NewService(cfg, NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
	svc.setRestartCommand(func(ctx context.Context) error {
		return fmt.Errorf("restart failed")
	})
	workspace := t.TempDir()
	if err := writeRestartAck(workspace, newRestartAck(feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
	})); err != nil {
		t.Fatalf("writeRestartAck() error = %v", err)
	}

	svc.runRestartCommand(context.Background(), workspace)

	if _, err := os.Stat(restartAckPath(workspace)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart ack file err = %v, want removed after restart command failure", err)
	}
}

func TestStartMigratesWorkspaceLocalStateBeforeLoadingStores(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Config{
		Bots: []config.BotConfig{{
			ID:        "bot-a",
			Workspace: workspace,
		}},
		AgentList: []config.NamedAgentConfig{{
			Name: "traex",
			AgentConfig: config.AgentConfig{
				Command: "traex",
			},
		}},
	}
	session := Session{
		Key:          SessionKey{BotID: "bot-a", ChatID: "oc_chat"},
		AgentName:    "traex",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
		ACPSessionID: "acp-legacy",
	}
	task := ScheduledTask{
		ID:        "disabled",
		BotID:     "bot-a",
		Enabled:   false,
		Spec:      "@every 1h",
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "run",
	}
	writeJSONFile(t, filepath.Join(workspace, "sessions.json"), sessionFile{
		Version:  1,
		Sessions: []Session{session},
	})
	writeJSONFile(t, filepath.Join(workspace, "scheduled_tasks.json"), scheduledTaskFile{
		Version: 1,
		Tasks:   []ScheduledTask{task},
	})

	svc := NewService(cfg, nil)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := svc.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	})

	for _, name := range []string{"sessions.json", "scheduled_tasks.json"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy %s err = %v, want removed", name, err)
		}
		if _, err := os.Stat(filepath.Join(workspace, ".local", name)); err != nil {
			t.Fatalf("local %s err = %v, want migrated", name, err)
		}
	}
	store := svc.stores["bot-a"]
	if store == nil {
		t.Fatal("bot store is nil")
	}
	if got := store.Count(); got != 1 {
		t.Fatalf("store count = %d, want loaded legacy session", got)
	}
	if got, ok := store.Get(session.Key); !ok || got.ACPSessionID != session.ACPSessionID {
		t.Fatalf("loaded session = %+v, %v; want migrated session", got, ok)
	}
	scheduleStore := svc.scheduleStores["bot-a"]
	if scheduleStore == nil {
		t.Fatal("schedule store is nil")
	}
	if got, ok := scheduleStore.Get(task.ID); !ok || got.Prompt != task.Prompt {
		t.Fatalf("loaded task = %+v, %v; want migrated task", got, ok)
	}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent(%s) error = %v", path, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func TestShutdownCancelsRuntimeTasksBeforeRuntimeShutdown(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	cfg.Bots[0].Workspace = t.TempDir()
	svc := newTestService(cfg, store)
	rt := &fakeRuntime{blockPrompt: make(chan struct{})}
	svc.setRuntime(rt)
	session := Session{
		Key:          SessionKey{BotID: "bot-a", ChatID: "oc_chat"},
		AgentName:    "traex",
		Cwd:          t.TempDir(),
		ACPSessionID: "acp-session-1",
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := svc.runUserPrompt(context.Background(), feishu.Message{
			BotID:     "bot-a",
			ChatID:    "oc_chat",
			ChatType:  "p2p",
			MessageID: "om_prompt",
			Workspace: cfg.Bots[0].Workspace,
		}, session, mustConfigAgent(t, cfg, "traex"), "长任务")
		done <- err
	}()
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runUserPrompt() error = %v, want context.Canceled", err)
	}
	rt.mu.Lock()
	cancelCount := len(rt.cancelCalls)
	shutdownCancelCount := rt.shutdownCancelCount
	rt.mu.Unlock()
	if cancelCount != 1 {
		t.Fatalf("cancel calls = %d, want 1", cancelCount)
	}
	if shutdownCancelCount != 1 {
		t.Fatalf("shutdownCancelCount = %d, want runtime cancel completed before shutdown", shutdownCancelCount)
	}
}

func TestShutdownStopsScheduledTasksAndCancelsRunningScheduleRun(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	cfg := config.Config{
		Bots: []config.BotConfig{{
			ID:        "bot-a",
			Workspace: workspace,
		}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}
	store := NewScheduledTaskStore(filepath.Join(workspace, "scheduled_tasks.json"))
	if _, err := store.Upsert(ScheduledTask{
		ID:        "blocking",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "@every 20ms",
		AgentName: "traex",
		Cwd:       cwd,
		Prompt:    "run blocking schedule",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc := NewService(cfg, nil)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-schedule-blocking"},
		blockPrompt:    make(chan struct{}),
	}
	svc.setRuntime(rt)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	if got := svc.scheduledTaskJobCount(); got != 0 {
		t.Fatalf("scheduledTaskJobCount() = %d, want stopped scheduler jobs", got)
	}
	rt.mu.Lock()
	cancelCount := len(rt.cancelCalls)
	shutdownCancelCount := rt.shutdownCancelCount
	cancelCalls := append([]fakeCancelCall(nil), rt.cancelCalls...)
	rt.mu.Unlock()
	if cancelCount != 1 {
		t.Fatalf("cancel calls = %d, want running schedule prompt canceled", cancelCount)
	}
	if shutdownCancelCount != 1 {
		t.Fatalf("shutdownCancelCount = %d, want runtime cancel completed before shutdown", shutdownCancelCount)
	}
	if len(cancelCalls) != 1 || cancelCalls[0].Session.Key.Source != sessionSourceSchedule || cancelCalls[0].Session.Key.MainID != "task:blocking" {
		t.Fatalf("cancelCalls = %+v, want schedule runtime cancellation", cancelCalls)
	}
}
