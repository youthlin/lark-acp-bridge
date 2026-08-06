//go:build (!unix && !windows) || ios

package main

func acquireInstanceLock(configPath string) (func(), error) {
	return func() {}, nil
}
