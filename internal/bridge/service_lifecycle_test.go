package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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
