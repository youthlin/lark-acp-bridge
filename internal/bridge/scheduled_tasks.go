package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
type scheduledTaskStreamStarter func(context.Context, feishu.Message) (feishu.StreamCard, error)

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

type ScheduledTaskStore struct {
	path  string
	mu    sync.Mutex
	tasks map[string]ScheduledTask
}

type scheduledTaskStoreSnapshot struct {
	tasks map[string]ScheduledTask
}

func NewScheduledTaskStore(path string) *ScheduledTaskStore {
	return &ScheduledTaskStore{
		path:  path,
		tasks: make(map[string]ScheduledTask),
	}
}

func (s *ScheduledTaskStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.tasks = make(map[string]ScheduledTask)
			return nil
		}
		return fmt.Errorf("读取定时任务文件: %w", err)
	}
	var file scheduledTaskFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("解析定时任务文件: %w", err)
	}
	tasks := make(map[string]ScheduledTask, len(file.Tasks))
	for _, task := range file.Tasks {
		task = normalizeScheduledTask(task)
		if !validScheduledTask(task) {
			continue
		}
		tasks[task.ID] = task
	}
	s.tasks = tasks
	return nil
}

func (s *ScheduledTaskStore) Upsert(task ScheduledTask) (ScheduledTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task = normalizeScheduledTask(task)
	if !validScheduledTask(task) {
		return ScheduledTask{}, fmt.Errorf("定时任务字段不完整")
	}
	snapshot := s.snapshotLocked()
	now := time.Now()
	if existing, ok := s.tasks[task.ID]; ok && task.CreatedAt.IsZero() {
		task.CreatedAt = existing.CreatedAt
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	task.UpdatedAt = now
	s.tasks[task.ID] = task
	if err := s.writeOrRestoreLocked(snapshot); err != nil {
		return ScheduledTask{}, err
	}
	return task, nil
}

func (s *ScheduledTaskStore) Get(id string) (ScheduledTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[strings.TrimSpace(id)]
	return task, ok
}

func (s *ScheduledTaskStore) Update(id string, update func(*ScheduledTask)) (ScheduledTask, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id = strings.TrimSpace(id)
	task, ok := s.tasks[id]
	if !ok {
		return ScheduledTask{}, false, nil
	}
	snapshot := s.snapshotLocked()
	if update != nil {
		update(&task)
	}
	task = normalizeScheduledTask(task)
	if !validScheduledTask(task) {
		s.tasks = snapshot.tasks
		return ScheduledTask{}, false, fmt.Errorf("定时任务字段不完整")
	}
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	if err := s.writeOrRestoreLocked(snapshot); err != nil {
		return ScheduledTask{}, false, err
	}
	return task, true, nil
}

func (s *ScheduledTaskStore) List() []ScheduledTask {
	s.mu.Lock()
	defer s.mu.Unlock()

	tasks := make([]ScheduledTask, 0, len(s.tasks))
	for _, task := range s.tasks {
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].ID < tasks[j].ID
	})
	return tasks
}

func (s *ScheduledTaskStore) Delete(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id = strings.TrimSpace(id)
	if _, ok := s.tasks[id]; !ok {
		return false, nil
	}
	snapshot := s.snapshotLocked()
	delete(s.tasks, id)
	if err := s.writeOrRestoreLocked(snapshot); err != nil {
		return false, err
	}
	return true, nil
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
	jobCtx, cancel := context.WithCancel(ctx)
	job := &scheduledTaskJob{task: task, workspace: workspace, schedule: schedule, cancel: cancel}
	if s.scheduleJobs == nil {
		s.scheduleJobs = make(map[string]*scheduledTaskJob)
	}
	s.scheduleJobs[jobID] = job
	s.taskMu.Unlock()
	if existing != nil {
		existing.cancelActiveRuns(context.Background(), s)
		s.cancelScheduledTaskRuns(context.Background(), task)
	}
	if existing != nil && existing.cancel != nil {
		existing.cancel()
	}
	go s.runScheduledTaskJob(jobCtx, job)
	return nil
}

func (s *Service) stopScheduledTask(task ScheduledTask) {
	jobID := scheduledTaskJobID(task)
	s.taskMu.Lock()
	job := s.scheduleJobs[jobID]
	delete(s.scheduleJobs, jobID)
	s.taskMu.Unlock()
	if job != nil {
		job.cancelActiveRuns(context.Background(), s)
		s.cancelScheduledTaskRuns(context.Background(), task)
	}
	if job != nil && job.cancel != nil {
		job.cancel()
	}
}

