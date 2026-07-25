package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const appName = "lark-acp-bridge"

type Config struct {
	Agents         map[string]AgentConfig `json:"agents"`
	Bots           []BotConfig            `json:"bots"`
	RestartCommand []string               `json:"restart_command,omitempty"`
}

type AgentConfig struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args"`
	Env        map[string]string `json:"env,omitempty"`
	DefaultCwd string            `json:"default_cwd,omitempty"`
}

type BotConfig struct {
	ID           string   `json:"id"`
	AppID        string   `json:"app_id"`
	AppSecret    string   `json:"app_secret"`
	Workspace    string   `json:"workspace"`
	BotOpenID    string   `json:"bot_open_id,omitempty"`
	OwnerOpenIDs []string `json:"owner_open_ids,omitempty"`
}

func Default() Config {
	return Config{
		Bots: []BotConfig{
			{
				ID:        "default",
				AppID:     "",
				AppSecret: "",
				Workspace: "$HOME/." + appName + "/bots/default",
			},
		},
		Agents: map[string]AgentConfig{
			"traex": {
				Command:    "traex",
				Args:       []string{"acp", "serve", "-c", "permission_mode=auto"},
				DefaultCwd: "$HOME",
			},
		},
	}
}

type LoadResult struct {
	Config  Config
	Path    string
	Created bool
}

func DefaultPath() (string, error) {
	dir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func DataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("查找用户主目录: %w", err)
	}
	return filepath.Join(home, "."+appName), nil
}

func LoadOrCreate(path string) (LoadResult, error) {
	if path == "" {
		defaultPath, err := DefaultPath()
		if err != nil {
			return LoadResult{}, err
		}
		path = defaultPath
	}
	path, err := ExpandPath(path)
	if err != nil {
		return LoadResult{}, err
	}

	cfg, err := Load(path)
	if err == nil {
		return LoadResult{Config: cfg, Path: path}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return LoadResult{}, err
	}

	cfg = Default()
	if err := Write(path, cfg); err != nil {
		return LoadResult{}, err
	}
	if err := normalize(&cfg); err != nil {
		return LoadResult{}, err
	}
	return LoadResult{Config: cfg, Path: path, Created: true}, nil
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		if err := normalize(&cfg); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}

	path, err := ExpandPath(path)
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置文件: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("解析配置文件: %w", err)
	}
	if err := normalize(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Write(path string, cfg Config) error {
	path, err := ExpandPath(path)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("编码配置文件: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建配置目录: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("写入配置文件: %w", err)
	}
	return nil
}

func WriteResolvedBotFields(path string, bots []BotConfig) (bool, error) {
	path, err := ExpandPath(path)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("读取配置文件: %w", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return false, fmt.Errorf("解析配置文件: %w", err)
	}
	var rawBots []map[string]json.RawMessage
	if err := json.Unmarshal(root["bots"], &rawBots); err != nil {
		return false, fmt.Errorf("解析配置 bots: %w", err)
	}

	resolved := make(map[string]BotConfig, len(bots))
	for _, bot := range bots {
		bot.ID = strings.TrimSpace(bot.ID)
		if bot.ID == "" {
			continue
		}
		bot.BotOpenID = strings.TrimSpace(bot.BotOpenID)
		bot.OwnerOpenIDs = normalizeOpenIDs(bot.OwnerOpenIDs)
		if bot.BotOpenID == "" && len(bot.OwnerOpenIDs) == 0 {
			continue
		}
		resolved[bot.ID] = bot
	}

	changed := false
	for _, rawBot := range rawBots {
		id, ok := rawString(rawBot["id"])
		if !ok {
			continue
		}
		bot, ok := resolved[id]
		if !ok {
			continue
		}
		if bot.BotOpenID != "" && rawStringEmpty(rawBot["bot_open_id"]) {
			raw, err := json.Marshal(bot.BotOpenID)
			if err != nil {
				return false, fmt.Errorf("编码 bot_open_id: %w", err)
			}
			rawBot["bot_open_id"] = raw
			changed = true
		}
		if len(bot.OwnerOpenIDs) > 0 && rawOpenIDsEmpty(rawBot["owner_open_ids"]) {
			raw, err := json.Marshal(bot.OwnerOpenIDs)
			if err != nil {
				return false, fmt.Errorf("编码 owner_open_ids: %w", err)
			}
			rawBot["owner_open_ids"] = raw
			changed = true
		}
	}
	if !changed {
		return false, nil
	}

	raw, err := json.Marshal(rawBots)
	if err != nil {
		return false, fmt.Errorf("编码配置 bots: %w", err)
	}
	root["bots"] = raw
	data, err = json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, fmt.Errorf("编码配置文件: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("创建配置目录: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return false, fmt.Errorf("写入配置文件: %w", err)
	}
	return true, nil
}

func rawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	if value == nil {
		return "", true
	}
	return strings.TrimSpace(*value), true
}

func rawStringEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	value, ok := rawString(raw)
	return ok && value == ""
}

func rawOpenIDsEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return false
	}
	return len(normalizeOpenIDs(ids)) == 0
}

func ExpandPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("查找用户主目录: %w", err)
	}
	if path == "~" {
		path = home
	} else if strings.HasPrefix(path, "~/") {
		path = filepath.Join(home, path[2:])
	}
	path = os.ExpandEnv(path)
	return filepath.Clean(path), nil
}

func (c Config) MissingBotConfig() bool {
	if len(c.Bots) == 0 {
		return true
	}
	for _, bot := range c.Bots {
		if strings.TrimSpace(bot.AppID) == "" ||
			strings.TrimSpace(bot.AppSecret) == "" ||
			strings.TrimSpace(bot.Workspace) == "" {
			return true
		}
	}
	return false
}

func (c Config) ValidateAgentCommands() error {
	if len(c.Agents) == 0 {
		return fmt.Errorf("未配置 ACP agent")
	}
	for name, agent := range c.Agents {
		command := strings.TrimSpace(agent.Command)
		if command == "" {
			return fmt.Errorf("agent %q 启动命令为空", name)
		}
		if isPathCommand(command) {
			expanded, err := ExpandPath(command)
			if err != nil {
				return fmt.Errorf("展开 agent %q 启动命令: %w", name, err)
			}
			info, err := os.Stat(expanded)
			if err != nil {
				return fmt.Errorf("agent %q 启动命令不可访问: %s: %w", name, expanded, err)
			}
			if info.IsDir() {
				return fmt.Errorf("agent %q 启动命令是目录: %s", name, expanded)
			}
			if info.Mode().Perm()&0o111 == 0 {
				return fmt.Errorf("agent %q 启动命令不可执行: %s", name, expanded)
			}
			continue
		}
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Errorf("agent %q 启动命令不存在: %s: %w", name, command, err)
		}
	}
	if len(c.RestartCommand) > 0 && strings.TrimSpace(c.RestartCommand[0]) == "" {
		return fmt.Errorf("restart_command 启动命令为空")
	}
	return nil
}

func isPathCommand(command string) bool {
	return strings.ContainsAny(command, `/\`)
}

func normalize(cfg *Config) error {
	if len(cfg.Bots) == 0 {
		cfg.Bots = Default().Bots
	}
	seenBotIDs := make(map[string]struct{}, len(cfg.Bots))
	seenWorkspaces := make(map[string]string, len(cfg.Bots))
	for i, bot := range cfg.Bots {
		if strings.TrimSpace(bot.ID) == "" {
			bot.ID = fmt.Sprintf("bot-%d", i+1)
		}
		if _, ok := seenBotIDs[bot.ID]; ok {
			return fmt.Errorf("bot id 重复: %s", bot.ID)
		}
		seenBotIDs[bot.ID] = struct{}{}
		if strings.TrimSpace(bot.Workspace) != "" {
			expanded, err := ExpandPath(bot.Workspace)
			if err != nil {
				return fmt.Errorf("展开 bot %q 的 workspace: %w", bot.ID, err)
			}
			bot.Workspace = expanded
		}
		if strings.TrimSpace(bot.Workspace) != "" {
			if existingBotID, ok := seenWorkspaces[bot.Workspace]; ok {
				return fmt.Errorf("bot workspace 重复: %s 和 %s 使用 %s", existingBotID, bot.ID, bot.Workspace)
			}
			seenWorkspaces[bot.Workspace] = bot.ID
			if err := os.MkdirAll(bot.Workspace, 0o755); err != nil {
				return fmt.Errorf("创建 bot %q 的 workspace: %w", bot.ID, err)
			}
		} else {
			return fmt.Errorf("bot %q 的 workspace 不能为空", bot.ID)
		}
		bot.BotOpenID = strings.TrimSpace(bot.BotOpenID)
		bot.OwnerOpenIDs = normalizeOpenIDs(bot.OwnerOpenIDs)
		cfg.Bots[i] = bot
	}
	if len(cfg.Agents) == 0 {
		cfg.Agents = Default().Agents
	}
	cfg.RestartCommand = normalizeCommand(cfg.RestartCommand)
	for name, agent := range cfg.Agents {
		if agent.DefaultCwd != "" {
			expanded, err := ExpandPath(agent.DefaultCwd)
			if err != nil {
				return fmt.Errorf("展开 agent %q 的默认工作目录: %w", name, err)
			}
			agent.DefaultCwd = expanded
			cfg.Agents[name] = agent
		}
	}
	return nil
}

func normalizeOpenIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeCommand(command []string) []string {
	if len(command) == 0 {
		return nil
	}
	out := make([]string, 0, len(command))
	for _, part := range command {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if len(out) == 0 && isPathCommand(part) {
			if expanded, err := ExpandPath(part); err == nil {
				part = expanded
			}
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
