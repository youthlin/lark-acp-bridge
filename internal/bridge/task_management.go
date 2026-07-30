package bridge

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

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
}

type promptTaskRunResult struct {
	result       acp.PromptResult
	sentProgress bool
}

func (s *Service) startTask(ctx context.Context, session Session, agent config.AgentConfig, kind taskKind) (context.Context, func()) {
	return s.startTaskWithOptions(ctx, session, agent, kind, runningTaskOptions{})
}

func (s *Service) startTaskWithOptions(ctx context.Context, session Session, agent config.AgentConfig, kind taskKind, opts runningTaskOptions) (context.Context, func()) {
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

	var previous *runningTask
	s.taskMu.Lock()
	previous = s.tasks[session.Key]
	s.tasks[session.Key] = task
	s.taskMu.Unlock()
	if previous != nil {
		reason := replacementCancelReason(previous)
		previous.cancel()
		s.markCanceledTask(previous, reason)
		if previous.onCancel != nil {
			previous.onCancel(ctx, reason)
		}
		go s.cancelRuntimeTask(ctx, previous)
	}

	return ctx, func() {
		s.taskMu.Lock()
		if s.tasks[session.Key] == task {
			delete(s.tasks, session.Key)
		}
		s.taskMu.Unlock()
		cancel()
	}
}

func runUserTask[T any](s *Service, ctx context.Context, session Session, agent config.AgentConfig, opts runningTaskOptions, run func(context.Context) (T, error)) (T, error) {
	ctx, finish := s.startTaskWithOptions(ctx, session, agent, taskKindUser, opts)
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
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	task := s.tasks[key]
	delete(s.tasks, key)
	s.taskMu.Unlock()
	if task != nil {
		reason := replacementCancelReason(task)
		task.cancel()
		s.markCanceledTask(task, reason)
		if task.onCancel != nil {
			task.onCancel(ctx, reason)
		}
		go s.cancelRuntimeTask(ctx, task)
	}
}

func (s *Service) cancelRunningSessionWorkSync(ctx context.Context, key SessionKey) {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	task := s.tasks[key]
	delete(s.tasks, key)
	s.taskMu.Unlock()
	if task != nil {
		reason := replacementCancelReason(task)
		task.cancel()
		s.markCanceledTask(task, reason)
		if task.onCancel != nil {
			task.onCancel(ctx, reason)
		}
		s.cancelRuntimeTask(ctx, task)
	}
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
	s.taskMu.Lock()
	status, ok := s.loopStatuses[task.session.Key]
	if ok {
		status.running = false
		status.ended = time.Now()
		status.reason = reason
		status.lastError = ""
		s.loopStatuses[task.session.Key] = status
	}
	s.taskMu.Unlock()
}

func (s *Service) cancelSessionWork(ctx context.Context, key SessionKey) {
	key = normalizeSessionKey(key)
	s.cancelWikiTimer(key)
	s.cancelRunningSessionWork(ctx, key)
	s.cancelWikiTasks(ctx, key)
}

func (s *Service) cancelAllSessionWork(ctx context.Context) {
	s.taskMu.Lock()
	if s.wikiGenerations == nil {
		s.wikiGenerations = make(map[SessionKey]int64)
	}
	timers := make([]*time.Timer, 0, len(s.wikiTimers))
	for key, pending := range s.wikiTimers {
		s.wikiGenerations[key]++
		if pending != nil && pending.timer != nil {
			timers = append(timers, pending.timer)
		}
		delete(s.wikiTimers, key)
	}
	tasks := make([]*runningTask, 0, len(s.tasks)+len(s.wikiTasks))
	for key, task := range s.tasks {
		if task != nil {
			tasks = append(tasks, task)
		}
		delete(s.tasks, key)
	}
	for runtime, task := range s.wikiTasks {
		if task != nil {
			tasks = append(tasks, task)
		}
		delete(s.wikiTasks, runtime)
	}
	s.taskMu.Unlock()
	for _, timer := range timers {
		timer.Stop()
	}
	for _, task := range tasks {
		task.cancel()
		if task.onCancel != nil {
			task.onCancel(ctx, "已取消")
		}
		s.cancelRuntimeTask(ctx, task)
	}
}
