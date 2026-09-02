package bridge

import (
	"context"
	"log/slog"
	"sync"
)

// taskSupervisor 追踪 Service 启动的、脱离单次请求生命周期的后台 goroutine
// （定时任务调度、队列 drain、wiki 反思、auto-compact、自更新等），
// 让 Shutdown 能有界等待它们收尾，避免重启时丢失最后一轮卡片更新或持久化写入。
//
// 注意：仅用于真正脱离请求的后台任务。同步调用链内部用于取消/等待 predecessor 的
// 辅助 goroutine 不应纳入，否则可能与 cancelAllSessionWork 形成自锁。
type taskSupervisor struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelCauseFunc
	stopped bool
}

func newTaskSupervisor() taskSupervisor {
	ctx, cancel := context.WithCancelCause(context.Background())
	return taskSupervisor{ctx: ctx, cancel: cancel}
}

// Start 启动一个被追踪的后台 goroutine。传入的 parent 保留调用链上的
// 日志字段及自身取消语义，同时任务也会在 Stop 时被统一取消。
func (s *taskSupervisor) Start(parent context.Context, name string, fn func(context.Context)) bool {
	if parent == nil {
		parent = context.Background()
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		slog.Warn("服务正在关闭，跳过后台任务启动", "name", name)
		return false
	}
	root := s.ctx
	if root == nil {
		root, s.cancel = context.WithCancelCause(context.Background())
		s.ctx = root
	}
	s.wg.Add(1)
	s.mu.Unlock()

	ctx, cancel := context.WithCancelCause(parent)
	stopRootCancel := context.AfterFunc(root, func() { cancel(context.Cause(root)) })
	go func() {
		defer s.wg.Done()
		defer cancel(nil)
		defer stopRootCancel()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("后台任务发生 panic，已恢复", "name", name, "panic", r)
			}
		}()
		fn(ctx)
	}()
	return true
}

func (s *taskSupervisor) Stopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// StopStarts 阻止新后台任务注册，但暂不取消已注册任务。
// Shutdown 先执行任务自身的取消回调，再取消 Service 根 context，避免绕过
// loop 等业务任务的收尾状态更新。
func (s *taskSupervisor) StopStarts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
}

// Cancel 取消所有已注册任务的根 context。
func (s *taskSupervisor) Cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel(context.Canceled)
	}
}

// Stop 阻止新后台任务注册，并取消所有已注册任务的根 context。
// 与任务注册共用同一把锁，保证开始 Shutdown 后不会再发生 WaitGroup.Add。
func (s *taskSupervisor) Stop() {
	s.StopStarts()
	s.Cancel()
}

// Wait 停止 supervisor 并有界等待所有后台 goroutine 退出。
func (s *taskSupervisor) Wait(ctx context.Context) bool {
	s.Stop()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
