//go:build unix && !ios

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRunMode(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "default restart", args: nil, want: modeRestart},
		{name: "explicit run", args: []string{modeRun}, want: modeRun},
		{name: "start", args: []string{modeStart}, want: modeStart},
		{name: "stop", args: []string{modeStop}, want: modeStop},
		{name: "restart", args: []string{modeRestart}, want: modeRestart},
		{name: "unknown stays unknown", args: []string{"status"}, want: "status"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runMode(tt.args); got != tt.want {
				t.Fatalf("runMode(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestDaemonFilesUseConfigDirectory(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if got, want := daemonPIDFile(configPath), filepath.Join(filepath.Dir(configPath), "lark-acp-bridge.pid"); got != want {
		t.Fatalf("daemonPIDFile() = %q, want %q", got, want)
	}
	if got, want := daemonLogFile(configPath), filepath.Join(filepath.Dir(configPath), "lark-acp-bridge.log"); got != want {
		t.Fatalf("daemonLogFile() = %q, want %q", got, want)
	}
}

func TestChildArgsDropsMode(t *testing.T) {
	args := childArgs([]string{"-config", "/tmp/config.json", modeRestart})
	want := []string{"-daemon-child", "-config", "/tmp/config.json"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("childArgs() = %#v, want %#v", args, want)
	}
}

func TestChildArgsKeepsDoubleDashConfigValue(t *testing.T) {
	args := childArgs([]string{"--config", modeRestart, modeRestart})
	want := []string{"-daemon-child", "--config", modeRestart}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("childArgs() = %#v, want %#v", args, want)
	}
}

func TestReadRunningPIDRemovesStaleKernelPID(t *testing.T) {
	if _, err := os.Stat("/proc/1/environ"); err != nil {
		t.Skip("requires /proc")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "lark-acp-bridge.pid")
	if err := os.WriteFile(pidFile, []byte("1"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, running, err := readRunningPID(pidFile)
	if err != nil {
		t.Fatalf("readRunningPID() error = %v", err)
	}
	if running {
		t.Fatalf("pid 1 should not be treated as this daemon")
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("stale pid file should be removed, stat err = %v", err)
	}
}
