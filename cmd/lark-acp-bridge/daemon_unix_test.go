//go:build unix && !ios

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
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
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if got, want := daemonPIDFile(configPath), filepath.Join(filepath.Dir(configPath), "lark-acp-bridge.pid"); got != want {
		t.Fatalf("daemonPIDFile() = %q, want %q", got, want)
	}
	if got, want := daemonLogFile(configPath), filepath.Join(filepath.Dir(configPath), "lark-acp-bridge.log"); got != want {
		t.Fatalf("daemonLogFile() = %q, want %q", got, want)
	}
}

func TestDaemonFilesKeepLegacyNamesForNonDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "custom.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", configPath, err)
	}
	if got, want := daemonPIDFile(configPath), filepath.Join(dir, "lark-acp-bridge.pid"); got != want {
		t.Fatalf("daemonPIDFile() = %q, want %q", got, want)
	}
	if got, want := daemonLogFile(configPath), filepath.Join(dir, "lark-acp-bridge.log"); got != want {
		t.Fatalf("daemonLogFile() = %q, want %q", got, want)
	}
}

func TestStartDaemonDetectsLegacyNonDefaultConfigDaemon(t *testing.T) {
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("requires /proc")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "custom.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", configPath, err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestLegacyDaemonHelperProcess$", "--", "--daemon-child")
	cmd.Env = append(os.Environ(), daemonEnvToken, "LARK_ACP_BRIDGE_LEGACY_DAEMON_TEST=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start legacy daemon helper error = %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	pidFile := filepath.Join(dir, "lark-acp-bridge.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", pidFile, err)
	}
	deadline := time.Now().Add(time.Second)
	if !waitForDeadline(deadline, 10*time.Millisecond, func() bool {
		_, running, err := readRunningPID(pidFile)
		return err == nil && running
	}) {
		t.Fatal("legacy daemon helper was not recognized as running")
	}

	err := startDaemon(configPath)
	if err == nil {
		t.Fatal("startDaemon() error = nil, want legacy daemon conflict")
	}
	if !strings.Contains(err.Error(), "服务已在后台运行") {
		t.Fatalf("startDaemon() error = %v, want already running error", err)
	}
}

func TestLegacyDaemonHelperProcess(t *testing.T) {
	if os.Getenv("LARK_ACP_BRIDGE_LEGACY_DAEMON_TEST") != "1" {
		return
	}
	time.Sleep(time.Minute)
}

func TestChildArgsDropsMode(t *testing.T) {
	args := childArgs([]string{"-config", "/tmp/config.json", modeRestart})
	want := []string{"--daemon-child", "-config", "/tmp/config.json"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("childArgs() = %#v, want %#v", args, want)
	}
}

func TestChildArgsKeepsDoubleDashConfigValue(t *testing.T) {
	args := childArgs([]string{"--config", modeRestart, modeRestart})
	want := []string{"--daemon-child", "--config", modeRestart}
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

func TestWaitForDeadlineReturnsTrueWhenCondImmediatelyTrue(t *testing.T) {
	got := waitForDeadline(time.Now().Add(time.Second), 10*time.Millisecond, func() bool { return true })
	if !got {
		t.Fatal("waitForDeadline = false, want true")
	}
}

func TestWaitForDeadlineReturnsTrueWhenCondBecomesTrue(t *testing.T) {
	done := make(chan struct{})
	go func() {
		time.Sleep(30 * time.Millisecond)
		close(done)
	}()
	got := waitForDeadline(time.Now().Add(time.Second), 10*time.Millisecond, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	if !got {
		t.Fatal("waitForDeadline = false, want true after cond flips")
	}
}

func TestWaitForDeadlineReturnsFalseOnTimeout(t *testing.T) {
	start := time.Now()
	got := waitForDeadline(start.Add(40*time.Millisecond), 10*time.Millisecond, func() bool { return false })
	if got {
		t.Fatal("waitForDeadline = true, want false on timeout")
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("waitForDeadline returned too early: %s", elapsed)
	}
}
