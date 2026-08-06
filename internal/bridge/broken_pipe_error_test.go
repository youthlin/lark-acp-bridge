//go:build !plan9

package bridge

import (
	"fmt"
	"syscall"
	"testing"
)

func TestBrokenACPClientPipeErrorRecognizesEPIPE(t *testing.T) {
	err := fmt.Errorf("session/prompt: %w", syscall.EPIPE)
	if !isBrokenACPClientPipeError(err) {
		t.Fatalf("isBrokenACPClientPipeError(%v) = false, want true", err)
	}
}
