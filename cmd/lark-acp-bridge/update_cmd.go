package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/youthlin/lark-acp-bridge/internal/update"
)

type updateCommandOptions struct {
	CheckOnly     bool
	TargetVersion string
	Repo          string
	GiteeRepo     string
	BinaryPath    string
}

// runUpdate 处理 `update` 子命令。
func runUpdate(options updateCommandOptions) error {
	ctx, cancel := signalContext()
	defer cancel()

	opts := &update.Options{
		CurrentVersion: appVersion(),
		Repo:           strings.TrimSpace(options.Repo),
		GiteeRepo:      strings.TrimSpace(options.GiteeRepo),
		ExePath:        strings.TrimSpace(options.BinaryPath),
	}

	var rel *update.Release
	var err error
	if v := strings.TrimSpace(options.TargetVersion); v != "" {
		rel = opts.ReleaseForVersion(v)
	} else {
		fmt.Fprintln(os.Stderr, "正在查询最新版本...")
		rel, err = opts.LatestRelease(ctx)
		if err != nil {
			return err
		}
	}

	fmt.Printf("当前版本: %s\n", opts.CurrentVersion)
	fmt.Printf("目标版本: %s\n", rel.Tag)
	fmt.Printf("平台包:   %s\n", rel.AssetName)
	if len(rel.Mirrors) > 0 {
		names := make([]string, 0, len(rel.Mirrors))
		for _, m := range rel.Mirrors {
			names = append(names, m.Name)
		}
		fmt.Printf("下载源:   github（失败回退: %s）\n", strings.Join(names, ", "))
	}

	if !update.IsNewer(opts.CurrentVersion, rel.Tag) {
		fmt.Println("已是最新版本，无需更新。")
		return nil
	}
	if options.CheckOnly {
		fmt.Println("有新版本可用（--check 模式，不执行替换）。")
		return nil
	}
	if opts.ExePath == "" {
		if exe, err := os.Executable(); err == nil {
			if resolved, err2 := filepath.EvalSymlinks(exe); err2 == nil {
				exe = resolved
			}
			if looksLikeGoRunExecutable(exe) {
				return fmt.Errorf("当前可执行文件像 go run 临时文件，请先 go install ./cmd/lark-acp-bridge 后对已安装的二进制执行 update，或用 --binary 指定稳定路径")
			}
		}
	}

	fmt.Fprintf(os.Stderr, "正在下载并校验 %s ...\n", rel.AssetName)
	result, err := opts.Apply(ctx, rel)
	if err != nil {
		return err
	}
	fmt.Printf("已更新: %s -> %s（下载源: %s）\n", result.From, result.To, result.Source)
	fmt.Println("请重启 lark-acp-bridge 服务（如 systemctl --user restart lark-acp-bridge）使新版本生效。")
	return nil
}

func defaultGiteeUpdateRepo() string {
	return os.Getenv("LARK_ACP_UPDATE_GITEE_REPO")
}

// signalContext 返回一个在收到 SIGINT/SIGTERM 时取消的 context。
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}
