package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const (
	overviewActionOpenModel   = "open_model"
	overviewActionOpenMode    = "open_mode"
	overviewActionOpenSession = "open_session"
	overviewActionToggleShow  = "toggle_show"
	overviewActionToggleWiki  = "toggle_wiki"
	overviewActionSetAgent    = "set_agent"
)

func (s *Service) handleCardCommand(ctx context.Context, msg feishu.Message) string {
	card := s.buildOverviewCard(msg)
	sent, err := s.sendOverviewCardOutbound(ctx, msg, card)
	if err != nil {
		slog.ErrorContext(ctx, "发送全览卡片失败", "错误", err)
		return "发送全览卡片失败：" + err.Error()
	}
	if !sent {
		return s.formatOverviewText(card)
	}
	return ""
}

func (s *Service) buildOverviewCard(msg feishu.Message) feishu.OverviewCard {
	chat := s.chatConfigForMessage(msg)
	session, hasSession := s.findSession(msg)
	card := feishu.OverviewCard{
		BotID:               msg.BotID,
		ChatID:              msg.ChatID,
		ChatType:            msg.ChatType,
		ThreadID:            msg.ThreadID,
		GroupMessageType:    msg.GroupMessageType,
		RequesterID:         msg.SenderID,
		CurrentACPSessionID: "",
		HasSession:          hasSession && strings.TrimSpace(session.ACPSessionID) != "",
		ChatAgentName:       s.chatAgentName(msg),
		AtStatus:            overviewAtStatus(s, msg),
		Show: feishu.OverviewShowOptions{
			Step:    !chat.HideStepMessages,
			Plan:    !chat.HidePlans,
			Thought: chatThoughtsVisible(chat),
			Tool:    !chat.HideTools,
			Status:  !chat.HideStatusBar,
			Used:    !chat.HideUsageDetail,
		},
		WikiEnabled:  !chat.WikiDisabled,
		AgentOptions: s.overviewAgentOptions(msg),
		CommandHints: []string{
			"/new [cwd] [title]",
			"/schedule add <spec> <prompt>",
			"/loop [-t 0] [-n 0] [-i 10s] <prompt>",
			"/compact on 80%",
		},
	}
	card.WikiStatus = s.wikiStatusLine(msg, chat)
	if !card.HasSession {
		card.SessionTitle = "尚未创建"
		card.AgentName = card.ChatAgentName
		card.RuntimeStatus = "无会话"
		card.QueueStatus = "待执行 0 条"
		card.LoopStatus = "尚无状态"
		card.CompactStatus = "未配置"
		card.ACPErrorStatus = "无"
		return card
	}
	card.CurrentACPSessionID = session.ACPSessionID
	card.SessionTitle = displaySessionTitle(session)
	card.AgentName = session.AgentName
	card.Cwd = session.Cwd
	card.Model = currentModelDisplay(session)
	card.Mode = currentModeDisplay(session)
	if session.ContextWindow != nil {
		card.ContextUsage = formatContextUsage(*session.ContextWindow)
		if percent := contextUsagePercent(*session.ContextWindow); percent > 0 {
			card.ContextUsage += fmt.Sprintf("（%.1f%%）", percent)
		}
	}
	card.CompactStatus = formatCompactStatusInline(session)
	status := s.sessionRuntimeStatusSnapshot(session.Key)
	card.RuntimeStatus = formatSessionBusyStatus(status)
	card.QueueStatus = formatSessionQueueStatus(status)
	loopStatus, hasLoopStatus := s.loopStatusSnapshot(session.Key)
	card.LoopStatus = formatSessionLoopStatus(loopStatus, hasLoopStatus)
	acpError, hasACPError := s.acpErrorSnapshot(session)
	card.ACPErrorStatus = formatACPErrorStatus(acpError, hasACPError)
	return card
}

