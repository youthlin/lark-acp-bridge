package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	slog.SetDefault(slog.New(logging.NewCtxHandler(slog.NewJSONHandler(os.Stdout, nil))))
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
	if err := loaded.Config.ValidateAgentCommands(); err != nil {
		fmt.Fprintf(os.Stderr, "无法识别的acp命令失败, err=%v\n", err)
		return err
	}

	if !isDaemonChild() && mode != modeRun {
		return runDaemon(mode, loaded.Path)
	}
	return runForeground(loaded.Config, loaded.Path)
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
