package bridge

import (
	"context"
	"testing"
)

func TestServiceBackgroundFacadeDelegatesToSupervisor(t *testing.T) {
	s := &Service{taskSupervisor: newTaskSupervisor()}
	s.stopBackgroundStarts()
	if !s.backgroundStopped() {
		t.Fatal("backgroundStopped() = false after stopBackgroundStarts")
	}
	if s.goBackground(context.Background(), "late", func(context.Context) {}) {
		t.Fatal("goBackground() = true after stopBackgroundStarts")
	}
}
