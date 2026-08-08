package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

// errTaskPanicked 是任务回调 panic 被恢复后返回给调用方的哨兵错误。
var errTaskPanicked = errors.New("bridge task panicked")

// taskPanicError 包装被恢复的 panic 值，保留堆栈信息用于日志和错误展示。
type taskPanicError struct {
	value any
	stack []byte
}

func (e *taskPanicError) Error() string {
	return fmt.Sprintf("%s: %v", errTaskPanicked.Error(), e.value)
}

func (e *taskPanicError) Unwrap() error {
	return errTaskPanicked
}

// recoverTaskPanic 在 deferred 调用中恢复 run 回调 panic，
// 把它转换为可被正常错误处理路径消费的 error，并记录结构化日志和堆栈。
func recoverTaskPanic(ctx context.Context, session Session, kind taskKind, errp *error) {
	if r := recover(); r != nil {
		stack := debug.Stack()
		slog.ErrorContext(ctx, "bridge 任务执行发生 panic，已恢复",
			"panic", r,
			"stack", string(stack),
			"task_kind", string(kind),
			"session_key", fmt.Sprintf("%+v", session.Key),
			"agent", agentNameForLog(session.AgentName),
		)
		*errp = &taskPanicError{value: r, stack: stack}
	}
}

func agentNameForLog(name string) string {
	if name == "" {
		return "unknown"
	}
	return name
}

// runUserTask 包装 startTaskWithOptions 的生命周期，并在 run 回调 panic 时恢复。
// 因为 finish 在 defer 中执行，即使 run panic，任务也会从运行表中移除并释放后继任务，
// 不会留下卡住的 session。
func runUserTask[T any](s *Service, ctx context.Context, session Session, agent config.AgentConfig, opts runningTaskOptions, run func(context.Context) (T, error)) (result T, err error) {
	ctx, finish, err := s.startTaskWithOptions(ctx, session, agent, taskKindUser, opts)
	if err != nil {
		var zero T
		return zero, err
	}
	defer finish()
	defer recoverTaskPanic(ctx, session, taskKindUser, &err)
	return run(ctx)
}

func runPromptTask(s *Service, ctx context.Context, session Session, agent config.AgentConfig, opts runningTaskOptions, run func(context.Context) (acp.PromptResult, bool, error)) (promptTaskRunResult, error) {
	return runPromptTaskDetailed(s, ctx, session, agent, opts, func(taskCtx context.Context) (promptRuntimeResult, error) {
		result, sentProgress, err := run(taskCtx)
		return promptRuntimeResult{result: result, sentProgress: sentProgress, err: err}, err
	})
}

func runPromptTaskDetailed(s *Service, ctx context.Context, session Session, agent config.AgentConfig, opts runningTaskOptions, run func(context.Context) (promptRuntimeResult, error)) (promptTaskRunResult, error) {
	return runUserTask(s, ctx, session, agent, opts, func(taskCtx context.Context) (promptTaskRunResult, error) {
		run, err := run(taskCtx)
		return promptTaskRunResult{result: run.result, sentProgress: run.sentProgress, reply: run.reply, replySet: run.replySet}, err
	})
}
