package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
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

func main() {
	slog.SetDefault(slog.New(logging.NewCtxHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logging.ProgramLevel(),
	}))))
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), topLevelUsage())
	}
	var configPath string
	var showVersion bool
	flag.StringVar(&configPath, "config", "", "JSON 配置文件路径（默认：~/.lark-acp-bridge/config.json）")
	flag.BoolVar(&showVersion, "version", false, "打印版本号并退出")
	flag.Parse()

	if showVersion {
		fmt.Println(appVersion())
		return nil
	}

	args := flag.Args()
	if err := validateTopLevelCommand(args); err != nil {
		return reportCommandError(err)
	}
	if len(args) > 0 && args[0] == "bots" {
		return reportCommandError(runBotsCommand(configPath, args[1:]))
	}
	if len(args) > 0 && args[0] == "service" {
		return reportCommandError(runServiceCommand(configPath, args[1:]))
	}
	if len(args) > 0 && args[0] == "update" {
		return reportCommandError(runUpdateCommand(args[1:]))
	}
	if len(args) > 0 && isBotsShorthand(args[0]) {
		return reportCommandError(runBotsCommand(configPath, args))
	}

	loaded, err := config.LoadOrCreate(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败, err=%v\n", err)
		return err
	}
	if loaded.Created {
		slog.Info("已创建默认配置文件", "路径", loaded.Path)
	}
	mode := runMode(flag.Args())
	if !isDaemonChild() && mode == modeStop {
		return runDaemon(mode, loaded.Path)
	}
	loaded.Config, err = ensureInitialBotRegistered(loaded.Path, loaded.Config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "注册 default bot 失败, err=%v\n", err)
		return err
	}
	if loaded.Config.MissingBotConfig() {
		msg := fmt.Sprintf(`
未配置可用的 app_id/app_secret, 请编辑配置文件: %s
可运行 lark-acp-bridge bots register default 创建并写入默认 bot`, loaded.Path)
		fmt.Fprintln(os.Stderr, msg)
		return errors.New(msg)
	}
	if err := loaded.Config.ResolveSecrets(); err != nil {
		fmt.Fprintf(os.Stderr, "解析 bot secret 失败, err=%v\n", err)
		return err
	}
	filteredConfig, err := loaded.Config.FilterAvailableAgentCommands(os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "无法识别的acp命令失败, err=%v\n", err)
		return err
	}
	loaded.Config = filteredConfig

	if !isDaemonChild() && mode != modeRun {
		return runDaemon(mode, loaded.Path)
	}
	return runForeground(loaded.Config, loaded.Path)
}

func validateTopLevelCommand(args []string) error {
	if len(args) == 0 {
		return nil
	}
	cmd := strings.TrimSpace(args[0])
	if isRunMode(cmd) || isTopLevelSubcommand(cmd) || isBotsShorthand(cmd) {
		return nil
	}
	return fmt.Errorf("无法识别的命令: lark-acp-bridge %s\n\n%s", args[0], topLevelUsage())
}

func isTopLevelSubcommand(command string) bool {
	switch command {
	case "bots", "service", "update":
		return true
	default:
		return false
	}
}

func topLevelUsage() string {
	return `用法:
  lark-acp-bridge [--config <path>] [--version] [run|start|stop|restart]
  lark-acp-bridge [--config <path>] bots <list|add|register|create-lark-cli-profile|remove>
  lark-acp-bridge [--config <path>] service <install|uninstall>
  lark-acp-bridge update [--check] [--version <tag>] [--repo <owner/name>] [--gitee-repo <owner/name>|-] [--binary <path>]

运行模式:
  run       前台运行
  start     后台启动
  stop      停止后台服务
  restart   重启后台服务（默认）

子命令:
  bots      管理 bot 配置
  service   安装或卸载用户级服务
  update    更新 bridge 二进制（只替换不重启）

选项:
  -config string
        JSON 配置文件路径（默认：~/.lark-acp-bridge/config.json）
  -version
        打印版本号并退出
`
}

func reportCommandError(err error) error {
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
	}
	return err
}

func runBotsCommand(configPath string, args []string) error {
	path, err := configPathOrDefault(configPath)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("用法: lark-acp-bridge bots <list|add|register|create-lark-cli-profile|remove>")
	}
	switch args[0] {
	case "list":
		return runBotsList(path, args[1:])
	case "add":
		return runBotsAdd(path, args[1:])
	case "register":
		return runBotsRegister(path, args[1:])
	case "create-lark-cli-profile":
		return runBotsCreateLarkCLIProfile(path, args[1:])
	case "remove", "rm":
		return runBotsRemove(path, args[1:])
	default:
		return fmt.Errorf("未知 bots 子命令: %s", args[0])
	}
}

func isBotsShorthand(command string) bool {
	switch command {
	case "list", "add", "register", "remove", "rm":
		return true
	default:
		return false
	}
}

