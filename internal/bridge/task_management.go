package bridge

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

var (
	errSessionTaskBusy      = errors.New("session task busy")
	replacedTaskDoneTimeout = 2 * time.Minute
)

var closedRunningTaskDone = makeClosedTaskDone()

type taskKind string

const (
	taskKindUser taskKind = "user"
	taskKindWiki taskKind = "wiki"
	taskKindLoop taskKind = "loop"
)

type runningTask struct {
	kind                  taskKind
	runtime               runtimeKey
	cancel                context.CancelFunc
	done                  chan struct{}
	doneOnce              sync.Once
	predecessorDone       <-chan struct{}
	predecessorDetached   chan struct{}
	predecessorDetachOnce sync.Once
	completed             chan struct{}
	completedOnce         sync.Once
	session               Session
	agent                 config.AgentConfig
	drainPendingAtAuto    bool
	queuePendingAtAuto    bool
	onCancel              func(context.Context, string)
}

type runningTaskOptions struct {
	drainPendingAtAuto  bool
	queuePendingAtAuto  bool
	queuedContinuation  bool
	skipPostPromptWork  bool
	silentPrompt        bool
	keepWikiTimer       bool
	blockWorkspaceTasks bool
	replacementWait     replacedTaskWaitObserver
	replacementTimeout  time.Duration
}

type replacedTaskWaitObserver interface {
	ReplacementWaitStarted(context.Context)
	ReplacementWaitTick(context.Context)
	ReplacementWaitFinished(context.Context)
	ReplacementWaitTimedOut(context.Context)
}

func userPromptTaskOptions() runningTaskOptions {
	return runningTaskOptions{}
}

func atAutoUserPromptTaskOptions() runningTaskOptions {
	return runningTaskOptions{drainPendingAtAuto: true, queuePendingAtAuto: true}
}

func atAutoPromptTaskOptions() runningTaskOptions {
	return runningTaskOptions{drainPendingAtAuto: true, queuePendingAtAuto: true}
}

func queuedPromptTaskOptions() runningTaskOptions {
	return runningTaskOptions{
		queuedContinuation: true,
		skipPostPromptWork: true,
	}
}

func autoCompactTaskOptions() runningTaskOptions {
	return runningTaskOptions{
		skipPostPromptWork: true,
		silentPrompt:       true,
	}
}

func triggerPromptTaskOptions() runningTaskOptions {
	return runningTaskOptions{}
}

func loopTaskOptions() runningTaskOptions {
	return runningTaskOptions{}
}

func wikiLintTaskOptions() runningTaskOptions {
	return runningTaskOptions{
		keepWikiTimer:       true,
		queuedContinuation:  true,
		blockWorkspaceTasks: true,
	}
}

func wikiReflectionTaskOptions() runningTaskOptions {
	return runningTaskOptions{
		keepWikiTimer:       true,
		blockWorkspaceTasks: true,
	}
}

type promptTaskRunResult struct {
	result       acp.PromptResult
	sentProgress bool
	reply        string
	replySet     bool
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
	ctx, finish, _ := s.startTaskWithOptions(ctx, session, agent, kind, defaultTaskOptions(kind))
	return ctx, finish
}

func (s *Service) startLoopTask(ctx context.Context, session Session, agent config.AgentConfig) (context.Context, func()) {
	ctx, finish, _ := s.startTaskWithOptions(ctx, session, agent, taskKindLoop, loopTaskOptions())
	return ctx, finish
}

func defaultTaskOptions(kind taskKind) runningTaskOptions {
	switch kind {
	case taskKindLoop:
		return loopTaskOptions()
	case taskKindUser:
		return userPromptTaskOptions()
	default:
		return runningTaskOptions{}
	}
}

func (s *Service) startTaskWithOptions(ctx context.Context, session Session, agent config.AgentConfig, kind taskKind, opts runningTaskOptions) (context.Context, func(), error) {
	session.Key = normalizeSessionKey(session.Key)
	session.Workspace = s.workspaceForSessionTask(session)
	if !opts.keepWikiTimer {
		s.cancelWikiTimer(session.Key)
	}
	ctx, cancel := context.WithCancel(ctx)
	task := &runningTask{
		kind:                kind,
		runtime:             currentRuntimeKey(session.Key),
		cancel:              cancel,
		done:                make(chan struct{}),
		predecessorDetached: make(chan struct{}),
		session:             session,
		agent:               agent,
		drainPendingAtAuto:  opts.drainPendingAtAuto,
		queuePendingAtAuto:  opts.queuePendingAtAuto,
	}

	previous, busy := s.beginRunningTask(session.Key, task, opts)
	if busy {
		cancel()
		task.closeDone()
		return ctx, func() {}, errSessionTaskBusy
	}
	if previous != nil && !opts.queuedContinuation {
		if opts.replacementWait != nil {
			opts.replacementWait.ReplacementWaitStarted(ctx)
		}
		completed := s.cancelTask(ctx, previous, false)
		if err := s.waitForReplacedTaskCompleted(ctx, previous, completed, opts); err != nil {
			shouldDrainQueue := s.finishRunningTask(session.Key, task, kind, opts)
			cancel()
			task.closeDone()
			if shouldDrainQueue {
				s.drainPromptQueueAsync(context.WithoutCancel(ctx), session.Key)
			}
			return ctx, func() {}, err
		}
	}

	return ctx, func() {
		shouldDrainQueue := s.finishRunningTask(session.Key, task, kind, opts)
		cancel()
		task.closeDone()
		if shouldDrainQueue {
			s.drainPromptQueueAsync(context.WithoutCancel(ctx), session.Key)
		}
	}, nil
}

