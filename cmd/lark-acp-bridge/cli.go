package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type rootOptions struct {
	ConfigPath  string
	ShowVersion bool
}

var daemonChild bool

func execute(args []string) error {
	if err := validateTopLevelArgs(args); err != nil {
		return err
	}
	cmd := newRootCommand()
	cmd.SetArgs(args)
	return cmd.Execute()
}

func newRootCommand() *cobra.Command {
	options := &rootOptions{}
	cmd := &cobra.Command{
		Use:           "lark-acp-bridge",
		Short:         "连接飞书消息和本地 ACP Agent",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if options.ShowVersion {
				fmt.Fprintln(cmd.OutOrStdout(), appVersion())
				return nil
			}
			return runBridge(options.ConfigPath, runMode(nil))
		},
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.PersistentFlags().StringVar(&options.ConfigPath, "config", "", "JSON 配置文件路径（默认：~/.lark-acp-bridge/config.json）")
	cmd.Flags().BoolVar(&options.ShowVersion, "version", false, "打印版本号并退出")
	cmd.PersistentFlags().BoolVar(&daemonChild, "daemon-child", false, "内部参数：后台子进程模式")
	_ = cmd.PersistentFlags().MarkHidden("daemon-child")

	for _, mode := range []string{modeRun, modeStart, modeStop, modeRestart} {
		cmd.AddCommand(newRunModeCommand(options, mode))
	}
	cmd.AddCommand(newBotsCommand(options, false))
	cmd.AddCommand(newServiceCommand(options))
	cmd.AddCommand(newUpdateCommand())
	return cmd
}

func newRunModeCommand(options *rootOptions, mode string) *cobra.Command {
	return &cobra.Command{
		Use:          mode,
		Short:        runModeDescription(mode),
		Args:         noArgsUsage("用法: lark-acp-bridge " + mode),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBridge(options.ConfigPath, mode)
		},
	}
}

func runModeDescription(mode string) string {
	switch mode {
	case modeRun:
		return "前台运行"
	case modeStart:
		return "后台启动"
	case modeStop:
		return "停止后台服务"
	case modeRestart:
		return "重启后台服务"
	default:
		return ""
	}
}

func newBotsCommand(options *rootOptions, hidden bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "bots",
		Short:        "管理 bot 配置",
		Args:         noArgsUsage("用法: lark-acp-bridge bots <list|add|register|create-lark-cli-profile|remove>"),
		SilenceUsage: true,
		Hidden:       hidden,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("用法: lark-acp-bridge bots <list|add|register|create-lark-cli-profile|remove>")
		},
	}
	cmd.AddCommand(newBotsListCommand(options, false))
	cmd.AddCommand(newBotsAddCommand(options, false))
	cmd.AddCommand(newBotsRegisterCommand(options, false))
	cmd.AddCommand(newBotsCreateLarkCLIProfileCommand(options))
	cmd.AddCommand(newBotsRemoveCommand(options, false, "remove", nil))
	return cmd
}

func newBotsListCommand(options *rootOptions, hidden bool) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "list",
		Short:        "列出 bot 配置",
		Args:         noArgsUsage("用法: lark-acp-bridge bots list"),
		SilenceUsage: true,
		Hidden:       hidden,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := configPathOrDefault(options.ConfigPath)
			if err != nil {
				return err
			}
			return runBotsList(path)
		},
	}
	return cmd
}

func newBotsAddCommand(options *rootOptions, hidden bool) *cobra.Command {
	var addOptions botsAddOptions
	cmd := &cobra.Command{
		Use:          "add <id> <app_id>",
		Short:        "添加 bot 配置",
		Args:         exactArgsUsage(2, "用法: lark-acp-bridge bots add <id> <app_id> --stdin-secret"),
		SilenceUsage: true,
		Hidden:       hidden,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := configPathOrDefault(options.ConfigPath)
			if err != nil {
				return err
			}
			addOptions.ID = args[0]
			addOptions.AppID = args[1]
			return runBotsAdd(path, addOptions)
		},
	}
	cmd.Flags().BoolVar(&addOptions.StdinSecret, "stdin-secret", false, "从 stdin 读取 app secret")
	cmd.Flags().StringVar(&addOptions.Workspace, "workspace", "", "bot workspace 路径（默认：$HOME/.lark-acp-bridge/bots/<id>）")
	cmd.Flags().StringVar(&addOptions.SecretFile, "secret-file", "", "app secret 文件路径（默认：$HOME/.lark-acp-bridge/secrets/<id>.appsecret）")
	cmd.Flags().StringVar(&addOptions.BotOpenID, "bot-open-id", "", "bot 自己的 open_id")
	cmd.Flags().StringVar(&addOptions.OwnerOpenIDs, "owner-open-ids", "", "逗号分隔的 owner open_id 列表")
	return cmd
}

