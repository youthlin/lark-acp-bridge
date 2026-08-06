//go:build plan9

package bridge

import (
	"errors"
	"testing"
)

func TestPlatformBrokenPipeErrorUnavailable(t *testing.T) {
	if isPlatformBrokenPipeError(errors.New("plan9 pipe error")) {
		t.Fatal("isPlatformBrokenPipeError() = true, want false")
	}
}
