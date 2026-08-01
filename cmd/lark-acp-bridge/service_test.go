package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func TestRenderSystemdUserService(t *testing.T) {
	got := renderSystemdUserService(
		"/opt/lark acp/lark-acp-bridge",
		"/home/user/.lark-acp-bridge/config.json",
		"/home/user/bridge work",
	)
	for _, want := range []string{
		"[Unit]",
		"Description=Lark ACP Bridge",
		`WorkingDirectory="/home/user/bridge work"`,
		`ExecStart="/opt/lark acp/lark-acp-bridge" --config /home/user/.lark-acp-bridge/config.json run`,
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("systemd service =\n%s\nmissing %q", got, want)
		}
	}
}

func TestRenderSystemdUserServiceEscapesPercent(t *testing.T) {
	got := renderSystemdUserService(
		"/tmp/lark-acp-bridge",
		"/tmp/%i/config.json",
		"/tmp/work",
	)
	if !strings.Contains(got, "/tmp/%%i/config.json") {
		t.Fatalf("systemd service =\n%s\nwant escaped percent", got)
	}
}

func TestRenderLaunchdAgent(t *testing.T) {
	got := renderLaunchdAgent(
		"/Applications/Lark ACP Bridge/lark-acp-bridge",
		"/Users/me/.lark-acp-bridge/config.json",
		"/Users/me/work & test",
		"/Users/me/.lark-acp-bridge/lark-acp-bridge.log",
	)
	for _, want := range []string{
		"<key>Label</key>",
		"<string>com.youthlin.lark-acp-bridge</string>",
		"<key>ProgramArguments</key>",
		"<string>/Applications/Lark ACP Bridge/lark-acp-bridge</string>",
		"<string>--config</string>",
		"<string>/Users/me/.lark-acp-bridge/config.json</string>",
		"<string>run</string>",
		"<key>WorkingDirectory</key>",
		"<string>/Users/me/work &amp; test</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<key>StandardOutPath</key>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("launchd plist =\n%s\nmissing %q", got, want)
		}
	}
}

func TestServiceTargetPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	linux, err := serviceTargetPath("linux")
	if err != nil {
		t.Fatalf("serviceTargetPath(linux) error = %v", err)
	}
	if want := filepath.Join(home, ".config", "systemd", "user", serviceUnitName); linux != want {
		t.Fatalf("linux target = %q, want %q", linux, want)
	}

	darwin, err := serviceTargetPath("darwin")
	if err != nil {
		t.Fatalf("serviceTargetPath(darwin) error = %v", err)
	}
	if want := filepath.Join(home, "Library", "LaunchAgents", launchdServiceName+".plist"); darwin != want {
		t.Fatalf("darwin target = %q, want %q", darwin, want)
	}
}

func TestInstallServiceWritesLinuxUnit(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	binary := filepath.Join(tmp, "lark-acp-bridge")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(binary) error = %v", err)
	}
	workdir := filepath.Join(tmp, "work")
	if err := os.Mkdir(workdir, 0o755); err != nil {
		t.Fatalf("Mkdir(workdir) error = %v", err)
	}
	configPath := filepath.Join(home, ".lark-acp-bridge", "config.json")
	if err := config.Write(configPath, config.Config{
		Bots: []config.BotConfig{{
			ID:        "default",
			AppID:     "cli_xxx",
			AppSecret: config.FileSecret("$HOME/.lark-acp-bridge/secrets/default.appsecret"),
			Workspace: "$HOME/.lark-acp-bridge/bots/default",
		}},
		RestartCommand: []string{"/bin/false"},
	}); err != nil {
		t.Fatalf("Write(config) error = %v", err)
	}

	out := captureStdout(t, func() {
		if err := installService(serviceInstallOptions{
			GOOS:       "linux",
			ConfigPath: configPath,
			BinaryPath: binary,
			WorkingDir: workdir,
		}); err != nil {
			t.Fatalf("installService() error = %v", err)
		}
	})
	target := filepath.Join(home, ".config", "systemd", "user", serviceUnitName)
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(unit) error = %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "ExecStart="+binary+" --config "+configPath+" run") {
		t.Fatalf("unit =\n%s\nwant ExecStart", got)
	}
	if !strings.Contains(out, "systemctl --user enable --now "+serviceUnitName) ||
		!strings.Contains(out, "已更新配置 restart_command: systemctl --user restart "+serviceUnitName) {
		t.Fatalf("stdout = %q, want systemd next steps", out)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load(config) error = %v", err)
	}
	if !slices.Equal(cfg.RestartCommand, systemdRestartCommand()) {
		t.Fatalf("RestartCommand = %#v, want %#v", cfg.RestartCommand, systemdRestartCommand())
	}
	if cfg.Bots[0].AppSecret.RuntimeValue() != "" || cfg.Bots[0].AppSecret.Path == "" {
		t.Fatalf("AppSecret = %+v, want unresolved file secret reference", cfg.Bots[0].AppSecret)
	}
}

