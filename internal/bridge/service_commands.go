package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

type slashCommandHandler func(*Service, context.Context, string, feishu.Message) string

type slashCommandSpec struct {
	name      string
	helpLines []string
	run       slashCommandHandler
}

const slashHelpCommandHelp = "/help - 查看帮助"

var slashHelpCommand = slashCommandSpec{
	name:      "/help",
	helpLines: []string{slashHelpCommandHelp},
	run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
		return s.handleHelpCommand()
	},
}

var slashCommandTable = append([]slashCommandSpec{slashHelpCommand}, slashRoutedCommandTable...)

var slashRoutedCommandTable = []slashCommandSpec{
	{
		name:      "/agent",
		helpLines: []string{"/agent [name] - 查看或切换当前聊天默认使用的 ACP agent"},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleAgentCommand(ctx, text, msg)
		},
	},
	{
		name:      "/at",
		helpLines: []string{"/at - /at on: 必须at才响应; /at off auto|auto-reaction|every: 无需at, auto=自动判断且不加处理中表情, auto-reaction=自动判断并加处理中表情, every=每次响应"},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleAtCommand(ctx, msg, text)
		},
	},
	{
		name:      "/debug",
		helpLines: []string{"/debug status|on|off - 查看或设置当前 bridge 进程 debug 日志"},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleDebugCommand(ctx, text)
		},
	},
	{
		name: "/drive_comment",
		helpLines: []string{
			"/drive_comment on|off|status - 管理当前 bot 的云文档评论监听处理（owner only）",
			"/drive_comment trace on|off|new - 设置评论处理过程卡片目的地，按目的群 /show 配置展示，trace new 会新建话题群",
		},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleDriveCommentCommand(ctx, text, msg)
		},
	},
	{
		name: "/cmds",
		helpLines: []string{
			"/cmds - 查看 ACP server 支持的 slash commands",
			"/cmds /command [args] - 透传执行 ACP slash command",
		},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleCommandsCommand(ctx, text, msg)
		},
	},
	{
		name: "/config",
		helpLines: []string{
			"/config - 查看 ACP server 上报的配置项",
			"/config <id> - 查看指定配置项",
			"/config <id> <value> - 设置指定配置项",
		},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleConfigCommand(ctx, text, msg)
		},
	},
	{
		name:      "/compact",
		helpLines: []string{"/compact [on <percent>|off] - 查看或配置当前会话自动 compact，例如 /compact on 80%"},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleCompactCommand(ctx, text, msg)
		},
	},
	{
		name:      "/card",
		helpLines: []string{"/card - 打开当前聊天的配置与状态全览卡"},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleCardCommand(ctx, msg)
		},
	},
	{
		name: "/loop",
		helpLines: []string{
			"/loop [-t 0] [-n 0] [-i 10s] <prompt> - 循环执行提示词直到 DONE、超时或达到轮次",
			"/loop add <补充消息>|status|stop - 补充下一轮 loop prompt、查看或停止当前会话的循环任务",
			"/loop how <自然语言需求> - 生成可直接执行的 /loop 命令",
		},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleLoopCommand(ctx, text, msg)
		},
	},
	{
		name: "/model",
		helpLines: []string{
			"/model - 打开模型选择卡片",
			"/model <model> - 设置当前会话模型",
		},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleModelCommand(ctx, text, msg)
		},
	},
	{
		name: "/mode",
		helpLines: []string{
			"/mode - 打开模式选择卡片",
			"/mode <mode> - 设置当前会话模式",
		},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleModeCommand(ctx, text, msg)
		},
	},
	{
		name: "/new",
		helpLines: []string{
			"/new [cwd] [title] - 为当前会话创建新的 ACP 会话映射",
			"/new chat [group|topic] [群标题] [mentions...] - 新建普通群或话题群，群主为触发人",
		},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleNewCommand(ctx, text, msg)
		},
	},
	{
		name:      "/queue",
		helpLines: []string{"/queue <prompt> - 暂存提示词，当前任务结束后按顺序执行"},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleQueueCommand(ctx, text, msg)
		},
	},
	{
		name:      "/sid",
		helpLines: []string{"/sid <acp_session_id> <prompt> - 将本条消息发送到指定 ACP session"},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleSIDCommand(ctx, text, msg)
		},
	},
	{
		name:      "/restart",
		helpLines: []string{"/restart - 重启 bridge 服务，重启完成后自动回复确认"},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleRestartCommand(ctx, msg)
		},
	},
	{
		name: "/update",
		helpLines: []string{
			"/update [--check] [--version <tag>] - 更新 bridge 二进制（只替换不重启，owner only）",
		},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleUpdateCommand(ctx, text, msg)
		},
	},
	{
		name: "/schedule",
		helpLines: []string{
			"/schedule add <spec> <prompt> - 创建定时任务，spec 可用 @every 1h 或 5 段 cron",
			"/schedule once <时间> <prompt> - 创建只执行一次的任务，时间用 YYYY-MM-DD HH:MM（按任务时区）或 RFC3339",
			"/schedule how <自然语言需求> - 生成可直接执行的 /schedule add 命令",
		},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleScheduleCommand(ctx, text, msg)
		},
	},
	{
		name: "/session",
		helpLines: []string{
			"/session list - 列出当前聊天的历史 ACP 会话",
			"/session resume <index> - 恢复 /session list 中的指定会话",
			"/session title <title> - 设置当前 ACP 会话标题",
		},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleSessionCommand(ctx, text, msg)
		},
	},
	{
		name:      "/show",
		helpLines: []string{"/show step|plan|thought|tool|status|used on|off - 设置当前聊天流式卡片展示项"},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleShowCommand(ctx, msg, text)
		},
	},
	{
		name:      "/status",
		helpLines: []string{"/status - 查看服务状态"},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.status(msg)
		},
	},
	{
		name:      "/trace",
		helpLines: []string{"/trace [status]|on [7d]|off - 查看或设置本地 ACP JSONL trace"},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleTraceCommand(ctx, text, msg)
		},
	},
	{
		name:      "/usage",
		helpLines: []string{"/usage [day|week|month|year] - 查看按 agent 和模型聚合的 token 用量"},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleUsageCommand(text, msg)
		},
	},
	{
		name: "/wiki",
		helpLines: []string{
			"/wiki on|off|status|lint|upgrade|interval <duration> - 管理当前聊天的自动知识沉淀和一致性检查",
			"/wiki trace on|off|new - 管理当前 bot 的自动知识沉淀过程卡片",
		},
		run: func(s *Service, ctx context.Context, text string, msg feishu.Message) string {
			return s.handleWikiCommand(ctx, text, msg)
		},
	},
}

