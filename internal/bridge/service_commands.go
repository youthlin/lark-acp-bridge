package bridge

import (
	"context"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

func (s *Service) handleCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "" // text以/开头才会进来 这里不可能走到
	}
	if strings.HasPrefix(fields[0], "//") && len(fields[0]) > 2 {
		return s.forwardACPCommand(ctx, "/"+strings.TrimPrefix(commandRemainder(text, 0), "//"), msg)
	}
	switch fields[0] {
	case "/help":
		return strings.Join([]string{
			"当前支持的命令：",
			"/help - 查看帮助",
			"/new [cwd] [title] - 为当前会话创建新的 ACP 会话映射",
			"/agent [name] - 查看或切换当前聊天默认使用的 ACP agent",
			"/session list - 列出当前聊天的历史 ACP 会话",
			"/session resume <index> - 恢复 /session list 中的指定会话",
			"/session title <title> - 设置当前 ACP 会话标题",
			"/wiki on|off|status|interval <duration> - 管理当前聊天的自动知识沉淀",
			"/loop [-t 0] [-n 0] [-i 10s] <prompt> - 循环执行提示词直到 DONE、超时或达到轮次",
			"/loop add <补充消息>|status|stop - 补充下一轮 loop prompt、查看或停止当前会话的循环任务",
			"/queue <prompt> - 暂存提示词，当前任务结束后按顺序执行",
			"/schedule add <spec> <prompt> - 创建定时任务，spec 可用 @every 1h 或 5 段 cron",
			"/schedule how <自然语言需求> - 生成可直接执行的 /schedule add 命令",
			"/schedule list|status <id>|run <id>|edit <id> ...|pause <id>|resume <id>|delete <id> - 管理定时任务",
			"/cmds - 查看 ACP server 支持的 slash commands",
			"/cmds /command [args] - 透传执行 ACP slash command",
			"//command [args] - 透传执行 ACP slash command 的简写",
			"/config - 查看 ACP server 上报的配置项",
			"/config <id> - 查看指定配置项",
			"/config <id> <value> - 设置指定配置项",
			"/model - 打开模型选择卡片",
			"/model <model> - 设置当前会话模型",
			"/mode - 打开模式选择卡片",
			"/mode <mode> - 设置当前会话模式",
			"/usage [day|week|month|year] - 查看按 agent 和模型聚合的 token 用量",
			"/show step|plan|thought|tool|status|used on|off - 设置当前聊天流式卡片展示项",
			"/at - /at on: 必须at才响应; /at off auto|auto-reaction|every: 无需at, auto=自动判断且不加处理中表情, auto-reaction=自动判断并加处理中表情, every=每次响应",
			"/debug status|on|off - 查看或设置当前 bridge 进程 debug 日志",
			"/restart - 重启 bridge 服务，重启完成后自动回复确认",
			"/status - 查看服务状态",
			"",
			"普通文本消息会发送到当前会话的 ACP session；当前会话没有 session 时会自动创建。",
		}, "\n")
	case "/new":
		return s.newSession(ctx, fields, msg)
	case "/agent":
		return s.handleAgentCommand(ctx, text, msg)
	case "/session":
		return s.handleSessionCommand(ctx, text, msg)
	case "/wiki":
		return s.handleWikiCommand(ctx, text, msg)
	case "/loop":
		return s.handleLoopCommand(ctx, text, msg)
	case "/queue":
		return s.handleQueueCommand(ctx, text, msg)
	case "/schedule":
		return s.handleScheduleCommand(ctx, text, msg)
	case "/cmds":
		return s.handleCommandsCommand(ctx, text, msg)
	case "/config":
		return s.handleConfigCommand(ctx, text, msg)
	case "/model":
		return s.handleModelCommand(ctx, text, msg)
	case "/mode":
		return s.handleModeCommand(ctx, text, msg)
	case "/usage":
		return s.handleUsageCommand(text, msg)
	case "/show":
		return s.handleShowCommand(ctx, msg, text)
	case "/at":
		return s.handleAtCommand(ctx, msg, text)
	case "/debug":
		return s.handleDebugCommand(ctx, text)
	case "/restart":
		return s.handleRestartCommand(ctx, msg)
	case "/status":
		return s.status(msg)
	default:
		return "暂不支持这个命令。发送 /help 查看当前支持的命令。"
	}
}

func (s *Service) handleDebugCommand(ctx context.Context, text string) string {
	fields := strings.Fields(text)
	if len(fields) == 1 || len(fields) == 2 && strings.EqualFold(fields[1], "status") {
		return formatDebugStatus()
	}
	if len(fields) != 2 {
		return "请使用 /debug status、/debug on 或 /debug off。"
	}
	switch strings.ToLower(strings.TrimSpace(fields[1])) {
	case "on":
		logging.SetDebug(true)
		slog.InfoContext(ctx, "已开启 bridge debug 日志")
		return "已开启 bridge debug 日志。\n" + formatDebugStatus()
	case "off":
		logging.SetDebug(false)
		slog.InfoContext(ctx, "已关闭 bridge debug 日志")
		return "已关闭 bridge debug 日志。\n" + formatDebugStatus()
	default:
		return "请使用 /debug status、/debug on 或 /debug off。"
	}
}

