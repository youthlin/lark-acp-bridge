package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

func configAgentListNames(agentList []NamedAgentConfig) []string {
	names := make([]string, 0, len(agentList))
	for _, agent := range agentList {
		names = append(names, agent.Name)
	}
	return names
}

func testConfigWithAgent(name string, agent AgentConfig) Config {
	return Config{
		AgentList: []NamedAgentConfig{
			{Name: name, AgentConfig: agent},
		},
	}
}

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
	agent, ok := result.Config.Agent("traex")
	if !ok {
		t.Fatalf("missing traex agent: %#v", result.Config.AgentList)
	}
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
	traex, ok := cfg.Agent("traex")
	if !ok {
		t.Fatalf("config.example.json AgentList = %#v, want traex agent", cfg.AgentList)
	}
	got := traex.Args
	want := Default().AgentList[0].Args
	if !slices.Equal(got, want) {
		t.Fatalf("config.example.json traex args = %#v, want %#v", got, want)
	}
	if cfg.MessageReaction {
		t.Fatal("config.example.json should keep message_reaction disabled by default")
	}
	if len(cfg.Bots) == 0 || !cfg.Bots[0].Trace.Enabled || cfg.Bots[0].Trace.RetentionDays != 7 {
		t.Fatalf("config.example.json trace = %+v, want enabled with 7d retention", cfg.Bots[0].Trace)
	}
}

func TestLoadDefaultTraceEnabledForOldConfig(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": "default",
      "app_id": "cli_xxx",
      "app_secret": {"source": "file", "path": "$HOME/.lark-acp-bridge/secrets/default.appsecret"},
      "workspace": "` + filepath.ToSlash(tmp) + `"
    }
  ],
  "agent_list": [
    {"name": "traex", "command": "traex", "args": ["acp", "serve"], "default_cwd": "` + filepath.ToSlash(tmp) + `"}
  ]
}
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Bots[0].Trace.Enabled || cfg.Bots[0].Trace.RetentionDays != 7 {
		t.Fatalf("Trace = %+v, want default enabled 7d", cfg.Bots[0].Trace)
	}
}

func TestLoadTraceExplicitDisabled(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": "default",
      "app_id": "cli_xxx",
      "app_secret": {"source": "file", "path": "$HOME/.lark-acp-bridge/secrets/default.appsecret"},
      "workspace": "` + filepath.ToSlash(tmp) + `",
      "trace": {"enabled": false, "retention_days": 3}
    }
  ],
  "agent_list": [
    {"name": "traex", "command": "traex", "args": ["acp", "serve"], "default_cwd": "` + filepath.ToSlash(tmp) + `"}
  ]
}
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Bots[0].Trace.Enabled || !cfg.Bots[0].Trace.Disabled || cfg.Bots[0].Trace.RetentionDays != 3 {
		t.Fatalf("Trace = %+v, want explicit disabled 3d", cfg.Bots[0].Trace)
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
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "~/bridge/main"
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "~/go/src"
    }
  ]
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := filepath.Join(home, "go/src")
	agent, ok := cfg.Agent("traex")
	if !ok {
		t.Fatalf("missing traex agent: %#v", cfg.AgentList)
	}
	if agent.DefaultCwd != want {
		t.Fatalf("DefaultCwd = %q, want %q", agent.DefaultCwd, want)
	}
	wantWorkspace := filepath.Join(home, "bridge/main")
	if cfg.Bots[0].Workspace != wantWorkspace {
		t.Fatalf("Workspace = %q, want %q", cfg.Bots[0].Workspace, wantWorkspace)
	}
	if cfg.MissingBotConfig() {
		t.Fatalf("MissingBotConfig() = true, want false")
	}
}

func TestResolveSecretsRejectsPlaintextFileSecret(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	secretPath := filepath.Join(home, ".lark-acp-bridge", "secrets", "main.appsecret")
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(secret dir) error = %v", err)
	}
	if err := os.WriteFile(secretPath, []byte(" resolved-secret \n"), 0o600); err != nil {
		t.Fatalf("WriteFile(secret) error = %v", err)
	}

	configPath := filepath.Join(tmp, "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": "main",
      "app_id": "cli_xxx",
      "app_secret": {
        "source": "file",
        "path": "$HOME/.lark-acp-bridge/secrets/main.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/main"
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME"
    }
  ]
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MissingBotConfig() {
		t.Fatal("MissingBotConfig() = true, want false for file secret ref")
	}
	if got := cfg.Bots[0].AppSecret.RuntimeValue(); got != "" {
		t.Fatalf("RuntimeValue before ResolveSecrets = %q, want empty", got)
	}
	err = cfg.ResolveSecrets()
	if err == nil || !strings.Contains(err.Error(), "secret 文件必须是加密格式") {
		t.Fatalf("ResolveSecrets() error = %v, want encrypted format rejection", err)
	}
}

