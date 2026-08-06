package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func TestWaitBackgroundShutdownWaitsForTrackedGoroutine(t *testing.T) {
	s := NewService(config.Config{}, NewSessionStore(""))
	entered := make(chan struct{})
	release := make(chan struct{})
	s.goBackground("test", func() {
		close(entered)
		<-release
	})
	<-entered

	done := make(chan struct{})
	go func() {
		s.waitBackgroundShutdown(context.Background())
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("waitBackgroundShutdown returned before goroutine exited")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitBackgroundShutdown did not return after goroutine exited")
	}
}

func TestGoBackgroundAfterShutdownSkipsNewTask(t *testing.T) {
	s := NewService(config.Config{}, NewSessionStore(""))
	// 标记关闭，不启动任何实际任务。
	s.waitBackgroundShutdown(context.Background())

	started := make(chan struct{}, 1)
	s.goBackground("test", func() { started <- struct{}{} })

	select {
	case <-started:
		t.Fatal("goBackground ran a task after shutdown")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWaitBackgroundShutdownBoundedByContext(t *testing.T) {
	s := NewService(config.Config{}, NewSessionStore(""))
	release := make(chan struct{})
	defer close(release)
	s.goBackground("test", func() {
		<-release
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.waitBackgroundShutdown(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitBackgroundShutdown ignored context deadline")
	}
}