func configPathOrDefault(configPath string) (string, error) {
	if strings.TrimSpace(configPath) == "" {
		return config.DefaultPath()
	}
	return config.ExpandPath(configPath)
}

func runBotsList(configPath string, args []string) error {
	fs := flag.NewFlagSet("bots list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("用法: lark-acp-bridge bots list")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置失败, err=%v\n", err)
		return err
	}
	for _, bot := range cfg.Bots {
		fmt.Printf("%s\t%s\t%s\t%s\n", bot.ID, bot.AppID, bot.Workspace, bot.AppSecret.Summary())
	}
	return nil
}

func runBotsAdd(configPath string, args []string) error {
	fs := flag.NewFlagSet("bots add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stdinSecret := fs.Bool("stdin-secret", false, "从 stdin 读取 app secret")
	workspace := fs.String("workspace", "", "bot workspace 路径（默认：$HOME/.lark-acp-bridge/bots/<id>）")
	secretFile := fs.String("secret-file", "", "app secret 文件路径（默认：$HOME/.lark-acp-bridge/secrets/<id>.appsecret）")
	botOpenID := fs.String("bot-open-id", "", "bot 自己的 open_id")
	ownerOpenIDs := fs.String("owner-open-ids", "", "逗号分隔的 owner open_id 列表")
	flagArgs, positional, err := splitBotsAddArgs(args)
	if err != nil {
		return err
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(positional) != 2 {
		return fmt.Errorf("用法: lark-acp-bridge bots add <id> <app_id> --stdin-secret")
	}
	if !*stdinSecret {
		return fmt.Errorf("请使用 --stdin-secret 从 stdin 读取 app_secret")
	}
	secretData, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("读取 stdin secret: %w", err)
	}
	id := positional[0]
	bot := config.BotConfig{
		ID:           id,
		AppID:        positional[1],
		Workspace:    *workspace,
		AppSecret:    config.FileSecret(*secretFile),
		BotOpenID:    *botOpenID,
		OwnerOpenIDs: splitCSV(*ownerOpenIDs),
	}
	if strings.TrimSpace(*secretFile) == "" {
		bot.AppSecret = config.FileSecret(config.DefaultBotSecretPath(id))
	}
	if err := config.AddBot(configPath, bot, string(secretData)); err != nil {
		return err
	}
	fmt.Printf("已添加 bot %s，配置: %s\n", strings.TrimSpace(id), configPath)
	return nil
}

func runBotsRegister(configPath string, args []string) error {
	fs := flag.NewFlagSet("bots register", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	workspace := fs.String("workspace", "", "bot workspace 路径（默认：$HOME/.lark-acp-bridge/bots/<id>）")
	secretFile := fs.String("secret-file", "", "app secret 文件路径（默认：$HOME/.lark-acp-bridge/secrets/<id>.appsecret）")
	botOpenID := fs.String("bot-open-id", "", "bot 自己的 open_id")
	ownerOpenIDs := fs.String("owner-open-ids", "", "逗号分隔的 owner open_id 列表（默认使用扫码用户 open_id）")
	timeout := fs.Duration("timeout", 10*time.Minute, "等待用户完成一键创建的超时时间")
	appName := fs.String("app-name", "", "预填应用名称（用户可在创建页修改）")
	appDesc := fs.String("app-desc", "", "预填应用描述（用户可在创建页修改）")
	createOnly := fs.Bool("create-only", true, "只允许创建新应用")
	domain := fs.String("domain", "", "飞书认证域名（默认由 SDK 决定）")
	larkDomain := fs.String("lark-domain", "", "Lark 认证域名（默认由 SDK 决定）")
	flagArgs, positional, err := splitBotsRegisterArgs(args)
	if err != nil {
		return err
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("用法: lark-acp-bridge bots register <id>")
	}
	id := strings.TrimSpace(positional[0])
	if id == "" {
		return fmt.Errorf("bot id 不能为空")
	}
	if err := registerAndAddBot(configPath, botRegisterOptions{
		ID:           id,
		Workspace:    *workspace,
		SecretFile:   *secretFile,
		BotOpenID:    *botOpenID,
		OwnerOpenIDs: splitCSV(*ownerOpenIDs),
		Timeout:      *timeout,
		AppName:      *appName,
		AppDesc:      *appDesc,
		CreateOnly:   *createOnly,
		Domain:       *domain,
		LarkDomain:   *larkDomain,
	}); err != nil {
		return err
	}
	fmt.Printf("已注册并添加 bot %s，配置: %s\n", id, configPath)
	return nil
}

func runBotsCreateLarkCLIProfile(configPath string, args []string) error {
	fs := flag.NewFlagSet("bots create-lark-cli-profile", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	profile := fs.String("profile", "", "lark-cli profile 名称（默认：lark-acp-<bot-id>）")
	flagArgs, positional, err := splitBotsCreateLarkCLIProfileArgs(args)
	if err != nil {
		return err
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(positional) != 1 || fs.NArg() != 0 {
		return fmt.Errorf("用法: lark-acp-bridge bots create-lark-cli-profile <id> [--profile <name>]")
	}
	id := strings.TrimSpace(positional[0])
	if id == "" {
		return fmt.Errorf("bot id 不能为空")
	}
	profileName := strings.TrimSpace(*profile)
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

func splitBotsCreateLarkCLIProfileArgs(args []string) ([]string, []string, error) {
	valueFlags := map[string]struct{}{
		"-profile":  {},
		"--profile": {},
	}
	var flagArgs []string
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-profile=") || strings.HasPrefix(arg, "--profile=") {
			flagArgs = append(flagArgs, arg)
			continue
		}
		if _, ok := valueFlags[arg]; ok {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s 缺少参数值", arg)
			}
			flagArgs = append(flagArgs, arg, args[i+1])
			i++
			continue
		}
		positional = append(positional, arg)
	}
	return flagArgs, positional, nil
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
		CreateOnly: true,
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

func splitBotsAddArgs(args []string) ([]string, []string, error) {
	valueFlags := map[string]struct{}{
		"-workspace":       {},
		"--workspace":      {},
		"-secret-file":     {},
		"--secret-file":    {},
		"-bot-open-id":     {},
		"--bot-open-id":    {},
		"-owner-open-ids":  {},
		"--owner-open-ids": {},
	}
	boolFlags := map[string]struct{}{
		"-stdin-secret":  {},
		"--stdin-secret": {},
	}
	var flagArgs []string
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if _, ok := boolFlags[arg]; ok {
			flagArgs = append(flagArgs, arg)
			continue
		}
		if strings.HasPrefix(arg, "-workspace=") ||
			strings.HasPrefix(arg, "--workspace=") ||
			strings.HasPrefix(arg, "-secret-file=") ||
			strings.HasPrefix(arg, "--secret-file=") ||
			strings.HasPrefix(arg, "-bot-open-id=") ||
			strings.HasPrefix(arg, "--bot-open-id=") ||
			strings.HasPrefix(arg, "-owner-open-ids=") ||
			strings.HasPrefix(arg, "--owner-open-ids=") {
			flagArgs = append(flagArgs, arg)
			continue
		}
		if _, ok := valueFlags[arg]; ok {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s 缺少参数值", arg)
			}
			flagArgs = append(flagArgs, arg, args[i+1])
			i++
			continue
		}
		positional = append(positional, arg)
	}
	return flagArgs, positional, nil
}

func splitBotsRegisterArgs(args []string) ([]string, []string, error) {
	valueFlags := map[string]struct{}{
		"-workspace":       {},
		"--workspace":      {},
		"-secret-file":     {},
		"--secret-file":    {},
		"-bot-open-id":     {},
		"--bot-open-id":    {},
		"-owner-open-ids":  {},
		"--owner-open-ids": {},
		"-timeout":         {},
		"--timeout":        {},
		"-app-name":        {},
		"--app-name":       {},
		"-app-desc":        {},
		"--app-desc":       {},
		"-domain":          {},
		"--domain":         {},
		"-lark-domain":     {},
		"--lark-domain":    {},
	}
	boolFlags := map[string]struct{}{
		"-create-only":  {},
		"--create-only": {},
	}
	prefixes := []string{
		"-workspace=",
		"--workspace=",
		"-secret-file=",
		"--secret-file=",
		"-bot-open-id=",
		"--bot-open-id=",
		"-owner-open-ids=",
		"--owner-open-ids=",
		"-timeout=",
		"--timeout=",
		"-app-name=",
		"--app-name=",
		"-app-desc=",
		"--app-desc=",
		"-create-only=",
		"--create-only=",
		"-domain=",
		"--domain=",
		"-lark-domain=",
		"--lark-domain=",
	}
	var flagArgs []string
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if _, ok := boolFlags[arg]; ok {
			flagArgs = append(flagArgs, arg)
			continue
		}
		if hasAnyPrefix(arg, prefixes) {
			flagArgs = append(flagArgs, arg)
			continue
		}
		if _, ok := valueFlags[arg]; ok {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s 缺少参数值", arg)
			}
			flagArgs = append(flagArgs, arg, args[i+1])
			i++
			continue
		}
		positional = append(positional, arg)
	}
	return flagArgs, positional, nil
}

func hasAnyPrefix(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func runBotsRemove(configPath string, args []string) error {
	fs := flag.NewFlagSet("bots remove", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("用法: lark-acp-bridge bots remove <id>")
	}
	removed, err := config.RemoveBot(configPath, fs.Arg(0))
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("bot 不存在: %s", strings.TrimSpace(fs.Arg(0)))
	}
	fmt.Printf("已删除 bot %s，配置: %s\n", strings.TrimSpace(fs.Arg(0)), configPath)
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
