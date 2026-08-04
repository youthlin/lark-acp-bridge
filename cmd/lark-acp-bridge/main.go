package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"
	"github.com/youthlin/lark-acp-bridge/internal/bridge"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

var version string
var registerApp = registration.RegisterApp
var runLarkCLIProfileAdd = defaultRunLarkCLIProfileAdd

type botRegisterOptions struct {
	ID           string
	Workspace    string
	SecretFile   string
	BotOpenID    string
	OwnerOpenIDs []string
	Timeout      time.Duration
	AppName      string
	AppDesc      string
	CreateOnly   bool
	Domain       string
	LarkDomain   string
}

type botsAddOptions struct {
	ID           string
	AppID        string
	StdinSecret  bool
	Workspace    string
	SecretFile   string
	BotOpenID    string
	OwnerOpenIDs string
}

func main() {
	slog.SetDefault(slog.New(logging.NewCtxHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logging.ProgramLevel(),
	}))))
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	if err := execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return err
	}
	return nil
}

func runBridge(configPath, mode string) error {
	loaded, err := config.LoadOrCreate(configPath)
	if err != nil {
		return fmt.Errorf("读取配置失败, err=%v", err)
	}
	if loaded.Created {
		slog.Info("已创建默认配置文件", "路径", loaded.Path)
	}
	if !isDaemonChild() && mode == modeStop {
		return runDaemon(mode, loaded.Path)
	}
	loaded.Config, err = ensureInitialBotRegistered(loaded.Path, loaded.Config)
	if err != nil {
		return fmt.Errorf("注册 default bot 失败, err=%v", err)
	}
	if loaded.Config.MissingBotConfig() {
		msg := fmt.Sprintf(`
未配置可用的 app_id/app_secret, 请编辑配置文件: %s
可运行 lark-acp-bridge bots register default 创建并写入默认 bot`, loaded.Path)
		return errors.New(msg)
	}
	if err = loaded.Config.ResolveSecrets(); err != nil {
		return fmt.Errorf("解析 bot secret 失败, err=%v", err)
	}
	filteredConfig, err := loaded.Config.FilterAvailableAgentCommands(os.Stderr)
	if err != nil {
		return fmt.Errorf("无法识别的acp命令失败, err=%v", err)
	}
	loaded.Config = filteredConfig

	if !isDaemonChild() && mode != modeRun {
		return runDaemon(mode, loaded.Path)
	}
	return runForeground(loaded.Config, loaded.Path)
}

func configPathOrDefault(configPath string) (string, error) {
	if strings.TrimSpace(configPath) == "" {
		return config.DefaultPath()
	}
	return config.ExpandPath(configPath)
}

func runBotsList(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("读取配置失败, err=%v", err)
	}
	for _, bot := range cfg.Bots {
		fmt.Printf("%s\t%s\t%s\t%s\n", bot.ID, bot.AppID, bot.Workspace, bot.AppSecret.Summary())
	}
	return nil
}

func runBotsAdd(configPath string, options botsAddOptions) error {
	if !options.StdinSecret {
		return fmt.Errorf("请使用 --stdin-secret 从 stdin 读取 app_secret")
	}
	secretData, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("读取 stdin secret: %w", err)
	}
	id := options.ID
	bot := config.BotConfig{
		ID:           id,
		AppID:        options.AppID,
		Workspace:    options.Workspace,
		AppSecret:    config.FileSecret(options.SecretFile),
		BotOpenID:    options.BotOpenID,
		OwnerOpenIDs: splitCSV(options.OwnerOpenIDs),
	}
	if strings.TrimSpace(options.SecretFile) == "" {
		bot.AppSecret = config.FileSecret(config.DefaultBotSecretPath(id))
	}
	if err := config.AddBot(configPath, bot, string(secretData)); err != nil {
		return err
	}
	fmt.Printf("已添加 bot %s，配置: %s\n", strings.TrimSpace(id), configPath)
	return nil
}

func runBotsRegister(configPath string, options botRegisterOptions) error {
	id := strings.TrimSpace(options.ID)
	if id == "" {
		return fmt.Errorf("bot id 不能为空")
	}
	options.ID = id
	if err := registerAndAddBot(configPath, options); err != nil {
		return err
	}
	fmt.Printf("已注册并添加 bot %s，配置: %s\n", id, configPath)
	return nil
}

func runBotsCreateLarkCLIProfile(configPath, id, profile string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("bot id 不能为空")
	}
	profileName := strings.TrimSpace(profile)
	if profileName == "" {
		profileName = "lark-acp-" + id
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("读取配置: %w", err)
	}
	if err := cfg.ResolveSecrets(); err != nil {
		return fmt.Errorf("解析 bot secret: %w", err)
	}
	bot, ok := findBotByID(cfg, id)
	if !ok {
		return fmt.Errorf("未找到 bot %q", id)
	}
	appID := strings.TrimSpace(bot.AppID)
	if appID == "" {
		return fmt.Errorf("bot %q app_id 为空", id)
	}
	secret := strings.TrimSpace(bot.AppSecret.RuntimeValue())
	if secret == "" {
		return fmt.Errorf("bot %q app_secret 为空", id)
	}
	if err := runLarkCLIProfileAdd(context.Background(), profileName, appID, secret); err != nil {
		return err
	}
	fmt.Printf("已创建 lark-cli profile %s（bot: %s, app_id: %s）。\n", profileName, id, appID)
	return nil
}

