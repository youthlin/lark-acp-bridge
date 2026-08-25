package bridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func (s *Service) handleTraceCommand(ctx context.Context, text string, msg feishu.Message) string {
	if !s.slashCommandAllowed(msg) {
		if len(s.ownerOpenIDs(msg.BotID)) == 0 {
			return "未配置 bot owner，不能管理 trace。"
		}
		return "只有 bot owner 可以管理 trace。"
	}
	fields := strings.Fields(text)
	if len(fields) == 1 || len(fields) == 2 && strings.EqualFold(fields[1], "status") {
		return s.formatTraceStatus(msg.BotID)
	}
	switch strings.ToLower(strings.TrimSpace(fields[1])) {
	case "on":
		retentionDays := 0
		if len(fields) > 3 {
			return traceCommandUsage()
		}
		if len(fields) == 3 {
			days, ok := parseTraceRetentionDays(fields[2])
			if !ok {
				return "保留期格式不正确，请使用天数或 duration，例如 /trace on 7d。"
			}
			retentionDays = days
		}
		return s.updateTraceConfig(func(cfg *config.TraceConfig) {
			cfg.Enabled = true
			cfg.Disabled = false
			if retentionDays > 0 {
				cfg.RetentionDays = retentionDays
			}
		}, msg.BotID, "已开启 ACP trace。\n")
	case "off":
		if len(fields) != 2 {
			return traceCommandUsage()
		}
		return s.updateTraceConfig(func(cfg *config.TraceConfig) {
			cfg.Enabled = false
			cfg.Disabled = true
		}, msg.BotID, "已关闭 ACP trace。\n")
	default:
		return traceCommandUsage()
	}
}

func traceCommandUsage() string {
	return "请使用 /trace、/trace on [7d] 或 /trace off。"
}

func parseTraceRetentionDays(value string) (int, bool) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, false
	}
	if strings.HasSuffix(value, "d") {
		days, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(value, "d")))
		return days, err == nil && days > 0
	}
	if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
		days := int(duration.Hours() / 24)
		if days <= 0 || duration%(24*time.Hour) != 0 {
			return 0, false
		}
		return days, true
	}
	days, err := strconv.Atoi(value)
	return days, err == nil && days > 0
}

func (s *Service) updateTraceConfig(update func(*config.TraceConfig), botID string, prefix string) string {
	if strings.TrimSpace(s.configPath) == "" {
		return "写入 trace 配置失败：当前 bridge 没有配置文件路径，不能修改 trace 配置。"
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	index := botConfigIndex(s.cfg.Bots, botID)
	if index < 0 {
		return "写入 trace 配置失败：未找到当前 bot 配置。"
	}
	updated, err := config.UpdateBotTrace(s.configPath, s.cfg.Bots[index].ID, update)
	if err != nil {
		return "写入 trace 配置失败：" + err.Error()
	}
	s.cfg.Bots[index].Trace = updated
	bot := cloneBotConfig(s.cfg.Bots[index])
	s.setTraceStore(bot.ID, newTraceStore(bot.Workspace, updated))
	return prefix + formatTraceStatus(bot)
}

func (s *Service) formatTraceStatus(botID string) string {
	bot, ok := s.botConfig(botID)
	if !ok {
		return "未找到当前 bot 配置。"
	}
	return formatTraceStatus(bot)
}

func formatTraceStatus(bot config.BotConfig) string {
	cfg := effectiveTraceConfig(bot.Trace)
	lines := []string{
		"ACP trace：" + onOffStatus(cfg.Enabled),
		fmt.Sprintf("保留期：%dd", cfg.RetentionDays),
	}
	if strings.TrimSpace(bot.Workspace) != "" {
		lines = append(lines, "目录："+workspaceLocalPath(bot.Workspace, traceDirName))
	}
	return strings.Join(lines, "\n")
}
