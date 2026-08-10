package bridge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const (
	scheduleOverlapSkipIfRunning = "skip_if_running"
	sessionSourceSchedule        = "schedule"
	scheduleRunHistoryLimit      = 100
	scheduleStreamCardRunning    = "定时任务执行中"
	scheduleStreamCardCompleted  = "定时任务已完成"
	scheduleStreamCardResult     = "定时任务执行结果"
)

type scheduleRunState string

const (
	scheduleRunPending   scheduleRunState = "pending"
	scheduleRunRunning   scheduleRunState = "running"
	scheduleRunSkipped   scheduleRunState = "skipped"
	scheduleRunCompleted scheduleRunState = "completed"
	scheduleRunFailed    scheduleRunState = "failed"
	scheduleRunCancelled scheduleRunState = "cancelled"
)

// ScheduledTask 描述一个持久化定时任务定义。
type ScheduledTask struct {
	ID                   string                  `json:"id"`
	BotID                string                  `json:"bot_id"`
	Enabled              bool                    `json:"enabled"`
	Once                 bool                    `json:"once,omitempty"`
	Spec                 string                  `json:"spec"`
	Timezone             string                  `json:"timezone,omitempty"`
	AgentName            string                  `json:"agent_name"`
	Cwd                  string                  `json:"cwd"`
	Prompt               string                  `json:"prompt"`
	CreatorOpenID        string                  `json:"creator_open_id,omitempty"`
	CreatedFromChatID    string                  `json:"created_from_chat_id,omitempty"`
	CreatedFromThreadID  string                  `json:"created_from_thread_id,omitempty"`
	CreatedFromMessageID string                  `json:"created_from_message_id,omitempty"`
	ResultSink           ScheduledTaskResultSink `json:"result_sink,omitempty"`
	OverlapPolicy        string                  `json:"overlap_policy"`
	CreatedAt            time.Time               `json:"created_at"`
	UpdatedAt            time.Time               `json:"updated_at"`
}

