package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateRollbackCommandRestoresBackup(t *testing.T) {
	target := filepath.Join(t.TempDir(), "lark-acp-bridge")
	if err := os.WriteFile(target, []byte("new"), 0o755); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}
	backup := target + ".bak"
	if err := os.WriteFile(backup, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(backup) error = %v", err)
	}

	err := runUpdate(updateCommandOptions{Rollback: true, BinaryPath: target})
	if err != nil {
		t.Fatalf("runUpdate(rollback) error = %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("target = %q, want old", got)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("Stat(backup) error = %v", err)
	}
}