func TestInstallServiceWritesLaunchdPlist(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	binary := filepath.Join(tmp, "lark-acp-bridge")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(binary) error = %v", err)
	}
	workdir := filepath.Join(tmp, "work")
	if err := os.Mkdir(workdir, 0o755); err != nil {
		t.Fatalf("Mkdir(workdir) error = %v", err)
	}
	configPath := filepath.Join(home, ".lark-acp-bridge", "config.json")
	if err := config.Write(configPath, config.Config{
		Bots: []config.BotConfig{{
			ID:        "default",
			AppID:     "cli_xxx",
			AppSecret: config.FileSecret("$HOME/.lark-acp-bridge/secrets/default.appsecret"),
			Workspace: "$HOME/.lark-acp-bridge/bots/default",
		}},
	}); err != nil {
		t.Fatalf("Write(config) error = %v", err)
	}

	out := captureStdout(t, func() {
		if err := installService(serviceInstallOptions{
			GOOS:       "darwin",
			ConfigPath: configPath,
			BinaryPath: binary,
			WorkingDir: workdir,
		}); err != nil {
			t.Fatalf("installService() error = %v", err)
		}
	})
	target := filepath.Join(home, "Library", "LaunchAgents", launchdServiceName+".plist")
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(plist) error = %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "<string>"+binary+"</string>") ||
		!strings.Contains(got, "<string>"+configPath+"</string>") ||
		!strings.Contains(got, "<string>"+filepath.Join(filepath.Dir(configPath), "lark-acp-bridge.log")+"</string>") {
		t.Fatalf("plist =\n%s\nwant binary/config/log", got)
	}
	if !strings.Contains(out, "launchctl bootstrap gui/") ||
		!strings.Contains(out, "已更新配置 restart_command: launchctl kickstart -k gui/") {
		t.Fatalf("stdout = %q, want launchd next steps", out)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load(config) error = %v", err)
	}
	if want := launchdRestartCommand(os.Getuid()); !slices.Equal(cfg.RestartCommand, want) {
		t.Fatalf("RestartCommand = %#v, want %#v", cfg.RestartCommand, want)
	}
}

func TestInstallServiceRejectsGoRunExecutable(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	binaryDir := filepath.Join(tmp, "go-build123", "b001", "exe")
	if err := os.MkdirAll(binaryDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(binaryDir) error = %v", err)
	}
	binary := filepath.Join(binaryDir, "lark-acp-bridge")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(binary) error = %v", err)
	}
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatalf("Mkdir(home) error = %v", err)
	}
	if err := config.Write(filepath.Join(home, ".lark-acp-bridge", "config.json"), config.Default()); err != nil {
		t.Fatalf("Write(config) error = %v", err)
	}
	oldCurrentExecutable := currentExecutable
	currentExecutable = func() (string, error) {
		return binary, nil
	}
	t.Cleanup(func() {
		currentExecutable = oldCurrentExecutable
	})

	err := installService(serviceInstallOptions{GOOS: "linux", ConfigPath: filepath.Join(home, ".lark-acp-bridge", "config.json")})
	if err == nil || !strings.Contains(err.Error(), "go run 临时文件") {
		t.Fatalf("installService() error = %v, want go run guidance", err)
	}
}

func TestUninstallServiceRemovesTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".config", "systemd", "user", serviceUnitName)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("MkdirAll(target dir) error = %v", err)
	}
	if err := os.WriteFile(target, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}

	out := captureStdout(t, func() {
		if err := uninstallService("linux"); err != nil {
			t.Fatalf("uninstallService() error = %v", err)
		}
	})
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("Stat(target) err = %v, want not exist", err)
	}
	if !strings.Contains(out, "systemctl --user disable --now "+serviceUnitName) {
		t.Fatalf("stdout = %q, want disable guidance", out)
	}
}
