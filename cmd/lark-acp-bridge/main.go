package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/youthlin/lark-acp-bridge/internal/bridge"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

var version string

func main() {
	slog.SetDefault(slog.New(logging.NewCtxHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logging.ProgramLevel(),
	}))))
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	var configPath string
	var showVersion bool
	flag.StringVar(&configPath, "config", "", "JSON 配置文件路径（默认：~/.lark-acp-bridge/config.json）")
	flag.BoolVar(&showVersion, "version", false, "打印版本号并退出")
	flag.Parse()

	if showVersion {
		fmt.Println(appVersion())
		return nil
	}

	if args := flag.Args(); len(args) > 0 && args[0] == "bots" {
		return runBotsCommand(configPath, args[1:])
	}
	if args := flag.Args(); len(args) > 0 && isBotsShorthand(args[0]) {
		return runBotsCommand(configPath, args)
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
	if loaded.Config.MissingBotConfig() {
		msg := fmt.Sprintf(`
未配置 app_id/app_secret, 请编辑配置文件: %s
可访问 https://open.larkoffice.com/page/launcher 创建飞书智能体`, loaded.Path)
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

func runBotsCommand(configPath string, args []string) error {
	path, err := configPathOrDefault(configPath)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("用法: lark-acp-bridge bots <list|add|migrate-secret|remove>")
	}
	switch args[0] {
	case "list":
		return runBotsList(path, args[1:])
	case "add":
		return runBotsAdd(path, args[1:])
	case "migrate-secret":
		return runBotsMigrateSecret(path, args[1:])
	case "remove", "rm":
		return runBotsRemove(path, args[1:])
	default:
		return fmt.Errorf("未知 bots 子命令: %s", args[0])
	}
}

func isBotsShorthand(command string) bool {
	switch command {
	case "list", "add", "migrate-secret", "remove", "rm":
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

func runBotsMigrateSecret(configPath string, args []string) error {
	fs := flag.NewFlagSet("bots migrate-secret", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	secretFile := fs.String("secret-file", "", "迁移后的 app secret 文件路径（默认：$HOME/.lark-acp-bridge/secrets/<id>.appsecret）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("用法: lark-acp-bridge bots migrate-secret <id>")
	}
	path, err := config.MigrateBotSecret(configPath, fs.Arg(0), *secretFile)
	if err != nil {
		return err
	}
	fmt.Printf("已迁移 bot %s 的 app_secret 到加密文件引用: %s\n", strings.TrimSpace(fs.Arg(0)), path)
	return nil
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
		WithBuiltinRestart(isDaemonChild())
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
