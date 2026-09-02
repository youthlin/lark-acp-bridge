package bridge

import (
	"context"
	"log/slog"
	"time"
)

// shutdownBackgroundWait 是 Shutdown 时等待后台 goroutine 收尾的上限。
const shutdownBackgroundWait = 5 * time.Second

// Service 保留 facade，后台生命周期细节由 taskSupervisor 负责。
func (s *Service) goBackground(parent context.Context, name string, fn func(context.Context)) bool {
	return s.taskSupervisor.Start(parent, name, fn)
}

func (s *Service) backgroundStopped() bool {
	return s.taskSupervisor.Stopped()
}

func (s *Service) stopBackgroundStarts() {
	s.taskSupervisor.StopStarts()
}

func (s *Service) cancelBackgroundRoot() {
	s.taskSupervisor.Cancel()
}

func (s *Service) waitBackgroundShutdown(ctx context.Context) {
	if s.taskSupervisor.Wait(ctx) {
		slog.Info("后台任务已全部退出")
		return
	}
	slog.Warn("等待后台任务退出超时，继续关闭", "错误", ctx.Err())
}
