//go:build (!unix && !windows) || ios

package main

import "testing"

func TestAcquireInstanceLockKeepsForegroundAvailable(t *testing.T) {
	unlock, err := acquireInstanceLock("config.json")
	if err != nil {
		t.Fatalf("acquireInstanceLock() error = %v", err)
	}
	unlock()
}
