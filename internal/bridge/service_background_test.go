package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func TestWaitBackgroundShutdownWaitsForTrackedGoroutine(t *testing.T) {
	s := NewService(config.Config{}, NewSessionStore(""))
	entered := make(chan struct{})
	release := make(chan struct{})
	s.goBackground(context.Background(), "test", func(context.Context) {
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
	s.goBackground(context.Background(), "test", func(context.Context) { started <- struct{}{} })

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
	s.goBackground(context.Background(), "test", func(context.Context) {
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

func TestWaitBackgroundShutdownCancelsServiceContext(t *testing.T) {
	s := NewService(config.Config{}, NewSessionStore(""))
	started := make(chan struct{})
	stopped := make(chan error, 1)
	if ok := s.goBackground(context.Background(), "test", func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		stopped <- context.Cause(ctx)
	}); !ok {
		t.Fatal("goBackground() = false before shutdown")
	}
	<-started

	s.waitBackgroundShutdown(context.Background())
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("background context cause = %v, want context.Canceled", err)
		}
	default:
		t.Fatal("background task did not observe service cancellation")
	}
}

func TestStopBackgroundStartsDoesNotCancelServiceContext(t *testing.T) {
	s := NewService(config.Config{}, NewSessionStore(""))
	started := make(chan struct{})
	stopped := make(chan struct{})
	if ok := s.goBackground(context.Background(), "test", func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(stopped)
	}); !ok {
		t.Fatal("goBackground() = false before shutdown")
	}
	<-started

	s.stopBackgroundStarts()
	if ok := s.goBackground(context.Background(), "late", func(context.Context) {}); ok {
		t.Fatal("goBackground() = true after stopBackgroundStarts")
	}
	select {
	case <-stopped:
		t.Fatal("stopBackgroundStarts canceled service context")
	case <-time.After(50 * time.Millisecond):
	}

	s.cancelBackgroundRoot()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("cancelBackgroundRoot did not cancel service context")
	}
	s.waitBackgroundShutdown(context.Background())
}

func TestGoBackgroundPreservesParentCancellation(t *testing.T) {
	s := NewService(config.Config{}, NewSessionStore(""))
	parent, cancel := context.WithCancelCause(context.Background())
	wantErr := errors.New("request stopped")
	done := make(chan error, 1)
	if ok := s.goBackground(parent, "test", func(ctx context.Context) {
		<-ctx.Done()
		done <- context.Cause(ctx)
	}); !ok {
		t.Fatal("goBackground() = false before shutdown")
	}
	cancel(wantErr)

	select {
	case err := <-done:
		if !errors.Is(err, wantErr) {
			t.Fatalf("background context cause = %v, want %v", err, wantErr)
		}
	case <-time.After(time.Second):
		t.Fatal("background task did not observe parent cancellation")
	}
	s.waitBackgroundShutdown(context.Background())
}

func TestGoBackgroundConcurrentWithShutdown(t *testing.T) {
	s := NewService(config.Config{}, NewSessionStore(""))
	const attempts = 100
	var starters sync.WaitGroup
	starters.Add(attempts)
	for range attempts {
		go func() {
			defer starters.Done()
			s.goBackground(context.Background(), "test", func(ctx context.Context) { <-ctx.Done() })
		}()
	}
	done := make(chan struct{})
	go func() {
		s.waitBackgroundShutdown(context.Background())
		close(done)
	}()
	starters.Wait()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("concurrent shutdown did not finish")
	}
}
