package bridge

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// shutdownBackgroundWait 是 Shutdown 时等待后台 goroutine 收尾的上限。
const shutdownBackgroundWait = 5 * time.Second

// backgroundGoroutines 追踪 Service 启动的、脱离单次请求生命周期的后台 goroutine
// （定时任务调度、队列 drain、wiki 反思、auto-compact、自更新等），
// 让 Shutdown 能有界等待它们收尾，避免重启时丢失最后一轮卡片更新或持久化写入。
//
// 注意：仅用于真正脱离请求的后台任务。同步调用链内部用于取消/等待 predecessor 的
// 辅助 goroutine 不应纳入，否则可能与 cancelAllSessionWork 形成自锁。
type backgroundGoroutines struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelCauseFunc
	stopped bool
}

func newBackgroundGoroutines() backgroundGoroutines {
	ctx, cancel := context.WithCancelCause(context.Background())
	return backgroundGoroutines{ctx: ctx, cancel: cancel}
}

// goBackground 启动一个被追踪的后台 goroutine。传入的 parent 保留调用链上的
// 日志字段及自身取消语义，同时任务也会在 Service Shutdown 时被统一取消。
func (s *Service) goBackground(parent context.Context, name string, fn func(context.Context)) bool {
	if parent == nil {
		parent = context.Background()
	}
	s.backgroundWg.mu.Lock()
	if s.backgroundWg.stopped {
		s.backgroundWg.mu.Unlock()
		slog.Warn("服务正在关闭，跳过后台任务启动", "name", name)
		return false
	}
	root := s.backgroundWg.ctx
	if root == nil {
		root, s.backgroundWg.cancel = context.WithCancelCause(context.Background())
		s.backgroundWg.ctx = root
	}
	s.backgroundWg.wg.Add(1)
	s.backgroundWg.mu.Unlock()

	ctx, cancel := context.WithCancelCause(parent)
	stopRootCancel := context.AfterFunc(root, func() { cancel(context.Cause(root)) })
	go func() {
		defer s.backgroundWg.wg.Done()
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

func (s *Service) backgroundStopped() bool {
	s.backgroundWg.mu.Lock()
	defer s.backgroundWg.mu.Unlock()
	return s.backgroundWg.stopped
}

// stopBackgroundStarts 阻止新后台任务注册，但暂不取消已注册任务。
// Shutdown 先执行任务自身的取消回调，再取消 Service 根 context，避免绕过
// loop 等业务任务的收尾状态更新。
func (s *Service) stopBackgroundStarts() {
	s.backgroundWg.mu.Lock()
	defer s.backgroundWg.mu.Unlock()
	if s.backgroundWg.stopped {
		return
	}
	s.backgroundWg.stopped = true
}

// cancelBackgroundRoot 取消所有已注册任务的 Service 根 context。
func (s *Service) cancelBackgroundRoot() {
	s.backgroundWg.mu.Lock()
	defer s.backgroundWg.mu.Unlock()
	if s.backgroundWg.cancel != nil {
		s.backgroundWg.cancel(context.Canceled)
	}
}

// stopBackground 阻止新后台任务注册，并取消所有已注册任务的 Service 根 context。
// 与任务注册共用同一把锁，保证开始 Shutdown 后不会再发生 WaitGroup.Add。
func (s *Service) stopBackground() {
	s.stopBackgroundStarts()
	s.cancelBackgroundRoot()
}

// waitBackgroundShutdown 有界等待后台 goroutine 退出，返回后无论是否超时都继续关闭。
func (s *Service) waitBackgroundShutdown(ctx context.Context) {
	s.stopBackground()
	s.backgroundWg.mu.Lock()
	done := make(chan struct{})
	go func() {
		s.backgroundWg.wg.Wait()
		close(done)
	}()
	s.backgroundWg.mu.Unlock()
	select {
	case <-done:
		slog.Info("后台任务已全部退出")
	case <-ctx.Done():
		slog.Warn("等待后台任务退出超时，继续关闭", "错误", ctx.Err())
	}
}
