package bridge

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func (s *Service) handleMeetingCommand(_ context.Context, text string, msg feishu.Message) string {
	if !s.slashCommandAllowed(msg) {
		if len(s.ownerOpenIDs(msg.BotID)) == 0 {
			return "未配置 bot owner，不能管理会议助手。"
		}
		return "只有 bot owner 可以管理会议助手。"
	}
	fields := strings.Fields(text)
	if len(fields) == 1 || len(fields) == 2 && strings.EqualFold(fields[1], "status") {
		return s.formatMeetingStatus(msg.BotID)
	}
	switch strings.ToLower(strings.TrimSpace(fields[1])) {
	case "on":
		recipient, errText := s.meetingRecipient(msg.BotID)
		if errText != "" {
			return errText
		}
		return s.updateMeetingConfig(func(cfg *config.MeetingConfig) {
			cfg.Enabled = true
			if strings.TrimSpace(cfg.RecipientOpenID) == "" {
				cfg.RecipientOpenID = recipient
			}
		}, msg.BotID, "已开启静默会议助手。\n")
	case "off":
		return s.updateMeetingConfig(func(cfg *config.MeetingConfig) {
			cfg.Enabled = false
		}, msg.BotID, "已关闭新的会议邀请处理；正在进行的会议仍会整理到结束。\n")
	case "trace":
		if len(fields) != 3 {
			return "请使用 /meeting trace on 或 /meeting trace off。"
		}
		switch strings.ToLower(strings.TrimSpace(fields[2])) {
		case "on":
			chatID := strings.TrimSpace(msg.ChatID)
			if chatID == "" {
				return "当前消息缺少 chat_id，不能设置为会议整理过程卡片目的地。"
			}
			return s.updateMeetingConfig(func(cfg *config.MeetingConfig) {
				cfg.TraceEnabled = true
				cfg.TraceChatID = chatID
			}, msg.BotID, "已开启会议整理过程展示，并设置当前聊天为卡片目的地。\n")
		case "off":
			return s.updateMeetingConfig(func(cfg *config.MeetingConfig) {
				cfg.TraceEnabled = false
			}, msg.BotID, "已关闭会议整理过程展示。\n")
		default:
			return "请使用 /meeting trace on 或 /meeting trace off。"
		}
	default:
		return "请使用 /meeting on、/meeting off、/meeting status、/meeting trace on 或 /meeting trace off。"
	}
}

func (s *Service) updateMeetingConfig(update func(*config.MeetingConfig), botID string, prefix string) string {
	if strings.TrimSpace(s.configPath) == "" {
		return "写入会议助手配置失败：当前 bridge 没有配置文件路径。"
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	index := botConfigIndex(s.cfg.Bots, botID)
	if index < 0 {
		return "写入会议助手配置失败：未找到当前 bot 配置。"
	}
	updated, err := config.UpdateBotMeeting(s.configPath, s.cfg.Bots[index].ID, update)
	if err != nil {
		return "写入会议助手配置失败：" + err.Error()
	}
	s.cfg.Bots[index].Meeting = updated
	return prefix + formatMeetingConfigStatus(s.cfg.Bots[index])
}

func (s *Service) meetingRecipient(botID string) (string, string) {
	bot, ok := s.botConfig(botID)
	if !ok {
		return "", "未找到当前 bot 配置。"
	}
	if recipient := strings.TrimSpace(bot.Meeting.RecipientOpenID); recipient != "" {
		return recipient, ""
	}
	if len(bot.OwnerOpenIDs) == 1 {
		return strings.TrimSpace(bot.OwnerOpenIDs[0]), ""
	}
	return "", "开启会议助手前需要配置唯一的 meeting.recipient_open_id；仅当 bot 恰好有一个 owner 时才会自动使用该 owner。"
}

func (s *Service) formatMeetingStatus(botID string) string {
	bot, ok := s.botConfig(botID)
	if !ok {
		return "未找到当前 bot 配置。"
	}
	lines := []string{formatMeetingConfigStatus(bot)}
	store := s.meetingStore(botID)
	if store == nil {
		return strings.Join(lines, "\n")
	}
	active, pending, latestFlush, latestError := 0, 0, time.Time{}, ""
	for _, state := range store.List() {
		if !meetingStateIncomplete(state) {
			continue
		}
		active++
		pending += len(state.PendingEvents)
		if state.LastFlushAt.After(latestFlush) {
			latestFlush = state.LastFlushAt
		}
		if latestError == "" && state.LastError != "" {
			latestError = state.LastError
		}
	}
	lines = append(lines, "未完成会议："+strconv.Itoa(active), "待处理事件："+strconv.Itoa(pending))
	if !latestFlush.IsZero() {
		lines = append(lines, "最近整理："+latestFlush.Local().Format("2006-01-02 15:04:05"))
	}
	if latestError != "" {
		lines = append(lines, "最近错误："+latestError)
	}
	return strings.Join(lines, "\n")
}

func formatMeetingConfigStatus(bot config.BotConfig) string {
	recipient := strings.TrimSpace(bot.Meeting.RecipientOpenID)
	if recipient == "" && len(bot.OwnerOpenIDs) == 1 {
		recipient = strings.TrimSpace(bot.OwnerOpenIDs[0]) + "（唯一 owner）"
	}
	if recipient == "" {
		recipient = "未配置"
	}
	return strings.Join([]string{
		"静默会议助手：" + onOffStatus(bot.Meeting.Enabled),
		"会议卡片接收人：" + recipient,
		"会议整理过程展示：" + onOffStatus(bot.Meeting.TraceEnabled),
	}, "\n") + formatMeetingTraceChatStatus(bot.Meeting)
}

func formatMeetingTraceChatStatus(cfg config.MeetingConfig) string {
	if strings.TrimSpace(cfg.TraceChatID) == "" {
		return ""
	}
	return "\n过程卡片目的地：" + strings.TrimSpace(cfg.TraceChatID)
}
