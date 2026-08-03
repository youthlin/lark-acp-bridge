package bridge

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

var errSessionTaskBusy = errors.New("session task busy")

type taskKind string

const (
	taskKindUser taskKind = "user"
	taskKindWiki taskKind = "wiki"
	taskKindLoop taskKind = "loop"
)

type runningTask struct {
	kind               taskKind
	runtime            runtimeKey
	cancel             context.CancelFunc
	session            Session
	agent              config.AgentConfig
	drainPendingAtAuto bool
	onCancel           func(context.Context, string)
}

type runningTaskOptions struct {
	drainPendingAtAuto bool
	queuedContinuation bool
	skipPostPromptWork bool
	silentPrompt       bool
}

type promptTaskRunResult struct {
	result       acp.PromptResult
	sentProgress bool
}

type sessionWorkSnapshot struct {
	timers []*time.Timer
	tasks  []*runningTask
}

type sessionRuntimeStatus struct {
	Busy          bool
	RunningKind   taskKind
	QueueLen      int
	QueueDraining bool
}

type acpErrorSnapshot struct {
	occurred     time.Time
	acpSessionID string
	operation    string
	message      string
}

func (s *Service) startTask(ctx context.Context, session Session, agent config.AgentConfig, kind taskKind) (context.Context, func()) {
	ctx, finish, _ := s.startTaskWithOptions(ctx, session, agent, kind, runningTaskOptions{})
	return ctx, finish
}

func (s *Service) startTaskWithOptions(ctx context.Context, session Session, agent config.AgentConfig, kind taskKind, opts runningTaskOptions) (context.Context, func(), error) {
	session.Key = normalizeSessionKey(session.Key)
	s.cancelWikiTimer(session.Key)
	ctx, cancel := context.WithCancel(ctx)
	task := &runningTask{
		kind:               kind,
		runtime:            currentRuntimeKey(session.Key),
		cancel:             cancel,
		session:            session,
		agent:              agent,
		drainPendingAtAuto: opts.drainPendingAtAuto,
	}

	previous, busy := s.beginRunningTask(session.Key, task, opts)
	if busy {
		cancel()
		return ctx, func() {}, errSessionTaskBusy
	}
	if previous != nil && !opts.queuedContinuation {
		s.cancelTask(ctx, previous, true)
	}

	return ctx, func() {
		shouldDrainQueue := s.finishRunningTask(session.Key, task, kind, opts)
		cancel()
		if shouldDrainQueue {
			s.drainPromptQueueAsync(context.WithoutCancel(ctx), session.Key)
		}
	}, nil
}

func (s *Service) beginRunningTask(key SessionKey, task *runningTask, opts runningTaskOptions) (*runningTask, bool) {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	previous := s.tasks[key]
	if previous != nil && opts.queuedContinuation {
		return previous, true
	}
	s.tasks[key] = task
	return previous, false
}

func (s *Service) finishRunningTask(key SessionKey, task *runningTask, kind taskKind, opts runningTaskOptions) bool {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.tasks[key] != task {
		return false
	}
	delete(s.tasks, key)
	return kind == taskKindUser && !opts.queuedContinuation
}

func (s *Service) sessionHasRunningUserTask(key SessionKey) bool {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	task := s.tasks[key]
	return task != nil && task.kind == taskKindUser
}

func (s *Service) sessionRuntimeStatusSnapshot(key SessionKey) sessionRuntimeStatus {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	var status sessionRuntimeStatus
	if task := s.tasks[key]; task != nil {
		status.Busy = true
		status.RunningKind = task.kind
	}
	if queue := s.promptQueues[key]; queue != nil {
		status.QueueLen = len(queue.items)
		status.QueueDraining = queue.draining
	}
	return status
}

func (s *Service) recordACPError(session Session, operation string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	key := normalizeSessionKey(session.Key)
	snapshot := acpErrorSnapshot{
		occurred:     time.Now(),
		acpSessionID: strings.TrimSpace(session.ACPSessionID),
		operation:    truncateRunes(strings.TrimSpace(operation), 80),
		message:      sanitizeACPDiagnosticText(err.Error(), 240),
	}
	s.taskMu.Lock()
	if s.acpErrors == nil {
		s.acpErrors = make(map[SessionKey]acpErrorSnapshot)
	}
	s.acpErrors[key] = snapshot
	s.taskMu.Unlock()
}

