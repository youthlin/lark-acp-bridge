package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func (s *Service) handleDriveCommentCommand(ctx context.Context, text string, msg feishu.Message) string {
	if !s.slashCommandAllowed(msg) {
		if len(s.ownerOpenIDs(msg.BotID)) == 0 {
			return "未配置 bot owner，不能管理云文档评论处理。"
		}
		return "只有 bot owner 可以管理云文档评论处理。"
	}
	fields := strings.Fields(text)
	if len(fields) == 1 || len(fields) == 2 && strings.EqualFold(fields[1], "status") {
		return s.formatDriveCommentStatus(msg.BotID)
	}
	switch strings.ToLower(strings.TrimSpace(fields[1])) {
	case "on":
		return s.updateDriveCommentConfig(func(cfg *config.DriveCommentConfig) {
			cfg.Enabled = true
		}, msg.BotID, "已开启云文档评论监听处理。\n")
	case "off":
		return s.updateDriveCommentConfig(func(cfg *config.DriveCommentConfig) {
			cfg.Enabled = false
		}, msg.BotID, "已关闭云文档评论监听处理。已有处理过程群里的对话仍可继续。\n")
	case "trace":
		if len(fields) != 3 {
			return "请使用 /drive_comment trace on、/drive_comment trace off 或 /drive_comment trace new。"
		}
		switch strings.ToLower(strings.TrimSpace(fields[2])) {
		case "on":
			if strings.TrimSpace(msg.ChatID) == "" {
				return "当前消息缺少 chat_id，不能设置为 trace 目的地。"
			}
			chatID := strings.TrimSpace(msg.ChatID)
			return s.updateDriveCommentConfig(func(cfg *config.DriveCommentConfig) {
				cfg.TraceEnabled = true
				cfg.TraceChatID = chatID
			}, msg.BotID, "已开启云文档评论处理过程展示，并设置当前聊天为卡片目的地。\n")
		case "off":
			return s.updateDriveCommentConfig(func(cfg *config.DriveCommentConfig) {
				cfg.TraceEnabled = false
			}, msg.BotID, "已关闭云文档评论处理过程展示。\n")
		case "new":
			return s.handleDriveCommentTraceNew(ctx, msg)
		default:
			return "请使用 /drive_comment trace on、/drive_comment trace off 或 /drive_comment trace new。"
		}
	default:
		return "请使用 /drive_comment on、/drive_comment off、/drive_comment status、/drive_comment trace on、/drive_comment trace off 或 /drive_comment trace new。"
	}
}

func (s *Service) handleDriveCommentTraceNew(ctx context.Context, msg feishu.Message) string {
	senderID := strings.TrimSpace(msg.SenderID)
	if senderID == "" {
		return "当前消息缺少发送者 open_id，不能创建 trace 话题群。"
	}
	bot, ok := s.botConfig(msg.BotID)
	if !ok {
		return "未找到当前 bot 配置。"
	}
	botName, warning := s.resolveDriveCommentTraceBotName(ctx, bot)
	name := driveCommentTraceChatName(bot, botName)
	chat, sent, err := s.createDriveCommentTraceChat(ctx, msg, feishu.CreateDriveCommentTraceChatRequest{
		Name:        name,
		OwnerOpenID: senderID,
		UserOpenIDs: []string{senderID},
	})
	if err != nil {
		return "创建云文档评论处理通知话题群失败：" + err.Error()
	}
	if !sent {
		return "当前上下文不支持创建云文档评论处理通知话题群。"
	}
	if strings.TrimSpace(chat.ChatID) == "" {
		return "创建云文档评论处理通知话题群失败：接口未返回 chat_id。"
	}
	chatID := strings.TrimSpace(chat.ChatID)
	success := fmt.Sprintf("%s已创建云文档评论处理通知话题群，并设置为卡片目的地。\n群名：%s\nchat_id：%s\n", warning, name, chatID)
	failure := fmt.Sprintf("%s群已创建，但设置为卡片目的地失败，请手动删除该群或重新配置。\n群名：%s\nchat_id：%s\n写入配置失败：", warning, name, chatID)
	return s.updateDriveCommentConfigWithFailure(func(cfg *config.DriveCommentConfig) {
		cfg.TraceEnabled = true
		cfg.TraceChatID = chatID
	}, msg.BotID, success, failure)
}