func TestLoadSupportsEncryptedFileSecretReference(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".lark-acp-bridge", "config.json")
	if err := AddBot(configPath, BotConfig{ID: "default", AppID: "cli_xxx"}, "super-secret"); err != nil {
		t.Fatalf("AddBot() error = %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if err := cfg.ResolveSecrets(); err != nil {
		t.Fatalf("ResolveSecrets() error = %v", err)
	}
	if got := cfg.Bots[0].AppSecret.RuntimeValue(); got != "super-secret" {
		t.Fatalf("RuntimeValue = %q, want super-secret", got)
	}
}

func TestResolveSecretsRejectsMissingFileSecret(t *testing.T) {
	cfg := Config{Bots: []BotConfig{{
		ID:        "bot-a",
		AppID:     "cli_xxx",
		AppSecret: FileSecret(filepath.Join(t.TempDir(), "missing")),
		Workspace: t.TempDir(),
	}}}

	err := cfg.ResolveSecrets()
	if err == nil || !strings.Contains(err.Error(), "读取 bot \"bot-a\" app_secret file secret") {
		t.Fatalf("ResolveSecrets() error = %v, want missing file error", err)
	}
}

func TestUpdateBotDriveCommentUpdatesOnlyDriveCommentField(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".lark-acp-bridge", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	raw := `{
  "bots": [
    {
      "id": "bot-a",
      "app_id": "cli_a",
      "app_secret": {
        "source": "file",
        "path": "$HOME/.lark-acp-bridge/secrets/bot-a.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/bot-a",
      "owner_open_ids": ["ou_owner"]
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME"
    }
  ]
}
`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	updated, err := UpdateBotDriveComment(configPath, "bot-a", func(cfg *DriveCommentConfig) {
		cfg.Enabled = true
		cfg.TraceEnabled = true
		cfg.TraceChatID = " oc_trace "
	})
	if err != nil {
		t.Fatalf("UpdateBotDriveComment() error = %v", err)
	}
	if !updated.Enabled || !updated.TraceEnabled || updated.TraceChatID != "oc_trace" {
		t.Fatalf("updated drive_comment = %+v, want normalized enabled trace", updated)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`"path": "$HOME/.lark-acp-bridge/secrets/bot-a.appsecret"`,
		`"workspace": "$HOME/.lark-acp-bridge/bots/bot-a"`,
		`"trace_chat_id": "oc_trace"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config = %s, want preserved/updated field %s", text, want)
		}
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Bots[0].DriveComment.Enabled || !cfg.Bots[0].DriveComment.TraceEnabled || cfg.Bots[0].DriveComment.TraceChatID != "oc_trace" {
		t.Fatalf("loaded drive_comment = %+v, want updated config", cfg.Bots[0].DriveComment)
	}
}

func TestUpdateBotDriveCommentSerializesConcurrentReadModifyWrite(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := Write(configPath, Config{Bots: []BotConfig{{
		ID:        "bot-a",
		AppID:     "cli_a",
		AppSecret: FileSecret("secret.appsecret"),
		Workspace: t.TempDir(),
	}}}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		if _, err := UpdateBotDriveComment(configPath, "bot-a", func(cfg *DriveCommentConfig) {
			cfg.Enabled = true
		}); err != nil {
			t.Errorf("UpdateBotDriveComment(enabled) error = %v", err)
		}
	}()
	go func() {
		defer wait.Done()
		<-start
		if _, err := UpdateBotDriveComment(configPath, "bot-a", func(cfg *DriveCommentConfig) {
			cfg.TraceEnabled = true
			cfg.TraceChatID = "oc_trace"
		}); err != nil {
			t.Errorf("UpdateBotDriveComment(trace) error = %v", err)
		}
	}()
	close(start)
	wait.Wait()

	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got := loaded.Bots[0].DriveComment
	if !got.Enabled || !got.TraceEnabled || got.TraceChatID != "oc_trace" {
		t.Fatalf("drive_comment = %+v, want both concurrent updates preserved", got)
	}
}

func TestUpdateBotWikiTraceUpdatesOnlyWikiTraceField(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".lark-acp-bridge", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	raw := `{
  "bots": [
    {
      "id": "bot-a",
      "app_id": "cli_a",
      "app_secret": {
        "source": "file",
        "path": "$HOME/.lark-acp-bridge/secrets/bot-a.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/bot-a"
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "default_cwd": "$HOME"
    }
  ]
}
`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	updated, err := UpdateBotWikiTrace(configPath, "bot-a", func(cfg *WikiTraceConfig) {
		cfg.Enabled = true
		cfg.ChatID = " oc_wiki "
	})
	if err != nil {
		t.Fatalf("UpdateBotWikiTrace() error = %v", err)
	}
	if !updated.Enabled || updated.ChatID != "oc_wiki" {
		t.Fatalf("updated wiki_trace = %+v, want normalized enabled", updated)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`"path": "$HOME/.lark-acp-bridge/secrets/bot-a.appsecret"`,
		`"workspace": "$HOME/.lark-acp-bridge/bots/bot-a"`,
		`"chat_id": "oc_wiki"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config = %s, want preserved/updated field %s", text, want)
		}
	}
	if strings.Contains(text, `"mode"`) {
		t.Fatalf("config = %s, want no wiki_trace mode field", text)
	}
	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := loaded.Bots[0].WikiTrace; !got.Enabled || got.ChatID != "oc_wiki" {
		t.Fatalf("loaded wiki_trace = %+v, want enabled", got)
	}
}