func findBotByID(cfg config.Config, id string) (config.BotConfig, bool) {
	id = strings.TrimSpace(id)
	for _, bot := range cfg.Bots {
		if strings.TrimSpace(bot.ID) == id {
			return bot, true
		}
	}
	return config.BotConfig{}, false
}

func defaultRunLarkCLIProfileAdd(ctx context.Context, profile, appID, secret string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "lark-cli", "profile", "add",
		"--name", profile,
		"--app-id", appID,
		"--app-secret-stdin",
	)
	cmd.Stdin = strings.NewReader(secret + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		detail = redactSecretText(detail, secret)
		if detail != "" {
			return fmt.Errorf("创建 lark-cli profile 失败: %w: %s", err, detail)
		}
		return fmt.Errorf("创建 lark-cli profile 失败: %w", err)
	}
	return nil
}

func redactSecretText(text, secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, "[已隐藏]")
}

func ensureInitialBotRegistered(configPath string, cfg config.Config) (config.Config, error) {
	if !shouldRegisterDefaultBotOnStartup(cfg) {
		return cfg, nil
	}
	fmt.Fprintln(os.Stdout, "当前未配置可用 bot，将先注册 default bot。")
	if err := registerAndAddBot(configPath, botRegisterOptions{
		ID:         "default",
		Timeout:    10 * time.Minute,
		CreateOnly: false,
	}); err != nil {
		return config.Config{}, err
	}
	updated, err := config.Load(configPath)
	if err != nil {
		return config.Config{}, fmt.Errorf("重新读取配置: %w", err)
	}
	return updated, nil
}

func shouldRegisterDefaultBotOnStartup(cfg config.Config) bool {
	if len(cfg.Bots) == 0 {
		return true
	}
	if len(cfg.Bots) != 1 {
		return false
	}
	bot := cfg.Bots[0]
	if strings.TrimSpace(bot.ID) != "" && strings.TrimSpace(bot.ID) != "default" {
		return false
	}
	if strings.TrimSpace(bot.AppID) != "" {
		return false
	}
	if strings.TrimSpace(bot.BotOpenID) != "" || len(bot.OwnerOpenIDs) > 0 {
		return false
	}
	if !defaultBotWorkspace(bot.Workspace) {
		return false
	}
	secret := bot.AppSecret
	secretPath := strings.TrimSpace(secret.Path)
	return !secret.IsConfigured() ||
		(strings.TrimSpace(secret.Source) == "file" && (secretPath == "" || secretPath == config.DefaultBotSecretPath("default")))
}

func defaultBotWorkspace(workspace string) bool {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" || workspace == config.DefaultBotWorkspace("default") {
		return true
	}
	expanded, err := config.ExpandPath(config.DefaultBotWorkspace("default"))
	return err == nil && workspace == expanded
}

