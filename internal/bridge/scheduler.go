package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

// scheduler 负责定时任务定义、调度 job、运行状态和结果发送路由。
// 需要触达会话或 Service 生命周期的操作通过窄 hooks 注入。
type scheduler struct {
	mu sync.Mutex

	stores               map[string]*ScheduledTaskStore
	senders              map[string]scheduledTaskIMSender
	messageSenders       map[string]scheduledTaskMessageSender
	streams              map[string]scheduledTaskStreamStarter
	runs                 map[string]scheduleRunStatus
	runsByTask           map[string]map[string]struct{}
	jobs                 map[string]*scheduledTaskJob
	persistenceRetryBase time.Duration

	hooks schedulerHooks
}

type schedulerHooks struct {
	startBackground              func(context.Context, string, func(context.Context)) bool
	runTriggerPrompt             func(context.Context, TriggerRequest) (TriggerResult, error)
	runBoundScheduledTask        func(context.Context, ScheduledTask, string, time.Time) (TriggerResult, error)
	cancelRunningSessionWorkSync func(context.Context, SessionKey)
	cancelScheduledTaskRuns      func(context.Context, ScheduledTask)
	botWorkspace                 func(string) string
	storeForBotID                func(string) *SessionStore
	outboundForBot               func(string) feishu.Outbound
}

func newScheduler(hooks schedulerHooks) scheduler {
	return scheduler{
		stores:               make(map[string]*ScheduledTaskStore),
		senders:              make(map[string]scheduledTaskIMSender),
		messageSenders:       make(map[string]scheduledTaskMessageSender),
		streams:              make(map[string]scheduledTaskStreamStarter),
		runs:                 make(map[string]scheduleRunStatus),
		runsByTask:           make(map[string]map[string]struct{}),
		jobs:                 make(map[string]*scheduledTaskJob),
		persistenceRetryBase: time.Second,
		hooks:                hooks,
	}
}

func (s *scheduler) setStore(botID string, store *ScheduledTaskStore) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stores == nil {
		s.stores = make(map[string]*ScheduledTaskStore)
	}
	s.stores[strings.TrimSpace(botID)] = store
}

func (s *scheduler) storesSnapshot() map[string]*ScheduledTaskStore {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stores := make(map[string]*ScheduledTaskStore, len(s.stores))
	for botID, store := range s.stores {
		stores[botID] = store
	}
	return stores
}

func (s *scheduler) storeForBotID(botID string) *ScheduledTaskStore {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if store := s.stores[strings.TrimSpace(botID)]; store != nil {
		return store
	}
	return s.stores[""]
}

func (s *scheduler) setIMSender(botID string, sender scheduledTaskIMSender) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.senders == nil {
		s.senders = make(map[string]scheduledTaskIMSender)
	}
	s.senders[strings.TrimSpace(botID)] = sender
}

func (s *scheduler) setMessageSender(botID string, sender scheduledTaskMessageSender) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.messageSenders == nil {
		s.messageSenders = make(map[string]scheduledTaskMessageSender)
	}
	s.messageSenders[strings.TrimSpace(botID)] = sender
}

func (s *scheduler) setStreamStarter(botID string, starter scheduledTaskStreamStarter) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams == nil {
		s.streams = make(map[string]scheduledTaskStreamStarter)
	}
	s.streams[strings.TrimSpace(botID)] = starter
}

func (s *scheduler) loadAndStartScheduledTasks(ctx context.Context) error {
	for botID, store := range s.storesSnapshot() {
		if store == nil {
			continue
		}
		if err := store.Load(); err != nil {
			return err
		}
		workspace := s.hooks.botWorkspace(botID)
		tasks := store.List()
		registered := 0
		for _, task := range tasks {
			if !task.Enabled {
				continue
			}
			if err := s.startScheduledTask(ctx, task, workspace); err != nil {
				slog.WarnContext(ctx, "注册定时任务失败", "bot", displayBotID(botID), "task_id", task.ID, "错误", err)
				continue
			}
			registered++
		}
		slog.InfoContext(ctx, "已加载定时任务", "bot", displayBotID(botID), "数量", len(tasks), "已注册", registered)
	}
	return nil
}

