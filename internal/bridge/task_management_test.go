package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func TestRunUserTaskRegistersCancelsAndCleansTask(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	rt := &fakeRuntime{}
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-running"}
	agent := config.AgentConfig{Command: "traex"}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		_, err := runUserTask(svc, context.Background(), session, agent, runningTaskOptions{drainPendingAtAuto: true}, func(ctx context.Context) (struct{}, error) {
			close(started)
			<-release
			return struct{}{}, ctx.Err()
		})
		done <- err
	}()
	<-started

	svc.taskMu.Lock()
	task := svc.tasks[normalizeSessionKey(key)]
	svc.taskMu.Unlock()
	if task == nil {
		t.Fatal("runUserTask did not register task")
	}
	if task.kind != taskKindUser || !task.drainPendingAtAuto {
		t.Fatalf("task = %+v, want user task with drainPendingAtAuto", task)
	}
	cancelled := make(chan string, 1)
	svc.setTaskCancelHandler(key, func(_ context.Context, reason string) {
		cancelled <- reason
	})

	_, err := runUserTask(svc, context.Background(), session, agent, runningTaskOptions{}, func(context.Context) (struct{}, error) {
		return struct{}{}, nil
	})
	if err != nil {
		t.Fatalf("second runUserTask() error = %v", err)
	}
	select {
	case reason := <-cancelled:
		if reason != "已取消" {
			t.Fatalf("cancel reason = %q, want 已取消", reason)
		}
	default:
		t.Fatal("previous task was not cancelled")
	}

	close(release)
	if err := <-done; err != context.Canceled {
		t.Fatalf("first runUserTask() error = %v, want context.Canceled", err)
	}
	svc.taskMu.Lock()
	remaining := svc.tasks[normalizeSessionKey(key)]
	svc.taskMu.Unlock()
	if remaining != nil {
		t.Fatalf("remaining task = %+v, want cleaned after runs", remaining)
	}
	deadline := time.After(time.Second)
	for {
		rt.mu.Lock()
		cancelCalls := append([]fakeCancelCall(nil), rt.cancelCalls...)
		rt.mu.Unlock()
		if len(cancelCalls) == 1 && cancelCalls[0].Session.ACPSessionID == "acp-running" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("cancelCalls = %+v, want previous runtime session cancelled", cancelCalls)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestRunPromptTaskSharesUserTaskLifecycle(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-running"}
	agent := config.AgentConfig{Command: "traex"}
	observed := make(chan *runningTask, 1)

	out, err := runPromptTask(svc, context.Background(), session, agent, runningTaskOptions{drainPendingAtAuto: true}, func(context.Context) (acp.PromptResult, bool, error) {
		svc.taskMu.Lock()
		observed <- svc.tasks[normalizeSessionKey(key)]
		svc.taskMu.Unlock()
		return acp.PromptResult{Text: "done"}, true, nil
	})
	if err != nil {
		t.Fatalf("runPromptTask() error = %v", err)
	}
	if out.result.Text != "done" || !out.sentProgress {
		t.Fatalf("runPromptTask() = %+v, want prompt result and sentProgress", out)
	}
	task := <-observed
	if task == nil || task.kind != taskKindUser || !task.drainPendingAtAuto {
		t.Fatalf("observed task = %+v, want shared user task lifecycle", task)
	}
	svc.taskMu.Lock()
	remaining := svc.tasks[normalizeSessionKey(key)]
	svc.taskMu.Unlock()
	if remaining != nil {
		t.Fatalf("remaining task = %+v, want cleaned", remaining)
	}
}