func TestAddBotWritesSecretFileAndConfigReference(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".lark-acp-bridge", "config.json")

	err := AddBot(configPath, BotConfig{
		ID:    "default",
		AppID: "cli_xxx",
	}, "super-secret")
	if err != nil {
		t.Fatalf("AddBot() error = %v", err)
	}

	secretPath := filepath.Join(home, ".lark-acp-bridge", "secrets", "default.appsecret")
	keyPath := filepath.Join(home, ".lark-acp-bridge", "secrets", "default.key")
	secret, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("ReadFile(secret) error = %v", err)
	}
	if strings.Contains(string(secret), "super-secret") {
		t.Fatalf("secret file leaked plaintext: %q", secret)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(secret)), encryptedSecretPrefix) {
		t.Fatalf("secret file = %q, want encrypted prefix", secret)
	}
	if info, err := os.Stat(secretPath); err != nil {
		t.Fatalf("Stat(secret) error = %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret mode = %o, want 600", info.Mode().Perm())
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("ReadFile(key) error = %v", err)
	}
	if strings.Contains(string(key), "super-secret") {
		t.Fatalf("key file leaked plaintext: %q", key)
	}
	if info, err := os.Stat(keyPath); err != nil {
		t.Fatalf("Stat(key) error = %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %o, want 600", info.Mode().Perm())
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}
	if strings.Contains(string(raw), "super-secret") {
		t.Fatalf("config leaked secret:\n%s", raw)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal(config) error = %v", err)
	}
	if len(cfg.Bots) != 1 {
		t.Fatalf("len(Bots) = %d, want 1", len(cfg.Bots))
	}
	if cfg.Bots[0].AppSecret.Source != "file" || cfg.Bots[0].AppSecret.Path != DefaultBotSecretPath("default") {
		t.Fatalf("AppSecret = %+v, want default file ref", cfg.Bots[0].AppSecret)
	}
	if err := cfg.ResolveSecrets(); err != nil {
		t.Fatalf("ResolveSecrets() error = %v", err)
	}
	if got := cfg.Bots[0].AppSecret.RuntimeValue(); got != "super-secret" {
		t.Fatalf("RuntimeValue = %q, want super-secret", got)
	}
}

func TestAddBotReplacesEmptyDefaultPlaceholder(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".lark-acp-bridge", "config.json")
	if _, err := LoadOrCreate(configPath); err != nil {
		t.Fatalf("LoadOrCreate() error = %v", err)
	}

	if err := AddBot(configPath, BotConfig{ID: "default", AppID: "cli_xxx"}, "secret"); err != nil {
		t.Fatalf("AddBot() error = %v", err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Bots) != 1 || cfg.Bots[0].AppID != "cli_xxx" {
		t.Fatalf("Bots = %+v, want replaced default bot", cfg.Bots)
	}
}

