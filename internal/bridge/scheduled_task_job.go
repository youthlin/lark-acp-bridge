package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

func (s *Service) loadAndStartScheduledTasks(ctx context.Context) error {
	for botID, store := range s.scheduleStores {
		if store == nil {
			continue
		}
		if err := store.Load(); err != nil {
			return err
		}
		workspace := s.botWorkspace(botID)
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

func (s *Service) startScheduledTask(ctx context.Context, task ScheduledTask, workspace string) error {
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
	s.taskMu.Lock()
	existing := s.scheduleJobs[jobID]
	jobBaseCtx := context.WithoutCancel(ctx)
	jobCtx, cancel := context.WithCancel(jobBaseCtx)
	job := &scheduledTaskJob{task: task, workspace: workspace, schedule: schedule, cancel: cancel}
	if s.scheduleJobs == nil {
		s.scheduleJobs = make(map[string]*scheduledTaskJob)
	}
	s.scheduleJobs[jobID] = job
	s.taskMu.Unlock()
	if existing != nil {
		existing.cancelActiveRuns(ctx, s)
		s.cancelScheduledTaskRuns(ctx, task)
	}
	if existing != nil && existing.cancel != nil {
		existing.cancel()
	}
	s.goBackground("scheduled-task:"+task.ID, func() { s.runScheduledTaskJob(jobCtx, job) })
	return nil
}

func (s *Service) stopScheduledTask(ctx context.Context, task ScheduledTask) {
	jobID := scheduledTaskJobID(task)
	s.taskMu.Lock()
	job := s.scheduleJobs[jobID]
	delete(s.scheduleJobs, jobID)
	s.taskMu.Unlock()
	if job != nil {
		job.cancelActiveRuns(ctx, s)
		s.cancelScheduledTaskRuns(ctx, task)
	}
	if job != nil && job.cancel != nil {
		job.cancel()
	}
}

func (s *Service) runScheduledTaskJob(ctx context.Context, job *scheduledTaskJob) {
	defer s.removeScheduledTaskJob(job)
	for {
		now := time.Now()
		// Missed-run policy is intentionally simple: once tasks whose trigger
		// time is already past are deleted without catch-up, and recurring
		// tasks compute only the next run after now. Manual /schedule run bypasses
		// this loop and starts an immediate run explicitly.
		next, ok := job.schedule.Next(now)
		if !ok {
			if job.task.Once {
				s.completeOnceScheduledTask(ctx, job.task)
				return
			}
			err := fmt.Errorf("定时任务无法计算下次触发时间")
			s.markScheduleRunFinished(job.task.ID, scheduledTaskRunID(job.task, now), now, err)
			slog.WarnContext(ctx, "定时任务无法计算下次触发时间", "task_id", job.task.ID, "spec", job.task.Spec)
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
		runID := scheduledTaskRunID(job.task, triggeredAt)
		select {
		case <-ctx.Done():
			return
		default:
		}
		runCtx, cancel := context.WithCancel(ctx)
		job.addActiveRun(runID, scheduledTaskRunKey(job.task, runID), cancel)
		if job.task.Once {
			// 一次性任务：同步执行本次 run，完成后删除任务定义并退出调度循环。
			s.executeScheduledTaskRun(runCtx, job, runID, triggeredAt)
			cancel()
			job.removeActiveRun(runID)
			s.completeOnceScheduledTask(ctx, job.task)
			return
		}
		s.goBackground("scheduled-run:"+job.task.ID+"/"+runID, func() {
			defer job.removeActiveRun(runID)
			defer cancel()
			s.executeScheduledTaskRun(runCtx, job, runID, triggeredAt)
		})
	}
}

func (s *Service) executeScheduledTaskRun(ctx context.Context, job *scheduledTaskJob, runID string, triggeredAt time.Time) {
	if _, err := s.runScheduledTaskOnce(ctx, job.task, runID, triggeredAt, job.workspace, nil); err != nil {
		slog.ErrorContext(ctx, "定时任务执行失败", "task_id", job.task.ID, "run_id", runID, "错误", err)
	}
}

// completeOnceScheduledTask 删除已执行完毕的一次性任务定义（持久化 + 内存），
// 使其不会在重启后再次出现。
func (s *Service) completeOnceScheduledTask(ctx context.Context, task ScheduledTask) {
	store := s.scheduledTaskStoreForBotID(task.BotID)
	if store != nil {
		if _, err := store.Delete(task.ID); err != nil {
			slog.WarnContext(ctx, "删除已完成一次性定时任务失败", "task_id", task.ID, "错误", err)
		}
	}
	s.stopScheduledTask(ctx, task)
	slog.InfoContext(ctx, "一次性定时任务已完成并删除", "task_id", task.ID)
}

func (s *Service) removeScheduledTaskJob(job *scheduledTaskJob) {
	if job == nil {
		return
	}
	jobID := scheduledTaskJobID(job.task)
	s.taskMu.Lock()
	if s.scheduleJobs[jobID] == job {
		delete(s.scheduleJobs, jobID)
	}
	s.taskMu.Unlock()
}

func (s *Service) stopScheduledTasks() {
	s.taskMu.Lock()
	jobs := make([]*scheduledTaskJob, 0, len(s.scheduleJobs))
	for id, job := range s.scheduleJobs {
		if job != nil {
			jobs = append(jobs, job)
		}
		delete(s.scheduleJobs, id)
	}
	s.taskMu.Unlock()
	for _, job := range jobs {
		// Service shutdown is not tied to a Feishu request; use a bounded
		// independent context so cancellation bookkeeping can finish.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		job.cancelActiveRuns(ctx, s)
		s.cancelScheduledTaskRuns(ctx, job.task)
		cancel()
		if job.cancel != nil {
			job.cancel()
		}
	}
}

func (s *Service) cancelScheduledTaskRuns(ctx context.Context, task ScheduledTask) {
	task = normalizeScheduledTask(task)
	if task.ID == "" {
		return
	}
	prefix := "task:" + task.ID
	s.taskMu.Lock()
	tasks := make([]*runningTask, 0)
	for key, running := range s.tasks {
		key = normalizeSessionKey(key)
		if key.Source == sessionSourceSchedule && key.MainID == prefix {
			if running != nil {
				tasks = append(tasks, running)
			}
			delete(s.tasks, key)
		}
	}
	s.taskMu.Unlock()
	for _, task := range tasks {
		task.cancel()
		if task.onCancel != nil {
			task.onCancel(ctx, "已取消")
		}
		s.cancelRuntimeTask(ctx, task)
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
	if job.activeRuns == nil {
		job.activeRuns = make(map[string]scheduledTaskActiveRun)
	}
	job.activeRuns[runID] = scheduledTaskActiveRun{
		key:    normalizeSessionKey(key),
		cancel: cancel,
	}
	job.mu.Unlock()
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
	delete(job.activeRuns, runID)
	job.mu.Unlock()
}

func (job *scheduledTaskJob) cancelActiveRuns(ctx context.Context, s *Service) {
	if job == nil {
		return
	}
	job.mu.Lock()
	runs := make([]scheduledTaskActiveRun, 0, len(job.activeRuns))
	for runID, run := range job.activeRuns {
		runs = append(runs, run)
		delete(job.activeRuns, runID)
	}
	job.mu.Unlock()
	for _, run := range runs {
		if s != nil && run.key.Valid() {
			s.cancelRunningSessionWorkSync(ctx, run.key)
		}
		if run.cancel != nil {
			run.cancel()
		}
	}
}

func (s *Service) botWorkspace(botID string) string {
	bot, ok := s.botConfig(botID)
	if !ok {
		return ""
	}
	return strings.TrimSpace(bot.Workspace)
}
