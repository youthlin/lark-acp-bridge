package bridge

import (
	"context"
	"errors"
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
		_, err := runUserTask(svc, context.Background(), session, agent, runningTaskOptions{drainPendingAtAuto: true, queuePendingAtAuto: true}, func(ctx context.Context) (struct{}, error) {
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
	if task.kind != taskKindUser || !task.drainPendingAtAuto || !task.queuePendingAtAuto {
		t.Fatalf("task = %+v, want user task with pending at-auto flags", task)
	}
	cancelled := make(chan string, 1)
	svc.setTaskCancelHandler(key, func(_ context.Context, reason string) {
		cancelled <- reason
	})

	secondDone := make(chan error, 1)
	go func() {
		_, err := runUserTask(svc, context.Background(), session, agent, runningTaskOptions{}, func(context.Context) (struct{}, error) {
			return struct{}{}, nil
		})
		secondDone <- err
	}()
	select {
	case reason := <-cancelled:
		if reason != "已取消" {
			t.Fatalf("cancel reason = %q, want 已取消", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("previous task was not cancelled")
	}
	select {
	case err := <-secondDone:
		t.Fatalf("second runUserTask finished before previous task exited: %v", err)
	default:
	}

	close(release)
	if err := <-done; err != context.Canceled {
		t.Fatalf("first runUserTask() error = %v, want context.Canceled", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second runUserTask() error = %v", err)
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

func TestStartTaskWithOptionsWaitsForReplacedTaskDone(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	rt := &fakeRuntime{}
	svc.setRuntime(rt)
	key := imSessionKey("bot-a", "chat-a", "")
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-running"}
	agent := config.AgentConfig{Command: "traex"}
	previousCtx, previousFinish := svc.startTask(context.Background(), session, agent, taskKindUser)
	incomingDone := make(chan error, 1)

	go func() {
		_, incomingFinish, err := svc.startTaskWithOptions(context.Background(), session, agent, taskKindUser, runningTaskOptions{})
		if err == nil {
			incomingFinish()
		}
		incomingDone <- err
	}()

	select {
	case <-previousCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("previous task context was not cancelled")
	}
	select {
	case err := <-incomingDone:
		t.Fatalf("incoming task started before previous task finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	previousFinish()
	select {
	case err := <-incomingDone:
		if err != nil {
			t.Fatalf("incoming startTaskWithOptions() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("incoming task did not start after previous task finished")
	}
}

func TestStartTaskWithOptionsClosesRuntimeWhenReplacedTaskNeverCompletes(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	rt := &fakeRuntime{}
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "chat-a", ""))
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-running"}
	agent := config.AgentConfig{Command: "traex"}
	previousCtx, _ := svc.startTask(context.Background(), session, agent, taskKindUser)
	incomingDone := make(chan error, 1)

	go func() {
		_, incomingFinish, err := svc.startTaskWithOptions(context.Background(), session, agent, taskKindUser, runningTaskOptions{
			replacementTimeout: 20 * time.Millisecond,
		})
		if err == nil {
			incomingFinish()
		}
		incomingDone <- err
	}()

	select {
	case <-previousCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("previous task context was not cancelled")
	}
	select {
	case err := <-incomingDone:
		if err != nil {
			t.Fatalf("incoming startTaskWithOptions() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("incoming task did not start after replacement timeout")
	}
	rt.mu.Lock()
	closed := append([]runtimeKey(nil), rt.closedRuntimeKeys...)
	rt.mu.Unlock()
	if len(closed) != 1 || closed[0] != currentRuntimeKey(key) {
		t.Fatalf("closed runtime keys = %+v, want current runtime", closed)
	}
}

func TestStartTaskWithOptionsWaitsForReplacedTaskChain(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	rt := &fakeRuntime{}
	svc.setRuntime(rt)
	key := imSessionKey("bot-a", "chat-a", "")
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-running"}
	agent := config.AgentConfig{Command: "traex"}
	previousCtx, previousFinish := svc.startTask(context.Background(), session, agent, taskKindUser)
	secondDone := make(chan error, 1)
	thirdDone := make(chan error, 1)

	go func() {
		_, finish, err := svc.startTaskWithOptions(context.Background(), session, agent, taskKindUser, runningTaskOptions{})
		if err == nil {
			finish()
		}
		secondDone <- err
	}()
	select {
	case <-previousCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("previous task context was not cancelled by second task")
	}

	go func() {
		_, finish, err := svc.startTaskWithOptions(context.Background(), session, agent, taskKindUser, runningTaskOptions{})
		if err == nil {
			finish()
		}
		thirdDone <- err
	}()
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("second startTaskWithOptions() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second task was not cancelled by third task")
	}
	select {
	case err := <-thirdDone:
		t.Fatalf("third task started before first task finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	previousFinish()
	select {
	case err := <-thirdDone:
		if err != nil {
			t.Fatalf("third startTaskWithOptions() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("third task did not start after replacement chain finished")
	}
}

func TestStartTaskWithOptionsDetachesTimedOutPredecessorChain(t *testing.T) {
	oldTimeout := replacedTaskDoneTimeout
	replacedTaskDoneTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		replacedTaskDoneTimeout = oldTimeout
	})

	svc := NewService(config.Config{}, NewSessionStore(""))
	key := imSessionKey("bot-a", "chat-a", "")
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-running"}
	agent := config.AgentConfig{Command: "traex"}
	previousCtx, _ := svc.startTask(context.Background(), session, agent, taskKindUser)
	secondDone := make(chan error, 1)

	go func() {
		_, finish, err := svc.startTaskWithOptions(context.Background(), session, agent, taskKindUser, runningTaskOptions{})
		if err == nil {
			finish()
		}
		secondDone <- err
	}()
	select {
	case <-previousCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("previous task context was not cancelled by second task")
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("second startTaskWithOptions() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second task did not start after predecessor timeout")
	}

	start := time.Now()
	_, thirdFinish, err := svc.startTaskWithOptions(context.Background(), session, agent, taskKindUser, runningTaskOptions{})
	if err != nil {
		t.Fatalf("third startTaskWithOptions() error = %v", err)
	}
	thirdFinish()
	if elapsed := time.Since(start); elapsed >= replacedTaskDoneTimeout {
		t.Fatalf("third start waited %s, want detached from timed out predecessor chain", elapsed)
	}
}

func TestRunPromptTaskSharesUserTaskLifecycle(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	key := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "task:daily", SubID: "run:1"}
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-running"}
	agent := config.AgentConfig{Command: "traex"}
	observed := make(chan *runningTask, 1)

	out, err := runPromptTask(svc, context.Background(), session, agent, runningTaskOptions{drainPendingAtAuto: true, queuePendingAtAuto: true}, func(context.Context) (acp.PromptResult, bool, error) {
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
	if task == nil || task.kind != taskKindUser || !task.drainPendingAtAuto || !task.queuePendingAtAuto {
		t.Fatalf("observed task = %+v, want shared user task lifecycle", task)
	}
	svc.taskMu.Lock()
	remaining := svc.tasks[normalizeSessionKey(key)]
	svc.taskMu.Unlock()
	if remaining != nil {
		t.Fatalf("remaining task = %+v, want cleaned", remaining)
	}
}

func TestStartTaskWithOptionsSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", Source: "im", MainID: "chat-a"}
	agent := config.AgentConfig{Command: "traex"}
	cases := []struct {
		name                 string
		existingKind         taskKind
		incomingKind         taskKind
		incomingOptions      runningTaskOptions
		wantBusy             bool
		wantExistingCanceled bool
		wantCancelReason     string
		wantStoredKind       taskKind
	}{
		{
			name:                 "普通用户任务替换旧用户任务",
			existingKind:         taskKindUser,
			incomingKind:         taskKindUser,
			wantExistingCanceled: true,
			wantCancelReason:     "已取消",
			wantStoredKind:       taskKindUser,
		},
		{
			name:                 "队列续跑遇到用户任务返回 busy",
			existingKind:         taskKindUser,
			incomingKind:         taskKindUser,
			incomingOptions:      runningTaskOptions{queuedContinuation: true},
			wantBusy:             true,
			wantExistingCanceled: false,
			wantStoredKind:       taskKindUser,
		},
		{
			name:                 "普通用户任务打断 loop",
			existingKind:         taskKindLoop,
			incomingKind:         taskKindUser,
			wantExistingCanceled: true,
			wantCancelReason:     "已被新消息打断",
			wantStoredKind:       taskKindUser,
		},
		{
			name:                 "新 loop 替换旧用户任务",
			existingKind:         taskKindUser,
			incomingKind:         taskKindLoop,
			wantExistingCanceled: true,
			wantCancelReason:     "已取消",
			wantStoredKind:       taskKindLoop,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			session := Session{Key: key, AgentName: "traex"}
			_, existingFinish := svc.startTask(context.Background(), session, agent, tt.existingKind)
			cancelled := make(chan string, 1)
			svc.setTaskCancelHandler(key, func(_ context.Context, reason string) {
				cancelled <- reason
				existingFinish()
			})

			ctx, incomingFinish, err := svc.startTaskWithOptions(context.Background(), session, agent, tt.incomingKind, tt.incomingOptions)
			if tt.wantBusy {
				if err != errSessionTaskBusy {
					t.Fatalf("startTaskWithOptions() error = %v, want errSessionTaskBusy", err)
				}
				select {
				case <-ctx.Done():
				default:
					t.Fatal("busy incoming task context was not cancelled")
				}
			} else if err != nil {
				t.Fatalf("startTaskWithOptions() error = %v", err)
			}

			select {
			case reason := <-cancelled:
				if !tt.wantExistingCanceled {
					t.Fatalf("unexpected cancel reason = %q", reason)
				}
				if reason != tt.wantCancelReason {
					t.Fatalf("cancel reason = %q, want %q", reason, tt.wantCancelReason)
				}
			default:
				if tt.wantExistingCanceled {
					t.Fatalf("existing %s task was not cancelled", tt.existingKind)
				}
			}

			svc.taskMu.Lock()
			stored := svc.tasks[normalizeSessionKey(key)]
			svc.taskMu.Unlock()
			if stored == nil || stored.kind != tt.wantStoredKind {
				t.Fatalf("stored task = %+v, want kind %s", stored, tt.wantStoredKind)
			}

			incomingFinish()
			existingFinish()
			svc.taskMu.Lock()
			remaining := svc.tasks[normalizeSessionKey(key)]
			svc.taskMu.Unlock()
			if remaining != nil {
				t.Fatalf("remaining task = %+v, want cleaned", remaining)
			}
		})
	}
}

func TestBeginRunningTaskSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "chat-a", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "chat-b", ""))
	cases := []struct {
		name            string
		setup           func(svc *Service, existing *runningTask)
		opts            runningTaskOptions
		wantPrevious    bool
		wantBusy        bool
		wantCurrentTask string
		wantOtherAlive  bool
	}{
		{
			name:            "无当前任务时写入新任务",
			setup:           func(svc *Service, existing *runningTask) {},
			wantCurrentTask: "incoming",
		},
		{
			name: "普通任务替换当前任务并返回 previous",
			setup: func(svc *Service, existing *runningTask) {
				svc.tasks[normalizedKey] = existing
			},
			wantPrevious:    true,
			wantCurrentTask: "incoming",
		},
		{
			name: "queued continuation 遇当前任务返回 busy 且不覆盖",
			setup: func(svc *Service, existing *runningTask) {
				svc.tasks[normalizedKey] = existing
			},
			opts:            runningTaskOptions{queuedContinuation: true},
			wantPrevious:    true,
			wantBusy:        true,
			wantCurrentTask: "existing",
		},
		{
			name: "非规范化 key 只影响当前 session 不覆盖其他 session",
			setup: func(svc *Service, existing *runningTask) {
				svc.tasks[otherKey] = &runningTask{kind: taskKindLoop}
			},
			wantCurrentTask: "incoming",
			wantOtherAlive:  true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			existing := &runningTask{kind: taskKindUser}
			incoming := &runningTask{kind: taskKindLoop}
			tt.setup(svc, existing)

			previous, busy := svc.beginRunningTask(key, incoming, tt.opts)
			if (previous == existing) != tt.wantPrevious {
				t.Fatalf("previous = %+v, want existing=%v", previous, tt.wantPrevious)
			}
			if busy != tt.wantBusy {
				t.Fatalf("busy = %v, want %v", busy, tt.wantBusy)
			}
			stored := svc.tasks[normalizedKey]
			switch tt.wantCurrentTask {
			case "incoming":
				if stored != incoming {
					t.Fatalf("stored task = %+v, want incoming", stored)
				}
			case "existing":
				if stored != existing {
					t.Fatalf("stored task = %+v, want existing", stored)
				}
			default:
				t.Fatalf("unknown wantCurrentTask %q", tt.wantCurrentTask)
			}
			if gotOtherAlive := svc.tasks[otherKey] != nil; gotOtherAlive != tt.wantOtherAlive {
				t.Fatalf("other task alive = %v, want %v", gotOtherAlive, tt.wantOtherAlive)
			}
		})
	}
}

func TestSessionHasRunningUserTaskSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "chat-a", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "chat-b", ""))
	cases := []struct {
		name  string
		setup func(svc *Service)
		want  bool
	}{
		{
			name:  "没有运行任务返回 false",
			setup: func(svc *Service) {},
			want:  false,
		},
		{
			name: "非规范化 key 也能识别 user task",
			setup: func(svc *Service) {
				svc.tasks[normalizedKey] = &runningTask{kind: taskKindUser}
			},
			want: true,
		},
		{
			name: "wiki task 不算 user task",
			setup: func(svc *Service) {
				svc.tasks[normalizedKey] = &runningTask{kind: taskKindWiki}
			},
			want: false,
		},
		{
			name: "loop task 不算 user task",
			setup: func(svc *Service) {
				svc.tasks[normalizedKey] = &runningTask{kind: taskKindLoop}
			},
			want: false,
		},
		{
			name: "其他 session 的 user task 不影响当前 session",
			setup: func(svc *Service) {
				svc.tasks[otherKey] = &runningTask{kind: taskKindUser}
			},
			want: false,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			tt.setup(svc)

			got := svc.sessionHasRunningUserTask(key)
			if got != tt.want {
				t.Fatalf("sessionHasRunningUserTask() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSetTaskCancelHandlerSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "chat-a", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "chat-b", ""))
	handler := func(context.Context, string) {}
	cases := []struct {
		name                string
		setup               func(svc *Service)
		handler             func(context.Context, string)
		wantHasHandler      bool
		wantOtherHasHandler bool
	}{
		{
			name:           "nil handler 不写入当前任务",
			setup:          func(svc *Service) { svc.tasks[normalizedKey] = &runningTask{kind: taskKindUser} },
			handler:        nil,
			wantHasHandler: false,
		},
		{
			name:           "不存在运行任务时忽略 handler",
			setup:          func(svc *Service) {},
			handler:        handler,
			wantHasHandler: false,
		},
		{
			name: "非规范化 key 能给当前任务写入 handler",
			setup: func(svc *Service) {
				svc.tasks[normalizedKey] = &runningTask{kind: taskKindUser}
			},
			handler:        handler,
			wantHasHandler: true,
		},
		{
			name: "其他 session 的任务不被写入 handler",
			setup: func(svc *Service) {
				svc.tasks[otherKey] = &runningTask{kind: taskKindUser}
			},
			handler:             handler,
			wantHasHandler:      false,
			wantOtherHasHandler: false,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			tt.setup(svc)

			svc.setTaskCancelHandler(key, tt.handler)

			task := svc.tasks[normalizedKey]
			if gotHasHandler := task != nil && task.onCancel != nil; gotHasHandler != tt.wantHasHandler {
				t.Fatalf("current task has handler = %v, want %v", gotHasHandler, tt.wantHasHandler)
			}
			otherTask := svc.tasks[otherKey]
			gotOtherHasHandler := otherTask != nil && otherTask.onCancel != nil
			if gotOtherHasHandler != tt.wantOtherHasHandler {
				t.Fatalf("other task has handler = %v, want %v", gotOtherHasHandler, tt.wantOtherHasHandler)
			}
		})
	}
	t.Run("写入当前任务时不覆盖其他 session handler", func(t *testing.T) {
		svc := NewService(config.Config{}, NewSessionStore(""))
		currentCalled := make(chan struct{}, 1)
		otherCalled := make(chan struct{}, 1)
		svc.tasks[normalizedKey] = &runningTask{kind: taskKindUser}
		svc.tasks[otherKey] = &runningTask{
			kind: taskKindUser,
			onCancel: func(context.Context, string) {
				otherCalled <- struct{}{}
			},
		}

		svc.setTaskCancelHandler(key, func(context.Context, string) {
			currentCalled <- struct{}{}
		})

		svc.tasks[normalizedKey].onCancel(context.Background(), "当前")
		svc.tasks[otherKey].onCancel(context.Background(), "其他")
		select {
		case <-currentCalled:
		default:
			t.Fatal("current task handler was not set")
		}
		select {
		case <-otherCalled:
		default:
			t.Fatal("other task handler was overwritten or not called")
		}
	})
}

func TestTakeRunningTaskSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "chat-a", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "chat-b", ""))
	cases := []struct {
		name             string
		setup            func(svc *Service, task *runningTask)
		wantTask         bool
		wantCurrentAlive bool
		wantOtherAlive   bool
	}{
		{
			name: "非规范化 key 能取出并删除当前任务",
			setup: func(svc *Service, task *runningTask) {
				svc.tasks[normalizedKey] = task
			},
			wantTask:         true,
			wantCurrentAlive: false,
		},
		{
			name:             "不存在运行任务返回 nil",
			setup:            func(svc *Service, task *runningTask) {},
			wantTask:         false,
			wantCurrentAlive: false,
		},
		{
			name: "只取出当前 session 不影响其他 session",
			setup: func(svc *Service, task *runningTask) {
				svc.tasks[normalizedKey] = task
				svc.tasks[otherKey] = &runningTask{kind: taskKindLoop}
			},
			wantTask:         true,
			wantCurrentAlive: false,
			wantOtherAlive:   true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			task := &runningTask{kind: taskKindUser}
			tt.setup(svc, task)

			got := svc.takeRunningTask(key)
			if (got == task) != tt.wantTask {
				t.Fatalf("takeRunningTask() = %+v, want task=%v", got, tt.wantTask)
			}
			if gotAlive := svc.tasks[normalizedKey] != nil; gotAlive != tt.wantCurrentAlive {
				t.Fatalf("current task alive = %v, want %v", gotAlive, tt.wantCurrentAlive)
			}
			if gotOtherAlive := svc.tasks[otherKey] != nil; gotOtherAlive != tt.wantOtherAlive {
				t.Fatalf("other task alive = %v, want %v", gotOtherAlive, tt.wantOtherAlive)
			}
		})
	}
}

func TestTakeRunningTaskOfKindSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "chat-a", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "chat-b", ""))
	cases := []struct {
		name             string
		existingKind     taskKind
		wantKind         taskKind
		withCurrent      bool
		withOther        bool
		wantTask         bool
		wantCurrentAlive bool
		wantOtherAlive   bool
	}{
		{
			name:         "类型匹配时取出并删除当前任务",
			existingKind: taskKindLoop,
			wantKind:     taskKindLoop,
			withCurrent:  true,
			wantTask:     true,
		},
		{
			name:             "类型不匹配时不取出也不删除",
			existingKind:     taskKindUser,
			wantKind:         taskKindLoop,
			withCurrent:      true,
			wantTask:         false,
			wantCurrentAlive: true,
		},
		{
			name:     "不存在当前任务返回 nil",
			wantKind: taskKindLoop,
			wantTask: false,
		},
		{
			name:           "只取出当前 session 不影响其他 session",
			existingKind:   taskKindLoop,
			wantKind:       taskKindLoop,
			withCurrent:    true,
			withOther:      true,
			wantTask:       true,
			wantOtherAlive: true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			task := &runningTask{kind: tt.existingKind}
			if tt.withCurrent {
				svc.tasks[normalizedKey] = task
			}
			if tt.withOther {
				svc.tasks[otherKey] = &runningTask{kind: taskKindLoop}
			}

			got := svc.takeRunningTaskOfKind(key, tt.wantKind)
			if (got == task) != tt.wantTask {
				t.Fatalf("takeRunningTaskOfKind() = %+v, want task=%v", got, tt.wantTask)
			}
			if gotAlive := svc.tasks[normalizedKey] != nil; gotAlive != tt.wantCurrentAlive {
				t.Fatalf("current task alive = %v, want %v", gotAlive, tt.wantCurrentAlive)
			}
			if gotOtherAlive := svc.tasks[otherKey] != nil; gotOtherAlive != tt.wantOtherAlive {
				t.Fatalf("other task alive = %v, want %v", gotOtherAlive, tt.wantOtherAlive)
			}
		})
	}
}

func TestFinishRunningTaskSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "chat-a", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "chat-b", ""))
	cases := []struct {
		name             string
		storedTask       func(current *runningTask) *runningTask
		kind             taskKind
		opts             runningTaskOptions
		withOther        bool
		wantDrain        bool
		wantCurrentAlive bool
		wantOtherAlive   bool
	}{
		{
			name:       "匹配的普通 user task 完成后删除并触发队列 drain",
			storedTask: func(current *runningTask) *runningTask { return current },
			kind:       taskKindUser,
			wantDrain:  true,
		},
		{
			name:       "queued continuation 完成后删除但不触发队列 drain",
			storedTask: func(current *runningTask) *runningTask { return current },
			kind:       taskKindUser,
			opts:       runningTaskOptions{queuedContinuation: true},
		},
		{
			name:       "wiki task 完成后删除但不触发队列 drain",
			storedTask: func(current *runningTask) *runningTask { return current },
			kind:       taskKindWiki,
		},
		{
			name:             "当前 task 已被替换时不删除新 task",
			storedTask:       func(current *runningTask) *runningTask { return &runningTask{kind: taskKindUser} },
			kind:             taskKindUser,
			wantCurrentAlive: true,
		},
		{
			name:           "只完成当前 session 不影响其他 session",
			storedTask:     func(current *runningTask) *runningTask { return current },
			kind:           taskKindUser,
			withOther:      true,
			wantDrain:      true,
			wantOtherAlive: true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			current := &runningTask{kind: tt.kind}
			svc.tasks[normalizedKey] = tt.storedTask(current)
			if tt.withOther {
				svc.tasks[otherKey] = &runningTask{kind: taskKindUser}
			}

			gotDrain := svc.finishRunningTask(key, current, tt.kind, tt.opts)
			if gotDrain != tt.wantDrain {
				t.Fatalf("finishRunningTask() drain = %v, want %v", gotDrain, tt.wantDrain)
			}
			if gotAlive := svc.tasks[normalizedKey] != nil; gotAlive != tt.wantCurrentAlive {
				t.Fatalf("current task alive = %v, want %v", gotAlive, tt.wantCurrentAlive)
			}
			if gotOtherAlive := svc.tasks[otherKey] != nil; gotOtherAlive != tt.wantOtherAlive {
				t.Fatalf("other task alive = %v, want %v", gotOtherAlive, tt.wantOtherAlive)
			}
		})
	}
}

func TestCancelTaskSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "chat-a", "")
	normalizedKey := normalizeSessionKey(key)
	agent := config.AgentConfig{Command: "traex"}
	cases := []struct {
		name       string
		kind       taskKind
		hasStatus  bool
		wantReason string
	}{
		{
			name:       "取消 user task 使用普通取消原因",
			kind:       taskKindUser,
			wantReason: "已取消",
		},
		{
			name:       "取消 loop task 使用打断原因并标记状态",
			kind:       taskKindLoop,
			hasStatus:  true,
			wantReason: "已被新消息打断",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			rt := &fakeRuntime{}
			svc.setRuntime(rt)
			taskCtx, taskCancel := context.WithCancel(context.Background())
			canceled := make(chan string, 1)
			task := &runningTask{
				kind:    tt.kind,
				runtime: currentRuntimeKey(normalizedKey),
				cancel:  taskCancel,
				session: Session{Key: key, AgentName: "traex", ACPSessionID: "acp-running"},
				agent:   agent,
				onCancel: func(_ context.Context, reason string) {
					canceled <- reason
				},
			}
			if tt.hasStatus {
				svc.loopStatuses[normalizedKey] = loopRunStatus{
					running:   true,
					started:   time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
					lastError: "旧错误",
				}
			}

			svc.cancelTask(context.Background(), task, true)

			if taskCtx.Err() != context.Canceled {
				t.Fatalf("task ctx err = %v, want context.Canceled", taskCtx.Err())
			}
			select {
			case reason := <-canceled:
				if reason != tt.wantReason {
					t.Fatalf("cancel reason = %q, want %q", reason, tt.wantReason)
				}
			default:
				t.Fatal("cancel handler was not called")
			}
			rt.mu.Lock()
			cancelCalls := append([]fakeCancelCall(nil), rt.cancelCalls...)
			rt.mu.Unlock()
			if len(cancelCalls) != 1 {
				t.Fatalf("cancelCalls = %+v, want one runtime cancel", cancelCalls)
			}
			if cancelCalls[0].Runtime != currentRuntimeKey(normalizedKey) || cancelCalls[0].Session.ACPSessionID != "acp-running" {
				t.Fatalf("cancelCalls[0] = %+v, want current runtime and session", cancelCalls[0])
			}
			status, ok := svc.loopStatuses[normalizedKey]
			if tt.hasStatus {
				if !ok {
					t.Fatal("loop status missing after loop task cancel")
				}
				if status.running || status.reason != tt.wantReason || status.lastError != "" || status.ended.IsZero() {
					t.Fatalf("loop status = %+v, want canceled with reason %q", status, tt.wantReason)
				}
			} else if ok {
				t.Fatalf("loop status = %+v, want not created for non-loop task", status)
			}
		})
	}
}

func TestCancelRunningSessionWorkSyncSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "chat-a", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "chat-b", ""))
	agent := config.AgentConfig{Command: "traex"}
	svc := NewService(config.Config{}, NewSessionStore(""))
	rt := &fakeRuntime{}
	svc.setRuntime(rt)
	taskCtx, taskCancel := context.WithCancel(context.Background())
	otherCtx, otherCancel := context.WithCancel(context.Background())
	defer otherCancel()
	canceled := make(chan string, 1)
	svc.tasks[normalizedKey] = &runningTask{
		kind:    taskKindUser,
		runtime: currentRuntimeKey(normalizedKey),
		cancel:  taskCancel,
		session: Session{Key: key, AgentName: "traex", ACPSessionID: "acp-current"},
		agent:   agent,
		onCancel: func(_ context.Context, reason string) {
			canceled <- reason
		},
	}
	svc.tasks[otherKey] = &runningTask{
		kind:    taskKindUser,
		runtime: currentRuntimeKey(otherKey),
		cancel:  otherCancel,
		session: Session{Key: otherKey, AgentName: "traex", ACPSessionID: "acp-other"},
		agent:   agent,
	}

	svc.cancelRunningSessionWorkSync(context.Background(), key)

	if taskCtx.Err() != context.Canceled {
		t.Fatalf("task ctx err = %v, want context.Canceled", taskCtx.Err())
	}
	if otherCtx.Err() != nil {
		t.Fatalf("other ctx err = %v, want nil", otherCtx.Err())
	}
	select {
	case reason := <-canceled:
		if reason != "已取消" {
			t.Fatalf("cancel reason = %q, want 已取消", reason)
		}
	default:
		t.Fatal("cancel handler was not called")
	}
	if svc.tasks[normalizedKey] != nil {
		t.Fatalf("current task = %+v, want removed", svc.tasks[normalizedKey])
	}
	if svc.tasks[otherKey] == nil {
		t.Fatal("other task was removed")
	}
	rt.mu.Lock()
	cancelCalls := append([]fakeCancelCall(nil), rt.cancelCalls...)
	rt.mu.Unlock()
	if len(cancelCalls) != 1 {
		t.Fatalf("cancelCalls = %+v, want one current runtime cancel", cancelCalls)
	}
	if cancelCalls[0].Session.ACPSessionID != "acp-current" || cancelCalls[0].Runtime != currentRuntimeKey(normalizedKey) {
		t.Fatalf("cancelCalls[0] = %+v, want current runtime cancel", cancelCalls[0])
	}
}

func TestCancelRunningSessionWorkSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "chat-a", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "chat-b", ""))
	agent := config.AgentConfig{Command: "traex"}
	svc := NewService(config.Config{}, NewSessionStore(""))
	rt := &fakeRuntime{}
	svc.setRuntime(rt)
	taskCtx, taskCancel := context.WithCancel(context.Background())
	otherCtx, otherCancel := context.WithCancel(context.Background())
	defer otherCancel()
	canceled := make(chan string, 1)
	svc.tasks[normalizedKey] = &runningTask{
		kind:    taskKindUser,
		runtime: currentRuntimeKey(normalizedKey),
		cancel:  taskCancel,
		session: Session{Key: key, AgentName: "traex", ACPSessionID: "acp-current"},
		agent:   agent,
		onCancel: func(_ context.Context, reason string) {
			canceled <- reason
		},
	}
	svc.tasks[otherKey] = &runningTask{
		kind:    taskKindUser,
		runtime: currentRuntimeKey(otherKey),
		cancel:  otherCancel,
		session: Session{Key: otherKey, AgentName: "traex", ACPSessionID: "acp-other"},
		agent:   agent,
	}

	svc.cancelRunningSessionWork(context.Background(), key)

	if taskCtx.Err() != context.Canceled {
		t.Fatalf("task ctx err = %v, want context.Canceled", taskCtx.Err())
	}
	if otherCtx.Err() != nil {
		t.Fatalf("other ctx err = %v, want nil", otherCtx.Err())
	}
	select {
	case reason := <-canceled:
		if reason != "已取消" {
			t.Fatalf("cancel reason = %q, want 已取消", reason)
		}
	default:
		t.Fatal("cancel handler was not called")
	}
	if svc.tasks[normalizedKey] != nil {
		t.Fatalf("current task = %+v, want removed", svc.tasks[normalizedKey])
	}
	if svc.tasks[otherKey] == nil {
		t.Fatal("other task was removed")
	}
	deadline := time.After(time.Second)
	for {
		rt.mu.Lock()
		cancelCalls := append([]fakeCancelCall(nil), rt.cancelCalls...)
		rt.mu.Unlock()
		if len(cancelCalls) == 1 {
			if cancelCalls[0].Session.ACPSessionID != "acp-current" || cancelCalls[0].Runtime != currentRuntimeKey(normalizedKey) {
				t.Fatalf("cancelCalls[0] = %+v, want current runtime cancel", cancelCalls[0])
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("cancelCalls = %+v, want one current runtime cancel", cancelCalls)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestCancelSessionWorkSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "chat-a", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "chat-b", ""))
	agent := config.AgentConfig{Command: "traex"}
	svc := NewService(config.Config{}, NewSessionStore(""))
	rt := &fakeRuntime{}
	svc.setRuntime(rt)
	foregroundCtx, foregroundCancel := context.WithCancel(context.Background())
	wikiCtx, wikiCancel := context.WithCancel(context.Background())
	otherForegroundCtx, otherForegroundCancel := context.WithCancel(context.Background())
	otherWikiCtx, otherWikiCancel := context.WithCancel(context.Background())
	defer otherForegroundCancel()
	defer otherWikiCancel()
	canceled := make(chan string, 2)
	svc.tasks[normalizedKey] = &runningTask{
		kind:    taskKindUser,
		runtime: currentRuntimeKey(normalizedKey),
		cancel:  foregroundCancel,
		session: Session{Key: normalizedKey, AgentName: "traex", ACPSessionID: "acp-current"},
		agent:   agent,
		onCancel: func(_ context.Context, reason string) {
			canceled <- "foreground:" + reason
		},
	}
	svc.tasks[otherKey] = &runningTask{
		kind:    taskKindUser,
		runtime: currentRuntimeKey(otherKey),
		cancel:  otherForegroundCancel,
		session: Session{Key: otherKey, AgentName: "traex", ACPSessionID: "acp-other"},
		agent:   agent,
	}
	wikiRuntime := wikiRuntimeKey(normalizedKey, 1, "acp-wiki-current")
	otherWikiRuntime := wikiRuntimeKey(otherKey, 2, "acp-wiki-other")
	svc.wikiTasks[wikiRuntime] = &runningTask{
		kind:    taskKindWiki,
		runtime: wikiRuntime,
		cancel:  wikiCancel,
		session: Session{Key: normalizedKey, AgentName: "traex", ACPSessionID: "acp-wiki-current"},
		agent:   agent,
		onCancel: func(_ context.Context, reason string) {
			canceled <- "wiki:" + reason
		},
	}
	svc.wikiTasks[otherWikiRuntime] = &runningTask{
		kind:    taskKindWiki,
		runtime: otherWikiRuntime,
		cancel:  otherWikiCancel,
		session: Session{Key: otherKey, AgentName: "traex", ACPSessionID: "acp-wiki-other"},
		agent:   agent,
	}
	timer := time.NewTimer(time.Hour)
	otherTimer := time.NewTimer(time.Hour)
	defer timer.Stop()
	defer otherTimer.Stop()
	svc.wikiGenerations[normalizedKey] = 5
	svc.wikiGenerations[otherKey] = 8
	svc.wikiTimers[normalizedKey] = &pendingWikiRun{timer: timer, session: Session{Key: normalizedKey}}
	svc.wikiTimers[otherKey] = &pendingWikiRun{timer: otherTimer, session: Session{Key: otherKey}}

	svc.cancelSessionWork(context.Background(), key)

	if foregroundCtx.Err() != context.Canceled {
		t.Fatalf("foreground ctx err = %v, want context.Canceled", foregroundCtx.Err())
	}
	if wikiCtx.Err() != context.Canceled {
		t.Fatalf("wiki ctx err = %v, want context.Canceled", wikiCtx.Err())
	}
	if otherForegroundCtx.Err() != nil {
		t.Fatalf("other foreground ctx err = %v, want nil", otherForegroundCtx.Err())
	}
	if otherWikiCtx.Err() != nil {
		t.Fatalf("other wiki ctx err = %v, want nil", otherWikiCtx.Err())
	}
	gotReasons := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case reason := <-canceled:
			gotReasons[reason] = true
		default:
			t.Fatalf("cancel handlers called %d times, want 2", i)
		}
	}
	if !gotReasons["foreground:已取消"] || !gotReasons["wiki:已取消"] {
		t.Fatalf("cancel reasons = %+v, want foreground and wiki canceled", gotReasons)
	}
	if svc.tasks[normalizedKey] != nil {
		t.Fatalf("current task = %+v, want removed", svc.tasks[normalizedKey])
	}
	if svc.tasks[otherKey] == nil {
		t.Fatal("other foreground task was removed")
	}
	if _, ok := svc.wikiTasks[wikiRuntime]; ok {
		t.Fatal("current wiki task was not removed")
	}
	if _, ok := svc.wikiTasks[otherWikiRuntime]; !ok {
		t.Fatal("other wiki task was removed")
	}
	if svc.wikiTimers[normalizedKey] != nil {
		t.Fatalf("current wiki timer = %+v, want removed", svc.wikiTimers[normalizedKey])
	}
	if svc.wikiTimers[otherKey] == nil {
		t.Fatal("other wiki timer was removed")
	}
	if svc.wikiGenerations[normalizedKey] != 6 {
		t.Fatalf("current wiki generation = %d, want 6", svc.wikiGenerations[normalizedKey])
	}
	if svc.wikiGenerations[otherKey] != 8 {
		t.Fatalf("other wiki generation = %d, want 8", svc.wikiGenerations[otherKey])
	}

	deadline := time.After(time.Second)
	for {
		rt.mu.Lock()
		cancelCalls := append([]fakeCancelCall(nil), rt.cancelCalls...)
		rt.mu.Unlock()
		if len(cancelCalls) == 2 {
			gotSessions := map[string]bool{}
			for _, call := range cancelCalls {
				gotSessions[call.Session.ACPSessionID] = true
			}
			if !gotSessions["acp-current"] || !gotSessions["acp-wiki-current"] {
				t.Fatalf("cancelCalls = %+v, want current foreground and wiki runtime cancels", cancelCalls)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("cancelCalls = %+v, want current foreground and wiki runtime cancels", cancelCalls)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestMarkCanceledTaskLoopStatusBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "oc_chat", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "oc_other", ""))
	started := time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC)
	cases := []struct {
		name           string
		kind           taskKind
		hasStatus      bool
		wantMarked     bool
		wantHasStatus  bool
		wantRunning    bool
		wantReason     string
		wantLastError  string
		wantEnded      bool
		wantOtherAlive bool
	}{
		{
			name:           "非 loop task 不写 loop 状态",
			kind:           taskKindUser,
			hasStatus:      true,
			wantHasStatus:  true,
			wantRunning:    true,
			wantLastError:  "旧错误",
			wantOtherAlive: true,
		},
		{
			name:           "loop task 无既有状态时不创建状态",
			kind:           taskKindLoop,
			hasStatus:      false,
			wantHasStatus:  false,
			wantOtherAlive: true,
		},
		{
			name:           "loop task 有既有状态时标记取消",
			kind:           taskKindLoop,
			hasStatus:      true,
			wantMarked:     true,
			wantHasStatus:  true,
			wantRunning:    false,
			wantReason:     "已被新消息打断",
			wantLastError:  "",
			wantEnded:      true,
			wantOtherAlive: true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			if tt.hasStatus {
				svc.loopStatuses[normalizedKey] = loopRunStatus{
					running:   true,
					started:   started,
					lastError: "旧错误",
				}
			}
			svc.loopStatuses[otherKey] = loopRunStatus{
				running: true,
				started: started.Add(time.Minute),
				reason:  "其他 session",
			}
			task := &runningTask{
				kind:    tt.kind,
				session: Session{Key: key},
			}

			svc.markCanceledTask(task, "已被新消息打断")

			status, ok := svc.loopStatuses[normalizedKey]
			if ok != tt.wantHasStatus {
				t.Fatalf("status exists = %v, want %v", ok, tt.wantHasStatus)
			}
			if ok {
				if status.running != tt.wantRunning {
					t.Fatalf("status.running = %v, want %v", status.running, tt.wantRunning)
				}
				if status.reason != tt.wantReason {
					t.Fatalf("status.reason = %q, want %q", status.reason, tt.wantReason)
				}
				if status.lastError != tt.wantLastError {
					t.Fatalf("status.lastError = %q, want %q", status.lastError, tt.wantLastError)
				}
				if status.ended.IsZero() == tt.wantEnded {
					t.Fatalf("status.ended = %s, want ended set=%v", status.ended, tt.wantEnded)
				}
			}
			if gotMarked := ok && !status.running && status.reason == "已被新消息打断"; gotMarked != tt.wantMarked {
				t.Fatalf("marked canceled = %v, want %v", gotMarked, tt.wantMarked)
			}
			other := svc.loopStatuses[otherKey]
			if other.running != tt.wantOtherAlive || other.reason != "其他 session" {
				t.Fatalf("other status = %+v, want unchanged", other)
			}
		})
	}
}

