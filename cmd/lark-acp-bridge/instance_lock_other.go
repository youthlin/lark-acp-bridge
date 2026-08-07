//go:build !unix && !windows

package main

func acquireInstanceLock(configPath string) (func(), error) {
	return func() {}, nil
}
