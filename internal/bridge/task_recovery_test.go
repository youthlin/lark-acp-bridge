package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func TestRunUserTaskRecoversPanicAndCleansTask(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	key := SessionKey{BotID: "bot-a", Source: "p2p", MainID: "ou:panic"}
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-panic"}
	agent := config.AgentConfig{Command: "traex"}

	_, err := runUserTask(svc, context.Background(), session, agent, runningTaskOptions{}, func(context.Context) (struct{}, error) {
		panic("boom")
	})
	if !errors.Is(err, errTaskPanicked) {
		t.Fatalf("runUserTask error = %v, want errTaskPanicked", err)
	}

	svc.taskMu.Lock()
	remaining := svc.tasks[normalizeSessionKey(key)]
	svc.taskMu.Unlock()
	if remaining != nil {
		t.Fatalf("remaining task after panic = %+v, want cleaned", remaining)
	}
}

func TestRunPromptTaskRecoversPanic(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	key := SessionKey{BotID: "bot-a", Source: "p2p", MainID: "ou:prompt-panic"}
	session := Session{Key: key, AgentName: "traex"}
	agent := config.AgentConfig{Command: "traex"}

	_, err := runPromptTask(svc, context.Background(), session, agent, runningTaskOptions{}, func(context.Context) (acp.PromptResult, bool, error) {
		panic("prompt boom")
	})
	if !errors.Is(err, errTaskPanicked) {
		t.Fatalf("runPromptTask error = %v, want errTaskPanicked", err)
	}
}