func TestTakeAllSessionWorkClearsRuntimeState(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	foregroundKey := normalizeSessionKey(imSessionKey("bot-a", "chat-a", ""))
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "chat-b", ""))
	foregroundTask := &runningTask{kind: taskKindUser}
	wikiTask := &runningTask{kind: taskKindWiki}
	timer := time.NewTimer(time.Hour)
	otherTimer := time.NewTimer(time.Hour)
	defer timer.Stop()
	defer otherTimer.Stop()
	svc.tasks[foregroundKey] = foregroundTask
	wikiRuntime := wikiRuntimeKey(otherKey, 7, "acp-wiki")
	svc.wikiTasks[wikiRuntime] = wikiTask
	svc.wikiGenerations[foregroundKey] = 3
	svc.wikiGenerations[otherKey] = 5
	svc.wikiTimers[foregroundKey] = &pendingWikiRun{timer: timer, session: Session{Key: foregroundKey}}
	svc.wikiTimers[otherKey] = &pendingWikiRun{timer: otherTimer, session: Session{Key: otherKey}}

	snapshot := svc.takeAllSessionWork()

	if len(svc.tasks) != 0 {
		t.Fatalf("tasks = %+v, want cleared", svc.tasks)
	}
	if len(svc.wikiTasks) != 0 {
		t.Fatalf("wikiTasks = %+v, want cleared", svc.wikiTasks)
	}
	if len(svc.wikiTimers) != 0 {
		t.Fatalf("wikiTimers = %+v, want cleared", svc.wikiTimers)
	}
	if svc.wikiGenerations[foregroundKey] != 4 {
		t.Fatalf("foreground wiki generation = %d, want 4", svc.wikiGenerations[foregroundKey])
	}
	if svc.wikiGenerations[otherKey] != 6 {
		t.Fatalf("other wiki generation = %d, want 6", svc.wikiGenerations[otherKey])
	}
	gotTimers := map[*time.Timer]bool{}
	for _, got := range snapshot.timers {
		gotTimers[got] = true
	}
	if !gotTimers[timer] || !gotTimers[otherTimer] || len(gotTimers) != 2 {
		t.Fatalf("snapshot timers = %+v, want both timers", snapshot.timers)
	}
	gotTasks := map[*runningTask]bool{}
	for _, got := range snapshot.tasks {
		gotTasks[got] = true
	}
	if !gotTasks[foregroundTask] || !gotTasks[wikiTask] || len(gotTasks) != 2 {
		t.Fatalf("snapshot tasks = %+v, want foreground and wiki tasks", snapshot.tasks)
	}
}