func TestRemoveBotUpdatesConfig(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".lark-acp-bridge", "config.json")
	if err := AddBot(configPath, BotConfig{ID: "bot-a", AppID: "cli_a"}, "secret-a"); err != nil {
		t.Fatalf("AddBot(bot-a) error = %v", err)
	}
	if err := AddBot(configPath, BotConfig{ID: "bot-b", AppID: "cli_b"}, "secret-b"); err != nil {
		t.Fatalf("AddBot(bot-b) error = %v", err)
	}

	removed, err := RemoveBot(configPath, "bot-a")
	if err != nil {
		t.Fatalf("RemoveBot() error = %v", err)
	}
	if !removed {
		t.Fatal("RemoveBot() removed = false, want true")
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Bots) != 1 || cfg.Bots[0].ID != "bot-b" {
		t.Fatalf("Bots = %+v, want only bot-b", cfg.Bots)
	}
}

func TestLoadMessageReaction(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
  "message_reaction": true,
  "bots": [
    {
      "id": "default",
      "app_id": "cli_xxx",
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "` + filepath.ToSlash(filepath.Join(t.TempDir(), "workspace")) + `"
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": ["acp", "serve"]
    }
  ]
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.MessageReaction {
		t.Fatal("MessageReaction = false, want true")
	}
}

func TestLoadAgentListPreservesConfiguredOrder(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(tmp, "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": "main",
      "app_id": "cli_xxx",
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/main"
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME/traex"
    },
    {
      "name": "claude",
      "command": "claude",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME/claude"
    }
  ]
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got := configAgentListNames(cfg.AgentList)
	want := []string{"traex", "claude"}
	if !slices.Equal(got, want) {
		t.Fatalf("AgentList names = %#v, want %#v", got, want)
	}
	agent, ok := cfg.Agent("claude")
	if !ok {
		t.Fatalf("missing claude agent: %#v", cfg.AgentList)
	}
	if agent.DefaultCwd != filepath.Join(home, "claude") {
		t.Fatalf("claude default cwd = %q, want expanded path", agent.DefaultCwd)
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
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/main"
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME"
    }
  ]
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
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/main",
      "owner_open_ids": [" ou_owner ", "", "ou_owner", "ou_backup"]
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME"
    }
  ]
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
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/main",
      "bot_open_id": " ou_bot "
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME"
    }
  ]
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
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/a"
    },
    {
      "id": "same",
      "app_id": "cli_b",
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/b"
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME"
    }
  ]
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
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/a"
    },
    {
      "id": "same",
      "app_id": "cli_b",
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/b"
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME"
    }
  ]
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
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/shared"
    },
    {
      "id": "bot-b",
      "app_id": "cli_b",
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "~/.lark-acp-bridge/bots/shared"
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME"
    }
  ]
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
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/default"
    }
  ],
  "agent_list": [
    {
      "name": " traex ",
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME"
    }
  ]
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := configAgentListNames(cfg.AgentList); !slices.Equal(got, []string{"traex"}) {
		t.Fatalf("AgentList names = %#v, want trimmed traex", got)
	}
}

func TestLoadTrimsAgentListName(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(tmp, "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": "default",
      "app_id": "cli_a",
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/default"
    }
  ],
  "agent_list": [
    {
      "name": " traex ",
      "command": "traex",
      "args": ["acp", "serve"],
      "default_cwd": "$HOME"
    }
  ]
}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := configAgentListNames(cfg.AgentList); !slices.Equal(got, []string{"traex"}) {
		t.Fatalf("AgentList names = %#v, want trimmed traex", got)
	}
}

