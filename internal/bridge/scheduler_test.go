package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func (s *scheduler) setJobForTest(job *scheduledTaskJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[scheduledTaskJobID(job.task)] = job
}

func setScheduledTaskStorePathForTest(store *ScheduledTaskStore, path string) {
	store.mu.Lock()
	store.path = path
	store.mu.Unlock()
}

func TestSchedulerRecoversAfterClaimPersistenceFailure(t *testing.T) {
	workspace := t.TempDir()
	storePath := workspaceLocalPath(workspace, "scheduled_tasks.json")
	store := NewScheduledTaskStore(storePath)
	task := ScheduledTask{
		ID: "recover-claim", BotID: "bot-a", Enabled: true, Spec: "@every 5ms", AgentName: "traex", Cwd: workspace,
		Prompt: "run", MaxRuns: 1, OverlapPolicy: scheduleOverlapSkipIfRunning,
	}
	if _, err := store.Upsert(task); err != nil {
		t.Fatal(err)
	}
	blockingPath := filepath.Join(workspace, "blocking")
	if err := os.WriteFile(blockingPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	setScheduledTaskStorePathForTest(store, filepath.Join(blockingPath, "scheduled_tasks.json"))

	runStarted := make(chan struct{}, 1)
	scheduler := newScheduler(schedulerHooks{
		startBackground: func(ctx context.Context, _ string, run func(context.Context)) bool {
			go run(ctx)
			return true
		},
		runTriggerPrompt: func(_ context.Context, req TriggerRequest) (TriggerResult, error) {
			select {
			case runStarted <- struct{}{}:
			default:
			}
			return TriggerResult{Request: req}, nil
		},
	})
	scheduler.persistenceRetryBase = time.Millisecond
	scheduler.setStore("bot-a", store)
	if err := scheduler.startScheduledTask(context.Background(), task, workspace); err != nil {
		t.Fatal(err)
	}
	defer scheduler.stopScheduledTask(context.Background(), task)

	waitForCondition(t, time.Second, func() bool {
		status, ok := scheduler.lastRunStatus(task.BotID, task.ID)
		return ok && status.State == scheduleRunFailed && strings.Contains(status.LastError, "保存定时任务执行次数")
	})
	if got := scheduler.jobCount(); got != 1 {
		t.Fatalf("job count after claim persistence failure = %d, want 1", got)
	}
	stored, _ := store.Get(task.ID)
	if stored.RunCount != 0 || !stored.Enabled {
		t.Fatalf("stored task after failed claim = %+v, want enabled with run_count 0", stored)
	}

	setScheduledTaskStorePathForTest(store, storePath)
	select {
	case <-runStarted:
	case <-time.After(time.Second):
		t.Fatal("scheduled run did not recover after restoring persistence")
	}
	waitForCondition(t, time.Second, func() bool {
		stored, ok := store.Get(task.ID)
		return ok && stored.RunCount == 1 && !stored.Enabled
	})
}

func TestSchedulerRecoversAfterDisablePersistenceFailure(t *testing.T) {
	workspace := t.TempDir()
	storePath := workspaceLocalPath(workspace, "scheduled_tasks.json")
	store := NewScheduledTaskStore(storePath)
	task := ScheduledTask{
		ID: "recover-disable", BotID: "bot-a", Enabled: true, Spec: "@every 1m", AgentName: "traex", Cwd: workspace,
		Prompt: "run", MaxRuns: 1, RunCount: 1, OverlapPolicy: scheduleOverlapSkipIfRunning,
	}
	if _, err := store.Upsert(task); err != nil {
		t.Fatal(err)
	}
	blockingPath := filepath.Join(workspace, "blocking")
	if err := os.WriteFile(blockingPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	setScheduledTaskStorePathForTest(store, filepath.Join(blockingPath, "scheduled_tasks.json"))

	scheduler := newScheduler(schedulerHooks{
		startBackground: func(ctx context.Context, _ string, run func(context.Context)) bool {
			go run(ctx)
			return true
		},
	})
	scheduler.persistenceRetryBase = time.Millisecond
	scheduler.setStore("bot-a", store)
	if err := scheduler.startScheduledTask(context.Background(), task, workspace); err != nil {
		t.Fatal(err)
	}
	defer scheduler.stopScheduledTask(context.Background(), task)

	time.Sleep(10 * time.Millisecond)
	stored, _ := store.Get(task.ID)
	if !stored.Enabled {
		t.Fatal("task disabled in memory despite persistence failure")
	}
	if got := scheduler.jobCount(); got != 1 {
		t.Fatalf("job count after disable persistence failure = %d, want retrying job", got)
	}

	setScheduledTaskStorePathForTest(store, storePath)
	waitForCondition(t, time.Second, func() bool {
		stored, ok := store.Get(task.ID)
		return ok && !stored.Enabled
	})
	waitForCondition(t, time.Second, func() bool { return scheduler.jobCount() == 0 })
}

func TestSchedulerKeepsFinalRunManagedUntilItFinishes(t *testing.T) {
	workspace := t.TempDir()
	store := NewScheduledTaskStore(workspaceLocalPath(workspace, "scheduled_tasks.json"))
	task := ScheduledTask{
		ID: "limited", BotID: "bot-a", Enabled: true, Spec: "@every 5ms", AgentName: "traex", Cwd: workspace,
		Prompt: "run", MaxRuns: 1, OverlapPolicy: scheduleOverlapSkipIfRunning,
	}
	if _, err := store.Upsert(task); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan struct{})
	var startedOnce sync.Once
	var finishedOnce sync.Once
	scheduler := newScheduler(schedulerHooks{
		startBackground: func(ctx context.Context, _ string, run func(context.Context)) bool {
			go run(ctx)
			return true
		},
		runTriggerPrompt: func(ctx context.Context, req TriggerRequest) (TriggerResult, error) {
			startedOnce.Do(func() { close(started) })
			<-ctx.Done()
			finishedOnce.Do(func() { close(finished) })
			return TriggerResult{Request: req, Err: ctx.Err()}, ctx.Err()
		},
	})
	scheduler.setStore("bot-a", store)
	if err := scheduler.startScheduledTask(context.Background(), task, workspace); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for final run")
	}
	waitForCondition(t, time.Second, func() bool {
		stored, ok := store.Get(task.ID)
		return ok && !stored.Enabled && stored.RunCount == 1
	})
	if got := scheduler.jobCount(); got != 1 {
		t.Fatalf("job count = %d, want final active run still managed", got)
	}
	scheduler.stopScheduledTask(context.Background(), task)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("stopping task did not cancel final run")
	}
}

