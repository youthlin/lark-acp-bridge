//go:build unix

package config

import "os"

func warnIfSensitiveFileTooPermissive(category, path string) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return
	}
	mode := info.Mode()
	if mode.Perm()&sensitiveFileTooPermissiveMask == 0 {
		return
	}
	warnSensitiveFileTooPermissive(category, path, mode)
}
