package bridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestTaskSupervisorWaitsForTrackedGoroutine(t *testing.T) {
	s := newTaskSupervisor()
	entered := make(chan struct{})
	release := make(chan struct{})
	s.Start(context.Background(), "test", func(context.Context) {
		close(entered)
		<-release
	})
	<-entered

	done := make(chan struct{})
	go func() {
		s.Wait(context.Background())
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("Wait returned before goroutine exited")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after goroutine exited")
	}
}

func TestTaskSupervisorAfterStopSkipsNewTask(t *testing.T) {
	s := newTaskSupervisor()
	s.Wait(context.Background())

	started := make(chan struct{}, 1)
	s.Start(context.Background(), "test", func(context.Context) { started <- struct{}{} })

	select {
	case <-started:
		t.Fatal("Start ran a task after Stop")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestTaskSupervisorWaitBoundedByContext(t *testing.T) {
	s := newTaskSupervisor()
	release := make(chan struct{})
	defer close(release)
	s.Start(context.Background(), "test", func(context.Context) {
		<-release
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if s.Wait(ctx) {
		t.Fatal("Wait returned true before goroutine exited")
	}
}

func TestTaskSupervisorWaitCancelsRootContext(t *testing.T) {
	s := newTaskSupervisor()
	started := make(chan struct{})
	stopped := make(chan error, 1)
	if ok := s.Start(context.Background(), "test", func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		stopped <- context.Cause(ctx)
	}); !ok {
		t.Fatal("Start() = false before Stop")
	}
	<-started

	if !s.Wait(context.Background()) {
		t.Fatal("Wait() = false after task observed cancellation")
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("background context cause = %v, want context.Canceled", err)
		}
	default:
		t.Fatal("background task did not observe supervisor cancellation")
	}
}

func TestTaskSupervisorStopStartsDoesNotCancelRootContext(t *testing.T) {
	s := newTaskSupervisor()
	started := make(chan struct{})
	stopped := make(chan struct{})
	if ok := s.Start(context.Background(), "test", func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(stopped)
	}); !ok {
		t.Fatal("Start() = false before StopStarts")
	}
	<-started

	s.StopStarts()
	if ok := s.Start(context.Background(), "late", func(context.Context) {}); ok {
		t.Fatal("Start() = true after StopStarts")
	}
	select {
	case <-stopped:
		t.Fatal("StopStarts canceled supervisor context")
	case <-time.After(50 * time.Millisecond):
	}

	s.Cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Cancel did not cancel supervisor context")
	}
	s.Wait(context.Background())
}

func TestTaskSupervisorPreservesParentCancellation(t *testing.T) {
	s := newTaskSupervisor()
	parent, cancel := context.WithCancelCause(context.Background())
	wantErr := errors.New("request stopped")
	done := make(chan error, 1)
	if ok := s.Start(parent, "test", func(ctx context.Context) {
		<-ctx.Done()
		done <- context.Cause(ctx)
	}); !ok {
		t.Fatal("Start() = false before Stop")
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
	s.Wait(context.Background())
}

func TestTaskSupervisorStartConcurrentWithStop(t *testing.T) {
	s := newTaskSupervisor()
	const attempts = 100
	var starters sync.WaitGroup
	starters.Add(attempts)
	for range attempts {
		go func() {
			defer starters.Done()
			s.Start(context.Background(), "test", func(ctx context.Context) { <-ctx.Done() })
		}()
	}
	done := make(chan struct{})
	go func() {
		s.Wait(context.Background())
		close(done)
	}()
	starters.Wait()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("concurrent Stop did not finish")
	}
}