func (s *Service) runScheduledTaskJob(ctx context.Context, job *scheduledTaskJob) {
	defer s.removeScheduledTaskJob(job)
	for {
		now := time.Now()
		next, ok := job.schedule.Next(now)
		if !ok {
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
		go func() {
			defer job.removeActiveRun(runID)
			defer cancel()
			if _, err := s.runScheduledTaskOnce(runCtx, job.task, runID, triggeredAt, job.workspace, nil); err != nil {
				slog.ErrorContext(runCtx, "定时任务执行失败", "task_id", job.task.ID, "run_id", runID, "错误", err)
			}
		}()
	}
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
		job.cancelActiveRuns(context.Background(), s)
		s.cancelScheduledTaskRuns(context.Background(), job.task)
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
	botID = strings.TrimSpace(botID)
	for _, bot := range s.cfg.Bots {
		if strings.TrimSpace(bot.ID) == botID {
			return strings.TrimSpace(bot.Workspace)
		}
	}
	return ""
}

func (s *Service) scheduledTaskSink(task ScheduledTask) TriggerSink {
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
		store:         s.storeForBotID(task.BotID),
		sender:        s.scheduleIMSender(task.BotID),
		messageSender: s.scheduleMessageSender(task.BotID),
		starter:       s.scheduleStreamStarter(task.BotID),
	}
}

func (s *Service) scheduleIMSender(botID string) scheduledTaskIMSender {
	if s == nil {
		return nil
	}
	botID = strings.TrimSpace(botID)
	if sender := s.scheduleSenders[botID]; sender != nil {
		return sender
	}
	return s.scheduleSenders[""]
}

func (s *Service) scheduleMessageSender(botID string) scheduledTaskMessageSender {
	if s == nil {
		return nil
	}
	botID = strings.TrimSpace(botID)
	if sender := s.scheduleMessageSenders[botID]; sender != nil {
		return sender
	}
	return s.scheduleMessageSenders[""]
}

func (s *Service) scheduleStreamStarter(botID string) scheduledTaskStreamStarter {
	if s == nil {
		return nil
	}
	botID = strings.TrimSpace(botID)
	if starter := s.scheduleStreams[botID]; starter != nil {
		return starter
	}
	return s.scheduleStreams[""]
}

func (s *scheduledTaskIMSink) OnUpdate(ctx context.Context, result TriggerResult) error {
	stream := s.ensureStream(ctx, result)
	if stream == nil {
		return nil
	}
	stream.updatePromptStatusFromUpdate(result.Update)
	if chunk, ok := promptUpdateChunk(result.Update); ok {
		if chunk.ToolBoundary {
			s.chunks.markToolBoundary()
		}
		s.chunks.add(chunk)
		return nil
	}
	if isToolBoundaryUpdateKind(promptUpdateKind(result.Update)) {
		s.chunks.markToolBoundary()
	} else {
		s.chunks.finishStream()
	}
	stream.updatePromptUpdate(result.Update)
	return nil
}

func (s *scheduledTaskIMSink) OnComplete(ctx context.Context, result TriggerResult) error {
	text := strings.TrimSpace(result.Text)
	if text == "" {
		text = "定时任务已完成，但没有返回文本。"
	}
	if stream := s.ensureStream(ctx, result); stream != nil {
		finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptCardFinalUpdateLimit)
		defer cancel()
		if s.chunks != nil {
			s.chunks.close()
		}
		stream.setFinalTextWithContext(finalCtx, text)
		stream.updatePromptStatusFromResultWithContext(finalCtx, result.ACPResult)
		stream.updatePromptResult(result.ACPResult)
		stream.finishPromptStatusWithContext(finalCtx, result.ACPResult.StopReason)
		stream.updateMetaWithContext(finalCtx, s.streamCardMetaWithTitle(result, scheduleStreamCardCompleted))
		stream.closeWithContext(finalCtx)
		return nil
	}
	return s.send(ctx, result, text)
}

