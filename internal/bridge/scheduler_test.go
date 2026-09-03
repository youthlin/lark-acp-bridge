package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func (s *scheduler) setJobForTest(job *scheduledTaskJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[scheduledTaskJobID(job.task)] = job
}

func (s *scheduler) runCountForTest() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

func (s *scheduler) runCountForTaskForTest(botID, taskID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runsByTask[scheduleRunTaskIndexID(botID, taskID)])
}

func (s *scheduler) hasRunIndexForTest(botID, taskID, runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.runsByTask[scheduleRunTaskIndexID(botID, taskID)][scheduleRunStatusID(botID, taskID, runID)]
	return ok
}

func TestSchedulerRunStateDoesNotUseServiceTaskLock(t *testing.T) {
	svc := NewService(config.Config{}, nil)
	task := ScheduledTask{ID: "daily", BotID: "bot-a"}
	done := make(chan struct{})

	svc.taskMu.Lock()
	go func() {
		svc.markScheduleRunPending(task, "run-1", scheduledTaskRunKey(task, "run-1"), time.Now())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		svc.taskMu.Unlock()
		t.Fatal("更新调度运行状态被 Service taskMu 阻塞")
	}
	svc.taskMu.Unlock()

	status, ok := svc.scheduleRunStatus(task, "run-1")
	if !ok || status.State != scheduleRunPending {
		t.Fatalf("schedule run status = %+v, %v; want pending", status, ok)
	}
}

func TestSchedulerRunStateIsIsolatedByBotID(t *testing.T) {
	svc := NewService(config.Config{}, nil)
	started := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	taskA := ScheduledTask{ID: "daily", BotID: "bot-a", OverlapPolicy: scheduleOverlapSkipIfRunning}
	taskB := ScheduledTask{ID: "daily", BotID: "bot-b", OverlapPolicy: scheduleOverlapSkipIfRunning}

	svc.markScheduleRunRunning(taskA, "run-a", scheduledTaskRunKey(taskA, "run-a"), started, "a")
	statusB, skipped := svc.markScheduleRunRunningOrSkipped(taskB, "run-b", scheduledTaskRunKey(taskB, "run-b"), started.Add(time.Minute), "b")
	if skipped || statusB.State != scheduleRunRunning {
		t.Fatalf("bot-b status = %+v skipped=%v, want independent running status", statusB, skipped)
	}
	svc.markScheduleRunFinished(taskB, "run-b", started.Add(2*time.Minute), nil)

	lastA, ok := svc.lastScheduleRunStatus(taskA)
	if !ok || lastA.RunID != "run-a" || lastA.State != scheduleRunRunning {
		t.Fatalf("bot-a last status = %+v ok=%v, want run-a running", lastA, ok)
	}
	lastB, ok := svc.lastScheduleRunStatus(taskB)
	if !ok || lastB.RunID != "run-b" || lastB.State != scheduleRunCompleted {
		t.Fatalf("bot-b last status = %+v ok=%v, want run-b completed", lastB, ok)
	}
}

func TestCancelScheduledTaskRunsIsIsolatedByBotID(t *testing.T) {
	svc := NewService(config.Config{}, nil)
	taskA := ScheduledTask{ID: "daily", BotID: "bot-a"}
	taskB := ScheduledTask{ID: "daily", BotID: "bot-b"}
	keyA := scheduledTaskRunKey(taskA, "run-a")
	keyB := scheduledTaskRunKey(taskB, "run-b")
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelA()
	defer cancelB()

	svc.taskMu.Lock()
	svc.tasks[keyA] = &runningTask{cancel: cancelA}
	svc.tasks[keyB] = &runningTask{cancel: cancelB}
	svc.taskMu.Unlock()

	svc.cancelScheduledTaskRuns(context.Background(), taskA)

	select {
	case <-ctxA.Done():
	default:
		t.Fatal("bot-a run was not canceled")
	}
	select {
	case <-ctxB.Done():
		t.Fatal("bot-b run was canceled by bot-a task")
	default:
	}
	svc.taskMu.Lock()
	remainingA := svc.tasks[keyA]
	remainingB := svc.tasks[keyB]
	svc.taskMu.Unlock()
	if remainingA != nil || remainingB == nil {
		t.Fatalf("remaining tasks: bot-a=%v bot-b=%v, want only bot-b retained", remainingA, remainingB)
	}
}

func TestSchedulerStopJobUsesInjectedCancellationHooks(t *testing.T) {
	scheduler := newScheduler(schedulerHooks{})
	task := ScheduledTask{ID: "daily", BotID: "bot-a"}
	key := scheduledTaskRunKey(task, "run-1")
	jobCtx, cancelJob := context.WithCancel(context.Background())
	runCtx, cancelRun := context.WithCancel(context.Background())
	job := &scheduledTaskJob{task: task, cancel: cancelJob}
	job.addActiveRun("run-1", key, cancelRun)
	scheduler.setJobForTest(job)

	canceledKeys := make(chan SessionKey, 1)
	canceledTasks := make(chan ScheduledTask, 1)
	scheduler.hooks.cancelRunningSessionWorkSync = func(_ context.Context, got SessionKey) {
		canceledKeys <- got
	}
	scheduler.hooks.cancelScheduledTaskRuns = func(_ context.Context, got ScheduledTask) {
		canceledTasks <- got
	}

	scheduler.stopScheduledTask(context.Background(), task)

	select {
	case got := <-canceledKeys:
		if got != key {
			t.Fatalf("canceled session key = %+v, want %+v", got, key)
		}
	default:
		t.Fatal("cancelRunningSessionWorkSync hook was not called")
	}
	select {
	case got := <-canceledTasks:
		if got.ID != task.ID || got.BotID != task.BotID {
			t.Fatalf("canceled task = %+v, want %+v", got, task)
		}
	default:
		t.Fatal("cancelScheduledTaskRuns hook was not called")
	}
	select {
	case <-jobCtx.Done():
	default:
		t.Fatal("job context remains active after scheduler stop")
	}
	select {
	case <-runCtx.Done():
	default:
		t.Fatal("run context remains active after scheduler stop")
	}
	if got := scheduler.jobCount(); got != 0 {
		t.Fatalf("scheduler job count = %d, want 0", got)
	}
}