func TestSchedulerOverlapSkipDoesNotClaimRunCount(t *testing.T) {
	workspace := t.TempDir()
	store := NewScheduledTaskStore(workspaceLocalPath(workspace, "scheduled_tasks.json"))
	task := ScheduledTask{
		ID: "limited", BotID: "bot-a", Enabled: true, Spec: "@every 1m", AgentName: "traex", Cwd: workspace,
		Prompt: "run", MaxRuns: 3, OverlapPolicy: scheduleOverlapSkipIfRunning,
	}
	stored, err := store.Upsert(task)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := newScheduler(schedulerHooks{})
	scheduler.setStore("bot-a", store)
	job := &scheduledTaskJob{task: stored, workspace: workspace}
	scheduler.markRunRunning(task, "run-1", scheduledTaskRunKey(task, "run-1"), time.Now(), task.Prompt)
	_, early, err := scheduler.prepareScheduledTaskRun(task, "run-2", time.Now(), workspace, nil, func() (ScheduledTask, error) {
		return scheduler.claimScheduledTaskRun(job)
	})
	if err != nil || early == nil || !early.TriggerResult.Skipped {
		t.Fatalf("prepare result early=%+v err=%v, want overlap skip", early, err)
	}
	got, _ := store.Get(task.ID)
	if got.RunCount != 0 {
		t.Fatalf("run count = %d, want overlap skip not counted", got.RunCount)
	}
}

func TestSchedulerTransientCleanupDoesNotOrphanConcurrentRun(t *testing.T) {
	for i := 0; i < 100; i++ {
		scheduler := newScheduler(schedulerHooks{})
		task := ScheduledTask{ID: "manual", BotID: "bot-a", AgentName: "traex", Cwd: t.TempDir(), Spec: "@every 1h", Prompt: "run"}
		_, cancelFirst := context.WithCancel(context.Background())
		first := scheduler.registerExecutionRun(task, task.Cwd, "run-1", scheduledTaskRunKey(task, "run-1"), cancelFirst)
		_, cancelSecond := context.WithCancel(context.Background())
		start := make(chan struct{})
		var second *scheduledTaskJob
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			scheduler.finishExecutionRun(first, "run-1")
		}()
		go func() {
			defer wg.Done()
			<-start
			second = scheduler.registerExecutionRun(task, task.Cwd, "run-2", scheduledTaskRunKey(task, "run-2"), cancelSecond)
		}()
		close(start)
		wg.Wait()
		if got := scheduler.jobCount(); got != 1 {
			t.Fatalf("iteration %d: job count = %d, want concurrent run managed", i, got)
		}
		second.mu.Lock()
		_, active := second.activeRuns["run-2"]
		second.mu.Unlock()
		if !active {
			t.Fatalf("iteration %d: run-2 missing from managed job", i)
		}
		scheduler.stopScheduledTask(context.Background(), task)
		cancelFirst()
		cancelSecond()
	}
}