func (s *Service) handleCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "" // text以/开头才会进来 这里不可能走到
	}
	if strings.HasPrefix(fields[0], "//") && len(fields[0]) > 2 {
		return s.forwardACPCommand(ctx, "/"+strings.TrimPrefix(commandRemainder(text, 0), "//"), msg)
	}
	if command, ok := lookupSlashCommand(fields[0]); ok {
		return command.run(s, ctx, text, msg)
	}
	return "暂不支持这个命令。发送 /help 查看当前支持的命令。"
}

func lookupSlashCommand(name string) (slashCommandSpec, bool) {
	name = strings.TrimSpace(name)
	for _, command := range slashCommandTable {
		if command.name == name {
			return command, true
		}
	}
	return slashCommandSpec{}, false
}

func lookupSlashCommandHelpIn(commands []slashCommandSpec, name string) string {
	command, ok := lookupSlashCommandIn(commands, name)
	if !ok {
		return ""
	}
	return strings.Join(command.helpLines, "\n")
}

func lookupSlashCommandIn(commands []slashCommandSpec, name string) (slashCommandSpec, bool) {
	name = strings.TrimSpace(name)
	for _, command := range commands {
		if command.name == name {
			return command, true
		}
	}
	return slashCommandSpec{}, false
}

func (s *Service) handleHelpCommand() string {
	lines := []string{
		"当前支持的命令：",
		slashHelpCommandHelp,
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/new"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/agent"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/session"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/wiki"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/loop"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/queue"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/sid"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/schedule"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/card"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/cmds"),
		"//command [args] - 透传执行 ACP slash command 的简写",
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/compact"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/config"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/model"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/mode"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/usage"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/show"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/at"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/debug"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/drive_comment"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/update"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/status"),
		lookupSlashCommandHelpIn(slashRoutedCommandTable, "/restart"),
		"",
		"普通文本消息会发送到当前会话的 ACP session；当前会话没有 session 时会自动创建。",
	}
	return strings.Join(lines, "\n")
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
	go func() {
		// The restart command must outlive the inbound request after the
		// acknowledgement has been sent, but it should not run forever.
		restartCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		s.runRestartCommand(restartCtx, workspace)
	}()
	return ""
}

