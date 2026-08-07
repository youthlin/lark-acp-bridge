//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAcquireInstanceLockRejectsSameConfigUntilReleased(t *testing.T) {
	configPath := writeInstanceLockTestConfig(t, "config.json")

	unlock, err := acquireInstanceLock(configPath)
	if err != nil {
		t.Fatalf("acquireInstanceLock() error = %v", err)
	}
	lockFile, err := instanceLockFile(configPath)
	if err != nil {
		t.Fatalf("instanceLockFile() error = %v", err)
	}
	data, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", lockFile, err)
	}
	if got, want := strings.TrimSpace(string(data)), strconv.Itoa(os.Getpid()); got != want {
		t.Fatalf("lock pid = %q, want %q", got, want)
	}

	if secondUnlock, err := acquireInstanceLock(configPath); err == nil {
		secondUnlock()
		t.Fatal("second acquireInstanceLock() error = nil, want already running error")
	} else if !strings.Contains(err.Error(), "同一配置的 Bridge 实例已在运行") {
		t.Fatalf("second acquireInstanceLock() error = %v", err)
	}

	unlock()
	reacquiredUnlock, err := acquireInstanceLock(configPath)
	if err != nil {
		t.Fatalf("acquireInstanceLock() after release error = %v", err)
	}
	reacquiredUnlock()
}

func TestAcquireInstanceLockAllowsDifferentConfigs(t *testing.T) {
	firstConfig := writeInstanceLockTestConfig(t, "first.json")
	secondConfig := writeInstanceLockTestConfig(t, "second.json")

	firstUnlock, err := acquireInstanceLock(firstConfig)
	if err != nil {
		t.Fatalf("acquire first config lock error = %v", err)
	}
	defer firstUnlock()
	secondUnlock, err := acquireInstanceLock(secondConfig)
	if err != nil {
		t.Fatalf("acquire second config lock error = %v", err)
	}
	secondUnlock()
}

func TestAcquireInstanceLockCanonicalizesConfigSymlink(t *testing.T) {
	configPath := writeInstanceLockTestConfig(t, "config.json")
	aliasPath := filepath.Join(filepath.Dir(configPath), "config-alias.json")
	if err := os.Symlink(configPath, aliasPath); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	unlock, err := acquireInstanceLock(configPath)
	if err != nil {
		t.Fatalf("acquireInstanceLock() error = %v", err)
	}
	defer unlock()

	if aliasUnlock, err := acquireInstanceLock(aliasPath); err == nil {
		aliasUnlock()
		t.Fatal("acquireInstanceLock(symlink) error = nil, want already running error")
	}
}

func writeInstanceLockTestConfig(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	return path
}