func TestLoadRejectsDuplicateAgentListNameAfterTrim(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(tmp, "config.json")
	data := []byte(`{
  "bots": [
    {
      "id": "default",
      "app_id": "cli_a",
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/default"
    }
  ],
  "agent_list": [
    {
      "name": " same ",
      "command": "traex",
      "args": ["acp", "serve"]
    },
    {
      "name": "same",
      "command": "other",
      "args": ["acp", "serve"]
    }
  ]
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
	cfg := testConfigWithAgent("fake", AgentConfig{Command: command})

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
	cfg := testConfigWithAgent("fake", AgentConfig{Command: "fake-acp"})

	if err := cfg.ValidateAgentCommands(); err != nil {
		t.Fatalf("ValidateAgentCommands() error = %v", err)
	}
}

func TestFilterAvailableAgentCommandsSkipsMissingCommand(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "fake-acp")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", dir)
	cfg := Config{AgentList: []NamedAgentConfig{
		{Name: "missing", AgentConfig: AgentConfig{Command: "missing-acp-server"}},
		{Name: "fake", AgentConfig: AgentConfig{Command: "fake-acp"}},
	}}

	var stderr strings.Builder
	filtered, err := cfg.FilterAvailableAgentCommands(&stderr)
	if err != nil {
		t.Fatalf("FilterAvailableAgentCommands() error = %v", err)
	}
	if got := configAgentListNames(filtered.AgentList); !slices.Equal(got, []string{"fake"}) {
		t.Fatalf("filtered agents = %#v, want only fake", got)
	}
	if !strings.Contains(stderr.String(), `跳过不可用的 ACP agent "missing"`) || !strings.Contains(stderr.String(), `missing-acp-server`) {
		t.Fatalf("stderr = %q, want skipped missing command", stderr.String())
	}
}

func TestFilterAvailableAgentCommandsRejectsWhenAllCommandsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := testConfigWithAgent("missing", AgentConfig{Command: "missing-acp-server"})

	var stderr strings.Builder
	_, err := cfg.FilterAvailableAgentCommands(&stderr)
	if err == nil || !strings.Contains(err.Error(), "没有可用的 ACP agent") {
		t.Fatalf("FilterAvailableAgentCommands() error = %v, want no available agent", err)
	}
	if !strings.Contains(stderr.String(), `跳过不可用的 ACP agent "missing"`) {
		t.Fatalf("stderr = %q, want skipped missing command", stderr.String())
	}
}

func TestValidateAgentCommandsRejectsEmptyCommand(t *testing.T) {
	cfg := testConfigWithAgent("empty", AgentConfig{Command: " "})

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
	cfg := testConfigWithAgent("  ", AgentConfig{Command: command})

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
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "` + filepath.ToSlash(filepath.Join(t.TempDir(), "workspace")) + `"
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": ["acp", "serve"]
    }
  ]
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
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/default"
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": [
        "acp",
        "serve"
      ]
    }
  ],
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
			AppSecret    Secret   `json:"app_secret"`
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
	if got.Bots[0].AppSecret.Source != "file" || got.Bots[0].AppSecret.Path != "secret.appsecret" {
		t.Fatalf("app_secret = %+v, want original file ref", got.Bots[0].AppSecret)
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
      "app_secret": {
        "source": "file",
        "path": "secret.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/default",
      "bot_open_id": "ou_configured_bot",
      "owner_open_ids": [
        "ou_configured_owner"
      ]
    }
  ],
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": [
        "acp",
        "serve"
      ]
    }
  ]
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
	matches, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*"))
	if globErr != nil {
		t.Fatalf("Glob() error = %v", globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary config files still exist: %v", matches)
	}
}

func TestValidateAgentCommandsRejectsDirectoryPath(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfigWithAgent("dir", AgentConfig{Command: dir})

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
	cfg := testConfigWithAgent("fake", AgentConfig{Command: command})

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
	cfg := testConfigWithAgent("fake", AgentConfig{Command: command, DefaultCwd: cwd})

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
	cfg := testConfigWithAgent("fake", AgentConfig{Command: command, DefaultCwd: filepath.Join(dir, "missing")})

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
	cfg := testConfigWithAgent("fake", AgentConfig{Command: command, DefaultCwd: cwd})

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
	cfg := testConfigWithAgent("fake", AgentConfig{Command: command})
	cfg.RestartCommand = []string{command}

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
	cfg := testConfigWithAgent("fake", AgentConfig{Command: "restart-bridge"})
	cfg.RestartCommand = []string{"restart-bridge"}

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
	cfg := testConfigWithAgent("fake", AgentConfig{Command: command})
	cfg.RestartCommand = []string{"missing-restart-command"}

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
	cfg := testConfigWithAgent("fake", AgentConfig{Command: command})
	cfg.RestartCommand = []string{dir}

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
	cfg := testConfigWithAgent("fake", AgentConfig{Command: agentCommand})
	cfg.RestartCommand = []string{restartCommand}

	err := cfg.ValidateAgentCommands()
	if err == nil || !strings.Contains(err.Error(), "restart_command 启动命令不可执行") {
		t.Fatalf("ValidateAgentCommands() error = %v, want non-executable restart command", err)
	}
}
