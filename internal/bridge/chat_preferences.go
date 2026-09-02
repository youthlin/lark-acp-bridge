package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const (
	atModeAuto             = "auto"
	atModeAutoReaction     = "auto-reaction"
	atModeEvery            = "every"
	maxPendingAtMessages   = 100
	maxPendingAtAuto       = 100
	pendingAtHistoryHeader = "## 以下是当前对话历史消息"
	pendingAtAutoHeader    = "## 以下是待判断是否需要响应的群消息"
)

type pendingAtMessage struct {
	MessageID          string
	ThreadID           string
	RootID             string
	ParentID           string
	SenderID           string
	Text               string
	ForceReplyInThread bool
}

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
	chat, err := store.UpdateChat(chat, func(current *ChatConfig) {
		setChatShowOption(current, fields[1], value)
	})
	if err != nil {
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
	_, err := store.UpdateChat(chat, func(current *ChatConfig) {
		current.AgentName = agentName
	})
	if err != nil {
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

func (s *Service) cachePendingAtText(msg feishu.Message) {
	entry := pendingAtMessageFromMessage(msg)
	if strings.TrimSpace(entry.Text) == "" {
		return
	}
	key := chatKeyFromMessage(msg)
	if !key.Valid() {
		return
	}
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	pending := append(s.pendingAtTexts[key], entry)
	if len(pending) > maxPendingAtMessages {
		pending = append([]pendingAtMessage(nil), pending[len(pending)-maxPendingAtMessages:]...)
	}
	s.pendingAtTexts[key] = pending
}

func (s *Service) promptTextWithPendingAtTexts(msg feishu.Message, promptText string) string {
	if !messageIsGroupChat(msg) || !s.chatRequiresMention(msg) || !messageMentionsBot(msg) {
		return promptText
	}
	key := chatKeyFromMessage(msg)
	if !key.Valid() {
		return promptText
	}
	s.taskMu.Lock()
	pending := append([]pendingAtMessage(nil), s.pendingAtTexts[key]...)
	delete(s.pendingAtTexts, key)
	s.taskMu.Unlock()
	if len(pending) == 0 {
		return promptText
	}
	return promptWithUserMessage([]string{
		formatPendingAtHistory(pending),
	}, formatCurrentAtUserMessage(msg, promptText))
}

func pendingAtMessageFromMessage(msg feishu.Message) pendingAtMessage {
	return pendingAtMessage{
		MessageID:          strings.TrimSpace(msg.MessageID),
		ThreadID:           strings.TrimSpace(msg.ThreadID),
		RootID:             strings.TrimSpace(msg.RootID),
		ParentID:           strings.TrimSpace(msg.ParentID),
		SenderID:           strings.TrimSpace(msg.SenderID),
		Text:               strings.TrimSpace(msg.PromptText()),
		ForceReplyInThread: msg.ForceReplyInThread,
	}
}

func formatPendingAtHistory(messages []pendingAtMessage) string {
	return formatPendingAtMessageBlock(pendingAtHistoryHeader, messages)
}

func formatPendingAtMessageBlock(header string, messages []pendingAtMessage) string {
	lines := []string{header}
	for _, message := range messages {
		text := strings.TrimSpace(message.Text)
		if text == "" {
			continue
		}
		lines = append(lines, formatPendingAtMessageLine(message.MessageID, message.SenderID, text))
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func formatCurrentAtUserMessage(msg feishu.Message, promptText string) string {
	promptText = strings.TrimSpace(promptText)
	if promptText == "" {
		return promptText
	}
	lines := []string{
		"sender：" + formatPendingAtSender(msg.SenderID),
		"content：" + promptText,
	}
	return strings.Join(lines, "\n")
}

func formatPendingAtMessageLine(messageID string, senderID string, text string) string {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		messageID = "unknown-message"
	}
	senderID = strings.TrimSpace(senderID)
	if senderID == "" {
		senderID = "unknown-sender"
	}
	return fmt.Sprintf("- （%s）（%s）%s", messageID, senderID, text)
}

func formatPendingAtSender(senderID string) string {
	senderID = strings.TrimSpace(senderID)
	if senderID == "" {
		return "用户"
	}
	return "用户(" + senderID + ")"
}

func (s *Service) shouldQueueAtAutoMessage(msg feishu.Message) bool {
	return s.shouldHandleAtAutoMessage(msg)
}

func (s *Service) queueAtAutoMessageIfBusy(msg feishu.Message) bool {
	entry := pendingAtMessageFromMessage(msg)
	if strings.TrimSpace(entry.Text) == "" {
		return false
	}
	key := normalizeSessionKey(sessionKeyFromMessage(msg))
	session, ok := s.findSession(msg)
	if ok {
		key = normalizeSessionKey(session.Key)
	}
	if !key.Valid() {
		return false
	}
	return s.appendPendingAtAutoMessage(key, entry)
}

func (s *Service) appendPendingAtAutoMessage(key SessionKey, entry pendingAtMessage) bool {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	task := s.tasks[key]
	if (task == nil || !task.queuePendingAtAuto) && !s.atAutoFlows[key] {
		return false
	}
	pending := append(s.pendingAtAuto[key], entry)
	if len(pending) > maxPendingAtAuto {
		pending = append([]pendingAtMessage(nil), pending[len(pending)-maxPendingAtAuto:]...)
	}
	s.pendingAtAuto[key] = pending
	return true
}

func (s *Service) takePendingAtAutoMessages(key SessionKey) []pendingAtMessage {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	pending := append([]pendingAtMessage(nil), s.pendingAtAuto[key]...)
	delete(s.pendingAtAuto, key)
	return pending
}

// restorePendingAtAutoMessages 把此前 take 走但未能处理的待处理消息放回队首，
// 避免后台 at-auto prompt 因 session 忙碌等原因启动失败时丢消息。
func (s *Service) restorePendingAtAutoMessages(key SessionKey, messages []pendingAtMessage) {
	if len(messages) == 0 {
		return
	}
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	existing := s.pendingAtAuto[key]
	combined := append(append([]pendingAtMessage(nil), messages...), existing...)
	if len(combined) > maxPendingAtAuto {
		combined = append([]pendingAtMessage(nil), combined[len(combined)-maxPendingAtAuto:]...)
	}
	s.pendingAtAuto[key] = combined
}

func (s *Service) shouldSuppressAtAutoReply(msg feishu.Message, reply string) bool {
	return s.shouldHandleAtAutoMessage(msg) && isSilentReplySentinel(reply)
}

func (s *Service) shouldSuppressInvalidMentionSilentReply(msg feishu.Message, reply string) bool {
	return s.shouldHandleAtAutoMentionMessage(msg) && strings.EqualFold(strings.TrimSpace(reply), silentReplySentinel)
}

func (s *Service) shouldDelayAtAutoProgress(msg feishu.Message) bool {
	return s.shouldHandleAtAutoMessage(msg)
}

func (s *Service) shouldHandleAtAutoMessage(msg feishu.Message) bool {
	return isAtAutoMode(s.chatAtMode(msg)) && !messageMentionsBot(msg)
}

func (s *Service) shouldHandleAtAutoMentionMessage(msg feishu.Message) bool {
	return isAtAutoMode(s.chatAtMode(msg)) && messageMentionsBot(msg)
}

func (s *Service) shouldStartProcessingReaction(msg feishu.Message) bool {
	return !s.shouldHandleAtAutoMessage(msg) || s.chatAtMode(msg) == atModeAutoReaction
}

func isAtAutoMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case atModeAuto, atModeAutoReaction:
		return true
	default:
		return false
	}
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

func (s *Service) chatAtMode(msg feishu.Message) string {
	if !messageIsGroupChat(msg) {
		return ""
	}
	store := s.storeForMessage(msg)
	if store == nil {
		return ""
	}
	chat, ok := store.GetChat(chatKeyFromMessage(msg))
	if !ok {
		return ""
	}
	mode := strings.ToLower(strings.TrimSpace(chat.AtMode))
	if mode != "" {
		return mode
	}
	if chat.MentionOptional {
		return atModeEvery
	}
	return ""
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

func stripCurrentBotMentionNames(text string, msg feishu.Message) string {
	text = strings.TrimSpace(text)
	botOpenID := strings.TrimSpace(msg.BotOpenID)
	for _, mention := range msg.Mentions {
		name := strings.TrimSpace(mention.Name)
		if name == "" {
			continue
		}
		mentionID := strings.TrimSpace(mention.ID)
		if botOpenID != "" && mentionID == botOpenID {
			text = strings.ReplaceAll(text, "@"+name, "")
			continue
		}
		if mentionID == "" {
			text = strings.ReplaceAll(text, "@"+name, "")
			continue
		}
		replacement := "@" + name
		if mentionID != "" {
			replacement += "(" + mentionID + ")"
		}
		text = strings.ReplaceAll(text, "@"+name, replacement)
	}
	return strings.TrimSpace(text)
}

func (s *Service) botOpenID(botID string) string {
	bot, ok := s.botConfig(botID)
	if !ok {
		return ""
	}
	return strings.TrimSpace(bot.BotOpenID)
}

func messageIsGroupChat(msg feishu.Message) bool {
	return strings.EqualFold(msg.ChatType, "group") || msg.IsTopicGroup()
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
	mode := ""
	if len(fields) >= 3 {
		mode = strings.ToLower(strings.TrimSpace(fields[2]))
	}
	if action == "" || action == "status" {
		return s.formatAtStatus(msg)
	}
	chat := s.chatConfigForMessage(msg)
	mentionOptional := false
	atMode := ""
	switch action {
	case "on":
		if mode != "" {
			return atCommandUsage()
		}
	case "off":
		if mode == "" {
			mode = atModeEvery
		}
		switch mode {
		case atModeAuto, atModeAutoReaction, atModeEvery:
			atMode = mode
			mentionOptional = true
		default:
			return atCommandUsage()
		}
	default:
		return atCommandUsage()
	}
	chat, err := store.UpdateChat(chat, func(current *ChatConfig) {
		current.AtMode = atMode
		current.MentionOptional = mentionOptional
	})
	if err != nil {
		slog.ErrorContext(ctx, "保存群聊 at 配置失败", "chat", msg.ChatID, "错误", err)
		return "保存群聊 at 配置失败：" + err.Error()
	}
	if chat.MentionOptional {
		switch chat.AtMode {
		case atModeAuto:
			return "已设置当前群聊：无需 at 也会进入自动判断。"
		case atModeAutoReaction:
			return "已设置当前群聊：无需 at 也会进入自动判断，并为未 at 消息添加处理中表情。"
		}
		return "已设置当前群聊：每条消息都会响应，无需 at。"
	}
	return "已设置当前群聊：需要 at 才响应。"
}

func atCommandUsage() string {
	return "请使用 /at status、/at on、/at off auto、/at off auto-reaction 或 /at off every。"
}

func (s *Service) formatAtStatus(msg feishu.Message) string {
	if msg.IsPrivateChat() {
		return "私聊不支持 /at 配置；私聊消息始终响应。"
	}
	if !messageIsGroupChat(msg) {
		return "当前会话类型不支持 /at 配置。"
	}
	switch s.chatAtMode(msg) {
	case atModeAuto:
		return "当前群聊：无需 at 也会进入自动判断，未 at 消息不添加处理中表情。\n使用 /at off auto-reaction 可为自动判断添加处理中表情；使用 /at off every 可改为每条消息都响应；使用 /at on 可恢复为需要 at。"
	case atModeAutoReaction:
		return "当前群聊：无需 at 也会进入自动判断，未 at 消息会添加处理中表情。\n使用 /at off auto 可关闭处理中表情；使用 /at off every 可改为每条消息都响应；使用 /at on 可恢复为需要 at。"
	}
	if s.chatRequiresMention(msg) {
		return "当前群聊：需要 at 才响应。\n使用 /at off auto 可改为自动判断；使用 /at off auto-reaction 可改为自动判断并添加处理中表情；使用 /at off every 可改为每条消息都响应。"
	}
	return "当前群聊：每条消息都会响应，无需 at。\n使用 /at on 可恢复为需要 at。"
}