func TestCancelAllSessionWorkClearsRuntimeState(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	rt := &fakeRuntime{}
	svc.setRuntime(rt)
	foregroundKey := normalizeSessionKey(imSessionKey("bot-a", "chat-a", ""))
	wikiKey := normalizeSessionKey(imSessionKey("bot-a", "chat-b", ""))
	foregroundCtx, foregroundCancel := context.WithCancel(context.Background())
	wikiCtx, wikiCancel := context.WithCancel(context.Background())
	foregroundCanceled := make(chan string, 1)
	wikiCanceled := make(chan string, 1)
	svc.tasks[foregroundKey] = &runningTask{
		kind:    taskKindUser,
		runtime: currentRuntimeKey(foregroundKey),
		cancel:  foregroundCancel,
		session: Session{Key: foregroundKey, AgentName: "traex", ACPSessionID: "acp-foreground"},
		agent:   config.AgentConfig{Command: "traex"},
		onCancel: func(_ context.Context, reason string) {
			foregroundCanceled <- reason
		},
	}
	wikiRuntime := wikiRuntimeKey(wikiKey, 7, "acp-wiki")
	svc.wikiTasks[wikiRuntime] = &runningTask{
		kind:    taskKindWiki,
		runtime: wikiRuntime,
		cancel:  wikiCancel,
		session: Session{Key: wikiKey, AgentName: "traex", ACPSessionID: "acp-wiki"},
		agent:   config.AgentConfig{Command: "traex"},
		onCancel: func(_ context.Context, reason string) {
			wikiCanceled <- reason
		},
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	svc.wikiTimers[foregroundKey] = &pendingWikiRun{timer: timer, session: Session{Key: foregroundKey}}

	svc.cancelAllSessionWork(context.Background())

	if len(svc.tasks) != 0 {
		t.Fatalf("tasks = %+v, want cleared", svc.tasks)
	}
	if len(svc.wikiTasks) != 0 {
		t.Fatalf("wikiTasks = %+v, want cleared", svc.wikiTasks)
	}
	if len(svc.wikiTimers) != 0 {
		t.Fatalf("wikiTimers = %+v, want cleared", svc.wikiTimers)
	}
	if svc.wikiGenerations[foregroundKey] != 1 {
		t.Fatalf("wiki generation = %d, want 1", svc.wikiGenerations[foregroundKey])
	}
	if foregroundCtx.Err() != context.Canceled {
		t.Fatalf("foreground ctx err = %v, want context.Canceled", foregroundCtx.Err())
	}
	if wikiCtx.Err() != context.Canceled {
		t.Fatalf("wiki ctx err = %v, want context.Canceled", wikiCtx.Err())
	}
	if reason := <-foregroundCanceled; reason != "已取消" {
		t.Fatalf("foreground cancel reason = %q, want 已取消", reason)
	}
	if reason := <-wikiCanceled; reason != "已取消" {
		t.Fatalf("wiki cancel reason = %q, want 已取消", reason)
	}
	rt.mu.Lock()
	cancelCalls := append([]fakeCancelCall(nil), rt.cancelCalls...)
	rt.mu.Unlock()
	if len(cancelCalls) != 2 {
		t.Fatalf("cancelCalls = %+v, want foreground and wiki runtime cancels", cancelCalls)
	}
	gotSessions := map[string]bool{}
	for _, call := range cancelCalls {
		gotSessions[call.Session.ACPSessionID] = true
	}
	if !gotSessions["acp-foreground"] || !gotSessions["acp-wiki"] {
		t.Fatalf("cancelCalls = %+v, want foreground and wiki sessions canceled", cancelCalls)
	}
}