func newBotsRegisterCommand(options *rootOptions, hidden bool) *cobra.Command {
	registerOptions := botRegisterOptions{Timeout: 10 * time.Minute, CreateOnly: true}
	var ownerOpenIDs string
	cmd := &cobra.Command{
		Use:          "register <id>",
		Short:        "一键创建并添加 bot",
		Args:         exactArgsUsage(1, "用法: lark-acp-bridge bots register <id>"),
		SilenceUsage: true,
		Hidden:       hidden,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := configPathOrDefault(options.ConfigPath)
			if err != nil {
				return err
			}
			registerOptions.ID = args[0]
			registerOptions.OwnerOpenIDs = splitCSV(ownerOpenIDs)
			return runBotsRegister(path, registerOptions)
		},
	}
	cmd.Flags().StringVar(&registerOptions.Workspace, "workspace", "", "bot workspace 路径（默认：$HOME/.lark-acp-bridge/bots/<id>）")
	cmd.Flags().StringVar(&registerOptions.SecretFile, "secret-file", "", "app secret 文件路径（默认：$HOME/.lark-acp-bridge/secrets/<id>.appsecret）")
	cmd.Flags().StringVar(&registerOptions.BotOpenID, "bot-open-id", "", "bot 自己的 open_id")
	cmd.Flags().StringVar(&ownerOpenIDs, "owner-open-ids", "", "逗号分隔的 owner open_id 列表（默认使用扫码用户 open_id）")
	cmd.Flags().DurationVar(&registerOptions.Timeout, "timeout", 10*time.Minute, "等待用户完成一键创建的超时时间")
	cmd.Flags().StringVar(&registerOptions.AppName, "app-name", "", "预填应用名称（用户可在创建页修改）")
	cmd.Flags().StringVar(&registerOptions.AppDesc, "app-desc", "", "预填应用描述（用户可在创建页修改）")
	cmd.Flags().BoolVar(&registerOptions.CreateOnly, "create-only", true, "只允许创建新应用")
	cmd.Flags().StringVar(&registerOptions.Domain, "domain", "", "飞书认证域名（默认由 SDK 决定）")
	cmd.Flags().StringVar(&registerOptions.LarkDomain, "lark-domain", "", "Lark 认证域名（默认由 SDK 决定）")
	return cmd
}

func newBotsCreateLarkCLIProfileCommand(options *rootOptions) *cobra.Command {
	var profile string
	cmd := &cobra.Command{
		Use:          "create-lark-cli-profile <id>",
		Short:        "为 bot 创建 lark-cli profile",
		Args:         exactArgsUsage(1, "用法: lark-acp-bridge bots create-lark-cli-profile <id> [--profile <name>]"),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := configPathOrDefault(options.ConfigPath)
			if err != nil {
				return err
			}
			return runBotsCreateLarkCLIProfile(path, args[0], profile)
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "lark-cli profile 名称（默认：lark-acp-<bot-id>）")
	return cmd
}

func newBotsRemoveCommand(options *rootOptions, hidden bool, use string, aliases []string) *cobra.Command {
	return &cobra.Command{
		Use:          use + " <id>",
		Aliases:      aliases,
		Short:        "删除 bot 配置",
		Args:         exactArgsUsage(1, "用法: lark-acp-bridge bots remove <id>"),
		SilenceUsage: true,
		Hidden:       hidden,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := configPathOrDefault(options.ConfigPath)
			if err != nil {
				return err
			}
			return runBotsRemove(path, args[0])
		},
	}
}

