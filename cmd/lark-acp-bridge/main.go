package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/youthlin/lark-acp-bridge/internal/bridge"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

var version = "dev"

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
		fmt.Println(version)
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

func runForeground(cfg config.Config, configPath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc := bridge.NewService(cfg, nil)
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