func (s *scheduler) startScheduledTask(ctx context.Context, task ScheduledTask, workspace string) error {
	task = normalizeScheduledTask(task)
	workspace = strings.TrimSpace(workspace)
	if !validScheduledTask(task) {
		return fmt.Errorf("定时任务字段不完整")
	}
	if workspace == "" {
		return fmt.Errorf("定时任务 workspace 不能为空")
	}
	schedule, err := parseScheduleSpec(task.Spec, task.Timezone)
	if err != nil {
		return err
	}
	jobID := scheduledTaskJobID(task)
	s.mu.Lock()
	existing := s.jobs[jobID]
	jobBaseCtx := context.WithoutCancel(ctx)
	jobCtx, cancel := context.WithCancel(jobBaseCtx)
	job := &scheduledTaskJob{task: task, workspace: workspace, schedule: schedule, cancel: cancel}
	if s.jobs == nil {
		s.jobs = make(map[string]*scheduledTaskJob)
	}
	s.jobs[jobID] = job
	s.mu.Unlock()
	if existing != nil {
		existing.cancelActiveRuns(ctx, s.hooks.cancelRunningSessionWorkSync)
		if s.hooks.cancelScheduledTaskRuns != nil {
			s.hooks.cancelScheduledTaskRuns(ctx, task)
		}
	}
	if existing != nil && existing.cancel != nil {
		existing.cancel()
	}
	if s.hooks.startBackground == nil || !s.hooks.startBackground(
		jobCtx,
		"scheduled-task:"+task.ID,
		func(ctx context.Context) {
			s.runScheduledTaskJob(ctx, job)
		},
	) {
		cancel()
		s.removeScheduledTaskJob(job)
		return fmt.Errorf("服务正在关闭")
	}
	return nil
}

func (s *scheduler) stopScheduledTask(ctx context.Context, task ScheduledTask) {
	jobID := scheduledTaskJobID(task)
	s.mu.Lock()
	job := s.jobs[jobID]
	delete(s.jobs, jobID)
	s.mu.Unlock()
	if job != nil {
		job.cancelActiveRuns(ctx, s.hooks.cancelRunningSessionWorkSync)
		if s.hooks.cancelScheduledTaskRuns != nil {
			s.hooks.cancelScheduledTaskRuns(ctx, task)
		}
	}
	if job != nil && job.cancel != nil {
		job.cancel()
	}
}

func (s *scheduler) registerExecutionRun(task ScheduledTask, workspace, runID string, key SessionKey, cancel context.CancelFunc) *scheduledTaskJob {
	task = normalizeScheduledTask(task)
	jobID := scheduledTaskJobID(task)
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[jobID]
	if job == nil {
		job = &scheduledTaskJob{task: task, workspace: strings.TrimSpace(workspace), transient: true}
		s.jobs[jobID] = job
	}
	job.mu.Lock()
	job.addActiveRunLocked(runID, key, cancel)
	job.mu.Unlock()
	return job
}

func (s *scheduler) finishExecutionRun(job *scheduledTaskJob, runID string) {
	if job == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job.mu.Lock()
	job.removeActiveRunLocked(runID)
	jobID := scheduledTaskJobID(job.task)
	if job.transient && len(job.activeRuns) == 0 && s.jobs[jobID] == job {
		delete(s.jobs, jobID)
	}
	job.mu.Unlock()
}

