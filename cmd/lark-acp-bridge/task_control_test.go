package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/bridge"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func TestTaskControlServerAndClientRoundTripError(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	svc := bridge.NewService(config.Config{}, bridge.NewSessionStore(""))
	control, err := startTaskControlServer(svc, configPath)
	if err != nil {
		t.Fatalf("startTaskControlServer() error = %v", err)
	}
	t.Cleanup(func() { _ = control.Shutdown(context.Background()) })

	input := strings.NewReader(`{"capability":"invalid","schedule":"@every 5m","prompt":"check","max_runs":1}`)
	err = runTaskCreate(context.Background(), configPath, input, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "capability") {
		t.Fatalf("runTaskCreate() error = %v, want capability rejection", err)
	}
	if _, err := os.Stat(taskControlStatePath(configPath)); err != nil {
		t.Fatalf("control state missing: %v", err)
	}
}

func TestTaskControlShutdownRemovesOwnedState(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	control, err := startTaskControlServer(bridge.NewService(config.Config{}, bridge.NewSessionStore("")), configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(taskControlStatePath(configPath)); !os.IsNotExist(err) {
		t.Fatalf("control state err = %v, want not exist", err)
	}
}

func TestTaskCreateDispatchesThroughRealCLIEntry(t *testing.T) {
	err := execute([]string{"--config", filepath.Join(t.TempDir(), "config.json"), "task", "create", "--stdin"})
	if err == nil || !strings.Contains(err.Error(), "读取 bridge 控制端点失败") {
		t.Fatalf("execute(task create) error = %v, want task command dispatched to control client", err)
	}
}
