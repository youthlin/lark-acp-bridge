package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const appName = "lark-acp-bridge"
const encryptedSecretPrefix = "lark-acp-bridge-secret:v1:"

type Config struct {
	AgentList       []NamedAgentConfig `json:"agent_list,omitempty"`
	Bots            []BotConfig        `json:"bots"`
	RestartCommand  []string           `json:"restart_command,omitempty"`
	MessageReaction bool               `json:"message_reaction,omitempty"`
}

type NamedAgentConfig struct {
	Name string `json:"name"`
	AgentConfig
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
	AppSecret    Secret   `json:"app_secret"`
	Workspace    string   `json:"workspace"`
	BotOpenID    string   `json:"bot_open_id,omitempty"`
	OwnerOpenIDs []string `json:"owner_open_ids,omitempty"`
}

type Secret struct {
	Plain    string `json:"-"`
	Source   string `json:"source,omitempty"`
	ID       string `json:"id,omitempty"`
	Path     string `json:"path,omitempty"`
	Name     string `json:"name,omitempty"`
	resolved string
}

func PlainSecret(value string) Secret {
	return Secret{Plain: value}
}

func FileSecret(path string) Secret {
	return Secret{Source: "file", Path: path}
}

func EnvSecret(name string) Secret {
	return Secret{Source: "env", Name: name}
}

func (s Secret) MarshalJSON() ([]byte, error) {
	if strings.TrimSpace(s.Source) == "" {
		return json.Marshal(s.Plain)
	}
	type secretRef Secret
	ref := secretRef(s)
	return json.Marshal(struct {
		Source string `json:"source"`
		ID     string `json:"id,omitempty"`
		Path   string `json:"path,omitempty"`
		Name   string `json:"name,omitempty"`
	}{
		Source: strings.TrimSpace(ref.Source),
		ID:     strings.TrimSpace(ref.ID),
		Path:   strings.TrimSpace(ref.Path),
		Name:   strings.TrimSpace(ref.Name),
	})
}