func newServiceCommand(options *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "service",
		Short:        "安装或卸载用户级服务",
		Args:         noArgsUsage("用法: lark-acp-bridge service <install|uninstall>"),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("用法: lark-acp-bridge service <install|uninstall>")
		},
	}
	cmd.AddCommand(newServiceInstallCommand(options))
	cmd.AddCommand(newServiceUninstallCommand())
	return cmd
}

func newServiceInstallCommand(options *rootOptions) *cobra.Command {
	installOptions := serviceInstallOptions{}
	cmd := &cobra.Command{
		Use:          "install",
		Short:        "安装用户级服务",
		Args:         noArgsUsage("用法: lark-acp-bridge service install [--binary <path>] [--working-dir <dir>] [--path <PATH>]"),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			installOptions.ConfigPath = options.ConfigPath
			return installService(installOptions)
		},
	}
	cmd.Flags().StringVar(&installOptions.BinaryPath, "binary", "", "bridge 可执行文件路径（默认：当前可执行文件）")
	cmd.Flags().StringVar(&installOptions.WorkingDir, "working-dir", "", "服务工作目录（默认：用户主目录）")
	cmd.Flags().StringVar(&installOptions.Path, "path", "", "服务进程 PATH（默认：当前 PATH 去除临时目录后的稳定部分）")
	return cmd
}

func newServiceUninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "uninstall",
		Short:        "卸载用户级服务",
		Args:         noArgsUsage("用法: lark-acp-bridge service uninstall"),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return uninstallService("")
		},
	}
}

func newUpdateCommand() *cobra.Command {
	options := updateCommandOptions{GiteeRepo: defaultGiteeUpdateRepo()}
	cmd := &cobra.Command{
		Use:          "update [rollback]",
		Short:        "更新 bridge 二进制（只替换不重启）",
		Args:         updateArgsUsage(&options),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(options)
		},
	}
	cmd.Flags().BoolVar(&options.CheckOnly, "check", false, "只检查是否有新版本，不下载替换")
	cmd.Flags().StringVar(&options.TargetVersion, "version", "", "升级到指定版本（如 v1.2.3），默认最新版本")
	cmd.Flags().StringVar(&options.Repo, "repo", "", "GitHub 发布仓库（形如 owner/name，默认 youthlin/lark-acp-bridge）")
	cmd.Flags().StringVar(&options.GiteeRepo, "gitee-repo", defaultGiteeUpdateRepo(), "Gitee 镜像仓库（owner/name），GitHub 下载失败时回退；传 \"-\" 禁用")
	cmd.Flags().StringVar(&options.BinaryPath, "binary", "", "待替换的可执行文件路径（默认当前可执行文件）")
	return cmd
}

func updateArgsUsage(options *updateCommandOptions) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		if len(args) == 1 && args[0] == "rollback" {
			options.Rollback = true
			return nil
		}
		return errors.New("用法: lark-acp-bridge update [rollback] [--check] [--version <tag>] [--repo <owner/name>] [--gitee-repo <owner/name>|-] [--binary <path>]")
	}
}

func noArgsUsage(usage string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != 0 {
			return errors.New(usage)
		}
		return nil
	}
}

func exactArgsUsage(count int, usage string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != count {
			return errors.New(usage)
		}
		return nil
	}
}

func validateTopLevelArgs(args []string) error {
	command := firstTopLevelCommandArg(args)
	if command == "" || isRunMode(command) || isTopLevelSubcommand(command) {
		return nil
	}
	return fmt.Errorf("无法识别的命令: lark-acp-bridge %s", command)
}

func firstTopLevelCommandArg(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
		name, hasInlineValue := splitFlagName(arg)
		switch name {
		case "config":
			if !hasInlineValue {
				i++
			}
		case "version", "daemon-child", "help", "h":
		default:
			return ""
		}
	}
	return ""
}

func splitFlagName(arg string) (string, bool) {
	arg = strings.TrimLeft(arg, "-")
	if i := strings.IndexByte(arg, '='); i >= 0 {
		return arg[:i], true
	}
	return arg, false
}

func isTopLevelSubcommand(command string) bool {
	switch command {
	case "bots", "service", "update":
		return true
	default:
		return false
	}
}