func (task *runningTask) detachPredecessor() {
	if task == nil || task.predecessorDetached == nil {
		return
	}
	task.predecessorDetachOnce.Do(func() {
		close(task.predecessorDetached)
	})
}

func (task *runningTask) closeDone() {
	if task == nil || task.done == nil {
		return
	}
	task.doneOnce.Do(func() {
		close(task.done)
	})
}

func runningTaskDone(task *runningTask) <-chan struct{} {
	if task == nil || task.done == nil {
		return closedRunningTaskDone
	}
	return task.done
}

func runningTaskCompleted(task *runningTask) <-chan struct{} {
	if task == nil {
		return closedRunningTaskDone
	}
	task.completedOnce.Do(func() {
		task.completed = make(chan struct{})
		go func() {
			if task.predecessorDone != nil {
				select {
				case <-task.predecessorDone:
				case <-task.predecessorDetached:
				}
			}
			<-runningTaskDone(task)
			close(task.completed)
		}()
	})
	return task.completed
}

func makeClosedTaskDone() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (s *Service) waitForReplacedTaskCompleted(ctx context.Context, previous *runningTask, completed <-chan struct{}, opts runningTaskOptions) error {
	if completed == nil {
		return nil
	}
	timeout := opts.replacementTimeout
	if timeout <= 0 {
		timeout = replacedTaskDoneTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-completed:
			if opts.replacementWait != nil {
				opts.replacementWait.ReplacementWaitFinished(ctx)
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if opts.replacementWait != nil {
				opts.replacementWait.ReplacementWaitTick(ctx)
			}
		case <-timer.C:
			sessionID := ""
			var kind taskKind
			if previous != nil {
				sessionID = previous.session.ACPSessionID
				kind = previous.kind
			}
			if opts.replacementWait != nil {
				opts.replacementWait.ReplacementWaitTimedOut(ctx)
			}
			slog.WarnContext(ctx, "等待旧任务结束超时，关闭旧 ACP runtime 后重建连接", "session", sessionID, "kind", kind, "timeout", timeout)
			if previous != nil {
				if err := s.runtime.CloseRuntimeKey(previous.runtime); err != nil {
					slog.WarnContext(ctx, "关闭超时旧 ACP runtime 失败", "session", sessionID, "kind", kind, "错误", err)
				}
				if previous.cancel != nil {
					previous.cancel()
				}
				previous.closeDone()
			}
			return nil
		}
	}
}

func (s *Service) beginRunningTask(key SessionKey, task *runningTask, opts runningTaskOptions) (*runningTask, bool) {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	previous := s.tasks[key]
	if previous != nil && opts.queuedContinuation {
		return previous, true
	}
	if opts.blockWorkspaceTasks && s.hasWorkspaceTaskLocked(key, task.session.Workspace) {
		return previous, true
	}
	task.predecessorDone = runningTaskCompleted(previous)
	s.tasks[key] = task
	s.workspaceLocks.set(task.session.Workspace, task)
	return previous, false
}

func (s *Service) hasWorkspaceTaskLocked(key SessionKey, workspace string) bool {
	return s.workspaceLocks.busy(key, workspace, s.tasks, s.wikiTasks)
}

func (s *Service) runtimeKeyBusy(key runtimeKey) bool {
	key = normalizeRuntimeKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if _, ok := s.wikiTasks[key]; ok {
		return true
	}
	if key.Scope == runtimeScopeWiki {
		return false
	}
	return s.tasks[normalizeSessionKey(key.SessionKey)] != nil
}

func (s *Service) workspaceForSessionTask(session Session) string {
	if workspace := strings.TrimSpace(session.Workspace); workspace != "" {
		return workspace
	}
	return s.botWorkspace(session.Key.BotID)
}

func (s *Service) taskRegisteredLocked(task *runningTask) bool {
	if task == nil {
		return false
	}
	for _, current := range s.tasks {
		if current == task {
			return true
		}
	}
	for _, current := range s.wikiTasks {
		if current == task {
			return true
		}
	}
	return false
}

func (s *Service) finishRunningTask(key SessionKey, task *runningTask, kind taskKind, opts runningTaskOptions) bool {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.tasks[key] != task {
		if !s.taskRegisteredLocked(task) {
			s.workspaceLocks.clear(task.session.Workspace, task)
		}
		return false
	}
	delete(s.tasks, key)
	s.workspaceLocks.clear(task.session.Workspace, task)
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
	if !status.Busy {
		for runtime := range s.wikiTasks {
			if normalizeSessionKey(runtime.SessionKey) == key {
				status.Busy = true
				status.RunningKind = taskKindWiki
				break
			}
		}
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
	if task != nil {
		s.workspaceLocks.clear(task.session.Workspace, task)
	}
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
	s.workspaceLocks.clear(task.session.Workspace, task)
	return task
}

func (s *Service) cancelTask(ctx context.Context, task *runningTask, syncRuntimeCancel bool) <-chan struct{} {
	if task == nil {
		return runningTaskCompleted(nil)
	}
	reason := replacementCancelReason(task)
	if task.cancel != nil {
		task.cancel()
	}
	s.markCanceledTask(task, reason)
	if task.onCancel != nil {
		task.onCancel(ctx, reason)
	}
	if syncRuntimeCancel {
		s.cancelRuntimeTask(ctx, task)
		return runningTaskCompleted(task)
	}
	go s.cancelRuntimeTask(context.WithoutCancel(ctx), task)
	return runningTaskCompleted(task)
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
	s.workspaceLocks.clearAll()
	return snapshot
}
