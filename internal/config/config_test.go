package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	wantArgs := []string{"acp", "serve", "-c", "permission_mode=auto"}
	if !slices.Equal(agent.Args, wantArgs) {
		t.Fatalf("Args = %#v, want %#v", agent.Args, wantArgs)
	}

	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(data), `"$HOME"`) {
		t.Fatalf("config file should keep portable $HOME path, got:\n%s", data)
	}
}

func TestConfigExampleUsesDefaultTraexArgs(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("ReadFile(config.example.json) error = %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Unmarshal(config.example.json) error = %v", err)
	}
	got := cfg.Agents["traex"].Args
	want := Default().Agents["traex"].Args
	if !slices.Equal(got, want) {
		t.Fatalf("config.example.json traex args = %#v, want %#v", got, want)
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

func TestLoadTrimsBotID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	configPath := filepath.Join(tmp, "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": " main ",
      "app_id": "cli_xxx",
      "app_secret": "secret",
      "workspace": "$HOME/.lark-acp-bridge/bots/main"
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
	if got := cfg.Bots[0].ID; got != "main" {
		t.Fatalf("Bot ID = %q, want trimmed main", got)
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

func TestLoadNormalizesBotOpenID(t *testing.T) {
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
      "bot_open_id": " ou_bot "
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
	if got, want := cfg.Bots[0].BotOpenID, "ou_bot"; got != want {
		t.Fatalf("BotOpenID = %q, want %q", got, want)
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

func TestLoadRejectsDuplicateBotIDAfterTrim(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	configPath := filepath.Join(tmp, "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": " same ",
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
		t.Fatalf("Load() error = %v, want duplicate bot id after trim", err)
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

func TestLoadTrimsAgentName(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(tmp, "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": "default",
      "app_id": "cli_a",
      "app_secret": "secret",
      "workspace": "$HOME/.lark-acp-bridge/bots/default"
    }
  ],
  "agents": {
    " traex ": {
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
	if _, ok := cfg.Agents["traex"]; !ok {
		t.Fatalf("Agents = %#v, want trimmed traex key", cfg.Agents)
	}
	if _, ok := cfg.Agents[" traex "]; ok {
		t.Fatalf("Agents = %#v, want raw agent key removed", cfg.Agents)
	}
}

func TestLoadRejectsDuplicateAgentNameAfterTrim(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(tmp, "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": "default",
      "app_id": "cli_a",
      "app_secret": "secret",
      "workspace": "$HOME/.lark-acp-bridge/bots/default"
    }
  ],
  "agents": {
    " same ": {
      "command": "traex",
      "args": ["acp", "serve"]
    },
    "same": {
      "command": "other",
      "args": ["acp", "serve"]
    }
  }
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := Load(configPath)
	if err == nil || !strings.Contains(err.Error(), "agent 名称重复") {
		t.Fatalf("Load() error = %v, want duplicate agent name after trim", err)
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

func TestValidateAgentCommandsRejectsBlankAgentName(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "fake-acp")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfg := Config{
		Agents: map[string]AgentConfig{
			"  ": {Command: command},
		},
	}

	err := cfg.ValidateAgentCommands()
	if err == nil || !strings.Contains(err.Error(), "agent 名称不能为空") {
		t.Fatalf("ValidateAgentCommands() error = %v, want blank agent name rejection", err)
	}
}

func TestLoadNormalizesRestartCommand(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
  "restart_command": [" ", " /bin/echo ", " restarted "],
  "bots": [
    {
      "id": "default",
      "app_id": "cli_a",
      "app_secret": "secret",
      "workspace": "` + filepath.ToSlash(filepath.Join(t.TempDir(), "workspace")) + `"
    }
  ],
  "agents": {
    "traex": {
      "command": "traex",
      "args": ["acp", "serve"]
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
	want := []string{"/bin/echo", "restarted"}
	if !slices.Equal(cfg.RestartCommand, want) {
		t.Fatalf("RestartCommand = %#v, want %#v", cfg.RestartCommand, want)
	}
}

func TestWriteResolvedBotFieldsFillsEmptyFields(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": "bot-a",
      "app_id": "cli_xxx",
      "app_secret": "secret",
      "workspace": "$HOME/.lark-acp-bridge/bots/default"
    }
  ],
  "agents": {
    "traex": {
      "command": "traex",
      "args": [
        "acp",
        "serve"
      ]
    }
  },
  "custom": true
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	wrote, err := WriteResolvedBotFields(configPath, []BotConfig{{
		ID:           "bot-a",
		BotOpenID:    " ou_bot ",
		OwnerOpenIDs: []string{" ou_owner ", "ou_owner"},
	}})
	if err != nil {
		t.Fatalf("WriteResolvedBotFields() error = %v", err)
	}
	if !wrote {
		t.Fatal("WriteResolvedBotFields() wrote = false, want true")
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}
	var got struct {
		Bots []struct {
			ID           string   `json:"id"`
			AppID        string   `json:"app_id"`
			AppSecret    string   `json:"app_secret"`
			Workspace    string   `json:"workspace"`
			BotOpenID    string   `json:"bot_open_id"`
			OwnerOpenIDs []string `json:"owner_open_ids"`
		} `json:"bots"`
		Custom bool `json:"custom"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal(config) error = %v", err)
	}
	if got.Bots[0].BotOpenID != "ou_bot" {
		t.Fatalf("bot_open_id = %q, want ou_bot", got.Bots[0].BotOpenID)
	}
	if want := []string{"ou_owner"}; !reflect.DeepEqual(got.Bots[0].OwnerOpenIDs, want) {
		t.Fatalf("owner_open_ids = %#v, want %#v", got.Bots[0].OwnerOpenIDs, want)
	}
	if got.Bots[0].Workspace != "$HOME/.lark-acp-bridge/bots/default" {
		t.Fatalf("workspace = %q, want original literal path", got.Bots[0].Workspace)
	}
	if !got.Custom {
		t.Fatal("custom field was not preserved")
	}
}

func TestWriteResolvedBotFieldsDoesNotOverwriteConfiguredFields(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": "bot-a",
      "app_id": "cli_xxx",
      "app_secret": "secret",
      "workspace": "$HOME/.lark-acp-bridge/bots/default",
      "bot_open_id": "ou_configured_bot",
      "owner_open_ids": [
        "ou_configured_owner"
      ]
    }
  ],
  "agents": {
    "traex": {
      "command": "traex",
      "args": [
        "acp",
        "serve"
      ]
    }
  }
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	wrote, err := WriteResolvedBotFields(configPath, []BotConfig{{
		ID:           "bot-a",
		BotOpenID:    "ou_resolved_bot",
		OwnerOpenIDs: []string{"ou_resolved_owner"},
	}})
	if err != nil {
		t.Fatalf("WriteResolvedBotFields() error = %v", err)
	}
	if wrote {
		t.Fatal("WriteResolvedBotFields() wrote = true, want false for already configured fields")
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}
	var got struct {
		Bots []struct {
			BotOpenID    string   `json:"bot_open_id"`
			OwnerOpenIDs []string `json:"owner_open_ids"`
		} `json:"bots"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal(config) error = %v", err)
	}
	if got.Bots[0].BotOpenID != "ou_configured_bot" {
		t.Fatalf("bot_open_id = %q, want configured value", got.Bots[0].BotOpenID)
	}
	if want := []string{"ou_configured_owner"}; !reflect.DeepEqual(got.Bots[0].OwnerOpenIDs, want) {
		t.Fatalf("owner_open_ids = %#v, want %#v", got.Bots[0].OwnerOpenIDs, want)
	}
}

func TestWriteCleansTemporaryFileWhenAtomicReplaceFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("Mkdir(config path) error = %v", err)
	}

	err := Write(path, Default())
	if err == nil {
		t.Fatal("Write() error = nil, want replace failure")
	}
	if _, statErr := os.Stat(path + ".tmp"); !os.IsNotExist(statErr) {
		t.Fatalf("temporary config file still exists or stat failed: %v", statErr)
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

func TestValidateAgentCommandsAcceptsDefaultCwdDirectory(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "fake-acp")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cwd := filepath.Join(dir, "repo")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatalf("Mkdir(default cwd) error = %v", err)
	}
	cfg := Config{
		Agents: map[string]AgentConfig{
			"fake": {Command: command, DefaultCwd: cwd},
		},
	}

	if err := cfg.ValidateAgentCommands(); err != nil {
		t.Fatalf("ValidateAgentCommands() error = %v", err)
	}
}

func TestValidateAgentCommandsRejectsMissingDefaultCwd(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "fake-acp")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfg := Config{
		Agents: map[string]AgentConfig{
			"fake": {Command: command, DefaultCwd: filepath.Join(dir, "missing")},
		},
	}

	err := cfg.ValidateAgentCommands()
	if err == nil || !strings.Contains(err.Error(), `agent "fake" 默认工作目录不可访问`) {
		t.Fatalf("ValidateAgentCommands() error = %v, want missing default cwd", err)
	}
}

func TestValidateAgentCommandsRejectsDefaultCwdFile(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "fake-acp")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(command) error = %v", err)
	}
	cwd := filepath.Join(dir, "repo.txt")
	if err := os.WriteFile(cwd, []byte("not a dir\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(default cwd) error = %v", err)
	}
	cfg := Config{
		Agents: map[string]AgentConfig{
			"fake": {Command: command, DefaultCwd: cwd},
		},
	}

	err := cfg.ValidateAgentCommands()
	if err == nil || !strings.Contains(err.Error(), `agent "fake" 默认工作目录不是目录`) {
		t.Fatalf("ValidateAgentCommands() error = %v, want default cwd file rejection", err)
	}
}

func TestValidateAgentCommandsAcceptsRestartCommandPath(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "restart-bridge")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfg := Config{
		Agents: map[string]AgentConfig{
			"fake": {Command: command},
		},
		RestartCommand: []string{command},
	}

	if err := cfg.ValidateAgentCommands(); err != nil {
		t.Fatalf("ValidateAgentCommands() error = %v", err)
	}
}

func TestValidateAgentCommandsAcceptsRestartCommandPathLookup(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "restart-bridge")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", dir)
	cfg := Config{
		Agents: map[string]AgentConfig{
			"fake": {Command: "restart-bridge"},
		},
		RestartCommand: []string{"restart-bridge"},
	}

	if err := cfg.ValidateAgentCommands(); err != nil {
		t.Fatalf("ValidateAgentCommands() error = %v", err)
	}
}

func TestValidateAgentCommandsRejectsMissingRestartCommand(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "fake-acp")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", dir)
	cfg := Config{
		Agents: map[string]AgentConfig{
			"fake": {Command: command},
		},
		RestartCommand: []string{"missing-restart-command"},
	}

	err := cfg.ValidateAgentCommands()
	if err == nil || !strings.Contains(err.Error(), "restart_command 启动命令不存在") {
		t.Fatalf("ValidateAgentCommands() error = %v, want missing restart command", err)
	}
}