func (s *scheduledTaskIMSink) OnError(ctx context.Context, result TriggerResult) error {
	text := "定时任务执行失败"
	if result.Err != nil {
		text += "：" + result.Err.Error()
	}
	if stream := s.ensureStream(ctx, result); stream != nil {
		finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptCardFinalUpdateLimit)
		defer cancel()
		if s.chunks != nil {
			s.chunks.close()
		}
		stream.updateProcessMessageWithContext(finalCtx, text)
		stream.failPromptStatusWithContext(finalCtx)
		stream.updateMetaWithContext(finalCtx, s.streamCardMetaWithTitle(result, scheduleStreamCardResult))
		stream.closeWithContext(finalCtx)
		return nil
	}
	return s.send(ctx, result, text)
}

func (s *scheduledTaskIMSink) ensureStream(ctx context.Context, result TriggerResult) *promptCardStream {
	if s.stream != nil {
		return s.stream
	}
	if s.starter == nil || strings.TrimSpace(s.message.ChatID) == "" {
		return nil
	}
	ctx = feishu.WithStreamCardStarter(ctx, s.starter)
	ctx = feishu.WithStreamCardMeta(ctx, s.streamCardMeta(result))
	session := result.Session
	if strings.TrimSpace(session.Cwd) == "" {
		session.Cwd = s.cwd
	}
	stream := newPromptCardStream(ctx, s.message, session, ChatConfig{})
	card := stream.ensureCardWithContext(ctx)
	if card == nil {
		return nil
	}
	s.stream = stream
	s.chunks = newPromptChunkAccumulator(stream)
	s.bindStreamMessage(ctx, result, card.Message())
	return stream
}

func (s *scheduledTaskIMSink) streamCardMeta(result TriggerResult) feishu.StreamCardMeta {
	return s.streamCardMetaWithTitle(result, scheduleStreamCardRunning)
}

func (s *scheduledTaskIMSink) streamCardMetaWithTitle(result TriggerResult, title string) feishu.StreamCardMeta {
	taskID := firstNonEmpty(s.taskID, result.Request.Metadata["task_id"])
	subtitle := ""
	if taskID != "" {
		subtitle = "task-id: " + taskID
	}
	return feishu.StreamCardMeta{
		Title:    title,
		Subtitle: subtitle,
		Footer:   "本消息的回复链将在本次执行会话中处理。",
	}
}

func (s *scheduledTaskIMSink) bindStreamMessage(ctx context.Context, result TriggerResult, sent feishu.SentMessage) {
	s.bindSentMessage(ctx, result, sent)
}

func (s *scheduledTaskIMSink) bindSentMessage(ctx context.Context, result TriggerResult, sent feishu.SentMessage) {
	if s.store == nil || strings.TrimSpace(sent.MessageID) == "" {
		return
	}
	chatID := firstNonEmpty(sent.ChatID, s.message.ChatID)
	if strings.TrimSpace(chatID) == "" {
		return
	}
	if _, err := s.store.BindMessageToSession(MessageSessionBinding{
		BotID:      result.Request.BotID,
		ChatID:     chatID,
		MessageID:  sent.MessageID,
		SessionKey: result.Session.Key,
	}); err != nil {
		slog.WarnContext(ctx, "保存定时任务结果消息会话绑定失败", "message_id", sent.MessageID, "session", result.Session.ACPSessionID, "错误", err)
	}
}

