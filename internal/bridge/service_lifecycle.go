package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

// Start 启动服务
func (s *Service) Start(ctx context.Context) error {
	if len(s.registry.Names()) == 0 {
		return fmt.Errorf("未配置 ACP agent")
	}
	for _, bot := range s.cfg.Bots {
		workspace := strings.TrimSpace(bot.Workspace)
		if workspace == "" {
			continue
		}
		if _, err := prepareWorkspaceLocalState(workspace); err != nil {
			return fmt.Errorf("准备 bot %q 的 workspace 本地状态: %w", bot.ID, err)
		}
	}

	// 从文件加载历史会话
	for botID, store := range s.stores {
		if store == nil {
			continue
		}
		if err := store.Load(); err != nil {
			return err
		}
		slog.Info("已加载持久化会话映射", "bot", displayBotID(botID), "数量", store.Count())
	}

	slog.Info("启动 ACP 桥接服务", "agent列表", s.registry.Names(), "bot数量", len(s.feishu))
	configChanged := false
	for i, adapter := range s.feishu {
		if err := adapter.Start(ctx); err != nil {
			return err
		}
		if i < len(s.cfg.Bots) {
			if s.syncResolvedBotConfig(i, adapter) {
				configChanged = true
			}
			s.consumeRestartAckAsync(ctx, adapter, s.cfg.Bots[i])
		}
	}
	if configChanged {
		s.persistResolvedConfig(ctx)
	}
	if err := s.loadAndStartScheduledTasks(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Service) syncResolvedBotConfig(i int, adapter *feishu.Adapter) bool {
	if adapter == nil || i < 0 || i >= len(s.cfg.Bots) {
		return false
	}
	changed := false
	if strings.TrimSpace(s.cfg.Bots[i].BotOpenID) == "" {
		if botOpenID := adapter.BotOpenID(); botOpenID != "" {
			s.cfg.Bots[i].BotOpenID = botOpenID
			changed = true
		}
	}
	if len(s.cfg.Bots[i].OwnerOpenIDs) == 0 {
		if ownerOpenIDs := adapter.OwnerOpenIDs(); len(ownerOpenIDs) > 0 {
			s.cfg.Bots[i].OwnerOpenIDs = ownerOpenIDs
			changed = true
		}
	}
	return changed
}

func (s *Service) persistResolvedConfig(ctx context.Context) {
	if strings.TrimSpace(s.configPath) == "" {
		return
	}
	wrote, err := config.WriteResolvedBotFields(s.configPath, s.cfg.Bots)
	if err != nil {
		slog.WarnContext(ctx, "写回自动解析的飞书配置失败", "错误", err)
		return
	}
	if wrote {
		slog.InfoContext(ctx, "已写回自动解析的飞书配置")
	}
}

func (s *Service) Shutdown(ctx context.Context) error {
	slog.Info("关闭 ACP 桥接服务")
	s.stopScheduledTasks()
	s.cancelAllSessionWork(ctx)
	waitCtx, cancel := context.WithTimeout(ctx, shutdownBackgroundWait)
	s.waitBackgroundShutdown(waitCtx)
	cancel()
	for _, adapter := range s.feishu {
		if err := adapter.Shutdown(ctx); err != nil {
			return err
		}
	}
	return s.runtime.Shutdown(ctx)
}

func (s *Service) consumeRestartAckAsync(ctx context.Context, adapter restartAckSender, bot config.BotConfig) {
	if adapter == nil {
		return
	}
	if strings.TrimSpace(bot.Workspace) == "" {
		return
	}
	s.goBackground("restart-ack", func() {
		ackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := consumeRestartAck(ackCtx, bot.Workspace, adapter, bot.ID); err != nil {
			slog.WarnContext(ctx, "消费重启确认消息失败", "bot", displayBotID(bot.ID), "错误", err)
		}
	})
}

func (s *Service) runRestartCommand(ctx context.Context, workspace string) {
	if err := s.executeRestartCommand(ctx); err != nil {
		removeRestartAck(workspace)
		slog.ErrorContext(ctx, "执行 bridge 重启命令失败", "错误", err)
	}
}

func (s *Service) executeRestartCommand(ctx context.Context) error {
	if s.restartCommand != nil {
		return s.restartCommand(ctx)
	}
	command := s.cfg.RestartCommand
	if len(command) == 0 {
		if !s.builtinRestart {
			return errBuiltinRestartUnavailable
		}
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("获取当前可执行文件路径: %w", err)
		}
		command = []string{exe, "restart"}
		if strings.TrimSpace(s.configPath) != "" {
			command = []string{exe, "-config", s.configPath, "restart"}
		}
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动重启命令: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("释放重启命令进程: %w", err)
	}
	return nil
}

var errBuiltinRestartUnavailable = errors.New("当前进程不是内置后台 daemon，未配置 restart_command，不能通过飞书重启；请配置 restart_command 交给 systemd 或进程管理器重启")