type ScheduledTaskResultSink struct {
	Type      string `json:"type,omitempty"`
	ChatID    string `json:"chat_id,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
}

type scheduleRunStatus struct {
	TaskID      string
	RunID       string
	State       scheduleRunState
	StartedAt   time.Time
	EndedAt     time.Time
	SkippedAt   time.Time
	SkipReason  string
	LastError   string
	SessionKey  SessionKey
	TriggerText string
}

type scheduledTaskRunResult struct {
	RunID         string
	TriggerResult TriggerResult
	Status        scheduleRunStatus
}

type scheduledTaskJob struct {
	task       ScheduledTask
	workspace  string
	schedule   scheduleSpec
	cancel     context.CancelFunc
	mu         sync.Mutex
	activeRuns map[string]scheduledTaskActiveRun
}

type scheduledTaskActiveRun struct {
	key    SessionKey
	cancel context.CancelFunc
}

type scheduledTaskIMSender func(context.Context, feishu.Message, string, feishu.OutboundRenderContext) error
type scheduledTaskMessageSender func(context.Context, feishu.Message, string) (feishu.SentMessage, error)
type scheduledTaskStreamStarter func(context.Context, feishu.Message, feishu.StreamCardOptions) (feishu.StreamCard, error)

type scheduledTaskIMSink struct {
	message       feishu.Message
	cwd           string
	taskID        string
	store         *SessionStore
	sender        scheduledTaskIMSender
	messageSender scheduledTaskMessageSender
	starter       scheduledTaskStreamStarter
	stream        *promptCardStream
	chunks        *promptChunkAccumulator
}

func (s *Service) runScheduledTaskOnce(ctx context.Context, task ScheduledTask, runID string, triggeredAt time.Time, workspace string, sink TriggerSink) (scheduledTaskRunResult, error) {
	task = normalizeScheduledTask(task)
	runID = strings.TrimSpace(runID)
	if triggeredAt.IsZero() {
		triggeredAt = time.Now()
	}
	if sink == nil {
		sink = s.scheduledTaskSink(task)
	}
	req, err := scheduledTaskTriggerRequest(task, runID, triggeredAt, workspace, sink)
	if err != nil {
		return scheduledTaskRunResult{RunID: runID}, err
	}
	if skipped, ok := s.markScheduleRunRunningOrSkipped(task, runID, req.Key, triggeredAt, req.Prompt); ok {
		return scheduledTaskRunResult{RunID: runID, Status: skipped, TriggerResult: TriggerResult{
			Request:    req,
			Skipped:    true,
			SkipReason: skipped.SkipReason,
		}}, nil
	}
	result, err := s.runTriggerPrompt(ctx, req)
	status := s.markScheduleRunFinished(task.ID, runID, time.Now(), err)
	return scheduledTaskRunResult{RunID: runID, TriggerResult: result, Status: status}, err
}

func scheduledTaskTriggerRequest(task ScheduledTask, runID string, triggeredAt time.Time, workspace string, sink TriggerSink) (TriggerRequest, error) {
	task = normalizeScheduledTask(task)
	runID = strings.TrimSpace(runID)
	workspace = strings.TrimSpace(workspace)
	if !validScheduledTask(task) {
		return TriggerRequest{}, fmt.Errorf("定时任务字段不完整")
	}
	if runID == "" {
		return TriggerRequest{}, fmt.Errorf("定时任务 run_id 不能为空")
	}
	if workspace == "" {
		return TriggerRequest{}, fmt.Errorf("定时任务 workspace 不能为空")
	}
	return TriggerRequest{
		BotID:     task.BotID,
		Key:       scheduledTaskRunKey(task, runID),
		Workspace: workspace,
		AgentName: task.AgentName,
		Cwd:       task.Cwd,
		Title:     task.ID,
		Prompt:    scheduledTaskRunPrompt(task, runID, triggeredAt),
		Metadata:  scheduledTaskRunMetadata(task, runID, triggeredAt),
		Sink:      sink,
	}, nil
}

func (s *Service) scheduledTaskStoreForBotID(botID string) *ScheduledTaskStore {
	if s.scheduleStores == nil {
		return nil
	}
	if store := s.scheduleStores[strings.TrimSpace(botID)]; store != nil {
		return store
	}
	return s.scheduleStores[""]
}

func (s *Service) scheduledTaskJobCount() int {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	return len(s.scheduleJobs)
}

func scheduledTaskJobID(task ScheduledTask) string {
	task = normalizeScheduledTask(task)
	return task.BotID + "\x00" + task.ID
}

func scheduledTaskRunID(task ScheduledTask, triggeredAt time.Time) string {
	task = normalizeScheduledTask(task)
	if triggeredAt.IsZero() {
		triggeredAt = time.Now()
	}
	return task.ID + "-" + triggeredAt.UTC().Format("20060102T150405.000000000Z")
}

func (s *Service) markScheduleRunRunningOrSkipped(task ScheduledTask, runID string, key SessionKey, startedAt time.Time, prompt string) (scheduleRunStatus, bool) {
	task = normalizeScheduledTask(task)
	runID = strings.TrimSpace(runID)
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	key = normalizeSessionKey(key)
	prompt = strings.TrimSpace(prompt)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if task.OverlapPolicy == scheduleOverlapSkipIfRunning {
		if running := s.runningScheduleTaskRunLocked(task.ID); running != "" {
			status := scheduleRunStatus{
				TaskID:     task.ID,
				RunID:      runID,
				State:      scheduleRunSkipped,
				SkippedAt:  startedAt,
				EndedAt:    startedAt,
				SkipReason: "已有运行中的定时任务 run: " + running,
				SessionKey: key,
			}
			s.setScheduleRunStatusLocked(status)
			s.pruneScheduleRunsLocked(task.ID)
			return status, true
		}
	}
	status := scheduleRunStatus{
		TaskID:      task.ID,
		RunID:       runID,
		State:       scheduleRunRunning,
		StartedAt:   startedAt,
		SessionKey:  key,
		TriggerText: prompt,
	}
	s.setScheduleRunStatusLocked(status)
	s.pruneScheduleRunsLocked(task.ID)
	return status, false
}

func (s *Service) markScheduleRunPending(task ScheduledTask, runID string, key SessionKey, triggeredAt time.Time) scheduleRunStatus {
	task = normalizeScheduledTask(task)
	runID = strings.TrimSpace(runID)
	if triggeredAt.IsZero() {
		triggeredAt = time.Now()
	}
	status := scheduleRunStatus{
		TaskID:     task.ID,
		RunID:      runID,
		State:      scheduleRunPending,
		StartedAt:  triggeredAt,
		SessionKey: normalizeSessionKey(key),
	}
	s.setScheduleRunStatus(status)
	return status
}

func (s *Service) markScheduleRunRunning(task ScheduledTask, runID string, key SessionKey, startedAt time.Time, prompt string) scheduleRunStatus {
	task = normalizeScheduledTask(task)
	runID = strings.TrimSpace(runID)
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	status := scheduleRunStatus{
		TaskID:      task.ID,
		RunID:       runID,
		State:       scheduleRunRunning,
		StartedAt:   startedAt,
		SessionKey:  normalizeSessionKey(key),
		TriggerText: strings.TrimSpace(prompt),
	}
	s.setScheduleRunStatus(status)
	return status
}

func (s *Service) markScheduleRunSkipped(task ScheduledTask, runID string, key SessionKey, skippedAt time.Time, reason string) scheduleRunStatus {
	task = normalizeScheduledTask(task)
	runID = strings.TrimSpace(runID)
	if skippedAt.IsZero() {
		skippedAt = time.Now()
	}
	status := scheduleRunStatus{
		TaskID:     task.ID,
		RunID:      runID,
		State:      scheduleRunSkipped,
		SkippedAt:  skippedAt,
		EndedAt:    skippedAt,
		SkipReason: strings.TrimSpace(reason),
		SessionKey: normalizeSessionKey(key),
	}
	s.setScheduleRunStatus(status)
	return status
}

func (s *Service) markScheduleRunFinished(taskID, runID string, endedAt time.Time, err error) scheduleRunStatus {
	if endedAt.IsZero() {
		endedAt = time.Now()
	}
	id := scheduleRunStatusID(taskID, runID)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	status := s.scheduleRuns[id]
	status.TaskID = strings.TrimSpace(firstNonEmpty(status.TaskID, taskID))
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
	s.setScheduleRunStatusLocked(status)
	s.pruneScheduleRunsLocked(status.TaskID)
	return status
}

func (s *Service) scheduleRunStatus(taskID, runID string) (scheduleRunStatus, bool) {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	status, ok := s.scheduleRuns[scheduleRunStatusID(taskID, runID)]
	return status, ok
}

func (s *Service) setScheduleRunStatus(status scheduleRunStatus) {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	s.setScheduleRunStatusLocked(status)
	s.pruneScheduleRunsLocked(status.TaskID)
}

func (s *Service) setScheduleRunStatusLocked(status scheduleRunStatus) {
	id := scheduleRunStatusID(status.TaskID, status.RunID)
	if s.scheduleRuns == nil {
		s.scheduleRuns = make(map[string]scheduleRunStatus)
	}
	if s.scheduleRunsByTask == nil {
		s.scheduleRunsByTask = make(map[string]map[string]struct{})
	}
	if previous, ok := s.scheduleRuns[id]; ok && previous.TaskID != "" && previous.TaskID != status.TaskID {
		s.removeScheduleRunIndexLocked(previous.TaskID, id)
	}
	s.scheduleRuns[id] = status
	taskID := strings.TrimSpace(status.TaskID)
	if taskID == "" {
		return
	}
	index := s.scheduleRunsByTask[taskID]
	if index == nil {
		index = make(map[string]struct{})
		s.scheduleRunsByTask[taskID] = index
	}
	index[id] = struct{}{}
}

func (s *Service) deleteScheduleRunStatusLocked(status scheduleRunStatus) {
	id := scheduleRunStatusID(status.TaskID, status.RunID)
	delete(s.scheduleRuns, id)
	s.removeScheduleRunIndexLocked(status.TaskID, id)
}

func (s *Service) removeScheduleRunIndexLocked(taskID, id string) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" || s.scheduleRunsByTask == nil {
		return
	}
	index := s.scheduleRunsByTask[taskID]
	if index == nil {
		return
	}
	delete(index, id)
	if len(index) == 0 {
		delete(s.scheduleRunsByTask, taskID)
	}
}

func (s *Service) pruneScheduleRunsLocked(taskID string) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return
	}
	index := s.scheduleRunsByTask[taskID]
	if len(index) <= scheduleRunHistoryLimit {
		return
	}
	history := make([]scheduleRunStatus, 0, len(index))
	for id := range index {
		status, ok := s.scheduleRuns[id]
		if !ok || status.TaskID != taskID {
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
		s.deleteScheduleRunStatusLocked(status)
		history = history[1:]
	}
}

func (s *Service) runningScheduleTaskRunLocked(taskID string) string {
	taskID = strings.TrimSpace(taskID)
	for id := range s.scheduleRunsByTask[taskID] {
		status, ok := s.scheduleRuns[id]
		if ok && status.TaskID == taskID && status.State == scheduleRunRunning {
			return status.RunID
		}
	}
	return ""
}

func scheduleRunStatusID(taskID, runID string) string {
	return strings.TrimSpace(taskID) + "\x00" + strings.TrimSpace(runID)
}

func scheduledTaskRunKey(task ScheduledTask, runID string) SessionKey {
	task = normalizeScheduledTask(task)
	runID = strings.TrimSpace(runID)
	return SessionKey{
		BotID:  task.BotID,
		Source: sessionSourceSchedule,
		MainID: "task:" + task.ID,
		SubID:  "run:" + runID,
	}
}

func scheduledTaskRunPrompt(task ScheduledTask, runID string, triggeredAt time.Time) string {
	return promptWithUserMessage([]string{
		promptMetadataSection("## Schedule Metadata", scheduledTaskRunOrderedMetadata(task, runID, triggeredAt)),
	}, strings.TrimSpace(task.Prompt))
}

func scheduledTaskRunMetadata(task ScheduledTask, runID string, triggeredAt time.Time) map[string]string {
	task = normalizeScheduledTask(task)
	runID = strings.TrimSpace(runID)
	metadata := make(map[string]string)
	for _, field := range scheduledTaskRunOrderedMetadata(task, runID, triggeredAt) {
		key := strings.TrimSpace(field.Key)
		value := strings.TrimSpace(field.Value)
		if key != "" && value != "" {
			metadata[key] = value
		}
	}
	return metadata
}

func scheduledTaskRunOrderedMetadata(task ScheduledTask, runID string, triggeredAt time.Time) orderedPromptMetadata {
	task = normalizeScheduledTask(task)
	triggeredAtText := ""
	if !triggeredAt.IsZero() {
		triggeredAtText = triggeredAt.Format(time.RFC3339)
	}
	return orderedPromptMetadata{
		{"source", sessionSourceSchedule},
		{"task_id", task.ID},
		{"run_id", strings.TrimSpace(runID)},
		{"schedule_spec", task.Spec},
		{"timezone", task.Timezone},
		{"triggered_at", triggeredAtText},
		{"creator_open_id", task.CreatorOpenID},
		{"created_from_chat_id", task.CreatedFromChatID},
		{"created_from_thread_id", task.CreatedFromThreadID},
		{"created_from_message_id", task.CreatedFromMessageID},
		{"result_sink_type", task.ResultSink.Type},
		{"result_sink_chat_id", task.ResultSink.ChatID},
		{"result_sink_thread_id", task.ResultSink.ThreadID},
		{"result_sink_message_id", task.ResultSink.MessageID},
	}
}

func normalizeScheduledTask(task ScheduledTask) ScheduledTask {
	task.ID = strings.TrimSpace(task.ID)
	task.BotID = strings.TrimSpace(task.BotID)
	task.Spec = strings.TrimSpace(task.Spec)
	task.Timezone = strings.TrimSpace(task.Timezone)
	task.AgentName = strings.TrimSpace(task.AgentName)
	task.Cwd = strings.TrimSpace(task.Cwd)
	task.Prompt = strings.TrimSpace(task.Prompt)
	task.CreatorOpenID = strings.TrimSpace(task.CreatorOpenID)
	task.CreatedFromChatID = strings.TrimSpace(task.CreatedFromChatID)
	task.CreatedFromThreadID = strings.TrimSpace(task.CreatedFromThreadID)
	task.CreatedFromMessageID = strings.TrimSpace(task.CreatedFromMessageID)
	task.ResultSink.Type = strings.TrimSpace(task.ResultSink.Type)
	task.ResultSink.ChatID = strings.TrimSpace(task.ResultSink.ChatID)
	task.ResultSink.ThreadID = strings.TrimSpace(task.ResultSink.ThreadID)
	task.ResultSink.MessageID = strings.TrimSpace(task.ResultSink.MessageID)
	task.OverlapPolicy = strings.TrimSpace(task.OverlapPolicy)
	if task.OverlapPolicy == "" {
		task.OverlapPolicy = scheduleOverlapSkipIfRunning
	}
	return task
}

func validScheduledTask(task ScheduledTask) bool {
	return task.ID != "" &&
		task.BotID != "" &&
		task.Spec != "" &&
		task.AgentName != "" &&
		task.Cwd != "" &&
		task.Prompt != ""
}