func (s *Secret) UnmarshalJSON(data []byte) error {
	var plain *string
	if err := json.Unmarshal(data, &plain); err == nil {
		if plain == nil {
			*s = Secret{}
			return nil
		}
		*s = PlainSecret(*plain)
		return nil
	}

	var ref struct {
		Source string `json:"source"`
		ID     string `json:"id"`
		Path   string `json:"path"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(data, &ref); err != nil {
		return err
	}
	*s = Secret{
		Source: ref.Source,
		ID:     ref.ID,
		Path:   ref.Path,
		Name:   ref.Name,
	}
	return nil
}

func (s Secret) RuntimeValue() string {
	if strings.TrimSpace(s.resolved) != "" {
		return s.resolved
	}
	return strings.TrimSpace(s.Plain)
}

func (s Secret) IsConfigured() bool {
	if strings.TrimSpace(s.Source) != "" {
		return true
	}
	return strings.TrimSpace(s.Plain) != ""
}

func (s Secret) Summary() string {
	source := strings.TrimSpace(s.Source)
	if source == "" {
		if strings.TrimSpace(s.Plain) == "" {
			return "missing"
		}
		return "plain"
	}
	switch source {
	case "env":
		return "env:" + strings.TrimSpace(s.Name)
	case "file":
		return "file:" + strings.TrimSpace(s.Path)
	default:
		if id := strings.TrimSpace(s.ID); id != "" {
			return source + ":" + id
		}
		return source
	}
}

func (s *Secret) normalize() {
	s.Plain = strings.TrimSpace(s.Plain)
	s.Source = strings.TrimSpace(s.Source)
	s.ID = strings.TrimSpace(s.ID)
	s.Path = strings.TrimSpace(s.Path)
	s.Name = strings.TrimSpace(s.Name)
}

func (s *Secret) Resolve(label string) error {
	s.normalize()
	switch s.Source {
	case "":
		if s.Plain == "" {
			return fmt.Errorf("%s为空", label)
		}
		s.resolved = s.Plain
		return nil
	case "env":
		if s.Name == "" {
			return fmt.Errorf("%s env name 为空", label)
		}
		value, ok := os.LookupEnv(s.Name)
		if !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s 环境变量 %s 为空或不存在", label, s.Name)
		}
		s.resolved = strings.TrimSpace(value)
		return nil
	case "file":
		if s.Path == "" {
			return fmt.Errorf("%s file path 为空", label)
		}
		path, err := ExpandPath(s.Path)
		if err != nil {
			return fmt.Errorf("展开 %s file path: %w", label, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("读取 %s file secret %s: %w", label, path, err)
		}
		value, err := resolveFileSecretValue(path, data)
		if err != nil {
			return fmt.Errorf("解析 %s file secret %s: %w", label, path, err)
		}
		if value == "" {
			return fmt.Errorf("%s file secret %s 为空", label, path)
		}
		s.resolved = value
		return nil
	default:
		return fmt.Errorf("%s 不支持的 secret source: %s", label, s.Source)
	}
}

func resolveFileSecretValue(path string, data []byte) (string, error) {
	value := strings.TrimSpace(string(data))
	if value == "" || !strings.HasPrefix(value, encryptedSecretPrefix) {
		return value, nil
	}
	keyPath := secretKeyPath(path)
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("读取 key 文件 %s: %w", keyPath, err)
	}
	plain, err := decryptSecret(value, strings.TrimSpace(string(keyData)))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(plain), nil
}

func secretKeyPath(secretPath string) string {
	ext := filepath.Ext(secretPath)
	if ext == "" {
		return secretPath + ".key"
	}
	return strings.TrimSuffix(secretPath, ext) + ".key"
}

func encryptSecret(plain string) (cipherText string, keyText string, err error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", "", fmt.Errorf("生成 secret key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", fmt.Errorf("生成 secret nonce: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, []byte(plain), nil)
	payload := append(nonce, sealed...)
	return encryptedSecretPrefix + base64.StdEncoding.EncodeToString(payload),
		base64.StdEncoding.EncodeToString(key),
		nil
}

func decryptSecret(cipherText, keyText string) (string, error) {
	if !strings.HasPrefix(cipherText, encryptedSecretPrefix) {
		return strings.TrimSpace(cipherText), nil
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyText))
	if err != nil {
		return "", fmt.Errorf("解析 secret key: %w", err)
	}
	payloadText := strings.TrimPrefix(strings.TrimSpace(cipherText), encryptedSecretPrefix)
	payload, err := base64.StdEncoding.DecodeString(payloadText)
	if err != nil {
		return "", fmt.Errorf("解析 secret 密文: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("初始化 secret cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("初始化 secret gcm: %w", err)
	}
	if len(payload) < gcm.NonceSize() {
		return "", fmt.Errorf("secret 密文长度不足")
	}
	nonce := payload[:gcm.NonceSize()]
	sealed := payload[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("解密 secret: %w", err)
	}
	return string(plain), nil
}

func Default() Config {
	traex := AgentConfig{
		Command:    "traex",
		Args:       []string{"acp", "serve", "-c", "permission_mode=auto"},
		DefaultCwd: "$HOME",
	}
	return Config{
		Bots: []BotConfig{
			{
				ID:        "default",
				AppID:     "",
				AppSecret: FileSecret(DefaultBotSecretPath("default")),
				Workspace: "$HOME/." + appName + "/bots/default",
			},
		},
		AgentList: []NamedAgentConfig{
			{Name: "traex", AgentConfig: traex},
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

func DefaultBotWorkspace(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		id = "default"
	}
	return "$HOME/." + appName + "/bots/" + id
}

func DefaultBotSecretPath(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		id = "default"
	}
	return "$HOME/." + appName + "/secrets/" + id + ".appsecret"
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
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("写入配置文件: %w", err)
	}
	return nil
}

func (c *Config) ResolveSecrets() error {
	for i := range c.Bots {
		label := fmt.Sprintf("bot %q app_secret", c.Bots[i].ID)
		if err := c.Bots[i].AppSecret.Resolve(label); err != nil {
			return err
		}
	}
	return nil
}

func AddBot(path string, bot BotConfig, secret string) error {
	path, err := ExpandPath(path)
	if err != nil {
		return err
	}
	bot.ID = strings.TrimSpace(bot.ID)
	bot.AppID = strings.TrimSpace(bot.AppID)
	if bot.ID == "" {
		return fmt.Errorf("bot id 不能为空")
	}
	if bot.AppID == "" {
		return fmt.Errorf("bot %q app_id 不能为空", bot.ID)
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return fmt.Errorf("bot %q app_secret 不能为空", bot.ID)
	}
	if strings.TrimSpace(bot.Workspace) == "" {
		bot.Workspace = DefaultBotWorkspace(bot.ID)
	}
	if !bot.AppSecret.IsConfigured() {
		bot.AppSecret = FileSecret(DefaultBotSecretPath(bot.ID))
	}
	if strings.TrimSpace(bot.AppSecret.Source) != "file" {
		return fmt.Errorf("bots add 仅支持写入 file secret 引用")
	}
	if strings.TrimSpace(bot.AppSecret.Path) == "" {
		bot.AppSecret.Path = DefaultBotSecretPath(bot.ID)
	}

	cfg, err := loadForEdit(path)
	if err != nil {
		return err
	}
	replaced := false
	for i, existing := range cfg.Bots {
		if strings.TrimSpace(existing.ID) == bot.ID {
			if strings.TrimSpace(existing.AppID) != "" {
				return fmt.Errorf("bot id 已存在: %s", bot.ID)
			}
			cfg.Bots[i] = bot
			replaced = true
			break
		}
	}
	if !replaced {
		cfg.Bots = append(cfg.Bots, bot)
	}
	if err := normalize(&cfg); err != nil {
		return err
	}

	secretPath, err := ExpandPath(bot.AppSecret.Path)
	if err != nil {
		return fmt.Errorf("展开 bot %q secret path: %w", bot.ID, err)
	}
	createdSecret, createdKey, err := writeEncryptedSecretFile(secretPath, secret)
	if err != nil {
		return err
	}
	if err := Write(path, cfg); err != nil {
		if createdSecret {
			_ = os.Remove(secretPath)
		}
		if createdKey {
			_ = os.Remove(secretKeyPath(secretPath))
		}
		return err
	}
	return nil
}

func MigrateBotSecret(path, id, secretFile string) (string, error) {
	path, err := ExpandPath(path)
	if err != nil {
		return "", err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("bot id 不能为空")
	}
	cfg, err := Load(path)
	if err != nil {
		return "", err
	}
	botIndex := -1
	for i, bot := range cfg.Bots {
		if strings.TrimSpace(bot.ID) == id {
			botIndex = i
			break
		}
	}
	if botIndex < 0 {
		return "", fmt.Errorf("bot 不存在: %s", id)
	}

	secretRef := cfg.Bots[botIndex].AppSecret
	if err := secretRef.Resolve(fmt.Sprintf("bot %q app_secret", id)); err != nil {
		return "", err
	}
	secret := secretRef.RuntimeValue()
	if secret == "" {
		return "", fmt.Errorf("bot %q app_secret 为空", id)
	}
	if strings.TrimSpace(secretFile) == "" {
		secretFile = DefaultBotSecretPath(id)
	}
	cfg.Bots[botIndex].AppSecret = FileSecret(secretFile)
	if err := normalize(&cfg); err != nil {
		return "", err
	}

	secretPath, err := ExpandPath(secretFile)
	if err != nil {
		return "", fmt.Errorf("展开 bot %q secret path: %w", id, err)
	}
	createdSecret, createdKey, err := writeEncryptedSecretFile(secretPath, secret)
	if err != nil {
		return "", err
	}
	if err := Write(path, cfg); err != nil {
		if createdSecret {
			_ = os.Remove(secretPath)
		}
		if createdKey {
			_ = os.Remove(secretKeyPath(secretPath))
		}
		return "", err
	}
	return secretFile, nil
}

func writeEncryptedSecretFile(secretPath, secret string) (createdSecret bool, createdKey bool, err error) {
	if err := os.MkdirAll(filepath.Dir(secretPath), 0o700); err != nil {
		return false, false, fmt.Errorf("创建 secret 目录: %w", err)
	}
	cipherText, keyText, err := encryptSecret(secret)
	if err != nil {
		return false, false, err
	}
	keyPath := secretKeyPath(secretPath)
	_, statErr := os.Stat(secretPath)
	createdSecret = errors.Is(statErr, os.ErrNotExist)
	_, keyStatErr := os.Stat(keyPath)
	createdKey = errors.Is(keyStatErr, os.ErrNotExist)
	if err := writeFileAtomic(secretPath, []byte(cipherText+"\n"), 0o600); err != nil {
		return false, false, fmt.Errorf("写入 secret 文件: %w", err)
	}
	if err := writeFileAtomic(keyPath, []byte(keyText+"\n"), 0o600); err != nil {
		if createdSecret {
			_ = os.Remove(secretPath)
		}
		return false, false, fmt.Errorf("写入 secret key 文件: %w", err)
	}
	return createdSecret, createdKey, nil
}

func RemoveBot(path, id string) (bool, error) {
	path, err := ExpandPath(path)
	if err != nil {
		return false, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return false, fmt.Errorf("bot id 不能为空")
	}
	cfg, err := loadForEdit(path)
	if err != nil {
		return false, err
	}
	out := make([]BotConfig, 0, len(cfg.Bots))
	removed := false
	for _, bot := range cfg.Bots {
		if strings.TrimSpace(bot.ID) == id {
			removed = true
			continue
		}
		out = append(out, bot)
	}
	if !removed {
		return false, nil
	}
	cfg.Bots = out
	if len(cfg.Bots) == 0 {
		return false, fmt.Errorf("不能删除最后一个 bot")
	}
	if err := normalize(&cfg); err != nil {
		return false, err
	}
	if err := Write(path, cfg); err != nil {
		return false, err
	}
	return true, nil
}

func loadForEdit(path string) (Config, error) {
	cfg, err := Load(path)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Config{}, err
	}
	cfg = Default()
	cfg.Bots = nil
	return cfg, nil
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
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return false, fmt.Errorf("写入配置文件: %w", err)
	}
	return true, nil
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
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
			!bot.AppSecret.IsConfigured() ||
			strings.TrimSpace(bot.Workspace) == "" {
			return true
		}
	}
	return false
}

func (c Config) ValidateAgentCommands() error {
	_, err := c.FilterAvailableAgentCommands(io.Discard)
	return err
}

func (c Config) FilterAvailableAgentCommands(stderr io.Writer) (Config, error) {
	agentList, err := normalizeAgentList(c.AgentList)
	if err != nil {
		return Config{}, err
	}
	if len(agentList) == 0 {
		return Config{}, fmt.Errorf("未配置 ACP agent")
	}
	filtered := make([]NamedAgentConfig, 0, len(agentList))
	for _, named := range agentList {
		name := named.Name
		agent := named.AgentConfig
		label := fmt.Sprintf("agent %q 启动命令", name)
		if err := validateExecutableCommand(label, agent.Command); err != nil {
			if commandNotFound(err) {
				if stderr != nil {
					fmt.Fprintf(stderr, "跳过不可用的 ACP agent %q: %v\n", name, err)
				}
				continue
			}
			return Config{}, err
		}
		if strings.TrimSpace(agent.DefaultCwd) != "" {
			if err := validateDirectory(fmt.Sprintf("agent %q 默认工作目录", name), agent.DefaultCwd); err != nil {
				return Config{}, err
			}
		}
		filtered = append(filtered, named)
	}
	if len(filtered) == 0 {
		return Config{}, fmt.Errorf("没有可用的 ACP agent")
	}
	if len(c.RestartCommand) > 0 {
		if err := validateExecutableCommand("restart_command 启动命令", c.RestartCommand[0]); err != nil {
			return Config{}, err
		}
	}
	c.AgentList = filtered
	return c, nil
}

type commandNotFoundError struct {
	message string
	err     error
}

func (e commandNotFoundError) Error() string {
	return e.message
}

func (e commandNotFoundError) Unwrap() error {
	return e.err
}

func commandNotFound(err error) bool {
	var target commandNotFoundError
	return errors.As(err, &target)
}

func validateExecutableCommand(label, command string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return fmt.Errorf("%s为空", label)
	}
	if isPathCommand(command) {
		expanded, err := ExpandPath(command)
		if err != nil {
			return fmt.Errorf("展开 %s: %w", label, err)
		}
		info, err := os.Stat(expanded)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return commandNotFoundError{
					message: fmt.Sprintf("%s不可访问: %s: %v", label, expanded, err),
					err:     err,
				}
			}
			return fmt.Errorf("%s不可访问: %s: %w", label, expanded, err)
		}
		if info.IsDir() {
			return fmt.Errorf("%s是目录: %s", label, expanded)
		}
		if info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("%s不可执行: %s", label, expanded)
		}
		return nil
	}
	if _, err := exec.LookPath(command); err != nil {
		return commandNotFoundError{
			message: fmt.Sprintf("%s不存在: %s: %v", label, command, err),
			err:     err,
		}
	}
	return nil
}

func validateDirectory(label, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("%s为空", label)
	}
	expanded, err := ExpandPath(path)
	if err != nil {
		return fmt.Errorf("展开 %s: %w", label, err)
	}
	info, err := os.Stat(expanded)
	if err != nil {
		return fmt.Errorf("%s不可访问: %s: %w", label, expanded, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s不是目录: %s", label, expanded)
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
		bot.ID = strings.TrimSpace(bot.ID)
		if bot.ID == "" {
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
		bot.AppSecret.normalize()
		cfg.Bots[i] = bot
	}
	if len(cfg.AgentList) == 0 {
		cfg.AgentList = Default().AgentList
	}
	agentList, err := normalizeAgentList(cfg.AgentList)
	if err != nil {
		return err
	}
	cfg.AgentList = agentList
	cfg.RestartCommand = normalizeCommand(cfg.RestartCommand)
	for i, named := range cfg.AgentList {
		agent := named.AgentConfig
		if agent.DefaultCwd != "" {
			expanded, err := ExpandPath(agent.DefaultCwd)
			if err != nil {
				return fmt.Errorf("展开 agent %q 的默认工作目录: %w", named.Name, err)
			}
			agent.DefaultCwd = expanded
			named.AgentConfig = agent
			cfg.AgentList[i] = named
		}
	}
	return nil
}

func normalizeAgentList(agentList []NamedAgentConfig) ([]NamedAgentConfig, error) {
	if len(agentList) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(agentList))
	normalized := make([]NamedAgentConfig, 0, len(agentList))
	for _, named := range agentList {
		name := strings.TrimSpace(named.Name)
		if name == "" {
			return nil, fmt.Errorf("agent 名称不能为空")
		}
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("agent 名称重复: %s", name)
		}
		seen[name] = struct{}{}
		named.Name = name
		normalized = append(normalized, named)
	}
	return normalized, nil
}

func AgentMap(agentList []NamedAgentConfig) map[string]AgentConfig {
	if len(agentList) == 0 {
		return nil
	}
	byName := make(map[string]AgentConfig, len(agentList))
	for _, named := range agentList {
		byName[named.Name] = named.AgentConfig
	}
	return byName
}

func (c Config) Agent(name string) (AgentConfig, bool) {
	for _, named := range c.AgentList {
		if named.Name == name {
			return named.AgentConfig, true
		}
	}
	return AgentConfig{}, false
}

func (c *Config) SetAgent(name string, agent AgentConfig) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	for i, named := range c.AgentList {
		if named.Name == name {
			c.AgentList[i].AgentConfig = agent
			return
		}
	}
	c.AgentList = append(c.AgentList, NamedAgentConfig{Name: name, AgentConfig: agent})
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