func TestSchedulerConcurrentStopAndManualRegistrationHasNoOrphan(t *testing.T) {
	for i := 0; i < 100; i++ {
		scheduler := newScheduler(schedulerHooks{})
		task := ScheduledTask{ID: "manual", BotID: "bot-a", AgentName: "traex", Cwd: t.TempDir(), Spec: "@every 1h", Prompt: "run"}
		_, cancelExisting := context.WithCancel(context.Background())
		scheduler.registerExecutionRun(task, task.Cwd, "existing", scheduledTaskRunKey(task, "existing"), cancelExisting)
		newCtx, cancelNew := context.WithCancel(context.Background())
		start := make(chan struct{})
		var registered *scheduledTaskJob
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			scheduler.stopScheduledTask(context.Background(), task)
		}()
		go func() {
			defer wg.Done()
			<-start
			registered = scheduler.registerExecutionRun(task, task.Cwd, "new", scheduledTaskRunKey(task, "new"), cancelNew)
		}()
		close(start)
		wg.Wait()
		select {
		case <-newCtx.Done():
			// Registration linearized before stop; the run was cancelled.
		default:
			if got := scheduler.jobCount(); got != 1 {
				t.Fatalf("iteration %d: uncancelled run is orphaned, job count = %d", i, got)
			}
			registered.mu.Lock()
			_, active := registered.activeRuns["new"]
			registered.mu.Unlock()
			if !active {
				t.Fatalf("iteration %d: uncancelled run missing from scheduler job", i)
			}
		}
		scheduler.stopScheduledTask(context.Background(), task)
		cancelExisting()
		cancelNew()
	}
}

func TestSchedulerConcurrentStopAndPersistenceRetryTransitionIsCancelable(t *testing.T) {
	for i := 0; i < 100; i++ {
		scheduler := newScheduler(schedulerHooks{})
		task := ScheduledTask{ID: "manual", BotID: "bot-a", AgentName: "traex", Cwd: t.TempDir(), Spec: "@every 1h", Prompt: "run"}
		_, runCancel := context.WithCancel(context.Background())
		job := scheduler.registerExecutionRun(task, task.Cwd, "run-1", scheduledTaskRunKey(task, "run-1"), runCancel)
		retryCtx, retryCancel := context.WithCancel(context.Background())
		start := make(chan struct{})
		var replaced bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			replaced = scheduler.replaceExecutionRunCancel(job, "run-1", retryCancel)
		}()
		go func() {
			defer wg.Done()
			<-start
			scheduler.stopScheduledTask(context.Background(), task)
		}()
		close(start)
		wg.Wait()
		if replaced {
			select {
			case <-retryCtx.Done():
			default:
				t.Fatalf("iteration %d: registered persistence retry was not cancelled by stop", i)
			}
		}
		runCancel()
		retryCancel()
	}
}

func TestSchedulerClaimUsesPersistedRunCount(t *testing.T) {
	workspace := t.TempDir()
	store := NewScheduledTaskStore(workspaceLocalPath(workspace, "scheduled_tasks.json"))
	task := ScheduledTask{
		ID: "limited", BotID: "bot-a", Enabled: true, Spec: "@every 1m", AgentName: "traex", Cwd: workspace,
		Prompt: "run", MaxRuns: 5, RunCount: 2,
	}
	if _, err := store.Upsert(task); err != nil {
		t.Fatal(err)
	}
	scheduler := newScheduler(schedulerHooks{})
	scheduler.setStore("bot-a", store)
	stale := task
	stale.RunCount = 0
	job := &scheduledTaskJob{task: stale, workspace: workspace}
	claimed, err := scheduler.claimScheduledTaskRun(job)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.RunCount != 3 {
		t.Fatalf("claimed run count = %d, want persisted 2 incremented to 3", claimed.RunCount)
	}
	persisted, _ := store.Get(task.ID)
	if persisted.RunCount != 3 {
		t.Fatalf("persisted run count = %d, want 3", persisted.RunCount)
	}

	if _, _, err := store.Update(task.ID, func(current *ScheduledTask) { current.RunCount = current.MaxRuns }); err != nil {
		t.Fatal(err)
	}
	job.mu.Lock()
	job.task.RunCount = 0
	job.mu.Unlock()
	if _, err := scheduler.claimScheduledTaskRun(job); err == nil || !strings.Contains(err.Error(), "最大执行次数") {
		t.Fatalf("claim error = %v, want persisted max-runs rejection", err)
	}
	persisted, _ = store.Get(task.ID)
	if persisted.RunCount != persisted.MaxRuns {
		t.Fatalf("persisted run count = %d, want no rollback from max %d", persisted.RunCount, persisted.MaxRuns)
	}
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
