package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func (s *Service) handleShowCommand(ctx context.Context, msg feishu.Message, text string) string {
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	chat := s.chatConfigForMessage(msg)
	fields := strings.Fields(text)
	if len(fields) == 1 || len(fields) == 2 && strings.EqualFold(strings.TrimSpace(fields[1]), "status") {
		return formatShowStatus(chat)
	}
	if len(fields) != 3 {
		return showCommandUsage()
	}
	value, ok := parseShowSwitch(fields[2])
	if !ok {
		return "请使用 on 或 off，例如 /show thought off。"
	}
	target, ok := setChatShowOption(&chat, fields[1], value)
	if !ok {
		return showCommandUsage()
	}
	if err := store.UpsertChat(chat); err != nil {
		slog.ErrorContext(ctx, "保存展示配置失败", "错误", err)
		return "保存展示配置失败：" + err.Error()
	}
	state := "开启"
	if !value {
		state = "关闭"
	}
	return fmt.Sprintf("已%s%s。\n%s", state, target, formatShowStatus(chat))
}

func showCommandUsage() string {
	return "请使用 /show step|plan|thought|tool|status|used on|off。"
}

func parseShowSwitch(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on":
		return true, true
	case "off":
		return false, true
	default:
		return false, false
	}
}

func setChatShowOption(chat *ChatConfig, target string, visible bool) (string, bool) {
	if chat == nil {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "step":
		chat.HideStepMessages = !visible
		return "过程消息展示", true
	case "plan":
		chat.HidePlans = !visible
		return "计划展示", true
	case "thought":
		chat.ShowThoughts = visible
		chat.HideThoughts = !visible
		return "思考消息展示", true
	case "tool":
		chat.HideTools = !visible
		return "工具调用展示", true
	case "status":
		chat.HideStatusBar = !visible
		return "状态栏展示", true
	case "used":
		chat.HideUsageDetail = !visible
		return "用量明细展示", true
	default:
		return "", false
	}
}

func (s *Service) chatConfigForMessage(msg feishu.Message) ChatConfig {
	chat := ChatConfig{Key: chatKeyFromMessage(msg)}
	store := s.storeForMessage(msg)
	if store == nil {
		return chat
	}
	if existing, ok := store.GetChat(chat.Key); ok {
		return existing
	}
	if session, ok := s.findSession(msg); ok {
		chat.WikiDisabled = session.WikiDisabled
		chat.WikiIntervalSec = session.WikiIntervalSec
		chat.HideStepMessages = session.HideStepMessages
		chat.HidePlans = session.HidePlans
		chat.ShowThoughts = session.ShowThoughts
		chat.HideThoughts = session.HideThoughts
		chat.HideTools = session.HideTools
		chat.HideStatusBar = session.HideStatusBar
		chat.HideUsageDetail = session.HideUsageDetail
	}
	return chat
}

func (s *Service) migrateSessionShowConfigToChat(ctx context.Context, msg feishu.Message) {
	store := s.storeForMessage(msg)
	if store == nil {
		return
	}
	chatKey := chatKeyFromMessage(msg)
	if _, ok := store.GetChat(chatKey); ok {
		return
	}
	session, ok := s.findSession(msg)
	if !ok || !sessionHasShowConfig(session) {
		return
	}
	chat := ChatConfig{
		Key:              chatKey,
		WikiDisabled:     session.WikiDisabled,
		WikiIntervalSec:  session.WikiIntervalSec,
		HideStepMessages: session.HideStepMessages,
		HidePlans:        session.HidePlans,
		ShowThoughts:     session.ShowThoughts,
		HideThoughts:     session.HideThoughts,
		HideTools:        session.HideTools,
		HideStatusBar:    session.HideStatusBar,
		HideUsageDetail:  session.HideUsageDetail,
	}
	if err := store.UpsertChat(chat); err != nil {
		slog.ErrorContext(ctx, "迁移会话展示配置到 chat 配置失败", "chat", msg.ChatID, "错误", err)
	}
}

func sessionHasShowConfig(session Session) bool {
	return session.HideStepMessages ||
		session.HidePlans ||
		session.ShowThoughts ||
		session.HideThoughts ||
		session.HideTools ||
		session.HideStatusBar ||
		session.HideUsageDetail ||
		session.WikiDisabled ||
		session.WikiIntervalSec > 0
}

func formatShowStatus(chat ChatConfig) string {
	return strings.Join([]string{
		"当前会话流式卡片展示：",
		"过程消息：" + showState(!chat.HideStepMessages),
		"计划：" + showState(!chat.HidePlans),
		"思考消息：" + showState(chatThoughtsVisible(chat)),
		"工具调用：" + showState(!chat.HideTools),
		"状态栏：" + showState(!chat.HideStatusBar),
		"用量明细：" + showState(!chat.HideUsageDetail),
	}, "\n")
}

