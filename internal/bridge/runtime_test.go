package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

func TestRuntimeDispatchSessionInfoSendsStateUpdates(t *testing.T) {
	r := newRuntimeManager()
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "thread-a"}
	var updates []acp.SessionUpdate
	unsub := r.SubscribeUpdates(key, func(sessionID string, update acp.SessionUpdate) {
		if sessionID != "session-1" {
			t.Fatalf("sessionID = %q, want session-1", sessionID)
		}
		updates = append(updates, update)
	})
	defer unsub()

	r.dispatchSessionInfo(key, "session-1", acp.SessionInfo{
		AvailableCommands: []acp.AvailableCommand{{Name: "review", Description: "Review changes"}},
		ConfigOptions: []acp.SessionConfigOption{
			{ID: "model", Name: "Model", Category: "model", Type: "select", CurrentValue: "gpt-5.6"},
		},
	})

	if len(updates) != 2 {
		t.Fatalf("updates = %+v, want commands and config updates", updates)
	}
	if updates[0].SessionUpdate != "available_commands_update" || len(updates[0].AvailableCommands) != 1 || updates[0].AvailableCommands[0].Name != "review" {
		t.Fatalf("first update = %+v, want available commands", updates[0])
	}
	if updates[1].SessionUpdate != "config_option_update" || len(updates[1].ConfigOptions) != 1 || updates[1].ConfigOptions[0].ID != "model" {
		t.Fatalf("second update = %+v, want config options", updates[1])
	}
}

func TestPromptActivityTimeoutExpiresAfterIdlePeriod(t *testing.T) {
	timeout := newPromptActivityTimeout(context.Background(), 30*time.Millisecond, time.Second)
	defer timeout.Stop()

	select {
	case <-timeout.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("prompt activity timeout did not expire")
	}

	cause := context.Cause(timeout.Context())
	if !errors.Is(cause, context.DeadlineExceeded) {
		t.Fatalf("cause = %v, want context.DeadlineExceeded", cause)
	}
	if !strings.Contains(cause.Error(), "未收到活动更新") {
		t.Fatalf("cause = %v, want idle timeout detail", cause)
	}
}

func TestPromptActivityTimeoutTouchExtendsIdlePeriod(t *testing.T) {
	timeout := newPromptActivityTimeout(context.Background(), 60*time.Millisecond, time.Second)
	defer timeout.Stop()

	time.Sleep(40 * time.Millisecond)
	timeout.Touch()

	select {
	case <-timeout.Context().Done():
		t.Fatalf("prompt activity timeout expired before extended idle deadline: %v", context.Cause(timeout.Context()))
	case <-time.After(40 * time.Millisecond):
	}

	select {
	case <-timeout.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("prompt activity timeout did not expire after extended idle deadline")
	}
}

func TestPromptActivityTimeoutKeepsAbsoluteLimit(t *testing.T) {
	timeout := newPromptActivityTimeout(context.Background(), 80*time.Millisecond, 120*time.Millisecond)
	defer timeout.Stop()

	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			timeout.Touch()
		case <-timeout.Context().Done():
			cause := context.Cause(timeout.Context())
			if !errors.Is(cause, context.DeadlineExceeded) {
				t.Fatalf("cause = %v, want context.DeadlineExceeded", cause)
			}
			if !strings.Contains(cause.Error(), "绝对上限") {
				t.Fatalf("cause = %v, want absolute timeout detail", cause)
			}
			return
		case <-time.After(time.Second):
			t.Fatal("prompt activity timeout did not reach absolute limit")
		}
	}
}
