//go:build !unix

package config

func warnIfSensitiveFileTooPermissive(category, path string) {}