func overviewAtStatus(s *Service, msg feishu.Message) string {
	if msg.IsPrivateChat() {
		return "私聊始终响应"
	}
	if !messageIsGroupChat(msg) {
		return "当前会话类型不支持"
	}
	switch s.chatAtMode(msg) {
	case atModeAuto:
		return "自动判断"
	case atModeAutoReaction:
		return "自动判断 + 处理中表情"
	case atModeEvery:
		return "每条消息响应"
	default:
		if s.chatRequiresMention(msg) {
			return "需要 at"
		}
		return "每条消息响应"
	}
}

func (s *Service) wikiStatusLine(msg feishu.Message, chat ChatConfig) string {
	enabled := !chat.WikiDisabled
	prefix := "关闭"
	if enabled {
		prefix = "开启"
	}
	parts := []string{prefix, "延迟 " + formatDuration(wikiInterval(chat))}
	session, hasSession := s.findSession(msg)
	if hasSession {
		status := formatSessionWikiStatus(s.wikiStatusSnapshot(session.Key))
		if status != "" {
			parts = append(parts, status)
		}
	}
	return strings.Join(parts, "，")
}

func (s *Service) overviewAgentOptions(msg feishu.Message) []feishu.OverviewOption {
	current := s.chatAgentName(msg)
	names := s.registry.Names()
	options := make([]feishu.OverviewOption, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		options = append(options, feishu.OverviewOption{
			Value:   name,
			Text:    name,
			Current: name == current,
		})
	}
	return options
}

func (s *Service) formatOverviewText(card feishu.OverviewCard) string {
	lines := []string{
		"当前聊天全览：",
		"默认 agent：" + defaultString(card.ChatAgentName, "未知"),
		"at：" + defaultString(card.AtStatus, "未知"),
		"wiki：" + defaultString(card.WikiStatus, "未知"),
		"展示：过程 " + showState(card.Show.Step) + "，计划 " + showState(card.Show.Plan) + "，思考 " + showState(card.Show.Thought) + "，工具 " + showState(card.Show.Tool) + "，状态栏 " + showState(card.Show.Status) + "，用量 " + showState(card.Show.Used),
	}
	if card.HasSession {
		lines = append(lines,
			"当前会话："+card.SessionTitle,
			"agent："+defaultString(card.AgentName, "未知"),
			"cwd："+defaultString(card.Cwd, "未知"),
			"model："+defaultString(card.Model, "未知"),
			"mode："+defaultString(card.Mode, "未知"),
			"运行态："+defaultString(card.RuntimeStatus, "未知"),
			"队列："+defaultString(card.QueueStatus, "未知"),
			"loop："+defaultString(card.LoopStatus, "未知"),
			"compact："+defaultString(card.CompactStatus, "未知"),
			"ACP错误："+defaultString(card.ACPErrorStatus, "无"),
		)
	} else {
		lines = append(lines, "当前还没有 ACP session，发送普通文本或 /new 后创建。")
	}
	lines = append(lines, "", "发送 /card 可打开交互卡片。")
	return strings.Join(lines, "\n")
}