func (s *scheduledTaskIMSink) send(ctx context.Context, result TriggerResult, text string) error {
	if s.messageSender != nil {
		sent, err := s.messageSender(ctx, s.message, text)
		if err == nil {
			s.bindSentMessage(ctx, result, sent)
			return nil
		}
		slog.WarnContext(ctx, "定时任务 IM result sink 发送新消息失败，尝试降级发送", "chat_id", s.message.ChatID, "错误", err)
	}
	ok, err := feishu.SendIntermediateReply(ctx, s.message, text)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	if s.sender != nil {
		return s.sender(ctx, s.message, text, feishu.OutboundRenderContext{BaseDir: s.cwd})
	}
	slog.WarnContext(ctx, "缺少定时任务 IM result sink 发送器", "chat_id", s.message.ChatID, "thread_id", s.message.ThreadID)
	return nil
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
	if s.scheduleRuns == nil {
		s.scheduleRuns = make(map[string]scheduleRunStatus)
	}
	if task.OverlapPolicy == scheduleOverlapSkipIfRunning {
		if running := runningScheduleTaskRun(s.scheduleRuns, task.ID); running != "" {
			status := scheduleRunStatus{
				TaskID:     task.ID,
				RunID:      runID,
				State:      scheduleRunSkipped,
				SkippedAt:  startedAt,
				EndedAt:    startedAt,
				SkipReason: "已有运行中的定时任务 run: " + running,
				SessionKey: key,
			}
			s.scheduleRuns[scheduleRunStatusID(task.ID, runID)] = status
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
	s.scheduleRuns[scheduleRunStatusID(task.ID, runID)] = status
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
	s.scheduleRuns[id] = status
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
	id := scheduleRunStatusID(status.TaskID, status.RunID)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.scheduleRuns == nil {
		s.scheduleRuns = make(map[string]scheduleRunStatus)
	}
	s.scheduleRuns[id] = status
	s.pruneScheduleRunsLocked(status.TaskID)
}

func (s *Service) pruneScheduleRunsLocked(taskID string) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" || len(s.scheduleRuns) <= scheduleRunHistoryLimit {
		return
	}
	history := make([]scheduleRunStatus, 0)
	for _, status := range s.scheduleRuns {
		if status.TaskID == taskID && status.State != scheduleRunRunning {
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
		delete(s.scheduleRuns, scheduleRunStatusID(status.TaskID, status.RunID))
		history = history[1:]
	}
}

func runningScheduleTaskRun(statuses map[string]scheduleRunStatus, taskID string) string {
	taskID = strings.TrimSpace(taskID)
	for _, status := range statuses {
		if status.TaskID == taskID && status.State == scheduleRunRunning {
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

func (s *ScheduledTaskStore) snapshotLocked() scheduledTaskStoreSnapshot {
	snapshot := scheduledTaskStoreSnapshot{tasks: make(map[string]ScheduledTask, len(s.tasks))}
	for id, task := range s.tasks {
		snapshot.tasks[id] = task
	}
	return snapshot
}

func (s *ScheduledTaskStore) writeOrRestoreLocked(snapshot scheduledTaskStoreSnapshot) error {
	if err := s.writeLocked(); err != nil {
		s.tasks = snapshot.tasks
		return err
	}
	return nil
}

func (s *ScheduledTaskStore) writeLocked() error {
	file := scheduledTaskFile{
		Version: 1,
		Tasks:   make([]ScheduledTask, 0, len(s.tasks)),
	}
	for _, task := range s.tasks {
		file.Tasks = append(file.Tasks, task)
	}
	sort.Slice(file.Tasks, func(i, j int) bool {
		return file.Tasks[i].ID < file.Tasks[j].ID
	})
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("编码定时任务文件: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("创建定时任务目录: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时定时任务文件: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换定时任务文件: %w", err)
	}
	return nil
}

type scheduledTaskFile struct {
	Version int             `json:"version"`
	Tasks   []ScheduledTask `json:"tasks"`
}

type scheduleSpec interface {
	Next(time.Time) (time.Time, bool)
}

type everyScheduleSpec struct {
	interval time.Duration
}

func (s everyScheduleSpec) Next(after time.Time) (time.Time, bool) {
	if s.interval <= 0 {
		return time.Time{}, false
	}
	if after.IsZero() {
		after = time.Now()
	}
	return after.Add(s.interval), true
}

type cronScheduleSpec struct {
	minute     cronField
	hour       cronField
	dayOfMonth cronField
	month      cronField
	weekday    cronField
	location   *time.Location
}

func (s cronScheduleSpec) Next(after time.Time) (time.Time, bool) {
	loc := s.location
	if loc == nil {
		loc = time.Local
	}
	if after.IsZero() {
		after = time.Now()
	}
	start := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	deadline := start.AddDate(5, 0, 0)
	for t := start; !t.After(deadline); {
		if !s.month.Matches(int(t.Month())) {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.matchesDay(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.hour.Matches(t.Hour()) {
			t = t.Add(time.Hour).Truncate(time.Hour)
			continue
		}
		if !s.minute.Matches(t.Minute()) {
			t = t.Add(time.Minute)
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

func (s cronScheduleSpec) matchesDay(t time.Time) bool {
	dayOfMonthMatches := s.dayOfMonth.Matches(t.Day())
	weekdayMatches := s.weekday.Matches(int(t.Weekday()))
	switch {
	case s.dayOfMonth.all && s.weekday.all:
		return true
	case s.dayOfMonth.all:
		return weekdayMatches
	case s.weekday.all:
		return dayOfMonthMatches
	default:
		return dayOfMonthMatches || weekdayMatches
	}
}

type cronField struct {
	min int
	max int
	all bool
	set map[int]struct{}
}

func (f cronField) Matches(value int) bool {
	if f.all {
		return true
	}
	_, ok := f.set[value]
	return ok
}

func parseScheduleSpec(spec string, timezone string) (scheduleSpec, error) {
	spec = strings.TrimSpace(spec)
	timezone = strings.TrimSpace(timezone)
	if spec == "" {
		return nil, fmt.Errorf("定时任务 spec 不能为空")
	}
	if strings.HasPrefix(spec, "@every ") {
		intervalText := strings.TrimSpace(strings.TrimPrefix(spec, "@every "))
		interval, err := time.ParseDuration(intervalText)
		if err != nil || interval <= 0 {
			return nil, fmt.Errorf("解析 @every 间隔: %w", err)
		}
		return everyScheduleSpec{interval: interval}, nil
	}
	loc := time.Local
	if timezone != "" {
		loaded, err := time.LoadLocation(timezone)
		if err != nil {
			return nil, fmt.Errorf("解析定时任务 timezone: %w", err)
		}
		loc = loaded
	}
	parts := strings.Fields(spec)
	if len(parts) != 5 {
		return nil, fmt.Errorf("定时任务 spec 仅支持 @every <duration> 或 5 段 cron")
	}
	minute, err := parseCronField(parts[0], 0, 59)
	if err != nil {
		return nil, fmt.Errorf("解析 cron minute: %w", err)
	}
	hour, err := parseCronField(parts[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("解析 cron hour: %w", err)
	}
	dayOfMonth, err := parseCronField(parts[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("解析 cron day of month: %w", err)
	}
	month, err := parseCronField(parts[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("解析 cron month: %w", err)
	}
	weekday, err := parseCronField(parts[4], 0, 7)
	if err != nil {
		return nil, fmt.Errorf("解析 cron weekday: %w", err)
	}
	if _, ok := weekday.set[7]; ok {
		delete(weekday.set, 7)
		weekday.set[0] = struct{}{}
	}
	return cronScheduleSpec{minute: minute, hour: hour, dayOfMonth: dayOfMonth, month: month, weekday: weekday, location: loc}, nil
}

func parseCronField(expr string, min int, max int) (cronField, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return cronField{}, fmt.Errorf("字段为空")
	}
	field := cronField{min: min, max: max, set: make(map[int]struct{})}
	for _, part := range strings.Split(expr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return cronField{}, fmt.Errorf("包含空片段")
		}
		rangePart := part
		step := 1
		if strings.Contains(part, "/") {
			pieces := strings.Split(part, "/")
			if len(pieces) != 2 {
				return cronField{}, fmt.Errorf("无效步长 %q", part)
			}
			rangePart = strings.TrimSpace(pieces[0])
			parsedStep, err := strconv.Atoi(strings.TrimSpace(pieces[1]))
			if err != nil || parsedStep <= 0 {
				return cronField{}, fmt.Errorf("无效步长 %q", part)
			}
			step = parsedStep
		}
		start, end, all, err := parseCronRange(rangePart, min, max)
		if err != nil {
			return cronField{}, err
		}
		if all && step == 1 {
			field.all = true
			field.set = nil
			return field, nil
		}
		for value := start; value <= end; value += step {
			field.set[value] = struct{}{}
		}
	}
	return field, nil
}

func parseCronRange(expr string, min int, max int) (int, int, bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" || expr == "*" {
		return min, max, true, nil
	}
	if strings.Contains(expr, "-") {
		pieces := strings.Split(expr, "-")
		if len(pieces) != 2 {
			return 0, 0, false, fmt.Errorf("无效范围 %q", expr)
		}
		start, err := parseCronNumber(strings.TrimSpace(pieces[0]), min, max)
		if err != nil {
			return 0, 0, false, err
		}
		end, err := parseCronNumber(strings.TrimSpace(pieces[1]), min, max)
		if err != nil {
			return 0, 0, false, err
		}
		if start > end {
			return 0, 0, false, fmt.Errorf("无效倒序范围 %q", expr)
		}
		return start, end, false, nil
	}
	value, err := parseCronNumber(expr, min, max)
	if err != nil {
		return 0, 0, false, err
	}
	return value, value, false, nil
}

func parseCronNumber(expr string, min int, max int) (int, error) {
	value, err := strconv.Atoi(expr)
	if err != nil {
		return 0, fmt.Errorf("无效数字 %q", expr)
	}
	if value < min || value > max {
		return 0, fmt.Errorf("数字 %d 超出范围 %d-%d", value, min, max)
	}
	return value, nil
}