func (s *Service) handleAgentCommand(ctx context.Context, text string, msg feishu.Message) string {
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	fields := strings.Fields(text)
	if len(fields) == 1 || len(fields) == 2 && strings.EqualFold(strings.TrimSpace(fields[1]), "status") {
		return s.formatAgentStatus(msg)
	}
	if len(fields) != 2 {
		return "请使用 /agent 或 /agent <name>。"
	}
	agentName := strings.TrimSpace(fields[1])
	if _, ok := s.registry.Get(agentName); !ok {
		return "未知 agent：" + agentName + "\n\n" + s.formatAgentStatus(msg)
	}
	chat := s.chatConfigForMessage(msg)
	chat.AgentName = agentName
	if err := store.UpsertChat(chat); err != nil {
		slog.ErrorContext(ctx, "保存聊天 agent 配置失败", "chat", msg.ChatID, "agent", agentName, "错误", err)
		return "保存聊天 agent 配置失败：" + err.Error()
	}
	lines := []string{
		"已设置当前聊天默认 agent：" + agentName,
	}
	if session, ok := s.findSession(msg); ok && strings.TrimSpace(session.AgentName) != "" && session.AgentName != agentName {
		lines = append(lines, "当前已有会话仍使用 agent："+session.AgentName+"；下一条普通消息或 /new 会使用新的默认 agent。")
	}
	return strings.Join(lines, "\n")
}

func (s *Service) formatAgentStatus(msg feishu.Message) string {
	names := s.registry.Names()
	current := s.chatAgentName(msg)
	lines := []string{
		"当前聊天默认 agent：" + current,
		"可用 agent：" + strings.Join(names, ", "),
	}
	if session, ok := s.findSession(msg); ok && strings.TrimSpace(session.AgentName) != "" {
		lines = append(lines, sessionLabel(msg)+"当前使用 agent："+session.AgentName)
	}
	return strings.Join(lines, "\n")
}

func chatThoughtsVisible(chat ChatConfig) bool {
	return chat.ShowThoughts && !chat.HideThoughts
}

func showState(visible bool) string {
	if visible {
		return "开启"
	}
	return "关闭"
}

func (s *Service) shouldIgnoreMessage(msg feishu.Message, text string) bool {
	if !messageIsGroupChat(msg) || !s.chatRequiresMention(msg) {
		return false
	}
	return !messageMentionsBot(msg)
}

func (s *Service) chatRequiresMention(msg feishu.Message) bool {
	if !messageIsGroupChat(msg) {
		return false
	}
	store := s.storeForMessage(msg)
	if store == nil {
		return true
	}
	chat, ok := store.GetChat(chatKeyFromMessage(msg))
	if !ok {
		return true
	}
	return !chat.MentionOptional
}

func messageMentionsBot(msg feishu.Message) bool {
	botOpenID := strings.TrimSpace(msg.BotOpenID)
	if botOpenID == "" {
		return false
	}
	for _, mention := range msg.Mentions {
		if strings.TrimSpace(mention.ID) == botOpenID {
			return true
		}
	}
	return false
}

func (s *Service) botOpenID(botID string) string {
	botID = strings.TrimSpace(botID)
	for _, bot := range s.cfg.Bots {
		if strings.TrimSpace(bot.ID) == botID {
			return strings.TrimSpace(bot.BotOpenID)
		}
	}
	return ""
}

func messageIsGroupChat(msg feishu.Message) bool {
	return strings.EqualFold(msg.ChatType, "group")
}

func (s *Service) handleAtCommand(ctx context.Context, msg feishu.Message, text string) string {
	if msg.IsPrivateChat() {
		return "私聊不支持 /at 配置；私聊消息始终响应。"
	}
	if !messageIsGroupChat(msg) {
		return "当前会话类型不支持 /at 配置。"
	}
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	fields := strings.Fields(text)
	action := ""
	if len(fields) >= 2 {
		action = strings.ToLower(strings.TrimSpace(fields[1]))
	}
	if action == "" || action == "status" {
		return s.formatAtStatus(msg)
	}
	chat := s.chatConfigForMessage(msg)
	switch action {
	case "on":
		chat.MentionOptional = false
	case "off":
		chat.MentionOptional = true
	default:
		return "请使用 /at status、/at on 或 /at off。"
	}
	if err := store.UpsertChat(chat); err != nil {
		slog.ErrorContext(ctx, "保存群聊 at 配置失败", "chat", msg.ChatID, "错误", err)
		return "保存群聊 at 配置失败：" + err.Error()
	}
	if chat.MentionOptional {
		return "已设置当前群聊：无需 at 也会响应。"
	}
	return "已设置当前群聊：需要 at 才响应。"
}

func (s *Service) formatAtStatus(msg feishu.Message) string {
	if msg.IsPrivateChat() {
		return "私聊不支持 /at 配置；私聊消息始终响应。"
	}
	if !messageIsGroupChat(msg) {
		return "当前会话类型不支持 /at 配置。"
	}
	if s.chatRequiresMention(msg) {
		return "当前群聊：需要 at 才响应。\n使用 /at off 可改为免 at。"
	}
	return "当前群聊：无需 at 也会响应。\n使用 /at on 可恢复为需要 at。"
}