func (s *Service) validateRestartCommand() error {
	if s.restartCommand != nil || len(s.configRestartCommand()) > 0 || s.builtinRestart {
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
	bot, ok := s.botConfig(botID)
	if !ok {
		return nil
	}
	return bot.OwnerOpenIDs
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
	if snapshot, ok := s.runtimeManagerSnapshot(); ok {
		lines = append(lines, "runtime："+formatRuntimeManagerStatus(snapshot))
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
		status := s.sessionRuntimeStatusSnapshot(session.Key)
		wikiStatus := s.wikiStatusSnapshot(session.Key)
		loopStatus, hasLoopStatus := s.loopStatusSnapshot(session.Key)
		acpError, hasACPError := s.acpErrorSnapshot(session)
		lines = append(lines,
			"运行态："+formatSessionBusyStatus(status),
			"runtime会话："+formatRuntimeSessionStatus(session.Key, s.runtimeSlotSnapshots()),
			"队列："+formatSessionQueueStatus(status),
			"wiki："+formatSessionWikiStatus(wikiStatus),
			"loop："+formatSessionLoopStatus(loopStatus, hasLoopStatus),
			"compact："+formatCompactStatusInline(session),
			"ACP错误："+formatACPErrorStatus(acpError, hasACPError),
		)
	} else {
		lines = append(lines, sessionLabel(msg)+"还没有会话映射；发送普通文本会自动创建，或用 /new <cwd> 指定工作目录。")
	}
	return strings.Join(lines, "\n")
}

func (s *Service) runtimeManagerSnapshot() (runtimeManagerSnapshot, bool) {
	runtime, ok := s.runtime.(*runtimeManager)
	if !ok || runtime == nil {
		return runtimeManagerSnapshot{}, false
	}
	return runtime.snapshot(), true
}

func (s *Service) runtimeSlotSnapshots() []runtimeSlotSnapshot {
	snapshot, ok := s.runtimeManagerSnapshot()
	if !ok {
		return nil
	}
	return snapshot.Slots
}

func formatSessionBusyStatus(status sessionRuntimeStatus) string {
	if !status.Busy {
		return "空闲"
	}
	switch status.RunningKind {
	case taskKindUser:
		return "忙碌（user）"
	case taskKindWiki:
		return "忙碌（wiki）"
	case taskKindLoop:
		return "忙碌（loop）"
	default:
		return "忙碌"
	}
}

func formatSessionQueueStatus(status sessionRuntimeStatus) string {
	draining := ""
	if status.QueueDraining {
		draining = "，正在执行"
	}
	return "待执行 " + strconv.Itoa(status.QueueLen) + " 条" + draining
}

func formatRuntimeManagerStatus(snapshot runtimeManagerSnapshot) string {
	limit := "不限"
	if snapshot.MaxSlots > 0 {
		limit = strconv.Itoa(snapshot.MaxSlots)
	}
	return fmt.Sprintf("slots %d，clients %d/%s，busy %d，idle %d，markers %d",
		snapshot.TotalSlots, snapshot.ClientSlots, limit, snapshot.ActiveSlots, snapshot.IdleSlots, snapshot.MarkerSlots)
}

func formatRuntimeSessionStatus(key SessionKey, slots []runtimeSlotSnapshot) string {
	key = normalizeSessionKey(key)
	if len(slots) == 0 {
		return "无"
	}
	total := 0
	clients := 0
	busy := 0
	idle := 0
	scopes := make([]string, 0, 2)
	seenScope := make(map[string]bool)
	for _, slot := range slots {
		if normalizeSessionKey(slot.Key.SessionKey) != key {
			continue
		}
		total++
		if slot.HasClient {
			clients++
		}
		if slot.Active > 0 {
			busy++
		}
		if slot.Idle {
			idle++
		}
		scope := slot.Key.Scope
		if scope == "" {
			scope = runtimeScopeCurrent
		}
		if !seenScope[scope] {
			seenScope[scope] = true
			scopes = append(scopes, scope)
		}
	}
	if total == 0 {
		return "无"
	}
	sort.Strings(scopes)
	text := fmt.Sprintf("slots %d，clients %d，busy %d，idle %d", total, clients, busy, idle)
	if len(scopes) > 0 {
		text += "，scope " + strings.Join(scopes, "/")
	}
	return text
}

func formatSessionWikiStatus(snapshot wikiStatusSnapshot) string {
	if snapshot.timerSet {
		return "等待定时触发"
	}
	if snapshot.status.running || snapshot.backgroundTask || (snapshot.foregroundTask != nil && snapshot.foregroundTask.kind == taskKindWiki) {
		return "正在反思"
	}
	if !snapshot.status.lastStarted.IsZero() {
		if snapshot.status.lastSuccess {
			return "最近一次成功"
		}
		return "最近一次失败"
	}
	return "尚未触发"
}

func formatSessionLoopStatus(status loopRunStatus, ok bool) string {
	if !ok || status.started.IsZero() {
		return "尚无状态"
	}
	parts := make([]string, 0, 4)
	if status.running {
		parts = append(parts, "运行中")
	} else {
		parts = append(parts, "已结束")
	}
	if status.round > 0 {
		parts = append(parts, "第 "+strconv.Itoa(status.round)+" 轮")
	}
	if status.reason != "" {
		parts = append(parts, "原因："+status.reason)
	}
	if strings.TrimSpace(status.pendingAdd) != "" {
		parts = append(parts, "有待处理补充消息")
	}
	return strings.Join(parts, "，")
}

func formatACPErrorStatus(snapshot acpErrorSnapshot, ok bool) string {
	if !ok || snapshot.occurred.IsZero() {
		return "无"
	}
	operation := strings.TrimSpace(snapshot.operation)
	if operation == "" {
		operation = "unknown"
	}
	message := strings.TrimSpace(snapshot.message)
	if message == "" {
		message = "无错误摘要"
	}
	return snapshot.occurred.Format("15:04:05") + "，" + operation + "：" + message
}