func (s *Service) HandleOverviewAction(ctx context.Context, action feishu.OverviewAction) (feishu.OverviewActionResult, error) {
	if err := validateSelectionRequester(action.RequesterID, action.OperatorID, "全览卡", "card", "操作全览卡"); err != nil {
		return feishu.OverviewActionResult{}, err
	}
	msg := feishu.Message{
		BotID:            action.BotID,
		ChatID:           action.ChatID,
		ChatType:         action.ChatType,
		ThreadID:         action.ThreadID,
		GroupMessageType: action.GroupMessageType,
		SenderID:         action.OperatorID,
	}
	if !s.slashCommandAllowed(msg) {
		if len(s.ownerOpenIDs(action.BotID)) == 0 {
			return feishu.OverviewActionResult{}, fmt.Errorf("未配置 bot owner，不能操作全览卡")
		}
		return feishu.OverviewActionResult{}, fmt.Errorf("只有 bot owner 可以操作全览卡")
	}
	switch strings.TrimSpace(action.Action) {
	case overviewActionOpenModel:
		return s.overviewModelSelection(action)
	case overviewActionOpenMode:
		return s.overviewModeSelection(action)
	case overviewActionOpenSession:
		return s.overviewSessionSelection(msg, action)
	case overviewActionToggleShow:
		if err := s.applyOverviewShowToggle(ctx, msg, action.Target, action.Value); err != nil {
			return feishu.OverviewActionResult{}, err
		}
		card := s.buildOverviewCard(msg)
		return feishu.OverviewActionResult{ToastType: "success", Toast: "展示配置已更新", Overview: &card}, nil
	case overviewActionToggleWiki:
		if err := s.applyOverviewWikiToggle(ctx, msg, action.Value); err != nil {
			return feishu.OverviewActionResult{}, err
		}
		card := s.buildOverviewCard(msg)
		return feishu.OverviewActionResult{ToastType: "success", Toast: "wiki 配置已更新", Overview: &card}, nil
	case overviewActionSetAgent:
		if err := s.applyOverviewAgent(ctx, msg, action.Value); err != nil {
			return feishu.OverviewActionResult{}, err
		}
		card := s.buildOverviewCard(msg)
		return feishu.OverviewActionResult{ToastType: "success", Toast: "默认 agent 已更新", Overview: &card}, nil
	default:
		return feishu.OverviewActionResult{}, fmt.Errorf("未知的全览卡操作")
	}
}

func (s *Service) overviewModelSelection(action feishu.OverviewAction) (feishu.OverviewActionResult, error) {
	msg := feishu.Message{
		BotID:            action.BotID,
		ChatID:           action.ChatID,
		ThreadID:         action.ThreadID,
		GroupMessageType: action.GroupMessageType,
	}
	session, err := s.selectionSession(msg, action.CurrentACPSessionID, "该全览卡已过期，请重新发送 /card")
	if err != nil {
		return feishu.OverviewActionResult{}, err
	}
	modelOpt, ok := findModelConfigOption(session)
	if !ok {
		return feishu.OverviewActionResult{}, fmt.Errorf("当前 ACP server 没有上报 model 配置项")
	}
	options := modelSelectionOptions(session, modelOpt)
	if len(options) == 0 {
		return feishu.OverviewActionResult{}, fmt.Errorf("当前 ACP server 没有上报可选模型")
	}
	card := feishu.ModelSelectionCard{
		BotID:            session.Key.BotID,
		ChatID:           session.Key.ChatID,
		ThreadID:         session.Key.SubID,
		GroupMessageType: action.GroupMessageType,
		ACPSessionID:     session.ACPSessionID,
		RequesterID:      action.RequesterID,
		CurrentModel:     currentModelDisplay(session),
		Options:          options,
	}
	return feishu.OverviewActionResult{ToastType: "success", Toast: "请选择模型", Model: &card}, nil
}

func (s *Service) overviewModeSelection(action feishu.OverviewAction) (feishu.OverviewActionResult, error) {
	msg := feishu.Message{
		BotID:            action.BotID,
		ChatID:           action.ChatID,
		ThreadID:         action.ThreadID,
		GroupMessageType: action.GroupMessageType,
	}
	session, err := s.selectionSession(msg, action.CurrentACPSessionID, "该全览卡已过期，请重新发送 /card")
	if err != nil {
		return feishu.OverviewActionResult{}, err
	}
	modeOpt, ok := findModeConfigOption(session)
	if !ok && session.Mode == nil {
		return feishu.OverviewActionResult{}, fmt.Errorf("当前 ACP server 没有上报 mode 配置项或 legacy modes")
	}
	options := modeSelectionOptions(session, modeOpt)
	if len(options) == 0 {
		return feishu.OverviewActionResult{}, fmt.Errorf("当前 ACP server 没有上报可选模式")
	}
	card := feishu.ModeSelectionCard{
		BotID:            session.Key.BotID,
		ChatID:           session.Key.ChatID,
		ThreadID:         session.Key.SubID,
		GroupMessageType: action.GroupMessageType,
		ACPSessionID:     session.ACPSessionID,
		RequesterID:      action.RequesterID,
		CurrentMode:      currentModeDisplay(session),
		Options:          options,
	}
	return feishu.OverviewActionResult{ToastType: "success", Toast: "请选择模式", Mode: &card}, nil
}

