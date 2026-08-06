//go:build plan9

package bridge

func isPlatformBrokenPipeError(err error) bool {
	return false
}
