package bridge

import (
	"context"
	"strings"
	"time"
)

// Service 保留定时任务 facade，定义、job 和运行状态由 scheduler 负责。
func (s *Service) loadAndStartScheduledTasks(ctx context.Context) error {
	return s.scheduler.loadAndStartScheduledTasks(ctx)
}

func (s *Service) startScheduledTask(ctx context.Context, task ScheduledTask, workspace string) error {
	return s.scheduler.startScheduledTask(ctx, task, workspace)
}

func (s *Service) stopScheduledTask(ctx context.Context, task ScheduledTask) {
	s.scheduler.stopScheduledTask(ctx, task)
}

func (s *Service) runScheduledTaskJob(ctx context.Context, job *scheduledTaskJob) {
	s.scheduler.runScheduledTaskJob(ctx, job)
}

func (s *Service) stopScheduledTasks() {
	s.scheduler.stopScheduledTasks()
}

func (s *Service) runScheduledTaskOnce(ctx context.Context, task ScheduledTask, runID string, triggeredAt time.Time, workspace string, sink TriggerSink) (scheduledTaskRunResult, error) {
	return s.scheduler.runScheduledTaskOnce(ctx, task, runID, triggeredAt, workspace, sink)
}

func (s *Service) scheduledTaskStoreForBotID(botID string) *ScheduledTaskStore {
	return s.scheduler.storeForBotID(botID)
}

func (s *Service) scheduledTaskJobCount() int {
	return s.scheduler.jobCount()
}

func (s *Service) markScheduleRunRunningOrSkipped(task ScheduledTask, runID string, key SessionKey, startedAt time.Time, prompt string) (scheduleRunStatus, bool) {
	return s.scheduler.markRunRunningOrSkipped(task, runID, key, startedAt, prompt)
}

func (s *Service) markScheduleRunPending(task ScheduledTask, runID string, key SessionKey, triggeredAt time.Time) scheduleRunStatus {
	return s.scheduler.markRunPending(task, runID, key, triggeredAt)
}

func (s *Service) markScheduleRunRunning(task ScheduledTask, runID string, key SessionKey, startedAt time.Time, prompt string) scheduleRunStatus {
	return s.scheduler.markRunRunning(task, runID, key, startedAt, prompt)
}

func (s *Service) markScheduleRunSkipped(task ScheduledTask, runID string, key SessionKey, skippedAt time.Time, reason string) scheduleRunStatus {
	return s.scheduler.markRunSkipped(task, runID, key, skippedAt, reason)
}

func (s *Service) markScheduleRunFinished(task ScheduledTask, runID string, endedAt time.Time, err error) scheduleRunStatus {
	return s.scheduler.markRunFinished(task, runID, endedAt, err)
}

func (s *Service) scheduleRunStatus(task ScheduledTask, runID string) (scheduleRunStatus, bool) {
	return s.scheduler.runStatus(task.BotID, task.ID, runID)
}

func (s *Service) lastScheduleRunStatus(task ScheduledTask) (scheduleRunStatus, bool) {
	return s.scheduler.lastRunStatus(task.BotID, task.ID)
}

func (s *Service) scheduledTaskSink(task ScheduledTask) TriggerSink {
	return s.scheduler.scheduledTaskSink(task)
}

func (s *Service) scheduleIMSender(botID string) scheduledTaskIMSender {
	return s.scheduler.imSender(botID)
}

func (s *Service) scheduleMessageSender(botID string) scheduledTaskMessageSender {
	return s.scheduler.messageSender(botID)
}

func (s *Service) scheduleStreamStarter(botID string) scheduledTaskStreamStarter {
	return s.scheduler.streamStarter(botID)
}

func (s *Service) setScheduledTaskIMSender(botID string, sender scheduledTaskIMSender) {
	s.scheduler.setIMSender(botID, sender)
}

func (s *Service) setScheduledTaskStreamStarter(botID string, starter scheduledTaskStreamStarter) {
	s.scheduler.setStreamStarter(botID, starter)
}

func (s *Service) botWorkspace(botID string) string {
	bot, ok := s.botConfig(botID)
	if !ok {
		return ""
	}
	return strings.TrimSpace(bot.Workspace)
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
		if key.BotID == task.BotID && key.Source == sessionSourceSchedule && key.MainID == prefix {
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