func (s *Service) overviewSessionSelection(msg feishu.Message, action feishu.OverviewAction) (feishu.OverviewActionResult, error) {
	store := s.storeForMessage(msg)
	if store == nil {
		return feishu.OverviewActionResult{}, fmt.Errorf("会话持久化未初始化")
	}
	items := store.ListByMain(sessionKeyFromMessage(msg))
	options := sessionSelectionOptions(items, maxSessionHistoryPerChat)
	if len(options) == 0 {
		return feishu.OverviewActionResult{}, fmt.Errorf("当前聊天还没有历史 ACP 会话")
	}
	card := feishu.SessionSelectionCard{
		BotID:               msg.BotID,
		ChatID:              msg.ChatID,
		ThreadID:            msg.ThreadID,
		GroupMessageType:    msg.GroupMessageType,
		RequesterID:         action.RequesterID,
		CurrentACPSessionID: currentACPSessionID(s, msg),
		Options:             options,
	}
	return feishu.OverviewActionResult{ToastType: "success", Toast: "请选择历史会话", Session: &card}, nil
}

func (s *Service) applyOverviewShowToggle(ctx context.Context, msg feishu.Message, target string, raw string) error {
	store := s.storeForMessage(msg)
	if store == nil {
		return fmt.Errorf("会话持久化未初始化")
	}
	visible, ok := parseShowSwitch(raw)
	if !ok {
		return fmt.Errorf("展示配置取值无效")
	}
	chat := s.chatConfigForMessage(msg)
	if _, ok := setChatShowOption(&chat, target, visible); !ok {
		return fmt.Errorf("未知展示项")
	}
	_, err := store.UpdateChat(chat, func(current *ChatConfig) {
		setChatShowOption(current, target, visible)
	})
	if err != nil {
		slog.ErrorContext(ctx, "保存全览卡展示配置失败", "target", target, "错误", err)
		return fmt.Errorf("保存展示配置失败：%w", err)
	}
	return nil
}

func (s *Service) applyOverviewWikiToggle(ctx context.Context, msg feishu.Message, raw string) error {
	store := s.storeForMessage(msg)
	if store == nil {
		return fmt.Errorf("会话持久化未初始化")
	}
	enabled, ok := parseShowSwitch(raw)
	if !ok {
		return fmt.Errorf("wiki 配置取值无效")
	}
	chat := s.chatConfigForMessage(msg)
	if !enabled {
		if session, ok := s.findSession(msg); ok {
			s.cancelWikiTimer(session.Key)
			s.cancelWikiTasks(ctx, session.Key)
		}
	}
	_, err := store.UpdateChat(chat, func(current *ChatConfig) {
		current.WikiDisabled = !enabled
	})
	if err != nil {
		slog.ErrorContext(ctx, "保存全览卡 wiki 配置失败", "错误", err)
		return fmt.Errorf("保存 wiki 配置失败：%w", err)
	}
	return nil
}

func (s *Service) applyOverviewAgent(ctx context.Context, msg feishu.Message, agentName string) error {
	store := s.storeForMessage(msg)
	if store == nil {
		return fmt.Errorf("会话持久化未初始化")
	}
	agentName = strings.TrimSpace(agentName)
	if _, ok := s.registry.Get(agentName); !ok {
		return fmt.Errorf("未知 agent：%s", agentName)
	}
	chat := s.chatConfigForMessage(msg)
	_, err := store.UpdateChat(chat, func(current *ChatConfig) {
		current.AgentName = agentName
	})
	if err != nil {
		slog.ErrorContext(ctx, "保存全览卡 agent 配置失败", "agent", agentName, "错误", err)
		return fmt.Errorf("保存聊天 agent 配置失败：%w", err)
	}
	return nil
}

func defaultString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
