package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func (s *Service) handleWikiTraceCommand(ctx context.Context, fields []string, msg feishu.Message) string {
	if !s.slashCommandAllowed(msg) {
		if len(s.ownerOpenIDs(msg.BotID)) == 0 {
			return "未配置 bot owner，不能管理 wiki 过程展示。"
		}
		return "只有 bot owner 可以管理 wiki 过程展示。"
	}
	if len(fields) < 3 {
		return wikiTraceCommandUsage()
	}
	switch strings.ToLower(strings.TrimSpace(fields[2])) {
	case "on":
		chatID := strings.TrimSpace(msg.ChatID)
		if chatID == "" {
			return "当前消息缺少 chat_id，不能设置为 wiki 过程卡片目的地。"
		}
		return s.updateWikiTraceConfig(func(cfg *config.WikiTraceConfig) {
			cfg.Enabled = true
			cfg.ChatID = chatID
		}, msg.BotID, "已开启自动知识沉淀过程展示，并设置当前聊天为卡片目的地。\n")
	case "off":
		return s.updateWikiTraceConfig(func(cfg *config.WikiTraceConfig) {
			cfg.Enabled = false
		}, msg.BotID, "已关闭自动知识沉淀过程展示。\n")
	case "new":
		return s.handleWikiTraceNew(ctx, msg)
	default:
		return wikiTraceCommandUsage()
	}
}

func wikiTraceCommandUsage() string {
	return "请使用 /wiki trace on、/wiki trace off 或 /wiki trace new。"
}

func (s *Service) handleWikiTraceNew(ctx context.Context, msg feishu.Message) string {
	senderID := strings.TrimSpace(msg.SenderID)
	if senderID == "" {
		return "当前消息缺少发送者 open_id，不能创建 wiki 过程通知话题群。"
	}
	bot, ok := s.botConfig(msg.BotID)
	if !ok {
		return "未找到当前 bot 配置。"
	}
	botName, warning := s.resolveDriveCommentTraceBotName(ctx, bot)
	name := wikiTraceChatName(bot, botName)
	chat, sent, err := s.createDriveCommentTraceChat(ctx, msg, feishu.CreateDriveCommentTraceChatRequest{
		Name:        name,
		OwnerOpenID: senderID,
		UserOpenIDs: []string{senderID},
	})
	if err != nil {
		return "创建 wiki 过程通知话题群失败：" + err.Error()
	}
	if !sent {
		return "当前上下文不支持创建 wiki 过程通知话题群。"
	}
	chatID := strings.TrimSpace(chat.ChatID)
	if chatID == "" {
		return "创建 wiki 过程通知话题群失败：接口未返回 chat_id。"
	}
	success := fmt.Sprintf("%s已创建 wiki 过程通知话题群，并设置为卡片目的地。\n群名：%s\nchat_id：%s\n", warning, name, chatID)
	failure := fmt.Sprintf("%s群已创建，但设置为卡片目的地失败，请手动删除该群或重新配置。\n群名：%s\nchat_id：%s\n写入配置失败：", warning, name, chatID)
	return s.updateWikiTraceConfigWithFailure(func(cfg *config.WikiTraceConfig) {
		cfg.Enabled = true
		cfg.ChatID = chatID
	}, msg.BotID, success, failure)
}

func (s *Service) updateWikiTraceConfig(update func(*config.WikiTraceConfig), botID, prefix string) string {
	return s.updateWikiTraceConfigWithFailure(update, botID, prefix, "写入 wiki 过程展示配置失败：")
}

func (s *Service) updateWikiTraceConfigWithFailure(update func(*config.WikiTraceConfig), botID, prefix, failurePrefix string) string {
	if strings.TrimSpace(s.configPath) == "" {
		return failurePrefix + "当前 bridge 没有配置文件路径，不能修改 wiki 过程展示配置。"
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	index := botConfigIndex(s.cfg.Bots, botID)
	if index < 0 {
		return failurePrefix + "未找到当前 bot 配置。"
	}
	updated, err := config.UpdateBotWikiTrace(s.configPath, s.cfg.Bots[index].ID, update)
	if err != nil {
		return failurePrefix + err.Error()
	}
	s.cfg.Bots[index].WikiTrace = updated
	return prefix + formatWikiTraceStatus(s.cfg.Bots[index].WikiTrace)
}

func formatWikiTraceStatus(cfg config.WikiTraceConfig) string {
	lines := []string{
		"自动知识沉淀过程展示：" + onOffStatus(cfg.Enabled),
	}
	if strings.TrimSpace(cfg.ChatID) != "" {
		lines = append(lines, "过程卡片目的地："+strings.TrimSpace(cfg.ChatID))
	}
	return strings.Join(lines, "\n")
}

func wikiTraceChatName(bot config.BotConfig, botName string) string {
	name := strings.TrimSpace(botName)
	if name == "" {
		name = strings.TrimSpace(bot.ID)
	}
	if name == "" {
		name = "default"
	}
	return "知识沉淀过程通知群-" + name
}