func (s *Service) clearACPError(session Session) {
	key := normalizeSessionKey(session.Key)
	s.taskMu.Lock()
	if snapshot, ok := s.acpErrors[key]; ok && sameACPSession(snapshot.acpSessionID, session.ACPSessionID) {
		delete(s.acpErrors, key)
	}
	s.taskMu.Unlock()
}

func (s *Service) acpErrorSnapshot(session Session) (acpErrorSnapshot, bool) {
	key := normalizeSessionKey(session.Key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	snapshot, ok := s.acpErrors[key]
	return snapshot, ok && !snapshot.occurred.IsZero() && sameACPSession(snapshot.acpSessionID, session.ACPSessionID)
}

func sameACPSession(a string, b string) bool {
	return strings.TrimSpace(a) != "" && strings.TrimSpace(a) == strings.TrimSpace(b)
}

func sanitizeACPDiagnosticText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if text == "" || limit <= 0 {
		return ""
	}
	fields := strings.Fields(text)
	for i, field := range fields {
		if shouldHideACPDiagnosticField(field) {
			fields[i] = hiddenACPDiagnosticField(field)
			valueCount := acpDiagnosticHiddenValueCount(fields, i)
			for j, hidden := i+1, 0; j < len(fields) && hidden < valueCount && shouldHideACPDiagnosticValueField(fields[j]); j, hidden = j+1, hidden+1 {
				fields[j] = "[已隐藏]"
			}
		}
	}
	return truncateRunes(strings.Join(fields, " "), limit)
}

func shouldHideACPDiagnosticField(field string) bool {
	name := strings.ToLower(field)
	if idx := strings.IndexAny(name, "=:,"); idx >= 0 {
		name = name[:idx]
	}
	for _, sensitive := range []string{"secret", "token", "authorization", "password", "passwd", "apikey", "api_key", "key", "prompt", "message", "text", "content"} {
		if strings.Contains(name, sensitive) {
			return true
		}
	}
	return false
}

func hiddenACPDiagnosticField(field string) string {
	for _, sep := range []string{"=", ":"} {
		if idx := strings.Index(field, sep); idx >= 0 {
			return field[:idx+len(sep)] + "[已隐藏]"
		}
	}
	return field
}

func shouldHideACPDiagnosticValueField(field string) bool {
	trimmed := strings.TrimSpace(field)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	if lower == "bearer" || lower == "basic" {
		return true
	}
	return !strings.ContainsAny(trimmed, "=:,;")
}

func acpDiagnosticHiddenValueCount(fields []string, keyIndex int) int {
	if keyIndex+1 >= len(fields) {
		return 0
	}
	next := strings.ToLower(strings.TrimSpace(fields[keyIndex+1]))
	if next == "bearer" || next == "basic" {
		return 2
	}
	return 1
}

func runUserTask[T any](s *Service, ctx context.Context, session Session, agent config.AgentConfig, opts runningTaskOptions, run func(context.Context) (T, error)) (T, error) {
	ctx, finish, err := s.startTaskWithOptions(ctx, session, agent, taskKindUser, opts)
	if err != nil {
		var zero T
		return zero, err
	}
	defer finish()
	return run(ctx)
}

func runPromptTask(s *Service, ctx context.Context, session Session, agent config.AgentConfig, opts runningTaskOptions, run func(context.Context) (acp.PromptResult, bool, error)) (promptTaskRunResult, error) {
	return runUserTask(s, ctx, session, agent, opts, func(taskCtx context.Context) (promptTaskRunResult, error) {
		result, sentProgress, err := run(taskCtx)
		return promptTaskRunResult{result: result, sentProgress: sentProgress}, err
	})
}

func (s *Service) setTaskCancelHandler(key SessionKey, handler func(context.Context, string)) {
	if handler == nil {
		return
	}
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	if task := s.tasks[key]; task != nil {
		task.onCancel = handler
	}
	s.taskMu.Unlock()
}

func (s *Service) cancelRuntimeTask(ctx context.Context, task *runningTask) {
	if task == nil || strings.TrimSpace(task.session.ACPSessionID) == "" {
		return
	}
	if err := s.runtime.CancelSession(ctx, task.runtime, task.session, task.agent); err != nil {
		slog.WarnContext(ctx, "取消 ACP session 失败", "session", task.session.ACPSessionID, "kind", task.kind, "错误", err)
	}
}