func (s *scheduler) replaceExecutionRunCancel(job *scheduledTaskJob, runID string, cancel context.CancelFunc) bool {
	if job == nil || cancel == nil {
		return false
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job.mu.Lock()
	defer job.mu.Unlock()
	if s.jobs[scheduledTaskJobID(job.task)] != job {
		return false
	}
	run, ok := job.activeRuns[runID]
	if !ok {
		return false
	}
	run.cancel = cancel
	job.activeRuns[runID] = run
	return true
}

func (s *scheduler) runScheduledTaskJob(ctx context.Context, job *scheduledTaskJob) {
	defer s.removeScheduledTaskJob(job)
	persistenceFailures := 0
	for {
		now := time.Now()
		task := job.taskSnapshot()
		if reason := scheduledTaskStopReason(task, now); reason != "" {
			if !s.disableScheduledTaskWithRetry(ctx, job, reason) {
				return
			}
			job.waitForActiveRuns(ctx)
			return
		}
		// 错过执行时不补跑：过期的一次性任务直接删除，循环任务只计算当前时间之后的下一次。
		// 手动 /schedule run 不经过此循环，会显式启动一次立即执行。
		next, ok := job.schedule.Next(now)
		if !ok {
			if task.Once {
				s.completeOnceScheduledTask(ctx, task)
				return
			}
			err := fmt.Errorf("定时任务无法计算下次触发时间")
			s.markRunFinished(task, scheduledTaskRunID(task, now), now, err)
			slog.WarnContext(ctx, "定时任务无法计算下次触发时间", "task_id", task.ID, "spec", task.Spec)
			return
		}
		if task.ExpiresAt != nil && next.After(*task.ExpiresAt) {
			if !s.disableScheduledTaskWithRetry(ctx, job, "已达到截止时间") {
				return
			}
			job.waitForActiveRuns(ctx)
			return
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		triggeredAt := next
		runID := scheduledTaskRunID(task, triggeredAt)
		select {
		case <-ctx.Done():
			return
		default:
		}
		runCtx, cancel := scheduledTaskRunContext(ctx, task)
		job.addActiveRun(runID, scheduledTaskExecutionKey(task, runID), cancel)
		prepared, early, err := s.prepareScheduledTaskRun(task, runID, triggeredAt, job.workspace, nil, func() (ScheduledTask, error) {
			return s.claimScheduledTaskRun(job)
		})
		if err != nil {
			cancel()
			job.removeActiveRun(runID)
			if stopped := new(scheduledTaskStoppedError); errors.As(err, stopped) {
				if !s.disableScheduledTaskWithRetry(ctx, job, stopped.reason) {
					return
				}
				job.waitForActiveRuns(ctx)
				return
			}
			if isScheduledTaskPersistenceError(err) {
				persistenceFailures++
				slog.WarnContext(ctx, "保存定时任务执行状态失败，稍后重试", "task_id", task.ID, "run_id", runID, "连续失败次数", persistenceFailures, "错误", err)
				if !s.waitForScheduledTaskPersistenceRetry(ctx, persistenceFailures) {
					return
				}
				continue
			}
			slog.ErrorContext(ctx, "准备定时任务执行失败", "task_id", task.ID, "run_id", runID, "错误", err)
			return
		}
		persistenceFailures = 0
		if early != nil {
			cancel()
			job.removeActiveRun(runID)
			continue
		}
		task = prepared.task
		if task.Once {
			// 一次性任务：同步执行本次 run，完成后删除任务定义并退出调度循环。
			s.executePreparedScheduledTaskRun(runCtx, prepared)
			cancel()
			job.removeActiveRun(runID)
			s.completeOnceScheduledTask(ctx, task)
			return
		}
		if s.hooks.startBackground == nil || !s.hooks.startBackground(
			runCtx,
			"scheduled-run:"+task.ID+"/"+runID,
			func(ctx context.Context) {
				defer job.removeActiveRun(runID)
				defer cancel()
				s.executePreparedScheduledTaskRun(ctx, prepared)
			},
		) {
			cancel()
			job.removeActiveRun(runID)
			return
		}
	}
}

func (job *scheduledTaskJob) taskSnapshot() ScheduledTask {
	if job == nil {
		return ScheduledTask{}
	}
	job.mu.Lock()
	defer job.mu.Unlock()
	return job.task
}

func (job *scheduledTaskJob) waitForActiveRuns(ctx context.Context) {
	if job == nil {
		return
	}
	for {
		job.mu.Lock()
		if len(job.activeRuns) == 0 {
			job.mu.Unlock()
			return
		}
		if job.activeWake == nil {
			job.activeWake = make(chan struct{}, 1)
		}
		wake := job.activeWake
		job.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
	}
}

func scheduledTaskStopReason(task ScheduledTask, now time.Time) string {
	if task.MaxRuns > 0 && task.RunCount >= task.MaxRuns {
		return "已达到最大执行次数"
	}
	if task.ExpiresAt != nil && !now.Before(*task.ExpiresAt) {
		return "已达到截止时间"
	}
	return ""
}

const scheduledTaskPersistenceRetryMax = time.Minute

type scheduledTaskPersistenceError struct {
	operation string
	err       error
}

func (e *scheduledTaskPersistenceError) Error() string {
	return e.operation + ": " + e.err.Error()
}

func (e *scheduledTaskPersistenceError) Unwrap() error {
	return e.err
}

func isScheduledTaskPersistenceError(err error) bool {
	var persistenceErr *scheduledTaskPersistenceError
	return errors.As(err, &persistenceErr)
}

func (s *scheduler) waitForScheduledTaskPersistenceRetry(ctx context.Context, failures int) bool {
	delay := s.persistenceRetryBase
	if delay <= 0 {
		delay = time.Second
	}
	if delay > scheduledTaskPersistenceRetryMax {
		delay = scheduledTaskPersistenceRetryMax
	}
	for i := 1; i < failures && delay < scheduledTaskPersistenceRetryMax; i++ {
		if delay > scheduledTaskPersistenceRetryMax/2 {
			delay = scheduledTaskPersistenceRetryMax
			break
		}
		delay *= 2
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *scheduler) claimScheduledTaskRun(job *scheduledTaskJob) (ScheduledTask, error) {
	initial := job.taskSnapshot()
	store := s.storeForBotID(initial.BotID)
	job.mu.Lock()
	defer job.mu.Unlock()
	task := job.task
	if store == nil {
		if reason := scheduledTaskStopReason(task, time.Now()); reason != "" {
			return task, scheduledTaskStoppedError{reason: reason}
		}
		task.RunCount++
		job.task = task
		return task, nil
	}
	stopReason := ""
	updated, ok, err := store.Update(task.ID, func(current *ScheduledTask) {
		if reason := scheduledTaskStopReason(*current, time.Now()); reason != "" {
			stopReason = reason
			return
		}
		current.RunCount++
	})
	if err != nil {
		return task, &scheduledTaskPersistenceError{operation: "保存定时任务执行次数", err: err}
	}
	if !ok {
		return task, fmt.Errorf("定时任务不存在：%s", task.ID)
	}
	job.task = updated
	if stopReason != "" {
		return updated, scheduledTaskStoppedError{reason: stopReason}
	}
	return updated, nil
}

func (s *scheduler) disableScheduledTask(ctx context.Context, job *scheduledTaskJob, reason string) bool {
	task := job.taskSnapshot()
	store := s.storeForBotID(task.BotID)
	if store != nil {
		updated, ok, err := store.Update(task.ID, func(current *ScheduledTask) {
			current.Enabled = false
		})
		if err != nil {
			slog.WarnContext(ctx, "停用已结束的定时任务失败", "task_id", task.ID, "原因", reason, "错误", err)
			return false
		}
		if !ok {
			slog.InfoContext(ctx, "定时任务已被删除，无需自动停用", "task_id", task.ID, "原因", reason)
			return true
		}
		if ok {
			job.mu.Lock()
			job.task = updated
			job.mu.Unlock()
		}
	} else {
		job.mu.Lock()
		job.task.Enabled = false
		job.mu.Unlock()
	}
	slog.InfoContext(ctx, "定时任务已自动停用", "task_id", task.ID, "原因", reason)
	return true
}

func (s *scheduler) disableScheduledTaskWithRetry(ctx context.Context, job *scheduledTaskJob, reason string) bool {
	for failures := 1; ; failures++ {
		if s.disableScheduledTask(ctx, job, reason) {
			return true
		}
		if !s.waitForScheduledTaskPersistenceRetry(ctx, failures) {
			return false
		}
	}
}

// completeOnceScheduledTask 删除已执行完毕的一次性任务定义（持久化 + 内存），
// 使其不会在重启后再次出现。
func (s *scheduler) completeOnceScheduledTask(ctx context.Context, task ScheduledTask) {
	store := s.storeForBotID(task.BotID)
	if store != nil {
		if _, err := store.Delete(task.ID); err != nil {
			slog.WarnContext(ctx, "删除已完成一次性定时任务失败", "task_id", task.ID, "错误", err)
		}
	}
	s.stopScheduledTask(ctx, task)
	slog.InfoContext(ctx, "一次性定时任务已完成并删除", "task_id", task.ID)
}

func (s *scheduler) removeScheduledTaskJob(job *scheduledTaskJob) {
	if job == nil {
		return
	}
	jobID := scheduledTaskJobID(job.taskSnapshot())
	s.mu.Lock()
	if s.jobs[jobID] == job {
		delete(s.jobs, jobID)
	}
	s.mu.Unlock()
}

func (s *scheduler) stopScheduledTasks() {
	s.mu.Lock()
	jobs := make([]*scheduledTaskJob, 0, len(s.jobs))
	for id, job := range s.jobs {
		if job != nil {
			jobs = append(jobs, job)
		}
		delete(s.jobs, id)
	}
	s.mu.Unlock()
	for _, job := range jobs {
		// Service 关闭不属于某个飞书请求，使用有界独立 context 完成取消收尾。
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		job.cancelActiveRuns(ctx, s.hooks.cancelRunningSessionWorkSync)
		if s.hooks.cancelScheduledTaskRuns != nil {
			s.hooks.cancelScheduledTaskRuns(ctx, job.taskSnapshot())
		}
		cancel()
		if job.cancel != nil {
			job.cancel()
		}
	}
}

func (job *scheduledTaskJob) addActiveRun(runID string, key SessionKey, cancel context.CancelFunc) {
	if job == nil || cancel == nil {
		return
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return
	}
	job.mu.Lock()
	job.addActiveRunLocked(runID, key, cancel)
	job.mu.Unlock()
}

func (job *scheduledTaskJob) addActiveRunLocked(runID string, key SessionKey, cancel context.CancelFunc) {
	if job == nil || cancel == nil {
		return
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return
	}
	if job.activeRuns == nil {
		job.activeRuns = make(map[string]scheduledTaskActiveRun)
	}
	job.activeRuns[runID] = scheduledTaskActiveRun{
		key:    normalizeSessionKey(key),
		cancel: cancel,
	}
	job.notifyActiveChangedLocked()
}

func (job *scheduledTaskJob) removeActiveRun(runID string) {
	if job == nil {
		return
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return
	}
	job.mu.Lock()
	job.removeActiveRunLocked(runID)
	job.mu.Unlock()
}

func (job *scheduledTaskJob) removeActiveRunLocked(runID string) {
	if job == nil {
		return
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return
	}
	if _, ok := job.activeRuns[runID]; ok {
		delete(job.activeRuns, runID)
		job.notifyActiveChangedLocked()
	}
}

func (job *scheduledTaskJob) notifyActiveChangedLocked() {
	if job.activeWake == nil {
		return
	}
	select {
	case job.activeWake <- struct{}{}:
	default:
	}
}

func (job *scheduledTaskJob) cancelActiveRuns(ctx context.Context, cancelRunningSessionWorkSync func(context.Context, SessionKey)) {
	if job == nil {
		return
	}
	job.mu.Lock()
	runs := make([]scheduledTaskActiveRun, 0, len(job.activeRuns))
	for _, run := range job.activeRuns {
		runs = append(runs, run)
	}
	job.mu.Unlock()
	for _, run := range runs {
		if cancelRunningSessionWorkSync != nil && run.key.Valid() {
			cancelRunningSessionWorkSync(ctx, run.key)
		}
		if run.cancel != nil {
			run.cancel()
		}
	}
}

func (s *scheduler) scheduledTaskSink(task ScheduledTask) TriggerSink {
	task = normalizeScheduledTask(task)
	if !strings.EqualFold(task.ResultSink.Type, "im") || strings.TrimSpace(task.ResultSink.ChatID) == "" {
		return nil
	}
	return &scheduledTaskIMSink{message: feishu.Message{
		BotID:     task.BotID,
		ChatID:    task.ResultSink.ChatID,
		Workspace: task.Cwd,
	},
		cwd:           task.Cwd,
		taskID:        task.ID,
		store:         s.sessionStore(task.BotID),
		sender:        s.imSender(task.BotID),
		messageSender: s.messageSender(task.BotID),
		starter:       s.streamStarter(task.BotID),
	}
}

func (s *scheduler) sessionStore(botID string) *SessionStore {
	if s == nil || s.hooks.storeForBotID == nil {
		return nil
	}
	return s.hooks.storeForBotID(botID)
}

func (s *scheduler) imSender(botID string) scheduledTaskIMSender {
	if s == nil {
		return nil
	}
	botID = strings.TrimSpace(botID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if sender := s.senders[botID]; sender != nil {
		return sender
	}
	return s.senders[""]
}

func (s *scheduler) messageSender(botID string) scheduledTaskMessageSender {
	if s == nil {
		return nil
	}
	botID = strings.TrimSpace(botID)
	if s.hooks.outboundForBot != nil {
		if sender, ok := s.hooks.outboundForBot(botID).(sentMessageSender); ok && sender != nil {
			return sender.SendTextMessage
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sender := s.messageSenders[botID]; sender != nil {
		return sender
	}
	return s.messageSenders[""]
}

func (s *scheduler) streamStarter(botID string) scheduledTaskStreamStarter {
	if s == nil {
		return nil
	}
	botID = strings.TrimSpace(botID)
	if s.hooks.outboundForBot != nil {
		if starter, ok := s.hooks.outboundForBot(botID).(streamCardStarter); ok && starter != nil {
			return starter.StartStreamCard
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if starter := s.streams[botID]; starter != nil {
		return starter
	}
	return s.streams[""]
}

type scheduledTaskStoppedError struct {
	reason string
}

func (e scheduledTaskStoppedError) Error() string {
	return e.reason
}

type preparedScheduledTaskRun struct {
	task        ScheduledTask
	runID       string
	triggeredAt time.Time
	request     TriggerRequest
}

func (s *scheduler) prepareScheduledTaskRun(task ScheduledTask, runID string, triggeredAt time.Time, workspace string, sink TriggerSink, claim func() (ScheduledTask, error)) (preparedScheduledTaskRun, *scheduledTaskRunResult, error) {
	task = normalizeScheduledTask(task)
	runID = strings.TrimSpace(runID)
	if triggeredAt.IsZero() {
		triggeredAt = time.Now()
	}
	if reason := scheduledTaskStopReason(task, time.Now()); reason != "" {
		return preparedScheduledTaskRun{}, nil, scheduledTaskStoppedError{reason: reason}
	}
	if sink == nil {
		sink = s.scheduledTaskSink(task)
	}
	req, err := scheduledTaskTriggerRequest(task, runID, triggeredAt, workspace, sink)
	if err != nil {
		return preparedScheduledTaskRun{}, nil, err
	}
	if skipped, ok := s.markRunRunningOrSkipped(task, runID, req.Key, triggeredAt, req.Prompt); ok {
		result := scheduledTaskRunResult{RunID: runID, Status: skipped, TriggerResult: TriggerResult{
			Request:    req,
			Skipped:    true,
			SkipReason: skipped.SkipReason,
		}}
		return preparedScheduledTaskRun{}, &result, nil
	}
	if claim != nil {
		task, err = claim()
		if err != nil {
			status := s.markRunFinished(task, runID, time.Now(), err)
			return preparedScheduledTaskRun{}, &scheduledTaskRunResult{RunID: runID, Status: status, TriggerResult: TriggerResult{Request: req, Err: err}}, err
		}
		req, err = scheduledTaskTriggerRequest(task, runID, triggeredAt, workspace, sink)
		if err != nil {
			status := s.markRunFinished(task, runID, time.Now(), err)
			return preparedScheduledTaskRun{}, &scheduledTaskRunResult{RunID: runID, Status: status, TriggerResult: TriggerResult{Request: req, Err: err}}, err
		}
	}
	return preparedScheduledTaskRun{task: task, runID: runID, triggeredAt: triggeredAt, request: req}, nil, nil
}

func (s *scheduler) executePreparedScheduledTaskRun(ctx context.Context, prepared preparedScheduledTaskRun) (scheduledTaskRunResult, error) {
	task := prepared.task
	runID := prepared.runID
	req := prepared.request
	if task.SessionMode == scheduleSessionModeBound {
		if s.hooks.runBoundScheduledTask == nil {
			err := fmt.Errorf("未配置 bound session 定时任务执行器")
			status := s.markRunFinished(task, runID, time.Now(), err)
			return scheduledTaskRunResult{RunID: runID, Status: status, TriggerResult: TriggerResult{Request: req, Err: err}}, err
		}
		result, err := s.hooks.runBoundScheduledTask(ctx, task, runID, prepared.triggeredAt)
		if result.Request.Key == (SessionKey{}) {
			result.Request = req
		}
		status := s.markRunFinished(task, runID, time.Now(), err)
		return scheduledTaskRunResult{RunID: runID, TriggerResult: result, Status: status}, err
	}
	result, err := s.hooks.runTriggerPrompt(ctx, req)
	status := s.markRunFinished(task, runID, time.Now(), err)
	return scheduledTaskRunResult{RunID: runID, TriggerResult: result, Status: status}, err
}

func (s *scheduler) runScheduledTaskOnce(ctx context.Context, task ScheduledTask, runID string, triggeredAt time.Time, workspace string, sink TriggerSink) (scheduledTaskRunResult, error) {
	prepared, early, err := s.prepareScheduledTaskRun(task, runID, triggeredAt, workspace, sink, nil)
	if early != nil {
		return *early, err
	}
	if err != nil {
		return scheduledTaskRunResult{RunID: strings.TrimSpace(runID)}, err
	}
	return s.executePreparedScheduledTaskRun(ctx, prepared)
}

func scheduledTaskExecutionKey(task ScheduledTask, runID string) SessionKey {
	task = normalizeScheduledTask(task)
	if task.SessionMode == scheduleSessionModeBound {
		// Bound runs share a user session and are cancelled through their own
		// run context. Cancelling by session key could stop an unrelated user
		// turn while this run is still waiting in the FIFO queue.
		return SessionKey{}
	}
	return scheduledTaskRunKey(task, runID)
}

func scheduledTaskRunContext(ctx context.Context, task ScheduledTask) (context.Context, context.CancelFunc) {
	if task.RunTimeoutSec > 0 {
		return context.WithTimeout(ctx, time.Duration(task.RunTimeoutSec)*time.Second)
	}
	return context.WithCancel(ctx)
}

func (s *scheduler) jobCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

func (s *scheduler) markRunRunningOrSkipped(task ScheduledTask, runID string, key SessionKey, startedAt time.Time, prompt string) (scheduleRunStatus, bool) {
	task = normalizeScheduledTask(task)
	runID = strings.TrimSpace(runID)
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	key = normalizeSessionKey(key)
	prompt = strings.TrimSpace(prompt)
	s.mu.Lock()
	defer s.mu.Unlock()
	if task.OverlapPolicy == scheduleOverlapSkipIfRunning {
		if running := s.runningTaskRunLocked(task.BotID, task.ID); running != "" {
			status := scheduleRunStatus{
				BotID:      task.BotID,
				TaskID:     task.ID,
				RunID:      runID,
				State:      scheduleRunSkipped,
				SkippedAt:  startedAt,
				EndedAt:    startedAt,
				SkipReason: "已有运行中的定时任务 run: " + running,
				SessionKey: key,
			}
			s.setRunStatusLocked(status)
			s.pruneRunsLocked(task.BotID, task.ID)
			return status, true
		}
	}
	status := scheduleRunStatus{
		BotID:       task.BotID,
		TaskID:      task.ID,
		RunID:       runID,
		State:       scheduleRunRunning,
		StartedAt:   startedAt,
		SessionKey:  key,
		TriggerText: prompt,
	}
	s.setRunStatusLocked(status)
	s.pruneRunsLocked(task.BotID, task.ID)
	return status, false
}

func (s *scheduler) markRunPending(task ScheduledTask, runID string, key SessionKey, triggeredAt time.Time) scheduleRunStatus {
	task = normalizeScheduledTask(task)
	runID = strings.TrimSpace(runID)
	if triggeredAt.IsZero() {
		triggeredAt = time.Now()
	}
	status := scheduleRunStatus{
		BotID:      task.BotID,
		TaskID:     task.ID,
		RunID:      runID,
		State:      scheduleRunPending,
		StartedAt:  triggeredAt,
		SessionKey: normalizeSessionKey(key),
	}
	s.setRunStatus(status)
	return status
}

func (s *scheduler) markRunRunning(task ScheduledTask, runID string, key SessionKey, startedAt time.Time, prompt string) scheduleRunStatus {
	task = normalizeScheduledTask(task)
	runID = strings.TrimSpace(runID)
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	status := scheduleRunStatus{
		BotID:       task.BotID,
		TaskID:      task.ID,
		RunID:       runID,
		State:       scheduleRunRunning,
		StartedAt:   startedAt,
		SessionKey:  normalizeSessionKey(key),
		TriggerText: strings.TrimSpace(prompt),
	}
	s.setRunStatus(status)
	return status
}

func (s *scheduler) markRunSkipped(task ScheduledTask, runID string, key SessionKey, skippedAt time.Time, reason string) scheduleRunStatus {
	task = normalizeScheduledTask(task)
	runID = strings.TrimSpace(runID)
	if skippedAt.IsZero() {
		skippedAt = time.Now()
	}
	status := scheduleRunStatus{
		BotID:      task.BotID,
		TaskID:     task.ID,
		RunID:      runID,
		State:      scheduleRunSkipped,
		SkippedAt:  skippedAt,
		EndedAt:    skippedAt,
		SkipReason: strings.TrimSpace(reason),
		SessionKey: normalizeSessionKey(key),
	}
	s.setRunStatus(status)
	return status
}

func (s *scheduler) markRunFinished(task ScheduledTask, runID string, endedAt time.Time, err error) scheduleRunStatus {
	task = normalizeScheduledTask(task)
	if endedAt.IsZero() {
		endedAt = time.Now()
	}
	id := scheduleRunStatusID(task.BotID, task.ID, runID)
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.runs[id]
	status.BotID = strings.TrimSpace(firstNonEmpty(status.BotID, task.BotID))
	status.TaskID = strings.TrimSpace(firstNonEmpty(status.TaskID, task.ID))
	status.RunID = strings.TrimSpace(firstNonEmpty(status.RunID, runID))
	status.EndedAt = endedAt
	if errors.Is(err, context.Canceled) {
		status.State = scheduleRunCancelled
		status.LastError = ""
	} else if err != nil {
		status.State = scheduleRunFailed
		status.LastError = err.Error()
	} else {
		status.State = scheduleRunCompleted
		status.LastError = ""
	}
	s.setRunStatusLocked(status)
	s.pruneRunsLocked(status.BotID, status.TaskID)
	return status
}

func (s *scheduler) runStatus(botID, taskID, runID string) (scheduleRunStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status, ok := s.runs[scheduleRunStatusID(botID, taskID, runID)]
	return status, ok
}

func (s *scheduler) setRunStatus(status scheduleRunStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setRunStatusLocked(status)
	s.pruneRunsLocked(status.BotID, status.TaskID)
}

func (s *scheduler) setRunStatusLocked(status scheduleRunStatus) {
	status.BotID = strings.TrimSpace(status.BotID)
	status.TaskID = strings.TrimSpace(status.TaskID)
	status.RunID = strings.TrimSpace(status.RunID)
	id := scheduleRunStatusID(status.BotID, status.TaskID, status.RunID)
	if s.runs == nil {
		s.runs = make(map[string]scheduleRunStatus)
	}
	if s.runsByTask == nil {
		s.runsByTask = make(map[string]map[string]struct{})
	}
	if previous, ok := s.runs[id]; ok {
		previousIndex := scheduleRunTaskIndexID(previous.BotID, previous.TaskID)
		index := scheduleRunTaskIndexID(status.BotID, status.TaskID)
		if previousIndex != "" && previousIndex != index {
			s.removeRunIndexLocked(previous.BotID, previous.TaskID, id)
		}
	}
	s.runs[id] = status
	indexID := scheduleRunTaskIndexID(status.BotID, status.TaskID)
	if indexID == "" {
		return
	}
	index := s.runsByTask[indexID]
	if index == nil {
		index = make(map[string]struct{})
		s.runsByTask[indexID] = index
	}
	index[id] = struct{}{}
}

func (s *scheduler) deleteRunStatusLocked(status scheduleRunStatus) {
	id := scheduleRunStatusID(status.BotID, status.TaskID, status.RunID)
	delete(s.runs, id)
	s.removeRunIndexLocked(status.BotID, status.TaskID, id)
}

func (s *scheduler) removeRunIndexLocked(botID, taskID, id string) {
	indexID := scheduleRunTaskIndexID(botID, taskID)
	if indexID == "" || s.runsByTask == nil {
		return
	}
	index := s.runsByTask[indexID]
	if index == nil {
		return
	}
	delete(index, id)
	if len(index) == 0 {
		delete(s.runsByTask, indexID)
	}
}

func (s *scheduler) pruneRunsLocked(botID, taskID string) {
	indexID := scheduleRunTaskIndexID(botID, taskID)
	if indexID == "" {
		return
	}
	index := s.runsByTask[indexID]
	if len(index) <= scheduleRunHistoryLimit {
		return
	}
	history := make([]scheduleRunStatus, 0, len(index))
	for id := range index {
		status, ok := s.runs[id]
		if !ok || scheduleRunTaskIndexID(status.BotID, status.TaskID) != indexID {
			delete(index, id)
			continue
		}
		if status.State != scheduleRunRunning {
			history = append(history, status)
		}
	}
	if len(history) <= scheduleRunHistoryLimit {
		return
	}
	sort.Slice(history, func(i, j int) bool {
		return scheduleRunStatusTime(history[i]).Before(scheduleRunStatusTime(history[j]))
	})
	for len(history) > scheduleRunHistoryLimit {
		status := history[0]
		s.deleteRunStatusLocked(status)
		history = history[1:]
	}
}

func (s *scheduler) runningTaskRunLocked(botID, taskID string) string {
	indexID := scheduleRunTaskIndexID(botID, taskID)
	for id := range s.runsByTask[indexID] {
		status, ok := s.runs[id]
		if ok && scheduleRunTaskIndexID(status.BotID, status.TaskID) == indexID && status.State == scheduleRunRunning {
			return status.RunID
		}
	}
	return ""
}

func (s *scheduler) lastRunStatus(botID, taskID string) (scheduleRunStatus, bool) {
	indexID := scheduleRunTaskIndexID(botID, taskID)
	s.mu.Lock()
	defer s.mu.Unlock()
	var last scheduleRunStatus
	var ok bool
	for id := range s.runsByTask[indexID] {
		status, exists := s.runs[id]
		if !exists || scheduleRunTaskIndexID(status.BotID, status.TaskID) != indexID {
			continue
		}
		if !ok || scheduleRunStatusTime(status).After(scheduleRunStatusTime(last)) {
			last = status
			ok = true
		}
	}
	return last, ok
}

func scheduleRunStatusTime(status scheduleRunStatus) time.Time {
	for _, t := range []time.Time{status.EndedAt, status.SkippedAt, status.StartedAt} {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func scheduleRunStatusID(botID, taskID, runID string) string {
	return scheduleRunTaskIndexID(botID, taskID) + "\x00" + strings.TrimSpace(runID)
}

func scheduleRunTaskIndexID(botID, taskID string) string {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return ""
	}
	return strings.TrimSpace(botID) + "\x00" + taskID
}
