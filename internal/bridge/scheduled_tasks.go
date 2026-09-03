package bridge

import (
	"context"
	"fmt"
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
	BotID       string
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
		BotID:          task.BotID,
		Key:            scheduledTaskRunKey(task, runID),
		TraceMessageID: scheduledTaskTraceMessageID(task, runID),
		Workspace:      workspace,
		AgentName:      task.AgentName,
		Cwd:            task.Cwd,
		Title:          task.ID,
		Prompt:         scheduledTaskRunPrompt(task, runID, triggeredAt),
		Metadata:       scheduledTaskRunMetadata(task, runID, triggeredAt),
		Sink:           sink,
	}, nil
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

func scheduledTaskTraceMessageID(task ScheduledTask, runID string) string {
	task = normalizeScheduledTask(task)
	return traceMessageID(sessionSourceSchedule, task.ID, runID)
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