func (s *Service) cancelRunningSessionWork(ctx context.Context, key SessionKey) {
	task := s.takeRunningTask(key)
	if task != nil {
		s.cancelTask(ctx, task, false)
	}
}

func (s *Service) cancelRunningSessionWorkSync(ctx context.Context, key SessionKey) {
	task := s.takeRunningTask(key)
	if task != nil {
		s.cancelTask(ctx, task, true)
	}
}

func (s *Service) takeRunningTask(key SessionKey) *runningTask {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	task := s.tasks[key]
	delete(s.tasks, key)
	return task
}

func (s *Service) takeRunningTaskOfKind(key SessionKey, kind taskKind) *runningTask {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	task := s.tasks[key]
	if task == nil || task.kind != kind {
		return nil
	}
	delete(s.tasks, key)
	return task
}

func (s *Service) cancelTask(ctx context.Context, task *runningTask, syncRuntimeCancel bool) {
	if task == nil {
		return
	}
	reason := replacementCancelReason(task)
	task.cancel()
	s.markCanceledTask(task, reason)
	if task.onCancel != nil {
		task.onCancel(ctx, reason)
	}
	if syncRuntimeCancel {
		s.cancelRuntimeTask(ctx, task)
		return
	}
	go s.cancelRuntimeTask(ctx, task)
}

func replacementCancelReason(task *runningTask) string {
	if task != nil && task.kind == taskKindLoop {
		return "已被新消息打断"
	}
	return "已取消"
}

func (s *Service) markCanceledTask(task *runningTask, reason string) {
	if task == nil || task.kind != taskKindLoop {
		return
	}
	task.session.Key = normalizeSessionKey(task.session.Key)
	s.markExistingLoopStatusCanceled(task.session.Key, reason)
}

func (s *Service) markLoopTaskCanceled(key SessionKey, reason string) {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	status := s.loopStatuses[key]
	status.running = false
	status.ended = time.Now()
	status.reason = reason
	status.lastError = ""
	s.loopStatuses[key] = status
	s.taskMu.Unlock()
}

func (s *Service) markExistingLoopStatusCanceled(key SessionKey, reason string) bool {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	status, ok := s.loopStatuses[key]
	if !ok {
		return false
	}
	status.running = false
	status.ended = time.Now()
	status.reason = reason
	status.lastError = ""
	s.loopStatuses[key] = status
	return true
}

func (s *Service) cancelSessionWork(ctx context.Context, key SessionKey) {
	key = normalizeSessionKey(key)
	s.cancelWikiTimer(key)
	s.cancelRunningSessionWork(ctx, key)
	s.cancelWikiTasks(ctx, key)
}

func (s *Service) cancelAllSessionWork(ctx context.Context) {
	snapshot := s.takeAllSessionWork()
	for _, timer := range snapshot.timers {
		timer.Stop()
	}
	for _, task := range snapshot.tasks {
		task.cancel()
		if task.onCancel != nil {
			task.onCancel(ctx, "已取消")
		}
		s.cancelRuntimeTask(ctx, task)
	}
}

func (s *Service) takeAllSessionWork() sessionWorkSnapshot {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.wikiGenerations == nil {
		s.wikiGenerations = make(map[SessionKey]int64)
	}
	snapshot := sessionWorkSnapshot{
		timers: make([]*time.Timer, 0, len(s.wikiTimers)),
		tasks:  make([]*runningTask, 0, len(s.tasks)+len(s.wikiTasks)),
	}
	for key, pending := range s.wikiTimers {
		s.wikiGenerations[key]++
		if pending != nil && pending.timer != nil {
			snapshot.timers = append(snapshot.timers, pending.timer)
		}
		delete(s.wikiTimers, key)
	}
	for key, task := range s.tasks {
		if task != nil {
			snapshot.tasks = append(snapshot.tasks, task)
		}
		delete(s.tasks, key)
	}
	for runtime, task := range s.wikiTasks {
		if task != nil {
			snapshot.tasks = append(snapshot.tasks, task)
		}
		delete(s.wikiTasks, runtime)
	}
	return snapshot
}