func formatDebugStatus() string {
	if logging.DebugEnabled() {
		return "当前 bridge debug 日志：开启。"
	}
	return "当前 bridge debug 日志：关闭。"
}

func (s *Service) handleRestartCommand(ctx context.Context, msg feishu.Message) string {
	if !s.restartAllowed(msg) {
		if len(s.ownerOpenIDs(msg.BotID)) == 0 {
			return "未配置 bot owner，不能通过飞书重启 bridge 服务。"
		}
		return "只有 bot owner 可以重启 bridge 服务。"
	}
	workspace := strings.TrimSpace(msg.Workspace)
	if workspace == "" {
		return "当前 bot workspace 为空，无法记录重启确认消息。"
	}
	if err := s.validateRestartCommand(); err != nil {
		return err.Error()
	}
	if err := writeRestartAck(workspace, newRestartAck(msg)); err != nil {
		slog.ErrorContext(ctx, "记录重启确认消息失败", "错误", err)
		return "记录重启确认消息失败：" + err.Error()
	}
	if ok, err := s.sendIntermediateReply(ctx, msg, "收到，准备重启 bridge 服务。"); err != nil {
		removeRestartAck(workspace)
		slog.ErrorContext(ctx, "发送重启准备消息失败", "错误", err)
		return "发送重启准备消息失败：" + err.Error()
	} else if !ok {
		removeRestartAck(workspace)
		return "当前上下文不支持主动发送重启准备消息。"
	}
	go s.runRestartCommand(context.Background(), workspace)
	return ""
}

func (s *Service) validateRestartCommand() error {
	if s.restartCommand != nil || len(s.cfg.RestartCommand) > 0 || s.builtinRestart {
		return nil
	}
	return errBuiltinRestartUnavailable
}

func (s *Service) restartAllowed(msg feishu.Message) bool {
	return s.slashCommandAllowed(msg)
}

func (s *Service) slashCommandAllowed(msg feishu.Message) bool {
	senderID := strings.TrimSpace(msg.SenderID)
	if senderID == "" {
		return false
	}
	for _, ownerID := range s.ownerOpenIDs(msg.BotID) {
		if strings.TrimSpace(ownerID) == senderID {
			return true
		}
	}
	return false
}

func (s *Service) ownerOpenIDs(botID string) []string {
	botID = strings.TrimSpace(botID)
	for _, bot := range s.cfg.Bots {
		if strings.TrimSpace(bot.ID) == botID {
			return bot.OwnerOpenIDs
		}
	}
	if len(s.cfg.Bots) == 1 {
		return s.cfg.Bots[0].OwnerOpenIDs
	}
	return nil
}

func commandRemainder(text string, skipFields int) string {
	if skipFields <= 0 {
		return strings.TrimSpace(text)
	}
	i := 0
	skipped := 0
	for i < len(text) && skipped < skipFields {
		for i < len(text) {
			r, size := utf8.DecodeRuneInString(text[i:])
			if !unicode.IsSpace(r) {
				break
			}
			i += size
		}
		if i >= len(text) {
			return ""
		}
		for i < len(text) {
			r, size := utf8.DecodeRuneInString(text[i:])
			if unicode.IsSpace(r) {
				break
			}
			i += size
		}
		skipped++
	}
	return strings.TrimSpace(text[i:])
}

func (s *Service) status(msg feishu.Message) string {
	lines := []string{
		"服务运行中。",
		"已配置 ACP agent：" + strings.Join(s.registry.Names(), ", "),
		"当前聊天默认 agent：" + s.chatAgentName(msg),
		"当前 bot：" + displayBotID(msg.BotID),
	}
	if strings.TrimSpace(msg.Workspace) != "" {
		lines = append(lines, "workspace："+msg.Workspace)
	}
	if s.storeForMessage(msg) == nil {
		return strings.Join(lines, "\n")
	}
	if session, ok := s.findSession(msg); ok {
		lines = append(lines,
			sessionLabel(msg)+"：",
			"标题："+displaySessionTitle(session),
			"agent："+session.AgentName,
			"cwd："+session.Cwd,
			"session："+session.ACPSessionID,
		)
	} else {
		lines = append(lines, sessionLabel(msg)+"还没有会话映射；发送普通文本会自动创建，或用 /new <cwd> 指定工作目录。")
	}
	return strings.Join(lines, "\n")
}
