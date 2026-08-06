//go:build !plan9

package bridge

import (
	"errors"
	"syscall"
)

func isPlatformBrokenPipeError(err error) bool {
	return errors.Is(err, syscall.EPIPE)
}