func TestValidateAgentCommandsRejectsRestartCommandDirectoryPath(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "fake-acp")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfg := Config{
		Agents: map[string]AgentConfig{
			"fake": {Command: command},
		},
		RestartCommand: []string{dir},
	}

	err := cfg.ValidateAgentCommands()
	if err == nil || !strings.Contains(err.Error(), "restart_command 启动命令是目录") {
		t.Fatalf("ValidateAgentCommands() error = %v, want restart command directory", err)
	}
}

func TestValidateAgentCommandsRejectsNonExecutableRestartCommandPath(t *testing.T) {
	dir := t.TempDir()
	agentCommand := filepath.Join(dir, "fake-acp")
	if err := os.WriteFile(agentCommand, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(agentCommand) error = %v", err)
	}
	restartCommand := filepath.Join(dir, "restart-bridge")
	if err := os.WriteFile(restartCommand, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(restartCommand) error = %v", err)
	}
	cfg := Config{
		Agents: map[string]AgentConfig{
			"fake": {Command: agentCommand},
		},
		RestartCommand: []string{restartCommand},
	}

	err := cfg.ValidateAgentCommands()
	if err == nil || !strings.Contains(err.Error(), "restart_command 启动命令不可执行") {
		t.Fatalf("ValidateAgentCommands() error = %v, want non-executable restart command", err)
	}
}