func registerAndAddBot(configPath string, options botRegisterOptions) error {
	id := strings.TrimSpace(options.ID)
	if id == "" {
		return fmt.Errorf("bot id 不能为空")
	}
	timeout := options.Timeout
	if timeout <= 0 {
		return fmt.Errorf("--timeout 必须大于 0")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var lastStatus string
	opts := &registration.Options{
		Source:     "lark-acp-bridge",
		Domain:     strings.TrimSpace(options.Domain),
		LarkDomain: strings.TrimSpace(options.LarkDomain),
		CreateOnly: options.CreateOnly,
		OnQRCode: func(info *registration.QRCodeInfo) {
			fmt.Fprintln(os.Stdout, "请在飞书或 Lark 中打开以下链接完成应用创建：")
			fmt.Fprintln(os.Stdout, strings.TrimSpace(info.URL))
			if info.ExpireIn > 0 {
				fmt.Fprintf(os.Stdout, "链接有效期：%d 秒\n", info.ExpireIn)
			}
		},
		OnStatusChange: func(info *registration.StatusChangeInfo) {
			status := strings.TrimSpace(info.Status)
			if status == "" {
				return
			}
			statusLine := registrationStatusLine(info)
			if statusLine == "" || statusLine == lastStatus {
				return
			}
			lastStatus = statusLine
			fmt.Fprintln(os.Stdout, statusLine)
		},
	}
	if strings.TrimSpace(options.AppName) != "" || strings.TrimSpace(options.AppDesc) != "" {
		opts.AppPreset = &registration.AppPreset{
			Name: strings.TrimSpace(options.AppName),
			Desc: strings.TrimSpace(options.AppDesc),
		}
	}

	result, err := registerApp(ctx, opts)
	if err != nil {
		var regErr *registration.RegisterAppError
		if errors.As(err, &regErr) {
			return fmt.Errorf("一键创建应用失败: code=%s, description=%s", regErr.Code, regErr.Description)
		}
		return fmt.Errorf("一键创建应用失败: %w", err)
	}
	if strings.TrimSpace(result.ClientID) == "" {
		return fmt.Errorf("一键创建应用未返回 app_id")
	}
	if strings.TrimSpace(result.ClientSecret) == "" {
		return fmt.Errorf("一键创建应用未返回 app_secret")
	}

	owners := append([]string(nil), options.OwnerOpenIDs...)
	if len(owners) == 0 && result.UserInfo != nil {
		if openID := strings.TrimSpace(result.UserInfo.OpenID); openID != "" {
			owners = []string{openID}
		}
	}
	bot := config.BotConfig{
		ID:           id,
		AppID:        result.ClientID,
		Workspace:    options.Workspace,
		AppSecret:    config.FileSecret(options.SecretFile),
		BotOpenID:    options.BotOpenID,
		OwnerOpenIDs: owners,
	}
	if strings.TrimSpace(options.SecretFile) == "" {
		bot.AppSecret = config.FileSecret(config.DefaultBotSecretPath(id))
	}
	if err := config.AddBot(configPath, bot, result.ClientSecret); err != nil {
		return err
	}
	return nil
}

func registrationStatusLine(info *registration.StatusChangeInfo) string {
	if info == nil {
		return ""
	}
	switch strings.TrimSpace(info.Status) {
	case registration.StatusPolling:
		return "等待用户确认应用创建..."
	case registration.StatusSlowDown:
		if info.Interval > 0 {
			return fmt.Sprintf("服务端要求降低轮询频率，下次轮询间隔：%d 秒", info.Interval)
		}
		return "服务端要求降低轮询频率"
	case registration.StatusDomainSwitched:
		return "已切换到 Lark 域名继续注册..."
	default:
		return "注册状态：" + strings.TrimSpace(info.Status)
	}
}

func runBotsRemove(configPath, id string) error {
	removed, err := config.RemoveBot(configPath, id)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("bot 不存在: %s", strings.TrimSpace(id))
	}
	fmt.Printf("已删除 bot %s，配置: %s\n", strings.TrimSpace(id), configPath)
	return nil
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func appVersion() string {
	info, ok := debug.ReadBuildInfo()
	return appVersionFromBuildInfo(version, info, ok)
}

func appVersionFromBuildInfo(injected string, info *debug.BuildInfo, ok bool) string {
	if injected = strings.TrimSpace(injected); injected != "" {
		return injected
	}
	if ok && info != nil {
		moduleVersion := strings.TrimSpace(info.Main.Version)
		revision, modified := vcsVersion(info.Settings)
		if moduleVersion != "" && moduleVersion != "(devel)" && !isPseudoVersion(moduleVersion) {
			return moduleVersion
		}
		if revision != "" {
			if len(revision) > 7 {
				revision = revision[:7]
			}
			if modified {
				revision += "-dirty"
			}
			return revision
		}
		if moduleVersion != "" && moduleVersion != "(devel)" {
			return moduleVersion
		}
	}
	return "dev"
}

func isPseudoVersion(version string) bool {
	parts := strings.Split(strings.TrimSpace(version), "-")
	if len(parts) < 3 {
		return false
	}
	rev := parts[len(parts)-1]
	if i := strings.IndexByte(rev, '+'); i >= 0 {
		rev = rev[:i]
	}
	if len(rev) < 12 {
		return false
	}
	ts := parts[len(parts)-2]
	if i := strings.LastIndexByte(ts, '.'); i >= 0 {
		ts = ts[i+1:]
	}
	if len(ts) != 14 {
		return false
	}
	for _, ch := range ts {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func vcsVersion(settings []debug.BuildSetting) (string, bool) {
	var revision string
	var modified bool
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			revision = strings.TrimSpace(setting.Value)
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	return revision, modified
}

func runForeground(cfg config.Config, configPath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc := bridge.NewService(cfg, nil).
		WithConfigPath(configPath).
		WithBuiltinRestart(isDaemonChild()).
		WithVersion(appVersion())
	if err := svc.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败, err=%v\n", err)
		return err
	}
	cleanup, err := writeDaemonPIDFile(configPath)
	if err != nil {
		_ = svc.Shutdown(context.Background())
		fmt.Fprintf(os.Stderr, "写入pid文件失败, err=%v\n", err)
		return err
	}
	defer cleanup()

	<-ctx.Done()
	if err := svc.Shutdown(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "停止服务异常, err=%v\n", err)
		return err
	}
	return nil
}
