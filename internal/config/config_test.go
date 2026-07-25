package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateUsesHomeDataDir(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)

	result, err := LoadOrCreate("")
	if err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}
	wantPath := filepath.Join(home, "."+appName, "config.json")
	if result.Path != wantPath {
		t.Fatalf("Path = %q, want %q", result.Path, wantPath)
	}
	if !result.Created {
		t.Fatalf("Created = false, want true")
	}
	if !result.Config.MissingBotConfig() {
		t.Fatalf("MissingBotConfig() = false, want true")
	}
	if len(result.Config.Bots) != 1 {
		t.Fatalf("len(Bots) = %d, want 1", len(result.Config.Bots))
	}
	wantWorkspace := filepath.Join(home, "."+appName, "bots/default")
	if result.Config.Bots[0].Workspace != wantWorkspace {
		t.Fatalf("Workspace = %q, want %q", result.Config.Bots[0].Workspace, wantWorkspace)
	}
	if info, err := os.Stat(wantWorkspace); err != nil {
		t.Fatalf("bot workspace not created: %v", err)
	} else if !info.IsDir() {
		t.Fatalf("workspace is not dir: %s", wantWorkspace)
	}
	agent := result.Config.Agents["traex"]
	if agent.DefaultCwd != home {
		t.Fatalf("DefaultCwd = %q, want %q", agent.DefaultCwd, home)
	}

	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(data), `"$HOME"`) {
		t.Fatalf("config file should keep portable $HOME path, got:\n%s", data)
	}
}

func TestLoadExpandsHomePath(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)

	configPath := filepath.Join(tmp, "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": "main",
      "app_id": "cli_xxx",
      "app_secret": "secret",
      "workspace": "~/bridge/main"
    }
  ],
  "agents": {
    "traex": {
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "~/go/src"
    }
  }
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := filepath.Join(home, "go/src")
	if cfg.Agents["traex"].DefaultCwd != want {
		t.Fatalf("DefaultCwd = %q, want %q", cfg.Agents["traex"].DefaultCwd, want)
	}
	wantWorkspace := filepath.Join(home, "bridge/main")
	if cfg.Bots[0].Workspace != wantWorkspace {
		t.Fatalf("Workspace = %q, want %q", cfg.Bots[0].Workspace, wantWorkspace)
	}
	if cfg.MissingBotConfig() {
		t.Fatalf("MissingBotConfig() = true, want false")
	}
}

func TestLoadNormalizesBotOwnerOpenIDs(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(tmp, "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": "main",
      "app_id": "cli_xxx",
      "app_secret": "secret",
      "workspace": "$HOME/.lark-acp-bridge/bots/main",
      "owner_open_ids": [" ou_owner ", "", "ou_owner", "ou_backup"]
    }
  ],
  "agents": {
    "traex": {
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME"
    }
  }
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got := cfg.Bots[0].OwnerOpenIDs
	want := []string{"ou_owner", "ou_backup"}
	if len(got) != len(want) {
		t.Fatalf("OwnerOpenIDs = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("OwnerOpenIDs = %#v, want %#v", got, want)
		}
	}
}

func TestLoadRejectsDuplicateBotID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	configPath := filepath.Join(tmp, "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": "same",
      "app_id": "cli_a",
      "app_secret": "secret",
      "workspace": "$HOME/.lark-acp-bridge/bots/a"
    },
    {
      "id": "same",
      "app_id": "cli_b",
      "app_secret": "secret",
      "workspace": "$HOME/.lark-acp-bridge/bots/b"
    }
  ],
  "agents": {
    "traex": {
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME"
    }
  }
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := Load(configPath)
	if err == nil || !strings.Contains(err.Error(), "bot id 重复") {
		t.Fatalf("Load() error = %v, want duplicate bot id", err)
	}
}

func TestLoadRejectsDuplicateBotWorkspace(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(tmp, "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": "bot-a",
      "app_id": "cli_a",
      "app_secret": "secret",
      "workspace": "$HOME/.lark-acp-bridge/bots/shared"
    },
    {
      "id": "bot-b",
      "app_id": "cli_b",
      "app_secret": "secret",
      "workspace": "~/.lark-acp-bridge/bots/shared"
    }
  ],
  "agents": {
    "traex": {
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME"
    }
  }
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := Load(configPath)
	if err == nil || !strings.Contains(err.Error(), "bot workspace 重复") {
		t.Fatalf("Load() error = %v, want duplicate bot workspace", err)
	}
}

func TestValidateAgentCommandsAcceptsPathCommand(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "fake-acp")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfg := Config{
		Agents: map[string]AgentConfig{
			"fake": {Command: command},
		},
	}

	if err := cfg.ValidateAgentCommands(); err != nil {
		t.Fatalf("ValidateAgentCommands() error = %v", err)
	}
}

func TestValidateAgentCommandsAcceptsPathLookupCommand(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "fake-acp")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", dir)
	cfg := Config{
		Agents: map[string]AgentConfig{
			"fake": {Command: "fake-acp"},
		},
	}

	if err := cfg.ValidateAgentCommands(); err != nil {
		t.Fatalf("ValidateAgentCommands() error = %v", err)
	}
}

func TestValidateAgentCommandsRejectsMissingCommand(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := Config{
		Agents: map[string]AgentConfig{
			"missing": {Command: "missing-acp-server"},
		},
	}

	err := cfg.ValidateAgentCommands()
	if err == nil || !strings.Contains(err.Error(), `agent "missing" 启动命令不存在`) {
		t.Fatalf("ValidateAgentCommands() error = %v, want missing command", err)
	}
}

func TestValidateAgentCommandsRejectsEmptyCommand(t *testing.T) {
	cfg := Config{
		Agents: map[string]AgentConfig{
			"empty": {Command: " "},
		},
	}

	err := cfg.ValidateAgentCommands()
	if err == nil || !strings.Contains(err.Error(), `agent "empty" 启动命令为空`) {
		t.Fatalf("ValidateAgentCommands() error = %v, want empty command", err)
	}
}

func TestValidateAgentCommandsRejectsDirectoryPath(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Agents: map[string]AgentConfig{
			"dir": {Command: dir},
		},
	}

	err := cfg.ValidateAgentCommands()
	if err == nil || !strings.Contains(err.Error(), `agent "dir" 启动命令是目录`) {
		t.Fatalf("ValidateAgentCommands() error = %v, want directory command", err)
	}
}

func TestValidateAgentCommandsRejectsNonExecutablePath(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "fake-acp")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfg := Config{
		Agents: map[string]AgentConfig{
			"fake": {Command: command},
		},
	}

	err := cfg.ValidateAgentCommands()
	if err == nil || !strings.Contains(err.Error(), `agent "fake" 启动命令不可执行`) {
		t.Fatalf("ValidateAgentCommands() error = %v, want non-executable command", err)
	}
}
