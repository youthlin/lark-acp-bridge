package bridge

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
	"github.com/youthlin/lark-acp-bridge/internal/update"
)

// currentExecutable 返回当前运行的可执行文件路径，测试可覆盖。
var currentExecutable = os.Executable

// updateCommandUsage 是 /update 斜杠命令的帮助。
const updateCommandUsage = `/update [rollback] [--check] [--version <tag>] - 更新 bridge 二进制（只替换不重启）
  --check            只检查是否有新版本，不下载替换
  --version <tag>    升级到指定版本（如 v1.2.3），默认最新版本
  rollback           恢复最近一次更新前保存的备份
更新或回滚完成后需用 /restart 重启服务使目标版本生效。`

// handleUpdateCommand 处理 owner-only 的 /update 斜杠命令。
// 它复用 internal/update 完成版本发现、下载、校验和原子替换，但不会自动重启服务。
func (s *Service) handleUpdateCommand(ctx context.Context, text string, msg feishu.Message) string {
	if !s.slashCommandAllowed(msg) {
		if len(s.ownerOpenIDs(msg.BotID)) == 0 {
			return "未配置 bot owner，不能通过飞书更新 bridge。"
		}
		return "只有 bot owner 可以更新 bridge。"
	}

	args := commandRemainder(text, 1)
	fields := strings.Fields(args)
	rollback := false
	if len(fields) > 0 && fields[0] == "rollback" {
		rollback = true
		fields = fields[1:]
	}
	fs := flag.NewFlagSet("/update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	checkOnly := fs.Bool("check", false, "只检查是否有新版本，不下载替换")
	target := fs.String("version", "", "升级到指定版本（如 v1.2.3），默认最新版本")
	repo := fs.String("repo", "", "GitHub 发布仓库（形如 owner/name）")
	giteeRepo := fs.String("gitee-repo", "", `Gitee 镜像仓库（owner/name），传 "-" 禁用`)
	binary := fs.String("binary", "", "待替换的可执行文件路径（默认当前可执行文件）")
	if err := fs.Parse(fields); err != nil {
		return "参数解析失败：" + err.Error() + "\n" + updateCommandUsage
	}
	if fs.NArg() != 0 {
		return updateCommandUsage
	}
	if rollback && (*checkOnly || strings.TrimSpace(*target) != "" || strings.TrimSpace(*repo) != "" || strings.TrimSpace(*giteeRepo) != "") {
		return "rollback 只支持 --binary 参数。\n" + updateCommandUsage
	}

	current := s.version
	if strings.TrimSpace(current) == "" {
		current = "dev"
	}
	opts := &update.Options{
		CurrentVersion: current,
		Repo:           strings.TrimSpace(*repo),
		GiteeRepo:      strings.TrimSpace(*giteeRepo),
		ExePath:        strings.TrimSpace(*binary),
	}

	// 解析目标版本前先校验当前可执行文件，避免下载后才发现无法原地替换。
	if !*checkOnly {
		if err := validateUpdateExePath(opts); err != nil {
			return err.Error()
		}
	}

	// 更新涉及网络下载，异步执行，避免阻塞飞书事件处理；通过中间消息回报结果。
	s.goBackground("update-command", func() {
		if rollback {
			s.runUpdateRollbackCommand(context.WithoutCancel(ctx), msg, opts)
			return
		}
		s.runUpdateCommand(context.WithoutCancel(ctx), msg, opts, *checkOnly, strings.TrimSpace(*target))
	})
	return ""
}

// runUpdateCommand 执行实际的版本发现与替换，并把进度/结果主动回复到原消息。
func (s *Service) runUpdateCommand(ctx context.Context, msg feishu.Message, opts *update.Options, checkOnly bool, targetVersion string) {
	reply := func(text string) {
		if err := s.mustSendIntermediateReply(ctx, msg, text, "缺少 /update 结果回复发送器"); err != nil {
			slog.WarnContext(ctx, "发送 /update 回复失败", "错误", err)
		}
	}

	var rel *update.Release
	var err error
	if targetVersion != "" {
		rel = opts.ReleaseForVersion(targetVersion)
	} else {
		reply("正在查询最新版本...")
		rel, err = opts.LatestRelease(ctx)
		if err != nil {
			reply("查询最新版本失败：" + err.Error())
			return
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "当前版本: %s\n", opts.CurrentVersion)
	fmt.Fprintf(&b, "目标版本: %s\n", rel.Tag)
	fmt.Fprintf(&b, "平台包:   %s", rel.AssetName)

	if !update.IsNewer(opts.CurrentVersion, rel.Tag) {
		b.WriteString("\n已是最新版本，无需更新。")
		reply(b.String())
		return
	}
	if checkOnly {
		b.WriteString("\n有新版本可用（--check 模式，不执行替换）。")
		reply(b.String())
		return
	}

	b.WriteString("\n正在下载并校验...")
	reply(b.String())

	result, err := opts.Apply(ctx, rel)
	if err != nil {
		reply("更新失败：" + err.Error())
		return
	}
	reply(fmt.Sprintf("已更新: %s -> %s（下载源: %s）。\n已保存旧版本备份: %s\n请用 /restart 重启 bridge 服务使新版本生效。", result.From, result.To, result.Source, result.BackupPath))
}

func (s *Service) runUpdateRollbackCommand(ctx context.Context, msg feishu.Message, opts *update.Options) {
	reply := func(text string) {
		if err := s.mustSendIntermediateReply(ctx, msg, text, "缺少 /update rollback 结果回复发送器"); err != nil {
			slog.WarnContext(ctx, "发送 /update rollback 回复失败", "错误", err)
		}
	}
	result, err := opts.Rollback(ctx)
	if err != nil {
		reply("回滚失败：" + err.Error())
		return
	}
	reply(fmt.Sprintf("已回滚到最近一次备份: %s\n目标文件: %s\n校验: sha256=%s size=%d\n请用 /restart 重启 bridge 服务使回滚版本生效。", result.BackupPath, result.ExePath, result.SHA256, result.Size))
}

// validateUpdateExePath 确定待替换的可执行文件路径，并拒绝 go run 临时二进制。
func validateUpdateExePath(opts *update.Options) error {
	exe := strings.TrimSpace(opts.ExePath)
	if exe != "" {
		return nil
	}
	resolved, err := currentExecutable()
	if err != nil {
		return fmt.Errorf("定位当前可执行文件失败: %w", err)
	}
	if r, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = r
	}
	if looksLikeGoRun(resolved) {
		return fmt.Errorf("当前可执行文件像 go run 临时文件，请先 go install ./cmd/lark-acp-bridge 后对已安装的二进制执行 update，或用 --binary 指定稳定路径")
	}
	return nil
}

// looksLikeGoRun 判断路径是否是 go run 产生的临时二进制。
func looksLikeGoRun(path string) bool {
	path = filepath.ToSlash(path)
	return strings.Contains(path, "/go-build") && strings.Contains(path, "/exe/")
}