func (s *Service) resolveDriveCommentTraceBotName(ctx context.Context, bot config.BotConfig) (string, string) {
	name, sent, err := s.driveCommentTraceBotName(ctx, bot.ID)
	if err != nil {
		return "", "获取 bot 名称失败，已使用配置 id 作为群名后缀：" + err.Error() + "\n"
	}
	if sent && strings.TrimSpace(name) == "" {
		return "", "未从飞书接口获取到 bot 名称，已使用配置 id 作为群名后缀。\n"
	}
	return name, ""
}

func (s *Service) updateDriveCommentConfig(update func(*config.DriveCommentConfig), botID string, prefix string) string {
	return s.updateDriveCommentConfigWithFailure(update, botID, prefix, "写入云文档评论配置失败：")
}

func (s *Service) updateDriveCommentConfigWithFailure(update func(*config.DriveCommentConfig), botID string, prefix string, failurePrefix string) string {
	if strings.TrimSpace(s.configPath) == "" {
		return failurePrefix + "当前 bridge 没有配置文件路径，不能修改云文档评论配置。"
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	index := botConfigIndex(s.cfg.Bots, botID)
	if index < 0 {
		return failurePrefix + "未找到当前 bot 配置。"
	}
	updated, err := config.UpdateBotDriveComment(s.configPath, s.cfg.Bots[index].ID, update)
	if err != nil {
		return failurePrefix + err.Error()
	}
	s.cfg.Bots[index].DriveComment = updated
	return prefix + formatDriveCommentStatus(s.cfg.Bots[index])
}

func (s *Service) formatDriveCommentStatus(botID string) string {
	bot, ok := s.botConfig(botID)
	if !ok {
		return "未找到当前 bot 配置。"
	}
	return formatDriveCommentStatus(bot)
}

func formatDriveCommentStatus(bot config.BotConfig) string {
	cfg := bot.DriveComment
	lines := []string{
		"云文档评论监听处理：" + onOffStatus(cfg.Enabled),
		"处理过程展示：" + onOffStatus(cfg.TraceEnabled),
	}
	if strings.TrimSpace(cfg.TraceChatID) != "" {
		lines = append(lines, "处理过程卡片目的地："+strings.TrimSpace(cfg.TraceChatID))
	}
	return strings.Join(lines, "\n")
}

func (s *Service) botConfig(botID string) (config.BotConfig, bool) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	index := botConfigIndex(s.cfg.Bots, botID)
	if index < 0 {
		return config.BotConfig{}, false
	}
	return cloneBotConfig(s.cfg.Bots[index]), true
}

func (s *Service) botConfigAt(index int) (config.BotConfig, bool) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if index < 0 || index >= len(s.cfg.Bots) {
		return config.BotConfig{}, false
	}
	return cloneBotConfig(s.cfg.Bots[index]), true
}

func cloneBotConfig(bot config.BotConfig) config.BotConfig {
	bot.OwnerOpenIDs = append([]string(nil), bot.OwnerOpenIDs...)
	return bot
}

func botConfigIndex(bots []config.BotConfig, botID string) int {
	botID = strings.TrimSpace(botID)
	for i, bot := range bots {
		if strings.TrimSpace(bot.ID) == botID {
			return i
		}
	}
	if botID == "" && len(bots) == 1 {
		return 0
	}
	if len(bots) == 1 {
		return 0
	}
	return -1
}

func driveCommentTraceChatName(bot config.BotConfig, botName string) string {
	name := strings.TrimSpace(botName)
	if name == "" {
		name = strings.TrimSpace(bot.ID)
	}
	if name == "" {
		name = "default"
	}
	return "云文档评论处理通知群-" + name
}

func onOffStatus(enabled bool) string {
	if enabled {
		return "开启"
	}
	return "关闭"
}
