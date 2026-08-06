package bridge

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
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
	wg      sync.WaitGroup
	stopped atomic.Bool
}

// goBackground 启动一个被追踪的后台 goroutine。
// fn 应监听传入的 ctx 以便在 Shutdown 时尽快退出。
func (s *Service) goBackground(name string, fn func()) {
	if s.backgroundWg.stopped.Load() {
		slog.Warn("服务正在关闭，跳过后台任务启动", "name", name)
		return
	}
	s.backgroundWg.wg.Add(1)
	go func() {
		defer s.backgroundWg.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("后台任务发生 panic，已恢复", "name", name, "panic", r)
			}
		}()
		fn()
	}()
}

// waitBackgroundShutdown 有界等待后台 goroutine 退出，返回后无论是否超时都继续关闭。
func (s *Service) waitBackgroundShutdown(ctx context.Context) {
	s.backgroundWg.stopped.Store(true)
	done := make(chan struct{})
	go func() {
		s.backgroundWg.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		slog.Info("后台任务已全部退出")
	case <-ctx.Done():
		slog.Warn("等待后台任务退出超时，继续关闭", "错误", ctx.Err())
	}
}
