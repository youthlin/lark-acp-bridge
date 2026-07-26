package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

// Service 本项目核心服务
type Service struct {
	cfg            config.Config            // 配置文件
	configPath     string                   // 配置文件路径，用于内置后台重启
	registry       *acp.Registry            // 对接的 acp, 比如 traex -> "traex acp serve"
	feishu         []*feishu.Adapter        // Bots 实例
	stores         map[string]*SessionStore // 会话存储, key=bot.id, 默认store用 "" 作为 key
	runtime        acpRuntime               // acp client 运行时
	restartCommand func(context.Context) error
	builtinRestart bool // 是否允许使用内置后台 restart

	taskMu          sync.Mutex
	tasks           map[SessionKey]*runningTask
	wikiTasks       map[runtimeKey]*runningTask
	wikiTimers      map[SessionKey]*pendingWikiRun
	wikiGenerations map[SessionKey]int64
	wikiStatuses    map[SessionKey]wikiRunStatus
	loopStatuses    map[SessionKey]loopRunStatus

	acpUpdateMu    sync.Mutex
	acpUpdateUnsub map[SessionKey]func()
}

type taskKind string

const (
	taskKindUser taskKind = "user"
	taskKindWiki taskKind = "wiki"
	taskKindLoop taskKind = "loop"

	defaultWikiInterval        = 5 * time.Minute
	defaultLoopInterval        = 10 * time.Second
	newSessionStateWait        = 600 * time.Millisecond
	newSessionPartialStateWait = 120 * time.Millisecond
)

type runningTask struct {
	kind     taskKind
	runtime  runtimeKey
	cancel   context.CancelFunc
	session  Session
	agent    config.AgentConfig
	onCancel func(context.Context, string)
}

type pendingWikiRun struct {
	timer      *time.Timer
	generation int64
	session    Session
	agent      config.AgentConfig
	scheduled  time.Time
}

type wikiRunStatus struct {
	running     bool
	lastStarted time.Time
	lastEnded   time.Time
	lastError   string
	lastSuccess bool
}

type loopRunStatus struct {
	running     bool
	started     time.Time
	ended       time.Time
	round       int
	maxRounds   int
	maxDuration time.Duration
	interval    time.Duration
	prompt      string
	reason      string
	lastError   string
}

type loopProgressState string

const (
	loopProgressStarted   loopProgressState = "started"
	loopProgressRunning   loopProgressState = "running"
	loopProgressCompleted loopProgressState = "completed"
	loopProgressFinished  loopProgressState = "finished"
)

type loopAnchor struct {
	message feishu.Message
	request loopRequest
	card    feishu.LoopStatusCard
}

type preparedPrompt struct {
	session Session
	agent   config.AgentConfig
	text    string
	errText string
}

// NewService 创建服务实例
//
//	@param cfg 服务配置
//	@param store 会话储存, 实际使用传nil即可, 单元测试可传 [NewSessionStore] 实例进来避免污染真实会话
func NewService(cfg config.Config, store *SessionStore) *Service {
	s := &Service{
		cfg:             cfg,
		registry:        acp.NewRegistry(cfg.Agents),
		stores:          make(map[string]*SessionStore),
		runtime:         newRuntimeManager(),
		tasks:           make(map[SessionKey]*runningTask),
		wikiTasks:       make(map[runtimeKey]*runningTask),
		wikiTimers:      make(map[SessionKey]*pendingWikiRun),
		wikiGenerations: make(map[SessionKey]int64),
		wikiStatuses:    make(map[SessionKey]wikiRunStatus),
		loopStatuses:    make(map[SessionKey]loopRunStatus),
		acpUpdateUnsub:  make(map[SessionKey]func()),
	}
	for _, bot := range cfg.Bots {
		// 见 [Service.HandleFeishuMessage], s 实现了 [feishu.Handler]
		s.feishu = append(s.feishu, feishu.NewAdapter(bot, s))
		if store != nil {
			s.stores[bot.ID] = store
		} else if strings.TrimSpace(bot.Workspace) != "" {
			s.stores[bot.ID] = NewSessionStore(filepath.Join(bot.Workspace, "sessions.json"))
		}
	}
	if store != nil {
		s.stores[""] = store
	}
	return s
}

func (s *Service) WithConfigPath(path string) *Service {
	s.configPath = strings.TrimSpace(path)
	return s
}

func (s *Service) WithBuiltinRestart(enabled bool) *Service {
	s.builtinRestart = enabled
	return s
}

// setRuntime 用于单元测试设置 fakeRuntime
func (s *Service) setRuntime(runtime acpRuntime) {
	if runtime != nil {
		s.runtime = runtime
	}
}

func (s *Service) setRestartCommand(command func(context.Context) error) {
	s.restartCommand = command
}

// Start 启动服务
func (s *Service) Start(ctx context.Context) error {
	if len(s.cfg.Agents) == 0 {
		return fmt.Errorf("未配置 ACP agent")
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
	s.cancelAllSessionWork(ctx)
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
	go func() {
		ackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := consumeRestartAck(ackCtx, bot.Workspace, adapter, bot.ID); err != nil {
			slog.WarnContext(ctx, "消费重启确认消息失败", "bot", displayBotID(bot.ID), "错误", err)
		}
	}()
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

var _ feishu.Handler = (*Service)(nil)
var _ feishu.ModelSelectionHandler = (*Service)(nil)
var _ feishu.ModeSelectionHandler = (*Service)(nil)
var _ feishu.SessionSelectionHandler = (*Service)(nil)

const maxSessionHistoryPerChat = 10

// HandleFeishuMessage 消息处理
// 实现 [feishu.Handler], 在 [NewService] 时将 [Service] 实例传入给了 [feishu.NewAdapter]
func (s *Service) HandleFeishuMessage(ctx context.Context, msg feishu.Message) (string, error) {
	if strings.TrimSpace(msg.BotOpenID) == "" {
		msg.BotOpenID = s.botOpenID(msg.BotID)
	}
	text := strings.TrimSpace(msg.Text)
	text = stripMentionNames(text, msg.Mentions)
	msg.Text = text
	promptText := strings.TrimSpace(msg.PromptText())
	slog.DebugContext(ctx, "处理解析后的消息", "text", text, "prompt_text", promptText)

	if s.shouldIgnoreMessage(msg, text) {
		slog.InfoContext(ctx, "群聊消息未 at bot，按当前 chat 配置跳过")
		return "", nil
	}
	if cleanup, ok := feishu.StartProcessingReaction(ctx, msg); ok {
		defer cleanup()
	}

	_, err := ensureWorkspace(msg.Workspace, msg.BotID)
	if err != nil {
		slog.ErrorContext(ctx, "初始化 workspace 失败", "workspace", msg.Workspace, "错误", err)
		return "初始化 workspace 失败：" + err.Error(), nil
	}
	if promptText == "" {
		return "暂不支持的消息类型。", nil
	}

	// 斜杠命令
	if strings.HasPrefix(text, "/") {
		if !s.slashCommandAllowed(msg) {
			if len(s.ownerOpenIDs(msg.BotID)) == 0 {
				return "未配置 bot owner，不能执行斜杠命令。", nil
			}
			return "只有 bot owner 可以执行斜杠命令。", nil
		}
		return s.handleCommand(ctx, text, msg), nil
	}
	// 普通消息
	return s.prompt(ctx, msg, promptText)
}

func (s *Service) handleCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "" // text以/开头才会进来 这里不可能走到
	}
	if strings.HasPrefix(fields[0], "//") && len(fields[0]) > 2 {
		return s.forwardACPCommand(ctx, "/"+strings.TrimPrefix(text, "//"), msg)
	}
	switch fields[0] {
	case "/help":
		return strings.Join([]string{
			"当前支持的命令：",
			"/help - 查看帮助",
			"/new [cwd] [title] - 为当前会话创建新的 ACP 会话映射",
			"/session list - 列出当前聊天的历史 ACP 会话",
			"/session resume <index> - 恢复 /session list 中的指定会话",
			"/session title <title> - 设置当前 ACP 会话标题",
			"/wiki on|off|status|interval <duration> - 管理当前聊天的自动知识沉淀",
			"/loop [-t 0] [-n 0] [-i 10s] <prompt> - 循环执行提示词直到 DONE、超时或达到轮次",
			"/loop status|stop - 查看或停止当前会话的循环任务",
			"/cmds - 查看 ACP server 支持的 slash commands",
			"/cmds /command [args] - 透传执行 ACP slash command",
			"//command [args] - 透传执行 ACP slash command 的简写",
			"/model - 打开模型选择卡片",
			"/model <model> - 设置当前会话模型",
			"/mode - 打开模式选择卡片",
			"/mode <mode> - 设置当前会话模式",
			"/show step|thought|tool|status|used on|off - 设置当前聊天流式卡片展示项",
			"/at status|on|off - 查看或设置当前群聊是否需要 at 才响应",
			"/debug status|on|off - 查看或设置当前 bridge 进程 debug 日志",
			"/restart - 重启 bridge 服务，重启完成后自动回复确认",
			"/status - 查看服务状态",
			"",
			"普通文本消息会发送到当前会话的 ACP session；当前会话没有 session 时会自动创建。",
		}, "\n")
	case "/new":
		if isTopicGroupMessage(msg) {
			return "话题群内不支持使用 /new 手动切换会话；新发一条话题消息会自动创建独立会话。"
		}
		return s.newSession(ctx, fields, msg)
	case "/session":
		if isTopicGroupMessage(msg) {
			return "话题群内不支持使用 /session 命令；每条话题会自动维护独立会话。"
		}
		return s.handleSessionCommand(ctx, text, msg)
	case "/wiki":
		return s.handleWikiCommand(ctx, text, msg)
	case "/loop":
		return s.handleLoopCommand(ctx, text, msg)
	case "/cmds":
		return s.handleCommandsCommand(ctx, text, msg)
	case "/model":
		return s.handleModelCommand(ctx, text, msg)
	case "/mode":
		return s.handleModeCommand(ctx, text, msg)
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
	if ok, err := feishu.SendIntermediateReply(ctx, msg, "收到，准备重启 bridge 服务。"); err != nil {
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

func (s *Service) handleShowCommand(ctx context.Context, msg feishu.Message, text string) string {
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	chat := s.chatConfigForMessage(msg)
	fields := strings.Fields(text)
	if len(fields) == 1 || len(fields) == 2 && fields[1] == "status" {
		return formatShowStatus(chat)
	}
	if len(fields) != 3 {
		return "请使用 /show step|thought|tool|status|used on|off。"
	}
	value, ok := parseShowSwitch(fields[2])
	if !ok {
		return "请使用 on 或 off，例如 /show thought off。"
	}
	target, ok := setChatShowOption(&chat, fields[1], value)
	if !ok {
		return "请使用 /show step|thought|tool|status|used on|off。"
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
	case "thought":
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
		"思考消息：" + showState(!chat.HideThoughts),
		"工具调用：" + showState(!chat.HideTools),
		"状态栏：" + showState(!chat.HideStatusBar),
		"用量明细：" + showState(!chat.HideUsageDetail),
	}, "\n")
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
	if len(fields) < 2 || fields[1] == "status" {
		return s.formatAtStatus(msg)
	}
	chat := s.chatConfigForMessage(msg)
	switch fields[1] {
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

func (s *Service) handleSessionCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return "可用命令：/session list、/session resume <index> 或 /session title <title>"
	}
	switch fields[1] {
	case "list":
		return s.sendSessionList(ctx, msg)
	case "resume":
		if len(fields) < 3 {
			return "请使用 /session resume <index> 指定要恢复的会话序号。"
		}
		index, err := strconv.Atoi(fields[2])
		if err != nil || index <= 0 {
			return "会话序号必须是正整数。"
		}
		return s.resumeSession(ctx, msg, index)
	case "title":
		title := strings.TrimSpace(strings.TrimPrefix(text, strings.Join(fields[:2], " ")))
		if title == "" {
			return "请使用 /session title <title> 设置当前会话标题。"
		}
		return s.setSessionTitle(ctx, msg, title)
	default:
		return "暂不支持这个 session 命令。可用 /session list、/session resume <index> 或 /session title <title>。"
	}
}

func (s *Service) handleCommandsCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) >= 2 {
		command := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
		return s.forwardACPCommand(ctx, command, msg)
	}
	session, ok := s.findSession(msg)
	if !ok || strings.TrimSpace(session.ACPSessionID) == "" {
		return "当前会话还没有 ACP session，发送普通文本或 /new 后再查看 ACP commands。"
	}
	if len(session.AvailableCommands) == 0 {
		return "当前 ACP server 还没有上报可用命令。可以先发送一条普通消息，等 server 返回 available_commands_update 后再查看。"
	}
	lines := []string{"当前 ACP server 支持的命令："}
	for _, cmd := range session.AvailableCommands {
		name := strings.TrimSpace(cmd.Name)
		if name == "" {
			continue
		}
		line := "/" + name
		if desc := strings.TrimSpace(cmd.Description); desc != "" {
			line += " - " + desc
		}
		if cmd.Input != nil && strings.TrimSpace(cmd.Input.Hint) != "" {
			line += "（参数：" + strings.TrimSpace(cmd.Input.Hint) + "）"
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", "执行命令：/cmds /review ...，或简写为 //review ...")
	return strings.Join(lines, "\n")
}

func (s *Service) forwardACPCommand(ctx context.Context, command string, msg feishu.Message) string {
	command = strings.TrimSpace(command)
	if !strings.HasPrefix(command, "/") || strings.HasPrefix(command, "//") {
		return "请使用 /cmds /command [args]，或简写为 //command [args]。"
	}
	session, ok := s.findSession(msg)
	if !ok || strings.TrimSpace(session.ACPSessionID) == "" {
		return "当前会话还没有 ACP session，发送普通文本或 /new 后再执行 ACP command。"
	}
	name := strings.TrimPrefix(strings.Fields(command)[0], "/")
	if len(session.AvailableCommands) > 0 && !sessionHasCommand(session, name) {
		return "当前 ACP server 未上报该命令：" + "/" + name + "。发送 /cmds 查看可用命令。"
	}
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		return "未找到 agent 配置：" + session.AgentName
	}
	result, _, err := s.runUserPrompt(ctx, msg, session, agent, command)
	reply := result.Text
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return ""
		}
		if strings.TrimSpace(reply) != "" {
			return reply
		}
		return "执行 ACP command 失败：" + err.Error()
	}
	if strings.TrimSpace(reply) == "" {
		return "ACP command 已执行完成。"
	}
	return reply
}

func sessionHasCommand(session Session, name string) bool {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	for _, cmd := range session.AvailableCommands {
		if cmd.Name == name {
			return true
		}
	}
	return false
}

func formatModelStatus(session Session) string {
	lines := []string{"当前会话模型："}
	current := currentModelDisplay(session)
	if current == "" {
		current = "未知"
	}
	lines = append(lines, current)
	modelOpt, hasModelOpt := findModelConfigOption(session)
	if hasModelOpt && len(modelOpt.Options) > 0 {
		lines = append(lines, "", "可用模型：")
		for _, opt := range modelOpt.Options {
			if strings.TrimSpace(opt.Value) == "" {
				continue
			}
			marker := ""
			if opt.Value == modelValueString(modelOpt.CurrentValue) {
				marker = " *"
			}
			label := opt.Value
			if strings.TrimSpace(opt.Name) != "" && opt.Name != opt.Value {
				label += " - " + strings.TrimSpace(opt.Name)
			}
			lines = append(lines, marker+" "+label)
		}
	} else if session.Models != nil && len(session.Models.AvailableModels) > 0 {
		lines = append(lines, "", "可用模型：")
		for _, model := range session.Models.AvailableModels {
			if strings.TrimSpace(model.ModelID) == "" {
				continue
			}
			marker := ""
			if model.ModelID == session.Models.CurrentModelID {
				marker = " *"
			}
			label := model.ModelID
			if strings.TrimSpace(model.Name) != "" && model.Name != model.ModelID {
				label += " - " + strings.TrimSpace(model.Name)
			}
			lines = append(lines, marker+" "+label)
		}
	} else {
		lines = append(lines, "", "当前 ACP server 还没有上报可用模型。")
	}
	lines = append(lines, "", "设置模型：/model <model>")
	return strings.Join(lines, "\n")
}

func formatModeStatus(session Session) string {
	lines := []string{"当前会话模式："}
	current := currentModeDisplay(session)
	if current == "" {
		current = "未知"
	}
	lines = append(lines, current)
	modeOpt, hasModeOpt := findModeConfigOption(session)
	if hasModeOpt && len(modeOpt.Options) > 0 {
		lines = append(lines, "", "可用模式：")
		for _, opt := range modeOpt.Options {
			if strings.TrimSpace(opt.Value) == "" {
				continue
			}
			marker := ""
			if opt.Value == configOptionValueString(modeOpt.CurrentValue) {
				marker = " *"
			}
			label := opt.Value
			if strings.TrimSpace(opt.Name) != "" && opt.Name != opt.Value {
				label += " - " + strings.TrimSpace(opt.Name)
			}
			lines = append(lines, marker+" "+label)
		}
	} else if session.Mode != nil && len(session.Mode.AvailableModes) > 0 {
		lines = append(lines, "", "可用模式：")
		for _, mode := range session.Mode.AvailableModes {
			if strings.TrimSpace(mode.ModeID) == "" {
				continue
			}
			marker := ""
			if mode.ModeID == session.Mode.CurrentModeID {
				marker = " *"
			}
			label := mode.ModeID
			if strings.TrimSpace(mode.Name) != "" && mode.Name != mode.ModeID {
				label += " - " + strings.TrimSpace(mode.Name)
			}
			lines = append(lines, marker+" "+label)
		}
	} else {
		lines = append(lines, "", "当前 ACP server 还没有上报可用模式。")
	}
	lines = append(lines, "", "设置模式：/mode <mode>")
	return strings.Join(lines, "\n")
}

func currentModeDisplay(session Session) string {
	if modeOpt, ok := findModeConfigOption(session); ok {
		current := configOptionValueString(modeOpt.CurrentValue)
		if current != "" {
			return current
		}
	}
	if session.Mode != nil {
		return strings.TrimSpace(session.Mode.CurrentModeID)
	}
	return ""
}

func currentModelDisplay(session Session) string {
	if modelOpt, ok := findModelConfigOption(session); ok {
		current := configOptionValueString(modelOpt.CurrentValue)
		if current != "" {
			return current
		}
	}
	if session.Models != nil {
		return strings.TrimSpace(session.Models.CurrentModelID)
	}
	return ""
}

func findModeConfigOption(session Session) (acp.SessionConfigOption, bool) {
	for _, opt := range session.ConfigOptions {
		if opt.ID == "mode" || opt.Category == "mode" {
			return opt, true
		}
	}
	return acp.SessionConfigOption{}, false
}

func findModelConfigOption(session Session) (acp.SessionConfigOption, bool) {
	for _, opt := range session.ConfigOptions {
		if opt.ID == "model" || opt.Category == "model" {
			return opt, true
		}
	}
	return acp.SessionConfigOption{}, false
}

func resolveConfigOptionValue(opt acp.SessionConfigOption, target string) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	if len(opt.Options) == 0 {
		return target, true
	}
	for _, value := range opt.Options {
		if value.Value == target || strings.EqualFold(value.Name, target) {
			return value.Value, true
		}
	}
	return "", false
}

func resolveModelValue(opt acp.SessionConfigOption, target string) (string, bool) {
	return resolveConfigOptionValue(opt, target)
}

func resolveModeValue(opt acp.SessionConfigOption, target string) (string, bool) {
	return resolveConfigOptionValue(opt, target)
}

func modelValueString(value any) string {
	return configOptionValueString(value)
}

func configOptionValueString(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func (s *Service) handleModelCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	session, ok := s.findSession(msg)
	if !ok || strings.TrimSpace(session.ACPSessionID) == "" {
		return "当前会话还没有 ACP session，发送普通文本或 /new 后再查看或设置模型。"
	}
	if len(fields) == 1 {
		return s.sendModelSelectionCard(ctx, msg, session)
	}
	target := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	if target == "" {
		return "请使用 /model <model> 设置当前会话模型。"
	}
	value, _, err := s.setSessionModel(ctx, msg, session, target)
	if err != nil {
		if errors.Is(err, errUnknownModel) {
			return "未知模型：" + target + "\n\n" + formatModelStatus(session)
		}
		return err.Error()
	}
	if value == "" {
		return "未知模型：" + target + "\n\n" + formatModelStatus(session)
	}
	return "已设置当前会话模型：" + value
}

var errUnknownModel = errors.New("未知模型")

func (s *Service) sendModelSelectionCard(ctx context.Context, msg feishu.Message, session Session) string {
	modelOpt, ok := findModelConfigOption(session)
	if !ok {
		return "当前 ACP server 没有上报 model 配置项，无法通过 /model 设置。"
	}
	options := modelSelectionOptions(session, modelOpt)
	if len(options) == 0 {
		return "当前 ACP server 没有上报可选模型，请使用 /model <model> 设置。"
	}
	sent, err := feishu.SendModelSelectionCard(ctx, msg, feishu.ModelSelectionCard{
		BotID:        session.Key.BotID,
		ChatID:       session.Key.ChatID,
		ThreadID:     session.Key.ThreadID,
		ACPSessionID: session.ACPSessionID,
		RequesterID:  msg.SenderID,
		CurrentModel: currentModelDisplay(session),
		Options:      options,
	})
	if err != nil {
		slog.ErrorContext(ctx, "发送模型选择卡片失败", "错误", err)
		return "发送模型选择卡片失败：" + err.Error()
	}
	if !sent {
		return formatModelStatus(session)
	}
	return ""
}

func modelSelectionOptions(session Session, modelOpt acp.SessionConfigOption) []feishu.ModelOption {
	options := make([]feishu.ModelOption, 0, len(modelOpt.Options))
	for _, option := range modelOpt.Options {
		if strings.TrimSpace(option.Value) == "" {
			continue
		}
		options = append(options, feishu.ModelOption{Value: option.Value, Name: option.Name})
	}
	if len(options) > 0 || session.Models == nil {
		return options
	}
	options = make([]feishu.ModelOption, 0, len(session.Models.AvailableModels))
	for _, model := range session.Models.AvailableModels {
		if strings.TrimSpace(model.ModelID) == "" {
			continue
		}
		options = append(options, feishu.ModelOption{Value: model.ModelID, Name: model.Name})
	}
	return options
}

func (s *Service) handleModeCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	session, ok := s.findSession(msg)
	if !ok || strings.TrimSpace(session.ACPSessionID) == "" {
		return "当前会话还没有 ACP session，发送普通文本或 /new 后再查看或设置模式。"
	}
	if len(fields) == 1 {
		return s.sendModeSelectionCard(ctx, msg, session)
	}
	target := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	if target == "" {
		return "请使用 /mode <mode> 设置当前会话模式。"
	}
	value, _, err := s.setSessionMode(ctx, msg, session, target)
	if err != nil {
		if errors.Is(err, errUnknownMode) {
			return "未知模式：" + target + "\n\n" + formatModeStatus(session)
		}
		return err.Error()
	}
	if value == "" {
		return "未知模式：" + target + "\n\n" + formatModeStatus(session)
	}
	return "已设置当前会话模式：" + value
}

var errUnknownMode = errors.New("未知模式")

func (s *Service) sendModeSelectionCard(ctx context.Context, msg feishu.Message, session Session) string {
	modeOpt, ok := findModeConfigOption(session)
	if !ok {
		return "当前 ACP server 没有上报 mode 配置项，无法通过 /mode 设置。"
	}
	options := modeSelectionOptions(session, modeOpt)
	if len(options) == 0 {
		return "当前 ACP server 没有上报可选模式，请使用 /mode <mode> 设置。"
	}
	sent, err := feishu.SendModeSelectionCard(ctx, msg, feishu.ModeSelectionCard{
		BotID:        session.Key.BotID,
		ChatID:       session.Key.ChatID,
		ThreadID:     session.Key.ThreadID,
		ACPSessionID: session.ACPSessionID,
		RequesterID:  msg.SenderID,
		CurrentMode:  currentModeDisplay(session),
		Options:      options,
	})
	if err != nil {
		slog.ErrorContext(ctx, "发送模式选择卡片失败", "错误", err)
		return "发送模式选择卡片失败：" + err.Error()
	}
	if !sent {
		return formatModeStatus(session)
	}
	return ""
}

func modeSelectionOptions(session Session, modeOpt acp.SessionConfigOption) []feishu.ModeOption {
	options := make([]feishu.ModeOption, 0, len(modeOpt.Options))
	for _, option := range modeOpt.Options {
		if strings.TrimSpace(option.Value) == "" {
			continue
		}
		options = append(options, feishu.ModeOption{Value: option.Value, Name: option.Name})
	}
	if len(options) > 0 || session.Mode == nil {
		return options
	}
	options = make([]feishu.ModeOption, 0, len(session.Mode.AvailableModes))
	for _, mode := range session.Mode.AvailableModes {
		if strings.TrimSpace(mode.ModeID) == "" {
			continue
		}
		options = append(options, feishu.ModeOption{Value: mode.ModeID, Name: mode.Name})
	}
	return options
}

func (s *Service) HandleModeSelection(ctx context.Context, selection feishu.ModeSelection) (string, error) {
	if selection.RequesterID != "" && selection.OperatorID != "" && selection.RequesterID != selection.OperatorID {
		return "", fmt.Errorf("只有发起该命令的用户可以设置模式")
	}
	msg := feishu.Message{
		BotID:    selection.BotID,
		ChatID:   selection.ChatID,
		ThreadID: selection.ThreadID,
	}
	store := s.storeForMessage(msg)
	if store == nil {
		return "", fmt.Errorf("会话持久化未初始化")
	}
	key := SessionKey{BotID: selection.BotID, ChatID: selection.ChatID, ThreadID: selection.ThreadID}
	session, ok := store.Get(key)
	if !ok || session.ACPSessionID != selection.ACPSessionID {
		return "", fmt.Errorf("该模式选择卡片已过期，请重新发送 /mode")
	}
	_, display, err := s.setSessionMode(ctx, msg, session, selection.Mode)
	if err != nil {
		return "", err
	}
	return display, nil
}

func (s *Service) setSessionMode(ctx context.Context, msg feishu.Message, session Session, target string) (string, string, error) {
	modeOpt, ok := findModeConfigOption(session)
	if !ok {
		return "", "", fmt.Errorf("当前 ACP server 没有上报 mode 配置项，无法设置模式")
	}
	value, ok := resolveModeValue(modeOpt, target)
	if !ok {
		return "", "", fmt.Errorf("%w：%s", errUnknownMode, target)
	}
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		return "", "", fmt.Errorf("未找到 agent 配置：%s", session.AgentName)
	}
	options, err := s.runtime.SetConfigOption(ctx, session, agent, modeOpt.ID, value)
	if err != nil {
		slog.ErrorContext(ctx, "设置 ACP mode 失败", "mode", value, "错误", err)
		return "", "", fmt.Errorf("设置模式失败：%w", err)
	}
	session.ConfigOptions = options
	if modeOpt, ok := findModeConfigOption(session); ok {
		if configOptionValueString(modeOpt.CurrentValue) == value && session.Mode != nil {
			session.Mode.CurrentModeID = value
		}
	}
	s.saveSessionState(ctx, msg, session)
	return value, configOptionDisplayName(modeOpt, value), nil
}

func (s *Service) HandleModelSelection(ctx context.Context, selection feishu.ModelSelection) (string, error) {
	if selection.RequesterID != "" && selection.OperatorID != "" && selection.RequesterID != selection.OperatorID {
		return "", fmt.Errorf("只有发起该命令的用户可以设置模型")
	}
	msg := feishu.Message{
		BotID:    selection.BotID,
		ChatID:   selection.ChatID,
		ThreadID: selection.ThreadID,
	}
	store := s.storeForMessage(msg)
	if store == nil {
		return "", fmt.Errorf("会话持久化未初始化")
	}
	key := SessionKey{BotID: selection.BotID, ChatID: selection.ChatID, ThreadID: selection.ThreadID}
	session, ok := store.Get(key)
	if !ok || session.ACPSessionID != selection.ACPSessionID {
		return "", fmt.Errorf("该模型选择卡片已过期，请重新发送 /model")
	}
	_, display, err := s.setSessionModel(ctx, msg, session, selection.Model)
	if err != nil {
		return "", err
	}
	return display, nil
}

func (s *Service) setSessionModel(ctx context.Context, msg feishu.Message, session Session, target string) (string, string, error) {
	modelOpt, ok := findModelConfigOption(session)
	if !ok {
		return "", "", fmt.Errorf("当前 ACP server 没有上报 model 配置项，无法设置模型")
	}
	value, ok := resolveModelValue(modelOpt, target)
	if !ok {
		return "", "", fmt.Errorf("%w：%s", errUnknownModel, target)
	}
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		return "", "", fmt.Errorf("未找到 agent 配置：%s", session.AgentName)
	}
	options, err := s.runtime.SetConfigOption(ctx, session, agent, modelOpt.ID, value)
	if err != nil {
		slog.ErrorContext(ctx, "设置 ACP model 失败", "model", value, "错误", err)
		return "", "", fmt.Errorf("设置模型失败：%w", err)
	}
	session.ConfigOptions = options
	if modelOpt, ok := findModelConfigOption(session); ok {
		if modelValueString(modelOpt.CurrentValue) == value && session.Models != nil {
			session.Models.CurrentModelID = value
		}
	}
	s.saveSessionState(ctx, msg, session)
	return value, modelOptionName(modelOpt, value), nil
}

func modelOptionName(opt acp.SessionConfigOption, value string) string {
	return configOptionDisplayName(opt, value)
}

func configOptionDisplayName(opt acp.SessionConfigOption, value string) string {
	for _, option := range opt.Options {
		if option.Value != value {
			continue
		}
		name := strings.TrimSpace(option.Name)
		if name != "" && name != value {
			return name + "（" + value + "）"
		}
		break
	}
	return value
}

func (s *Service) handleWikiCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return "可用命令：/wiki on、/wiki off、/wiki status 或 /wiki interval <duration>。"
	}
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	chat := s.chatConfigForMessage(msg)
	switch fields[1] {
	case "on":
		chat.WikiDisabled = false
		if err := store.UpsertChat(chat); err != nil {
			slog.ErrorContext(ctx, "保存 wiki 配置失败", "错误", err)
			return "保存 wiki 配置失败：" + err.Error()
		}
		return "已开启当前聊天的自动知识沉淀。"
	case "off":
		chat.WikiDisabled = true
		if session, ok := s.findSession(msg); ok {
			s.cancelWikiTimer(session.Key)
			s.cancelWikiTasks(ctx, session.Key)
		}
		if err := store.UpsertChat(chat); err != nil {
			slog.ErrorContext(ctx, "保存 wiki 配置失败", "错误", err)
			return "保存 wiki 配置失败：" + err.Error()
		}
		return "已关闭当前聊天的自动知识沉淀。"
	case "status":
		return s.wikiStatus(msg, chat)
	case "interval":
		if len(fields) < 3 {
			return "请使用 /wiki interval <duration> 指定时间，例如 /wiki interval 5m。"
		}
		interval, err := parseWikiInterval(fields[2])
		if err != nil {
			return err.Error()
		}
		chat.WikiIntervalSec = int(interval.Seconds())
		if err := store.UpsertChat(chat); err != nil {
			slog.ErrorContext(ctx, "保存 wiki interval 失败", "错误", err)
			return "保存 wiki interval 失败：" + err.Error()
		}
		if session, ok := s.findSession(msg); ok && s.hasWikiTimer(session.Key) {
			if agent, ok := s.registry.Get(session.AgentName); ok {
				s.scheduleWikiAfterUserPrompt(session, agent)
			}
		}
		return "已设置当前聊天自动知识沉淀延迟：" + formatDuration(interval) + "。"
	default:
		return "暂不支持这个 wiki 命令。可用 /wiki on、/wiki off、/wiki status 或 /wiki interval <duration>。"
	}
}

func (s *Service) handleLoopCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) >= 2 {
		switch fields[1] {
		case "status":
			return s.loopStatus(msg)
		case "stop":
			session, ok := s.findSession(msg)
			if !ok {
				return "当前会话没有正在运行的 loop。"
			}
			if s.cancelLoopTask(ctx, session.Key, "已手动停止") {
				return "已停止当前会话的 loop。"
			}
			return "当前会话没有正在运行的 loop。"
		}
	}
	req, err := parseLoopRequest(text)
	if err != nil {
		return err.Error()
	}
	prepared, err := s.preparePrompt(ctx, msg, req.Prompt)
	if err != nil {
		return "启动 loop 失败：" + err.Error()
	}
	if prepared.errText != "" {
		return prepared.errText
	}
	s.subscribeACPStateUpdates(ctx, msg, prepared.session.Key)
	session := s.updateAutomaticSessionTitle(ctx, msg, prepared.session, req.Prompt)
	startText := loopAnchorText(req, loopProgressStarted, 0, "", time.Now())
	cardReq := feishu.LoopStatusCardRequest{
		BotID:        session.Key.BotID,
		ChatID:       session.Key.ChatID,
		ThreadID:     session.Key.ThreadID,
		ACPSessionID: session.ACPSessionID,
		Text:         startText,
	}
	if card, ok, err := feishu.SendLoopStatusCard(ctx, msg, cardReq); err != nil {
		return "启动 loop 失败：" + err.Error()
	} else if ok {
		anchor := loopAnchor{message: loopAnchorMessage(msg, card.Message()), request: req, card: card}
		s.startLoop(ctx, msg, anchor, session, prepared.agent, req)
		return ""
	}
	s.startLoop(ctx, msg, loopAnchor{}, session, prepared.agent, req)
	return startText
}

func (s *Service) startLoop(ctx context.Context, msg feishu.Message, anchor loopAnchor, session Session, agent config.AgentConfig, req loopRequest) {
	started := time.Now()
	ctx, finish := s.startTask(context.WithoutCancel(ctx), session, agent, taskKindLoop)
	s.markLoopStarted(session.Key, started, req)
	s.setTaskCancelHandler(session.Key, func(cancelCtx context.Context, reason string) {
		s.updateLoopAnchor(cancelCtx, anchor, loopProgressFinished, 0, reason)
	})
	go func() {
		defer finish()
		reason, err := s.runLoop(ctx, msg, anchor, session, agent, req, started)
		s.markLoopFinished(session.Key, started, reason, err)
		s.updateLoopFinished(ctx, msg, anchor, reason, err)
	}()
}

func (s *Service) runLoop(ctx context.Context, msg feishu.Message, anchor loopAnchor, session Session, agent config.AgentConfig, req loopRequest, started time.Time) (string, error) {
	var deadline time.Time
	if req.MaxDuration > 0 {
		deadline = started.Add(req.MaxDuration)
	}
	basePrompt := promptTextWithReplyContext(msg, req.Prompt)
	cardMsg := loopRoundMessage(msg, anchor)
	for round := 1; ; round++ {
		if req.MaxRounds > 0 && round > req.MaxRounds {
			return "已达到最大轮次", nil
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return "已达到最长运行时间", nil
		}
		s.markLoopRound(session.Key, started, round)
		s.updateLoopAnchor(ctx, anchor, loopProgressRunning, round, "")
		roundPrompt := promptTextWithWorkspaceContext(sessionWorkspace(session, msg), msg, loopPrompt(basePrompt, req, round, started, deadline))
		result, _, rawResult, streamedReply, err := s.promptRuntimeWithProgressRaw(ctx, cardMsg, session, agent, roundPrompt)
		s.updateLoopAnchor(ctx, anchor, loopProgressCompleted, round, "")
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return "已取消", context.Canceled
			}
			if strings.TrimSpace(rawResult.Text) != "" || strings.TrimSpace(result.Text) != "" {
				return "执行失败：" + err.Error(), err
			}
			return "执行失败：" + err.Error(), err
		}
		if loopDone(rawResult.Text) || loopDone(result.Text) || loopDone(streamedReply) {
			return "agent 返回 DONE", nil
		}
		if req.MaxRounds > 0 && round >= req.MaxRounds {
			return "已达到最大轮次", nil
		}
		if !deadline.IsZero() && !time.Now().Add(req.Interval).Before(deadline) {
			return "已达到最长运行时间", nil
		}
		timer := time.NewTimer(req.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "已取消", ctx.Err()
		case <-timer.C:
		}
	}
}

func loopPrompt(promptText string, req loopRequest, round int, started time.Time, deadline time.Time) string {
	maxDuration := "不限"
	if req.MaxDuration > 0 {
		maxDuration = formatDuration(req.MaxDuration)
	}
	maxRounds := "不限"
	if req.MaxRounds > 0 {
		maxRounds = strconv.Itoa(req.MaxRounds)
	}
	deadlineText := "不限"
	if !deadline.IsZero() {
		deadlineText = deadline.Format(time.RFC3339)
	}
	prefixes := []string{
		"## Loop Metadata\n" + strings.Join([]string{
			"round: " + strconv.Itoa(round),
			"started_at: " + started.Format(time.RFC3339),
			"deadline: " + deadlineText,
			"max_duration: " + maxDuration,
			"max_rounds: " + maxRounds,
			"interval: " + formatDuration(req.Interval),
		}, "\n"),
		"## Loop Stop Rules\n" + strings.Join([]string{
			"这是 /loop 自动循环任务的一轮。",
			"如果用户目标已经完成、无需继续，或继续执行没有新增价值，最终回复必须只输出 DONE。",
			"如果还需要继续推进，请正常执行本轮工作并说明结果。",
			"不要因为这是循环任务而空转。",
		}, "\n"),
	}
	return promptWithUserMessage(prefixes, promptText)
}

func loopDone(text string) bool {
	return strings.TrimSpace(text) == "DONE"
}

func loopAnchorMessage(original feishu.Message, sent feishu.SentMessage) feishu.Message {
	msg := original
	msg.MessageID = strings.TrimSpace(sent.MessageID)
	msg.ChatID = firstNonEmptyString(sent.ChatID, original.ChatID)
	msg.ChatType = firstNonEmptyString(sent.ChatType, original.ChatType)
	msg.ThreadID = strings.TrimSpace(sent.ThreadID)
	msg.RootID = strings.TrimSpace(sent.RootID)
	msg.ParentID = strings.TrimSpace(sent.ParentID)
	msg.Text = ""
	msg.Reply = nil
	return msg
}

func loopRoundMessage(original feishu.Message, anchor loopAnchor) feishu.Message {
	if strings.TrimSpace(anchor.message.MessageID) == "" {
		return original
	}
	msg := anchor.message
	msg.BotID = firstNonEmptyString(msg.BotID, original.BotID)
	msg.BotOpenID = firstNonEmptyString(msg.BotOpenID, original.BotOpenID)
	msg.Workspace = firstNonEmptyString(msg.Workspace, original.Workspace)
	msg.SenderID = original.SenderID
	msg.SenderType = original.SenderType
	msg.ForceReplyInThread = true
	return msg
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (s *Service) updateLoopAnchor(ctx context.Context, anchor loopAnchor, state loopProgressState, round int, reason string) bool {
	if anchor.card == nil {
		return false
	}
	text := loopAnchorText(anchor.request, state, round, reason, time.Now())
	cardCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	var err error
	if state == loopProgressFinished {
		err = anchor.card.Finish(cardCtx, text)
	} else {
		err = anchor.card.Update(cardCtx, text)
	}
	if err != nil {
		messageID := strings.TrimSpace(anchor.message.MessageID)
		slog.WarnContext(ctx, "更新 loop 启动卡片失败", "message_id", messageID, "错误", err)
		return false
	}
	return true
}

func loopAnchorText(req loopRequest, state loopProgressState, round int, reason string, now time.Time) string {
	lines := []string{
		"已启动 loop。",
		formatLoopRequest(req),
		"",
		"状态：" + loopProgressText(state, round),
	}
	if strings.TrimSpace(reason) != "" {
		lines = append(lines, "结束原因："+strings.TrimSpace(reason))
	}
	if !now.IsZero() {
		lines = append(lines, "更新时间："+now.Format(time.RFC3339))
	}
	return strings.Join(lines, "\n")
}

func loopFinishedText(reason string, now time.Time) string {
	lines := []string{
		"loop 已结束。",
		"状态：" + loopProgressText(loopProgressFinished, 0),
	}
	if strings.TrimSpace(reason) != "" {
		lines = append(lines, "结束原因："+strings.TrimSpace(reason))
	}
	if !now.IsZero() {
		lines = append(lines, "更新时间："+now.Format(time.RFC3339))
	}
	return strings.Join(lines, "\n")
}

func loopProgressText(state loopProgressState, round int) string {
	switch state {
	case loopProgressRunning:
		if round > 0 {
			return "第 " + strconv.Itoa(round) + " 轮运行中"
		}
		return "运行中"
	case loopProgressCompleted:
		if round > 0 {
			return "第 " + strconv.Itoa(round) + " 轮已完成"
		}
		return "本轮已完成"
	case loopProgressFinished:
		return "已完成"
	default:
		return "已启动"
	}
}

func (s *Service) markLoopStarted(key SessionKey, started time.Time, req loopRequest) {
	s.taskMu.Lock()
	s.loopStatuses[key] = loopRunStatus{
		running:     true,
		started:     started,
		maxRounds:   req.MaxRounds,
		maxDuration: req.MaxDuration,
		interval:    req.Interval,
		prompt:      req.Prompt,
	}
	s.taskMu.Unlock()
}

func (s *Service) markLoopRound(key SessionKey, started time.Time, round int) {
	s.taskMu.Lock()
	status := s.loopStatuses[key]
	if status.started == started {
		status.running = true
		status.round = round
		s.loopStatuses[key] = status
	}
	s.taskMu.Unlock()
}

func (s *Service) markLoopFinished(key SessionKey, started time.Time, reason string, err error) {
	s.taskMu.Lock()
	status := s.loopStatuses[key]
	if status.started == started {
		if errors.Is(err, context.Canceled) && !status.running && status.reason != "" {
			s.taskMu.Unlock()
			return
		}
		status.running = false
		status.ended = time.Now()
		status.reason = reason
		if err != nil && !errors.Is(err, context.Canceled) {
			status.lastError = err.Error()
		} else {
			status.lastError = ""
		}
		s.loopStatuses[key] = status
	}
	s.taskMu.Unlock()
}

func (s *Service) updateLoopFinished(ctx context.Context, msg feishu.Message, anchor loopAnchor, reason string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.updateLoopAnchor(ctx, anchor, loopProgressFinished, 0, reason)
}

func (s *Service) HandleLoopCancel(ctx context.Context, cancel feishu.LoopCancel) (string, error) {
	msg := feishu.Message{
		BotID:    strings.TrimSpace(cancel.BotID),
		ChatID:   strings.TrimSpace(cancel.ChatID),
		ThreadID: strings.TrimSpace(cancel.ThreadID),
		SenderID: strings.TrimSpace(cancel.OperatorID),
	}
	if !s.slashCommandAllowed(msg) {
		if len(s.ownerOpenIDs(cancel.BotID)) == 0 {
			return "", fmt.Errorf("未配置 bot owner，不能取消 loop")
		}
		return "", fmt.Errorf("只有 bot owner 可以取消 loop")
	}
	key := SessionKey{BotID: msg.BotID, ChatID: msg.ChatID, ThreadID: msg.ThreadID}
	store := s.storeForMessage(msg)
	if store != nil {
		session, ok := store.Get(key)
		if !ok && key.ThreadID != "" {
			session, ok = store.Get(SessionKey{BotID: msg.BotID, ChatID: msg.ChatID})
		}
		if !ok {
			return "", fmt.Errorf("该 loop 会话不存在或已过期")
		}
		if strings.TrimSpace(cancel.ACPSessionID) != "" && strings.TrimSpace(session.ACPSessionID) != strings.TrimSpace(cancel.ACPSessionID) {
			return "", fmt.Errorf("该 loop 卡片已过期")
		}
		key = session.Key
	}
	reason := "已通过卡片取消"
	if s.cancelLoopTask(ctx, key, reason) {
		return loopFinishedText(reason, time.Now()), nil
	}
	return "", fmt.Errorf("当前会话没有正在运行的 loop")
}

func (s *Service) cancelLoopTask(ctx context.Context, key SessionKey, reason string) bool {
	s.taskMu.Lock()
	task := s.tasks[key]
	if task == nil || task.kind != taskKindLoop {
		s.taskMu.Unlock()
		return false
	}
	delete(s.tasks, key)
	status := s.loopStatuses[key]
	status.running = false
	status.ended = time.Now()
	status.reason = reason
	status.lastError = ""
	s.loopStatuses[key] = status
	s.taskMu.Unlock()
	task.cancel()
	if task.onCancel != nil {
		task.onCancel(ctx, reason)
	}
	go s.cancelRuntimeTask(ctx, task)
	return true
}

func (s *Service) loopStatus(msg feishu.Message) string {
	session, hasSession := s.findSession(msg)
	if !hasSession {
		return "当前会话还没有 loop 状态。"
	}
	s.taskMu.Lock()
	status, ok := s.loopStatuses[session.Key]
	s.taskMu.Unlock()
	if !ok || status.started.IsZero() {
		return "当前会话还没有 loop 状态。"
	}
	lines := []string{"当前会话 loop："}
	if status.running {
		lines = append(lines, "状态：运行中")
	} else {
		lines = append(lines, "状态：已结束")
	}
	if status.round > 0 {
		lines = append(lines, "当前轮次："+strconv.Itoa(status.round))
	}
	if !status.started.IsZero() {
		lines = append(lines, "开始时间："+status.started.Format(time.RFC3339))
	}
	if !status.ended.IsZero() {
		lines = append(lines, "结束时间："+status.ended.Format(time.RFC3339))
	}
	if status.reason != "" {
		lines = append(lines, "原因："+status.reason)
	}
	if status.lastError != "" {
		lines = append(lines, "错误："+status.lastError)
	}
	lines = append(lines,
		"最长运行："+loopDurationStatus(status.maxDuration),
		"最大轮次："+loopRoundsStatus(status.maxRounds),
		"每轮间隔："+formatDuration(status.interval),
	)
	if status.prompt != "" {
		lines = append(lines, "提示词："+truncateRunes(status.prompt, 80))
	}
	return strings.Join(lines, "\n")
}

func loopDurationStatus(d time.Duration) string {
	if d <= 0 {
		return "不限"
	}
	return formatDuration(d)
}

func loopRoundsStatus(n int) string {
	if n <= 0 {
		return "不限"
	}
	return strconv.Itoa(n)
}

type loopRequest struct {
	MaxDuration time.Duration
	MaxRounds   int
	Interval    time.Duration
	Prompt      string
}

func parseLoopRequest(text string) (loopRequest, error) {
	args := strings.Fields(strings.TrimSpace(strings.TrimPrefix(text, "/loop")))
	req := loopRequest{Interval: defaultLoopInterval}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-t":
			i++
			if i >= len(args) {
				return loopRequest{}, fmt.Errorf("请为 -t 指定 duration，例如 /loop -t 30m 提示词；-t 0 表示不限。")
			}
			d, err := parseLoopDuration(args[i], "time")
			if err != nil {
				return loopRequest{}, err
			}
			req.MaxDuration = d
		case "-n":
			i++
			if i >= len(args) {
				return loopRequest{}, fmt.Errorf("请为 -n 指定最大轮次；-n 0 表示不限。")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 0 {
				return loopRequest{}, fmt.Errorf("-n 必须是非负整数；-n 0 表示不限。")
			}
			req.MaxRounds = n
		case "-i":
			i++
			if i >= len(args) {
				return loopRequest{}, fmt.Errorf("请为 -i 指定每轮间隔，例如 /loop -i 10s 提示词。")
			}
			d, err := parseLoopDuration(args[i], "interval")
			if err != nil {
				return loopRequest{}, err
			}
			if d <= 0 {
				return loopRequest{}, fmt.Errorf("-i 必须大于 0，例如 10s。")
			}
			req.Interval = d
		default:
			if strings.HasPrefix(arg, "-") {
				return loopRequest{}, fmt.Errorf("未知 loop 参数：%s。用法：/loop [-t 0] [-n 0] [-i 10s] 提示词", arg)
			}
			req.Prompt = strings.TrimSpace(strings.Join(args[i:], " "))
			i = len(args)
		}
	}
	if req.Prompt == "" {
		return loopRequest{}, fmt.Errorf("提示词必填。用法：/loop [-t 0] [-n 0] [-i 10s] 提示词")
	}
	return req, nil
}

func parseLoopDuration(raw string, name string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("%s duration 不能为空", name)
	}
	if raw == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s duration 格式无效，可用 10s、30m、1h 或 0", name)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s duration 不能小于 0", name)
	}
	return d, nil
}

func formatLoopRequest(req loopRequest) string {
	maxDuration := "不限"
	if req.MaxDuration > 0 {
		maxDuration = formatDuration(req.MaxDuration)
	}
	maxRounds := "不限"
	if req.MaxRounds > 0 {
		maxRounds = strconv.Itoa(req.MaxRounds)
	}
	return strings.Join([]string{
		"最长运行：" + maxDuration,
		"最大轮次：" + maxRounds,
		"每轮间隔：" + formatDuration(req.Interval),
		"停止条件：agent 最终回复 DONE、达到最长运行、达到最大轮次，或发送新消息 / /loop stop。",
	}, "\n")
}

func (s *Service) sendSessionList(ctx context.Context, msg feishu.Message) string {
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	items := store.ListByChat(msg.BotID, msg.ChatID)
	if len(items) == 0 {
		return "当前聊天还没有历史 ACP 会话。发送普通文本会自动创建，或用 /new <cwd> 指定工作目录。"
	}
	sent, err := feishu.SendSessionSelectionCard(ctx, msg, feishu.SessionSelectionCard{
		BotID:               msg.BotID,
		ChatID:              msg.ChatID,
		ThreadID:            msg.ThreadID,
		RequesterID:         msg.SenderID,
		CurrentACPSessionID: currentACPSessionID(s, msg),
		Options:             sessionSelectionOptions(items, maxSessionHistoryPerChat),
	})
	if err != nil {
		slog.ErrorContext(ctx, "发送会话选择卡片失败", "错误", err)
		return "发送会话选择卡片失败：" + err.Error()
	}
	if sent {
		return ""
	}
	return s.formatSessionList(msg, 0)
}

func (s *Service) formatSessionList(msg feishu.Message, limit int) string {
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	items := store.ListByChat(msg.BotID, msg.ChatID)
	if len(items) == 0 {
		return "当前聊天还没有历史 ACP 会话。发送普通文本会自动创建，或用 /new <cwd> 指定工作目录。"
	}
	total := len(items)
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	current, hasCurrent := s.findSession(msg)
	lines := []string{"当前聊天的历史 ACP 会话："}
	for i, item := range items {
		marker := ""
		if hasCurrent && item.ACPSessionID == current.ACPSessionID {
			marker = " *"
		}
		lines = append(lines, fmt.Sprintf("%d. %s%s\n   标题：%s\n   cwd：%s", i+1, item.ACPSessionID, marker, displaySessionTitle(item), item.Cwd))
	}
	if limit > 0 && total > limit {
		lines = append(lines, fmt.Sprintf("仅显示最近 %d 个，共 %d 个历史会话。", limit, total))
	}
	lines = append(lines, "使用 /session resume <index> 恢复指定会话。")
	return strings.Join(lines, "\n")
}

func currentACPSessionID(svc *Service, msg feishu.Message) string {
	if svc == nil {
		return ""
	}
	session, ok := svc.findSession(msg)
	if !ok {
		return ""
	}
	return session.ACPSessionID
}

func sessionSelectionOptions(items []Session, limit int) []feishu.SessionOption {
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	options := make([]feishu.SessionOption, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.ACPSessionID) == "" {
			continue
		}
		options = append(options, feishu.SessionOption{
			ACPSessionID: item.ACPSessionID,
			Title:        displaySessionTitle(item),
			Cwd:          item.Cwd,
		})
	}
	return options
}

func (s *Service) resumeSession(ctx context.Context, msg feishu.Message, index int) string {
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	items := store.ListByChat(msg.BotID, msg.ChatID)
	if len(items) == 0 {
		return "当前聊天还没有历史 ACP 会话。"
	}
	if index > len(items) {
		return fmt.Sprintf("会话序号超出范围。当前共有 %d 个历史会话。", len(items))
	}
	session, errText := s.resumeSessionByID(ctx, msg, items[index-1].ACPSessionID)
	if errText != "" {
		return errText
	}
	return fmt.Sprintf("已恢复会话 %d。\n标题：%s\nagent：%s\ncwd：%s\nsession：%s", index, displaySessionTitle(session), session.AgentName, session.Cwd, session.ACPSessionID)
}

func (s *Service) resumeSessionByID(ctx context.Context, msg feishu.Message, acpSessionID string) (Session, string) {
	store := s.storeForMessage(msg)
	if store == nil {
		return Session{}, "会话持久化未初始化。"
	}
	acpSessionID = strings.TrimSpace(acpSessionID)
	if acpSessionID == "" {
		return Session{}, "会话 ID 不能为空。"
	}
	for _, item := range store.ListByChat(msg.BotID, msg.ChatID) {
		if item.ACPSessionID != acpSessionID {
			continue
		}
		session := item
		session.Key = sessionKeyFromMessage(msg)
		if err := store.Upsert(session); err != nil {
			slog.ErrorContext(ctx, "恢复会话映射失败", "错误", err)
			return Session{}, "恢复会话失败：" + err.Error()
		}
		if err := s.runtime.CloseSession(session.Key); err != nil {
			slog.WarnContext(ctx, "关闭旧 ACP runtime 失败", "key", session.Key, "错误", err)
		}
		return session, ""
	}
	return Session{}, "选择的会话不存在或已过期，请重新发送 /session list。"
}

func (s *Service) HandleSessionSelection(ctx context.Context, selection feishu.SessionSelection) (string, error) {
	msg := feishu.Message{
		BotID:    selection.BotID,
		ChatID:   selection.ChatID,
		ThreadID: selection.ThreadID,
		SenderID: selection.OperatorID,
	}
	if !s.slashCommandAllowed(msg) {
		if len(s.ownerOpenIDs(selection.BotID)) == 0 {
			return "", fmt.Errorf("未配置 bot owner，不能恢复会话")
		}
		return "", fmt.Errorf("只有 bot owner 可以恢复会话")
	}
	session, errText := s.resumeSessionByID(ctx, msg, selection.ACPSessionID)
	if errText != "" {
		return "", fmt.Errorf("%s", strings.TrimSuffix(errText, "。"))
	}
	return displaySessionTitle(session), nil
}

func (s *Service) setSessionTitle(ctx context.Context, msg feishu.Message, title string) string {
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	session, ok := s.findSession(msg)
	if !ok {
		return "当前会话还没有 ACP session，无法设置标题。"
	}
	session.Title = normalizeSessionTitle(title)
	session.ManualTitle = true
	if err := store.Upsert(session); err != nil {
		slog.ErrorContext(ctx, "设置会话标题失败", "错误", err)
		return "设置会话标题失败：" + err.Error()
	}
	return "已设置当前会话标题：" + session.Title
}

func (s *Service) subscribeACPStateUpdates(ctx context.Context, msg feishu.Message, key SessionKey) {
	s.acpUpdateMu.Lock()
	if old := s.acpUpdateUnsub[key]; old != nil {
		old()
	}
	unsub := s.runtime.SubscribeUpdates(key, func(sessionID string, update acp.SessionUpdate) {
		s.handleACPStateUpdate(ctx, msg, key, sessionID, update)
	})
	s.acpUpdateUnsub[key] = unsub
	s.acpUpdateMu.Unlock()
}

func (s *Service) handleACPStateUpdate(ctx context.Context, msg feishu.Message, key SessionKey, sessionID string, update acp.SessionUpdate) {
	if !isACPStateUpdate(update) {
		return
	}
	store := s.storeForMessage(msg)
	if store == nil {
		return
	}
	session, ok := store.Get(key)
	if !ok || session.ACPSessionID != sessionID {
		return
	}
	changed := applyACPStateUpdate(&session, update)
	if !changed {
		return
	}
	if err := store.Upsert(session); err != nil {
		slog.WarnContext(ctx, "保存 ACP session 状态失败", "session", sessionID, "update", update.SessionUpdate, "错误", err)
	}
}

func (s *Service) saveSessionState(ctx context.Context, msg feishu.Message, session Session) {
	store := s.storeForMessage(msg)
	if store == nil {
		return
	}
	if err := store.Upsert(session); err != nil {
		slog.WarnContext(ctx, "保存会话状态失败", "session", session.ACPSessionID, "错误", err)
	}
}

func isACPStateUpdate(update acp.SessionUpdate) bool {
	switch update.SessionUpdate {
	case "available_commands_update", "config_option_update":
		return true
	default:
		return update.Models != nil || update.Mode != nil
	}
}

func applyACPStateUpdate(session *Session, update acp.SessionUpdate) bool {
	if session == nil {
		return false
	}
	changed := false
	switch update.SessionUpdate {
	case "available_commands_update":
		session.AvailableCommands = append([]acp.AvailableCommand(nil), update.AvailableCommands...)
		changed = true
	case "config_option_update":
		session.ConfigOptions = append([]acp.SessionConfigOption(nil), update.ConfigOptions...)
		changed = true
	}
	if update.Models != nil {
		models := *update.Models
		models.AvailableModels = append([]acp.SessionModel(nil), update.Models.AvailableModels...)
		session.Models = &models
		changed = true
	}
	if update.Mode != nil {
		mode := *update.Mode
		mode.AvailableModes = append([]acp.SessionMode(nil), update.Mode.AvailableModes...)
		session.Mode = &mode
		changed = true
	}
	return changed
}

func (s *Service) newSession(ctx context.Context, fields []string, msg feishu.Message) string {
	session, _, source, errText := s.createSession(ctx, fields, msg)
	if errText != "" {
		return errText
	}
	session = s.waitForNewSessionState(ctx, msg, session.Key, session)
	return formatNewSessionReply(session, source)
}

func (s *Service) createSession(ctx context.Context, fields []string, msg feishu.Message) (Session, config.AgentConfig, string, string) {
	slog.InfoContext(ctx, "准备创建ACP会话", "cmd", fields)
	store := s.storeForMessage(msg)
	if store == nil {
		return Session{}, config.AgentConfig{}, "", "会话持久化未初始化。"
	}
	req, source, errText := s.resolveNewSessionRequest(fields, msg)
	if errText != "" {
		return Session{}, config.AgentConfig{}, "", errText
	}
	if req.Title == "" {
		req.Title = s.nextDefaultSessionTitle(store, msg)
	}
	cwd := req.Cwd
	if !filepath.IsAbs(cwd) {
		return Session{}, config.AgentConfig{}, "", "工作目录必须是绝对路径，可使用 /absolute/path 或 ~/path。"
	}
	if info, err := os.Stat(cwd); err != nil {
		return Session{}, config.AgentConfig{}, "", "工作目录不可访问：" + err.Error()
	} else if !info.IsDir() {
		return Session{}, config.AgentConfig{}, "", "工作目录不是目录：" + cwd
	}
	agentName := s.defaultAgentName()
	agent, ok := s.registry.Get(agentName)
	if !ok {
		return Session{}, config.AgentConfig{}, "", "未找到默认 agent 配置。"
	}
	if _, err := ensureWorkspace(msg.Workspace, msg.BotID); err != nil {
		slog.ErrorContext(ctx, "初始化 workspace 失败", "workspace", msg.Workspace, "错误", err)
		return Session{}, config.AgentConfig{}, "", "初始化 workspace 失败：" + err.Error()
	}
	key := sessionKeyFromMessage(msg)
	s.migrateSessionShowConfigToChat(ctx, msg)
	pendingWiki, hasPendingWiki := s.takePendingWiki(key)
	s.cancelRunningSessionWork(ctx, key)
	s.subscribeACPStateUpdates(ctx, msg, key)
	sessionInfo, err := s.runtime.NewSession(ctx, key, agentName, agent, filepath.Clean(cwd), msg.Workspace)
	if err != nil {
		if hasPendingWiki {
			s.restorePendingWiki(pendingWiki)
		}
		slog.ErrorContext(ctx, "创建 ACP session 失败", "agent", agentName, "cwd", cwd, "错误", err)
		return Session{}, config.AgentConfig{}, "", "创建 ACP session 失败：" + err.Error()
	}
	session := Session{
		Key:               key,
		Title:             req.Title,
		ManualTitle:       req.ManualTitle,
		AgentName:         agentName,
		ACPSessionID:      sessionInfo.SessionID,
		Cwd:               filepath.Clean(cwd),
		Workspace:         msg.Workspace,
		AvailableCommands: sessionInfo.AvailableCommands,
		ConfigOptions:     sessionInfo.ConfigOptions,
		Models:            sessionInfo.Models,
		Mode:              sessionInfo.Mode,
	}
	if err := store.Upsert(session); err != nil {
		if hasPendingWiki {
			s.restorePendingWiki(pendingWiki)
		}
		slog.ErrorContext(ctx, "保存会话映射失败", "错误", err)
		return Session{}, config.AgentConfig{}, "", "保存会话映射失败：" + err.Error()
	}
	if hasPendingWiki {
		s.runPendingWikiAsync(pendingWiki)
	}
	slog.InfoContext(ctx, "创建 ACP session 成功", "agent", agentName, "cwd", cwd)
	return session, agent, source, ""
}

func (s *Service) waitForNewSessionState(ctx context.Context, msg feishu.Message, key SessionKey, session Session) Session {
	store := s.storeForMessage(msg)
	session = latestSessionForKey(store, key, session)
	if newSessionStateReady(session) {
		return session
	}
	timer := time.NewTimer(newSessionStateWait)
	defer timer.Stop()
	var partialTimer *time.Timer
	var partialTimerC <-chan time.Time
	defer func() {
		if partialTimer != nil {
			partialTimer.Stop()
		}
	}()
	if newSessionStatePartial(session) {
		partialTimer = time.NewTimer(newSessionPartialStateWait)
		partialTimerC = partialTimer.C
	}
	updated := make(chan struct{}, 1)
	unsub := s.runtime.SubscribeUpdates(key, func(sessionID string, update acp.SessionUpdate) {
		if sessionID != session.ACPSessionID || !isACPStateUpdate(update) {
			return
		}
		select {
		case updated <- struct{}{}:
		default:
		}
	})
	defer unsub()
	for {
		select {
		case <-ctx.Done():
			return latestSessionForKey(store, key, session)
		case <-timer.C:
			return latestSessionForKey(store, key, session)
		case <-partialTimerC:
			return latestSessionForKey(store, key, session)
		case <-updated:
			current := latestSessionForKey(store, key, session)
			if newSessionStateReady(current) {
				return current
			}
			if newSessionStatePartial(current) && partialTimer == nil {
				partialTimer = time.NewTimer(newSessionPartialStateWait)
				partialTimerC = partialTimer.C
			}
		}
	}
}

func newSessionStateReady(session Session) bool {
	return currentModeDisplay(session) != "" && currentModelDisplay(session) != ""
}

func newSessionStatePartial(session Session) bool {
	return currentModeDisplay(session) != "" || currentModelDisplay(session) != ""
}

func latestSessionForKey(store *SessionStore, key SessionKey, fallback Session) Session {
	if store == nil {
		return fallback
	}
	if session, ok := store.Get(key); ok {
		return session
	}
	return fallback
}

func (s *Service) status(msg feishu.Message) string {
	lines := []string{
		"服务运行中。",
		"已配置 ACP agent：" + strings.Join(s.registry.Names(), ", "),
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

func (s *Service) prompt(ctx context.Context, msg feishu.Message, text string) (string, error) {
	prepared, err := s.preparePrompt(ctx, msg, text)
	if err != nil {
		return "", err
	}
	if prepared.errText != "" {
		return prepared.errText, nil
	}
	return s.promptSession(ctx, msg, prepared.session, prepared.agent, prepared.text, text)
}

func (s *Service) preparePrompt(ctx context.Context, msg feishu.Message, userText string) (preparedPrompt, error) {
	session, ok := s.findSession(msg)
	if !ok {
		created, agent, _, errText := s.createSession(ctx, []string{"/new"}, msg)
		if errText != "" {
			return preparedPrompt{errText: errText}, nil
		}
		text := promptTextWithWorkspaceContext(sessionWorkspace(created, msg), msg, promptTextWithReplyContext(msg, userText))
		return preparedPrompt{session: created, agent: agent, text: text}, nil
	}
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		return preparedPrompt{}, fmt.Errorf("未找到 agent 配置: %s", session.AgentName)
	}
	text := promptTextWithReplyContext(msg, userText)
	if strings.TrimSpace(session.ACPSessionID) == "" {
		created, _, _, errText := s.createSession(ctx, []string{"/new", session.Cwd}, msg)
		if errText != "" {
			return preparedPrompt{errText: errText}, nil
		}
		session = created
	}
	text = promptTextWithWorkspaceContext(sessionWorkspace(session, msg), msg, text)
	return preparedPrompt{session: session, agent: agent, text: text}, nil
}

func promptTextWithReplyContext(msg feishu.Message, text string) string {
	replyText := ""
	if msg.Reply != nil {
		replyText = strings.TrimSpace(msg.Reply.PromptText())
	}
	if replyText == "" {
		return text
	}
	sections := []string{
		replyMetadataPrompt(msg.Reply),
		"## Replied Message Context",
		replyText,
		"",
		"请结合上面的被回复消息理解下面的用户消息。",
		"",
		strings.TrimSpace(text),
	}
	return strings.Join(nonEmptySections(sections), "\n")
}

func messageMetadataPrompt(msg feishu.Message) string {
	metadata := orderedPromptMetadata{
		{"bot_id", msg.BotID},
		{"message_id", msg.MessageID},
		{"chat_id", msg.ChatID},
		{"chat_type", msg.ChatType},
		{"thread_id", msg.ThreadID},
		{"root_id", msg.RootID},
		{"parent_id", msg.ParentID},
		{"sender_id", msg.SenderID},
		{"sender_type", msg.SenderType},
		{"msg_type", msg.MsgType},
	}
	return promptMetadataSection("## Message Metadata", metadata)
}

func replyMetadataPrompt(reply *feishu.ReplyContext) string {
	if reply == nil {
		return ""
	}
	metadata := orderedPromptMetadata{
		{"message_id", reply.MessageID},
		{"sender_id", reply.SenderID},
		{"sender_type", reply.SenderType},
		{"msg_type", reply.MsgType},
	}
	return promptMetadataSection("## Replied Message Metadata", metadata)
}

type promptMetadataField struct {
	Key   string
	Value string
}

type orderedPromptMetadata []promptMetadataField

func (m orderedPromptMetadata) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	written := 0
	for _, field := range m {
		key := strings.TrimSpace(field.Key)
		value := strings.TrimSpace(field.Value)
		if key == "" || value == "" {
			continue
		}
		if written > 0 {
			b.WriteByte(',')
		}
		keyJSON, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		valueJSON, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		b.Write(keyJSON)
		b.WriteByte(':')
		b.Write(valueJSON)
		written++
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

func promptMetadataSection(title string, metadata orderedPromptMetadata) string {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil || string(data) == "{}" {
		return ""
	}
	return strings.Join([]string{
		title,
		"```json",
		string(data),
		"```",
	}, "\n")
}

func nonEmptySections(sections []string) []string {
	out := sections[:0]
	for _, section := range sections {
		if strings.TrimSpace(section) != "" {
			out = append(out, section)
		}
	}
	return out
}

func sessionWorkspace(session Session, msg feishu.Message) string {
	if strings.TrimSpace(session.Workspace) != "" {
		return session.Workspace
	}
	return msg.Workspace
}

func promptTextWithWorkspaceContext(workspace string, msg feishu.Message, text string) string {
	return promptWithUserMessage([]string{
		workspaceContextPrompt(workspace),
		workspaceMemoryPolicyPrompt(workspace),
		messageMetadataPrompt(msg),
	}, text)
}

func (s *Service) promptSession(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, userText string) (string, error) {
	s.subscribeACPStateUpdates(ctx, msg, session.Key)
	result, sentProgress, err := s.runUserPrompt(ctx, msg, session, agent, text)
	if errors.Is(err, errACPSessionUnavailable) && !sentProgress {
		refreshed, refreshErr := s.refreshACPSession(ctx, msg, session, agent)
		if refreshErr != nil {
			return "", refreshErr
		}
		session = refreshed
		result, sentProgress, err = s.runUserPrompt(ctx, msg, session, agent, text)
	}
	reply := result.Text
	if !errors.Is(err, context.Canceled) && (err == nil || strings.TrimSpace(reply) != "" || sentProgress) {
		session = s.updateAutomaticSessionTitle(ctx, msg, session, userText)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", nil
		}
		if strings.TrimSpace(reply) != "" {
			return reply, nil
		}
		if sentProgress {
			return "", nil
		}
		return "", err
	}
	if strings.TrimSpace(reply) == "" {
		if sentProgress {
			return "", nil
		}
		return "ACP session 已完成，但没有返回文本。", nil
	}
	return reply, nil
}

func (s *Service) refreshACPSession(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig) (Session, error) {
	cwd := strings.TrimSpace(session.Cwd)
	if cwd == "" {
		return Session{}, fmt.Errorf("当前会话缺少工作目录，无法重建 ACP session")
	}
	slog.WarnContext(ctx, "持久化 ACP session 不可恢复，准备重建", "session", session.ACPSessionID, "cwd", cwd)
	sessionInfo, err := s.runtime.NewSession(ctx, session.Key, session.AgentName, agent, filepath.Clean(cwd), sessionWorkspace(session, msg))
	if err != nil {
		return Session{}, fmt.Errorf("重建 ACP session 失败: %w", err)
	}
	session.ACPSessionID = sessionInfo.SessionID
	session.Cwd = filepath.Clean(cwd)
	if strings.TrimSpace(session.Workspace) == "" {
		session.Workspace = msg.Workspace
	}
	session.AvailableCommands = sessionInfo.AvailableCommands
	session.ConfigOptions = sessionInfo.ConfigOptions
	session.Models = sessionInfo.Models
	store := s.storeForMessage(msg)
	if store == nil {
		return session, nil
	}
	if err := store.Upsert(session); err != nil {
		return Session{}, fmt.Errorf("保存重建后的 ACP session 失败: %w", err)
	}
	slog.InfoContext(ctx, "已重建 ACP session", "session", session.ACPSessionID, "cwd", session.Cwd)
	return session, nil
}

func (s *Service) runUserPrompt(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string) (acp.PromptResult, bool, error) {
	ctx, finish := s.startTask(ctx, session, agent, taskKindUser)
	defer finish()
	result, sentProgress, err := s.promptRuntimeWithProgress(ctx, msg, session, agent, text)
	if errors.Is(err, context.Canceled) {
		return result, sentProgress, err
	}
	if err == nil {
		s.scheduleWikiAfterUserPrompt(session, agent)
	}
	return result, sentProgress, err
}

func (s *Service) promptRuntimeWithProgress(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string) (acp.PromptResult, bool, error) {
	result, sentProgress, _, _, err := s.promptRuntimeWithProgressRaw(ctx, msg, session, agent, text)
	return result, sentProgress, err
}

func (s *Service) promptRuntimeWithProgressRaw(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string) (acp.PromptResult, bool, acp.PromptResult, string, error) {
	slog.InfoContext(ctx, "准备发送消息给ACP后端")
	stream := newPromptCardStream(ctx, msg, session, s.chatConfigForMessage(msg))
	chunks := newPromptChunkAccumulator(stream)
	flushStreams := func() {
		chunks.finishStream()
	}
	opts := acp.PromptOptions{
		OnUpdate: func(update acp.PromptUpdate) {
			// slog.InfoContext(ctx, "ACP|OnUpdate", "update", update)
			stream.updatePromptStatusFromUpdate(update)
			if chunk, ok := promptUpdateChunk(update); ok {
				if chunk.ToolBoundary {
					chunks.markToolBoundary()
				}
				chunks.add(chunk)
				return
			}
			if isToolBoundaryUpdateKind(promptUpdateKind(update)) {
				chunks.markToolBoundary()
			} else {
				flushStreams()
			}
			stream.updatePromptUpdate(update)
		},
		OnPermissionRequest: func(reqCtx context.Context, req acp.PermissionRequest) (acp.PermissionOutcome, error) {
			outcome, ok, err := feishu.RequestPermission(reqCtx, msg, req)
			if err != nil {
				return acp.PermissionOutcome{}, err
			}
			if ok {
				return outcome, nil
			}
			return defaultPermissionOutcome(req), nil
		},
	}
	result, err := s.runtime.Prompt(ctx, session, agent, text, opts)
	rawResult := result
	chunks.close()
	streamedReply := chunks.finalText()
	if stream.hasStarted() {
		if finalReply := strings.TrimSpace(result.Text); finalReply != "" && !chunks.hasToolBoundary() && finalReply != streamedReply {
			stream.updateText(finalReply)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				stream.updateProcessMessage("已取消")
				stream.finishPromptStatus("cancelled")
			} else {
				stream.updateProcessMessage("执行失败：" + err.Error())
				stream.failPromptStatus()
			}
		} else {
			stream.updatePromptStatusFromResult(result)
			stream.updatePromptResult(result)
			stream.finishPromptStatus(result.StopReason)
		}
		stream.close()
	}
	if stream.hasStarted() {
		result.Text = ""
	}
	return result, stream.hasStarted(), rawResult, streamedReply, err
}

func defaultPermissionOutcome(req acp.PermissionRequest) acp.PermissionOutcome {
	for _, option := range req.Options {
		switch option.Kind {
		case "reject_once", "reject_always":
			if strings.TrimSpace(option.OptionID) != "" {
				return acp.PermissionOutcome{Outcome: "selected", OptionID: option.OptionID}
			}
		}
	}
	return acp.PermissionOutcome{Outcome: "cancelled"}
}

const (
	promptChunkFlushRunes = 300

	promptChunkTargetText           = "text"
	promptChunkTargetProcess        = "process"
	promptChunkTargetProcessMessage = "process_message"
	promptChunkTargetThought        = "thought"
	promptChunkTargetTool           = "tool"
)

type promptChunk struct {
	Target       string
	Key          string
	Text         string
	ToolBoundary bool
}

type promptChunkStream struct {
	target  string
	key     string
	pending strings.Builder
	full    strings.Builder
}

type promptChunkFlush struct {
	target string
	text   string
	finish bool
	clear  bool
}

type promptChunkAccumulator struct {
	mu              sync.Mutex
	stream          *promptCardStream
	current         *promptChunkStream
	reply           strings.Builder
	finalCandidate  strings.Builder
	hasTool         bool
	textVisible     bool
	timer           *time.Timer
	timerGeneration int64
	flushing        sync.WaitGroup
}

func newPromptChunkAccumulator(stream *promptCardStream) *promptChunkAccumulator {
	return &promptChunkAccumulator{stream: stream}
}

func (a *promptChunkAccumulator) add(chunk promptChunk) {
	if chunk.Text == "" {
		return
	}
	var flushes []promptChunkFlush
	a.mu.Lock()
	if a.current == nil || a.current.target != chunk.Target || a.current.key != chunk.Key {
		flushes = append(flushes, a.takeFlushLocked(true))
		a.current = &promptChunkStream{target: chunk.Target, key: chunk.Key}
	}
	current := a.current
	current.pending.WriteString(chunk.Text)
	current.full.WriteString(chunk.Text)
	shouldFlush := strings.Contains(chunk.Text, "\n") || len([]rune(current.pending.String())) >= promptChunkFlushRunes
	if chunk.Target == promptChunkTargetText {
		a.reply.WriteString(chunk.Text)
		a.finalCandidate.WriteString(chunk.Text)
	}
	if shouldFlush {
		flushes = append(flushes, a.takeFlushLocked(false))
		a.stopTimerLocked()
	} else {
		a.scheduleLocked()
	}
	a.mu.Unlock()
	for _, flush := range flushes {
		a.applyFlush(flush)
	}
}

func (a *promptChunkAccumulator) markToolBoundary() {
	var flushes []promptChunkFlush
	a.mu.Lock()
	flushes = append(flushes, a.takeFlushLocked(true))
	a.stopTimerLocked()
	processText := strings.TrimSpace(a.finalCandidate.String())
	if processText != "" {
		flushes = append(flushes, promptChunkFlush{target: promptChunkTargetProcessMessage, text: processText, finish: true})
		a.finalCandidate.Reset()
	}
	clearText := a.textVisible && processText != ""
	a.hasTool = true
	if clearText {
		flushes = append(flushes, promptChunkFlush{target: promptChunkTargetText, clear: true, finish: true})
		a.textVisible = false
	}
	a.mu.Unlock()
	for _, flush := range flushes {
		a.applyFlush(flush)
	}
}

func (a *promptChunkAccumulator) hasToolBoundary() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hasTool
}

func (a *promptChunkAccumulator) flush() {
	a.mu.Lock()
	flush := a.takeFlushLocked(false)
	a.mu.Unlock()
	a.applyFlush(flush)
}

func (a *promptChunkAccumulator) finishStream() {
	a.mu.Lock()
	a.stopTimerLocked()
	flush := a.takeFlushLocked(true)
	a.mu.Unlock()
	a.applyFlush(flush)
}

func (a *promptChunkAccumulator) close() {
	a.finishStream()
	a.flushing.Wait()
}

func (a *promptChunkAccumulator) replyText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.TrimSpace(a.reply.String())
}

func (a *promptChunkAccumulator) finalText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if text := strings.TrimSpace(a.finalCandidate.String()); text != "" {
		return text
	}
	return strings.TrimSpace(a.reply.String())
}

func (a *promptChunkAccumulator) takeFlushLocked(finish bool) promptChunkFlush {
	if a.current == nil {
		return promptChunkFlush{}
	}
	current := a.current
	hasPending := current.pending.Len() > 0
	text := strings.TrimSpace(current.full.String())
	if current.target == promptChunkTargetText {
		text = strings.TrimSpace(a.finalCandidate.String())
	}
	if finish {
		a.current = nil
	} else {
		current.pending.Reset()
	}
	if !hasPending {
		return promptChunkFlush{target: current.target, finish: finish}
	}
	if current.target == promptChunkTargetText && text != "" {
		a.textVisible = true
	}
	return promptChunkFlush{target: current.target, text: text, finish: finish}
}

func (a *promptChunkAccumulator) applyFlush(flush promptChunkFlush) {
	if flush.target == "" {
		return
	}
	switch flush.target {
	case promptChunkTargetText:
		if flush.clear {
			a.stream.updateText("")
		} else if flush.text != "" {
			a.stream.updateText(flush.text)
		}
	case promptChunkTargetProcessMessage:
		if flush.text != "" {
			a.stream.updateProcessMessage(flush.text)
		}
	case promptChunkTargetThought:
		if flush.text != "" {
			a.stream.updateThoughtStream(flush.text)
		}
		if flush.finish {
			a.stream.finishProcessStream()
		}
	case promptChunkTargetTool:
		if flush.text != "" {
			a.stream.updateToolStream(flush.text)
		}
		if flush.finish {
			a.stream.finishProcessStream()
		}
	case promptChunkTargetProcess:
		if flush.text != "" {
			a.stream.updateProcessStream(flush.text)
		}
		if flush.finish {
			a.stream.finishProcessStream()
		}
	}
}

func (a *promptChunkAccumulator) scheduleLocked() {
	a.timerGeneration++
	generation := a.timerGeneration
	if a.timer != nil {
		if a.timer.Stop() {
			a.flushing.Done()
		}
	}
	a.flushing.Add(1)
	a.timer = time.AfterFunc(promptCardFlushDelay, func() {
		defer a.flushing.Done()
		a.mu.Lock()
		if a.timerGeneration != generation {
			a.mu.Unlock()
			return
		}
		flush := a.takeFlushLocked(false)
		a.timer = nil
		a.mu.Unlock()
		a.applyFlush(flush)
	})
}

func (a *promptChunkAccumulator) stopTimerLocked() {
	a.timerGeneration++
	if a.timer == nil {
		return
	}
	if a.timer.Stop() {
		a.flushing.Done()
	}
	a.timer = nil
}

type promptCardStream struct {
	ctx     context.Context
	msg     feishu.Message
	session Session

	mu                sync.Mutex
	card              feishu.StreamCard
	available         bool
	creating          bool
	ready             chan struct{}
	started           bool
	showStepMessages  bool
	showThoughts      bool
	showTools         bool
	showStatusBar     bool
	showUsageDetail   bool
	text              string
	process           []string
	streaming         bool
	activeStreamClass promptProcessClass
	tools             []promptToolRow
	status            promptStatusBar
}

type promptToolRow struct {
	title  string
	line   int
	active bool
}

func newPromptCardStream(ctx context.Context, msg feishu.Message, session Session, show ChatConfig) *promptCardStream {
	return &promptCardStream{
		ctx:              ctx,
		msg:              msg,
		session:          session,
		available:        true,
		showStepMessages: !show.HideStepMessages,
		showThoughts:     !show.HideThoughts,
		showTools:        !show.HideTools,
		showStatusBar:    !show.HideStatusBar,
		showUsageDetail:  !show.HideUsageDetail,
		status:           promptStatusBar{state: promptStatusRunning},
	}
}

func (s *promptCardStream) hasStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *promptCardStream) updateText(text string) {
	text = strings.TrimSpace(text)
	card := s.ensureCard()
	if card == nil {
		return
	}
	s.mu.Lock()
	s.text = normalizeStreamMarkdown(text)
	fullText := s.text
	s.mu.Unlock()
	if err := card.UpdateText(s.ctx, fullText); err != nil {
		slog.ErrorContext(s.ctx, "更新 ACP 流式卡片文本失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) updatePromptStatusFromUpdate(update acp.PromptUpdate) {
	if !s.showStatusBar {
		return
	}
	if promptUpdateKind(update) != "usage_update" {
		return
	}
	u := update.Update
	if u.Used <= 0 && u.Size <= 0 {
		return
	}
	card := s.ensureCard()
	if card == nil {
		return
	}
	s.mu.Lock()
	s.status.Context = acp.ContextWindowUsage{Used: u.Used, Size: u.Size}
	statusText := s.status.text()
	s.mu.Unlock()
	if err := card.UpdateStatus(s.ctx, statusText); err != nil {
		slog.ErrorContext(s.ctx, "更新 ACP 流式卡片状态栏失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) updatePromptStatusFromResult(result acp.PromptResult) {
	if !s.showStatusBar {
		return
	}
	if result.Usage.InputTokens == 0 && result.Usage.OutputTokens == 0 && result.Meta.TraeTokenUsage == nil {
		return
	}
	card := s.ensureCard()
	if card == nil {
		return
	}
	s.mu.Lock()
	s.status.applyPromptResult(result)
	statusText := s.status.text()
	s.mu.Unlock()
	if err := card.UpdateStatus(s.ctx, statusText); err != nil {
		slog.ErrorContext(s.ctx, "更新 ACP 流式卡片状态栏失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) updatePromptResult(result acp.PromptResult) {
	if !s.showUsageDetail || !promptResultHasUsageDetail(result) {
		return
	}
	detail := formatPromptResultDetail(result)
	if detail == "" {
		return
	}
	card := s.ensureCard()
	if card == nil {
		return
	}
	if err := card.UpdateUsageDetail(s.ctx, detail); err != nil {
		slog.ErrorContext(s.ctx, "更新 ACP 流式卡片用量明细失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) finishPromptStatus(stopReason string) {
	if !s.showStatusBar {
		return
	}
	card := s.ensureCard()
	if card == nil {
		return
	}
	s.mu.Lock()
	s.status.state = promptStatusFromStopReason(stopReason)
	s.status.stopReason = strings.TrimSpace(stopReason)
	statusText := s.status.text()
	s.mu.Unlock()
	if err := card.UpdateStatus(s.ctx, statusText); err != nil {
		slog.ErrorContext(s.ctx, "更新 ACP 流式卡片状态栏失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) failPromptStatus() {
	if !s.showStatusBar {
		return
	}
	card := s.ensureCard()
	if card == nil {
		return
	}
	s.mu.Lock()
	s.status.state = promptStatusFailed
	statusText := s.status.text()
	s.mu.Unlock()
	if err := card.UpdateStatus(s.ctx, statusText); err != nil {
		slog.ErrorContext(s.ctx, "更新 ACP 流式卡片状态栏失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

type promptStatusState string

const (
	promptStatusRunning   promptStatusState = "running"
	promptStatusCompleted promptStatusState = "completed"
	promptStatusCancelled promptStatusState = "cancelled"
	promptStatusFailed    promptStatusState = "failed"
	promptStatusStopped   promptStatusState = "stopped"
)

type promptStatusBar struct {
	state       promptStatusState
	stopReason  string
	input       int64
	cachedInput int64
	output      int64
	Context     acp.ContextWindowUsage
}

func (s *promptStatusBar) applyPromptResult(result acp.PromptResult) {
	if tokenUsage := result.Meta.TraeTokenUsage; tokenUsage != nil {
		if result.Usage.InputTokens <= 0 && tokenUsage.TurnDisplay.InputTokens > 0 {
			s.input = tokenUsage.TurnDisplay.InputTokens
		}
		if result.Usage.OutputTokens <= 0 && tokenUsage.TurnDisplay.OutputTokens > 0 {
			s.output = tokenUsage.TurnDisplay.OutputTokens
		}
		if s.Context.Used <= 0 && s.Context.Size <= 0 && (tokenUsage.ContextWindow.Used > 0 || tokenUsage.ContextWindow.Size > 0) {
			s.Context = tokenUsage.ContextWindow
		}
	}
	if result.Usage.InputTokens > 0 {
		s.input = result.Usage.InputTokens
	}
	if result.Usage.CachedReadTokens > 0 {
		s.cachedInput = result.Usage.CachedReadTokens
	}
	if result.Usage.OutputTokens > 0 {
		s.output = result.Usage.OutputTokens
	}
}

func (s promptStatusBar) text() string {
	parts := []string{promptStatusStateLabel(s.state, s.stopReason)}
	if tokenUsage := formatPromptTokenUsage(s.input, s.cachedInput, s.output); tokenUsage != "" {
		parts = append(parts, tokenUsage)
	}
	if s.Context.Used > 0 || s.Context.Size > 0 {
		parts = append(parts, formatContextUsage(s.Context))
	}
	return strings.Join(parts, " | ")
}

func promptStatusFromStopReason(stopReason string) promptStatusState {
	switch strings.TrimSpace(stopReason) {
	case "", "end_turn":
		return promptStatusCompleted
	case "cancelled":
		return promptStatusCancelled
	default:
		return promptStatusStopped
	}
}

func promptStatusStateLabel(state promptStatusState, stopReason string) string {
	switch state {
	case promptStatusCompleted:
		return "已完成"
	case promptStatusCancelled:
		return "已取消"
	case promptStatusFailed:
		return "执行失败"
	case promptStatusStopped:
		if stopReason = strings.TrimSpace(stopReason); stopReason != "" {
			return "已停止：" + stopReason
		}
		return "已停止"
	default:
		return "执行中"
	}
}

func formatContextUsage(usage acp.ContextWindowUsage) string {
	if usage.Used <= 0 && usage.Size <= 0 {
		return ""
	}
	if usage.Size <= 0 {
		return formatContextTokenCount(usage.Used)
	}
	if usage.Used <= 0 {
		return formatContextTokenCount(usage.Size)
	}
	return formatContextTokenCount(usage.Used) + "/" + formatContextTokenCount(usage.Size)
}

func formatTokenCountWithUnit(value int64) string {
	if value <= 0 {
		return "0"
	}
	if value >= 1_000_000 {
		return fmt.Sprintf("%sM", formatDecimal(float64(value)/1_000_000, 1))
	}
	if value >= 1000 {
		return fmt.Sprintf("%sK", formatDecimal(float64(value)/1000, 1))
	}
	return strconv.FormatInt(value, 10)
}

func formatContextTokenCount(value int64) string {
	if value <= 0 {
		return "0K"
	}
	if value >= 1_000_000 {
		return fmt.Sprintf("%dM", value/1_000_000)
	}
	return fmt.Sprintf("%dK", value/1000)
}

func formatTokenCount(value int64) string {
	if value <= 0 {
		return "0"
	}
	return formatTokenCountWithUnit(value)
}

func formatPromptTokenUsage(input, cachedInput, output int64) string {
	items := make([]string, 0, 2)
	if input > 0 {
		items = append(items, formatTokenCount(input)+formatCacheHitRate(cachedInput, input))
	}
	if output > 0 {
		items = append(items, formatTokenCount(output))
	}
	return strings.Join(items, ", ")
}

func formatPromptResultDetail(result acp.PromptResult) string {
	raw := append(json.RawMessage(nil), result.Raw...)
	if len(raw) == 0 || string(raw) == "null" {
		var err error
		raw, err = json.Marshal(result)
		if err != nil {
			return ""
		}
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		pretty.Write(raw)
	}
	text := pretty.String()
	if strings.TrimSpace(text) == "" {
		return ""
	}
	fence := markdownCodeFence(text)
	return fence + "json\n" + text + "\n" + fence
}

func promptResultHasUsageDetail(result acp.PromptResult) bool {
	if promptTokenUsagePresent(result.Usage) || result.Meta.TraeTokenUsage != nil {
		return true
	}
	raw := bytes.TrimSpace(result.Raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	var payload struct {
		Usage json.RawMessage `json:"usage"`
		Meta  json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false
	}
	return rawJSONObjectHasFields(payload.Usage) || rawJSONObjectHasFields(payload.Meta)
}

func promptTokenUsagePresent(usage acp.TokenUsage) bool {
	return usage.TotalTokens > 0 ||
		usage.InputTokens > 0 ||
		usage.OutputTokens > 0 ||
		usage.ThoughtTokens > 0 ||
		usage.CachedReadTokens > 0 ||
		usage.CachedWriteTokens > 0
}

func rawJSONObjectHasFields(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	return len(obj) > 0
}

func markdownCodeFence(text string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence
}

func formatCacheHitRate(cached, total int64) string {
	if cached <= 0 || total <= 0 {
		return ""
	}
	percent := int64(math.Round(float64(cached) / float64(total) * 100))
	if percent <= 0 {
		return ""
	}
	if percent > 100 {
		percent = 100
	}
	return fmt.Sprintf("(%d%%)", percent)
}

func formatDecimal(value float64, precision int) string {
	text := strconv.FormatFloat(value, 'f', precision, 64)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	if text == "" {
		return "0"
	}
	return text
}

func (s *promptCardStream) updateProcessMessage(text string) {
	if !s.showStepMessages {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.updateProcess(formatProcessMessageText(text))
}

func (s *promptCardStream) updateThoughtStream(text string) {
	if !s.showThoughts {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.updateProcessStreamText(promptProcessThought, "🧠 "+text, false)
}

func (s *promptCardStream) updateToolStream(text string) {
	if !s.showTools {
		return
	}
	s.updateProcessStreamText(promptProcessTool, text, false)
}

func (s *promptCardStream) updateProcess(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	card := s.ensureCard()
	if card == nil {
		return
	}
	s.mu.Lock()
	s.process = append(s.process, normalizeStreamMarkdown(text))
	processText := truncateProcessText(strings.Join(s.process, "\n"))
	s.mu.Unlock()
	if err := card.UpdateProcess(s.ctx, processText); err != nil {
		slog.ErrorContext(s.ctx, "更新 ACP 流式卡片过程失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) updatePromptUpdate(update acp.PromptUpdate) {
	if s.updateToolProcess(update) {
		return
	}
	kind := promptUpdateKind(update)
	if isThoughtUpdateKind(kind) {
		if !s.showThoughts {
			return
		}
	} else if !s.showStepMessages {
		return
	}
	progressText := formatPromptUpdate(update)
	if progressText == "" {
		return
	}
	s.updateProcess(progressText)
}

func (s *promptCardStream) updateToolProcess(update acp.PromptUpdate) bool {
	u := update.Update
	kind := promptUpdateKind(update)
	if !isToolPromptUpdateKind(kind) {
		return false
	}
	if !s.showTools {
		return true
	}
	status := toolStatusFromUpdate(kind, u.Status)
	title := toolDisplayName(u)
	line, ok := s.toolProgressLine(status, title)
	if !ok {
		return true
	}
	card := s.ensureCard()
	if card == nil {
		return true
	}
	s.mu.Lock()
	processText := s.applyToolProgressLineLocked(status, title, line)
	s.mu.Unlock()
	if err := card.UpdateProcess(s.ctx, processText); err != nil {
		slog.ErrorContext(s.ctx, "更新 ACP 流式卡片过程失败", "session", s.session.ACPSessionID, "错误", err)
	}
	return true
}

func (s *promptCardStream) toolProgressLine(status toolProgressStatus, title string) (string, bool) {
	title = strings.TrimSpace(title)
	if title == "" && status != toolProgressRunning {
		s.mu.Lock()
		title = s.latestActiveToolTitleLocked()
		s.mu.Unlock()
	}
	if title == "" {
		title = "工具调用"
	}
	title = truncateRunes(title, 80)
	return toolStatusIcon(status) + " " + title, true
}

func (s *promptCardStream) applyToolProgressLineLocked(status toolProgressStatus, title, line string) string {
	title = strings.TrimSpace(title)
	if title == "" && status != toolProgressRunning {
		title = s.latestActiveToolTitleLocked()
	}
	if title == "" {
		title = "工具调用"
	}
	normalizedTitle := truncateRunes(title, 80)
	if status == toolProgressRunning {
		s.process = append(s.process, normalizeStreamMarkdown(line))
		s.tools = append(s.tools, promptToolRow{
			title:  normalizedTitle,
			line:   len(s.process) - 1,
			active: true,
		})
		return truncateProcessText(strings.Join(s.process, "\n"))
	}
	if idx := s.findToolRowLocked(normalizedTitle); idx >= 0 {
		row := &s.tools[idx]
		if row.line >= 0 && row.line < len(s.process) {
			s.process[row.line] = normalizeStreamMarkdown(line)
		} else {
			s.process = append(s.process, normalizeStreamMarkdown(line))
			row.line = len(s.process) - 1
		}
		row.active = false
		return truncateProcessText(strings.Join(s.process, "\n"))
	}
	s.process = append(s.process, normalizeStreamMarkdown(line))
	return truncateProcessText(strings.Join(s.process, "\n"))
}

func (s *promptCardStream) latestActiveToolTitleLocked() string {
	for i := len(s.tools) - 1; i >= 0; i-- {
		if s.tools[i].active && strings.TrimSpace(s.tools[i].title) != "" {
			return s.tools[i].title
		}
	}
	for i := len(s.tools) - 1; i >= 0; i-- {
		if strings.TrimSpace(s.tools[i].title) != "" {
			return s.tools[i].title
		}
	}
	return ""
}

func (s *promptCardStream) findToolRowLocked(title string) int {
	title = strings.TrimSpace(title)
	if title != "" {
		for i := len(s.tools) - 1; i >= 0; i-- {
			if s.tools[i].active && s.tools[i].title == title {
				return i
			}
		}
		for i := len(s.tools) - 1; i >= 0; i-- {
			if s.tools[i].title == title {
				return i
			}
		}
	}
	for i := len(s.tools) - 1; i >= 0; i-- {
		if s.tools[i].active {
			return i
		}
	}
	return -1
}

func (s *promptCardStream) updateProcessStream(text string) {
	if !s.showStepMessages {
		return
	}
	s.updateProcessStreamText(promptProcessStep, text, true)
}

type promptProcessClass string

const (
	promptProcessNone    promptProcessClass = ""
	promptProcessStep    promptProcessClass = "step"
	promptProcessThought promptProcessClass = "thought"
	promptProcessTool    promptProcessClass = "tool"
)

func (s *promptCardStream) updateProcessStreamText(class promptProcessClass, text string, prefixProcessMessage bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if prefixProcessMessage {
		text = formatProcessMessageText(text)
	}
	card := s.ensureCard()
	if card == nil {
		return
	}
	s.mu.Lock()
	normalized := normalizeStreamMarkdown(text)
	if s.streaming && s.activeStreamClass == class && len(s.process) > 0 {
		s.process[len(s.process)-1] = normalized
	} else {
		s.process = append(s.process, normalized)
		s.streaming = true
		s.activeStreamClass = class
	}
	processText := truncateProcessText(strings.Join(s.process, "\n"))
	s.mu.Unlock()
	if err := card.UpdateProcess(s.ctx, processText); err != nil {
		slog.ErrorContext(s.ctx, "更新 ACP 流式卡片过程失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) finishProcessStream() {
	s.mu.Lock()
	s.streaming = false
	s.activeStreamClass = promptProcessNone
	s.mu.Unlock()
}

const (
	promptCardFlushDelay  = 180 * time.Millisecond
	maxPromptProcessRunes = 6000
)

func (s *promptCardStream) close() {
	s.mu.Lock()
	card := s.card
	s.mu.Unlock()
	if card == nil {
		return
	}
	if err := card.Close(s.ctx); err != nil {
		slog.ErrorContext(s.ctx, "关闭 ACP 流式卡片失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) ensureCard() feishu.StreamCard {
	s.mu.Lock()
	for {
		if s.card != nil {
			card := s.card
			s.mu.Unlock()
			return card
		}
		if !s.available {
			s.mu.Unlock()
			return nil
		}
		if !s.creating {
			break
		}
		ready := s.ready
		s.mu.Unlock()
		select {
		case <-ready:
		case <-s.ctx.Done():
			return nil
		}
		s.mu.Lock()
	}
	ready := make(chan struct{})
	s.creating = true
	s.ready = ready
	s.mu.Unlock()

	cardCtx := feishu.WithStreamCardProcessPanel(s.ctx, s.showStepMessages || s.showThoughts || s.showTools)
	cardCtx = feishu.WithStreamCardStatusBar(cardCtx, s.showStatusBar)
	card, ok, err := feishu.StartStreamCard(cardCtx, s.msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creating = false
	if s.ready == ready {
		s.ready = nil
	}
	close(ready)
	if err != nil {
		s.available = false
		slog.ErrorContext(s.ctx, "创建 ACP 流式卡片失败", "session", s.session.ACPSessionID, "错误", err)
		return nil
	}
	if !ok || card == nil {
		s.available = false
		return nil
	}
	s.card = card
	s.started = true
	return s.card
}

func normalizeStreamMarkdown(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	var out strings.Builder
	inCodeBlock := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			appendLineBreak(&out)
			out.WriteString(line)
			inCodeBlock = !inCodeBlock
			if i < len(lines)-1 {
				out.WriteByte('\n')
			}
			continue
		}
		if inCodeBlock || trimmed == "" || isMarkdownBlockStart(trimmed) {
			appendLineBreak(&out)
			out.WriteString(line)
			if i < len(lines)-1 {
				out.WriteByte('\n')
			}
			continue
		}
		if out.Len() > 0 {
			current := out.String()
			last, _ := utf8.DecodeLastRuneInString(current)
			if last == '\n' || isSentenceEnd(last) {
				appendLineBreak(&out)
			} else if !isCJKContinuation(current, trimmed) {
				out.WriteByte(' ')
			}
		}
		out.WriteString(trimmed)
	}
	return strings.TrimSpace(out.String())
}

func truncateProcessText(text string) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= maxPromptProcessRunes {
		return text
	}
	return "（前面过程内容已省略）\n" + string(runes[len(runes)-maxPromptProcessRunes:])
}

func appendLineBreak(out *strings.Builder) {
	if out.Len() == 0 {
		return
	}
	if out.String()[out.Len()-1] != '\n' {
		out.WriteByte('\n')
	}
}

func isMarkdownBlockStart(line string) bool {
	if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ">") || strings.HasPrefix(line, "|") {
		return true
	}
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "+ ") {
		return true
	}
	if len(line) >= 3 && line[0] >= '0' && line[0] <= '9' {
		for i := 1; i < len(line)-1 && i < 4; i++ {
			if line[i] == '.' && line[i+1] == ' ' {
				return true
			}
			if line[i] < '0' || line[i] > '9' {
				return false
			}
		}
	}
	return false
}

func isCJKContinuation(current, next string) bool {
	if current == "" || next == "" {
		return false
	}
	prev, _ := utf8.DecodeLastRuneInString(current)
	first, _ := utf8.DecodeRuneInString(next)
	return isCJK(prev) || isCJK(first)
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x3040 && r <= 0x30FF) ||
		(r >= 0xAC00 && r <= 0xD7AF)
}

func isSentenceEnd(r rune) bool {
	switch r {
	case '.', '!', '?', ':', ';', '。', '！', '？', '：', '；':
		return true
	default:
		return false
	}
}

const maxPromptUpdateRunes = 1800

type toolProgressStatus int

const (
	toolProgressRunning toolProgressStatus = iota
	toolProgressCompleted
	toolProgressFailed
	toolProgressUnknown
)

func formatPromptUpdate(update acp.PromptUpdate) string {
	u := update.Update
	kind := strings.TrimSpace(firstNonEmpty(u.SessionUpdate, rawString(u.Raw, "type"), rawString(u.Raw, "event")))
	switch kind {
	case "agent_message_chunk":
		return ""
	case "agent_message", "assistant_message", "message":
		return formatProcessMessageText(truncateRunes(firstNonEmpty(u.Message, contentText(u.Content), rawText(u.Raw)), maxPromptUpdateRunes))
	case "plan", "thought", "reasoning":
		return formatThoughtProcessText(truncateRunes(firstNonEmpty(u.Message, contentText(u.Content), rawText(u.Raw)), maxPromptUpdateRunes))
	case "status", "progress":
		text := firstNonEmpty(u.Message, u.Status, contentText(u.Content), rawText(u.Raw))
		return formatProcessMessageText(truncateRunes(text, maxPromptUpdateRunes))
	default:
		if text := firstNonEmpty(u.Message, contentText(u.Content), rawText(u.Raw)); text != "" {
			return formatProcessMessageText(truncateRunes(text, maxPromptUpdateRunes))
		}
		return ""
	}
}

func formatProcessMessageText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return "💬 " + text
}

func formatThoughtProcessText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return "🧠 " + text
}

func isToolPromptUpdateKind(kind string) bool {
	switch kind {
	case "function_call", "tool_call", "custom_tool_call",
		"tool_call_update", "function_call_update", "custom_tool_call_update",
		"function_call_output", "tool_call_output", "custom_tool_call_output",
		"tool_call_error", "function_call_error":
		return true
	default:
		return (strings.Contains(kind, "tool") || strings.Contains(kind, "function")) && !isPromptChunkKind(kind)
	}
}

func isToolBoundaryUpdateKind(kind string) bool {
	switch kind {
	case "function_call", "tool_call", "custom_tool_call":
		return true
	default:
		return false
	}
}

func toolStatusFromUpdate(kind, status string) toolProgressStatus {
	if strings.Contains(kind, "error") {
		return toolProgressFailed
	}
	if strings.Contains(kind, "output") {
		return toolProgressCompleted
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "complete", "success", "succeeded", "done":
		return toolProgressCompleted
	case "failed", "failure", "error":
		return toolProgressFailed
	case "in_progress", "running", "pending", "":
		return toolProgressRunning
	default:
		return toolProgressUnknown
	}
}

func toolStatusIcon(status toolProgressStatus) string {
	switch status {
	case toolProgressCompleted:
		return "✅"
	case toolProgressFailed:
		return "❌"
	case toolProgressUnknown:
		return "•"
	default:
		return "⏳"
	}
}

func promptUpdateChunkText(update acp.PromptUpdate) string {
	if update.Update.SessionUpdate != "agent_message_chunk" {
		return ""
	}
	if update.Update.Title != "" {
		return ""
	}
	if update.Update.Content == nil || update.Update.Content.Text == "" {
		return ""
	}
	return update.Update.Content.Text
}

func promptUpdateChunk(update acp.PromptUpdate) (promptChunk, bool) {
	u := update.Update
	kind := promptUpdateKind(update)
	if !isPromptChunkKind(kind) || u.Title != "" {
		return promptChunk{}, false
	}
	text := promptUpdateChunkRawText(update)
	if text == "" {
		return promptChunk{}, false
	}
	target := promptChunkTargetProcess
	key := kind
	if kind == "agent_message_chunk" {
		target = promptChunkTargetText
		key = "agent_message"
	} else if isToolChunkUpdateKind(kind) {
		target = promptChunkTargetTool
	} else if isThoughtUpdateKind(kind) {
		target = promptChunkTargetThought
	} else if streamName := promptChunkStreamName(kind); streamName != "" {
		key = streamName
	}
	return promptChunk{Target: target, Key: key, Text: text}, true
}

func promptUpdateKind(update acp.PromptUpdate) string {
	u := update.Update
	return strings.TrimSpace(firstNonEmpty(u.SessionUpdate, rawString(u.Raw, "type"), rawString(u.Raw, "event")))
}

func isPromptChunkKind(kind string) bool {
	return kind == "agent_message_chunk" || strings.HasSuffix(kind, "_chunk")
}

func isToolChunkUpdateKind(kind string) bool {
	if !isPromptChunkKind(kind) {
		return false
	}
	return strings.Contains(kind, "tool") || strings.Contains(kind, "function")
}

func isThoughtUpdateKind(kind string) bool {
	switch kind {
	case "agent_thought_chunk", "thought_chunk", "reasoning_chunk", "plan_chunk",
		"thought", "reasoning", "plan":
		return true
	default:
		return strings.Contains(kind, "thought") || strings.Contains(kind, "reasoning")
	}
}

func promptChunkStreamName(kind string) string {
	kind = strings.TrimSuffix(kind, "_chunk")
	kind = strings.TrimSuffix(kind, ".chunk")
	return strings.TrimSpace(kind)
}

func promptUpdateChunkRawText(update acp.PromptUpdate) string {
	u := update.Update
	if u.Content != nil && (u.Content.Type == "" || u.Content.Type == "text" || u.Content.Type == "output_text") {
		return u.Content.Text
	}
	if u.Message != "" {
		return u.Message
	}
	return rawChunkText(u.Raw)
}

func rawChunkText(raw json.RawMessage) string {
	value := rawValue(raw)
	return rawChunkTextValue(value)
}

func rawChunkTextValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		for _, item := range v {
			if text := rawChunkTextValue(item); text != "" {
				return text
			}
		}
	case map[string]any:
		for _, key := range []string{"text", "delta", "output_text", "message"} {
			if text := rawChunkTextValue(v[key]); text != "" {
				return text
			}
		}
		for _, key := range []string{"content", "payload"} {
			if text := rawChunkTextValue(v[key]); text != "" {
				return text
			}
		}
	}
	return ""
}

func toolDisplayName(u acp.SessionUpdate) string {
	return firstNonEmpty(u.Title, u.Name, rawName(u.Raw), rawString(u.Raw, "title"))
}

func contentText(content *acp.ContentBlock) string {
	if content == nil {
		return ""
	}
	if content.Type != "" && content.Type != "text" && content.Type != "output_text" {
		return ""
	}
	return strings.TrimSpace(content.Text)
}

func rawText(raw json.RawMessage) string {
	value := rawValue(raw)
	return strings.TrimSpace(rawTextValue(value))
}

func rawTextValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		for _, item := range v {
			if text := rawTextValue(item); text != "" {
				return text
			}
		}
	case map[string]any:
		for _, key := range []string{"message", "text", "output_text", "delta"} {
			if text := rawTextValue(v[key]); text != "" {
				return text
			}
		}
		if content := rawTextValue(v["content"]); content != "" {
			return content
		}
		if payload := rawTextValue(v["payload"]); payload != "" {
			return payload
		}
	}
	return ""
}

func rawName(raw json.RawMessage) string {
	value := rawValue(raw)
	return strings.TrimSpace(rawNameValue(value))
}

func rawNameValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		for _, item := range v {
			if name := rawNameValue(item); name != "" {
				return name
			}
		}
	case map[string]any:
		for _, key := range []string{"title", "name", "toolName", "command"} {
			if name := rawNameValue(v[key]); name != "" {
				return name
			}
		}
		for _, key := range []string{"toolCall", "tool_call", "function", "payload", "item"} {
			if name := rawNameValue(v[key]); name != "" {
				return name
			}
		}
	}
	return ""
}

func rawString(raw json.RawMessage, key string) string {
	value, ok := rawValue(raw).(map[string]any)
	if !ok {
		return ""
	}
	text, _ := value[key].(string)
	return strings.TrimSpace(text)
}

func rawValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func truncateRunes(text string, limit int) string {
	text = strings.TrimSpace(text)
	if text == "" || limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

func formatNewSessionReply(session Session, source string) string {
	mode := currentModeDisplay(session)
	if mode == "" {
		mode = "未知"
	}
	model := currentModelDisplay(session)
	if model == "" {
		model = "未知"
	}
	return fmt.Sprintf("已为当前会话创建 ACP 会话。\n标题：%s\nmode：%s\nmodel：%s\nagent：%s\ncwd：%s\ncwd 来源：%s\nsession：%s",
		displaySessionTitle(session), mode, model, session.AgentName, session.Cwd, source, session.ACPSessionID)
}

func promptWithUserMessage(prefixes []string, text string) string {
	sections := make([]string, 0, len(prefixes)+2)
	for _, prefix := range prefixes {
		if strings.TrimSpace(prefix) != "" {
			sections = append(sections, prefix)
		}
	}
	if len(sections) == 0 {
		return text
	}
	sections = append(sections, "## User Message", text)
	return strings.Join(sections, "\n\n")
}

type newSessionRequest struct {
	Cwd         string
	Title       string
	ManualTitle bool
}

func (s *Service) resolveNewSessionRequest(fields []string, msg feishu.Message) (newSessionRequest, string, string) {
	args := fields[1:]
	req := newSessionRequest{}
	if len(args) > 0 {
		if args[0] == "--title" || args[0] == "-t" {
			req.Title = normalizeSessionTitle(strings.Join(args[1:], " "))
			req.ManualTitle = req.Title != ""
		} else {
			candidate, err := config.ExpandPath(args[0])
			if err == nil {
				if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
					req.Cwd = candidate
					req.Title = normalizeSessionTitle(strings.Join(args[1:], " "))
					req.ManualTitle = req.Title != ""
				} else {
					req.Title = normalizeSessionTitle(strings.Join(args, " "))
					req.ManualTitle = req.Title != ""
				}
			} else {
				req.Title = normalizeSessionTitle(strings.Join(args, " "))
				req.ManualTitle = req.Title != ""
			}
		}
	}
	if req.Cwd != "" {
		return req, "命令参数", ""
	}
	if session, ok := s.findSession(msg); ok && session.Cwd != "" {
		req.Cwd = session.Cwd
		return req, "当前会话已有会话", ""
	}
	agentName := s.defaultAgentName()
	agent, ok := s.registry.Get(agentName)
	if !ok || strings.TrimSpace(agent.DefaultCwd) == "" {
		return newSessionRequest{}, "", "当前会话还没有会话映射，且默认 agent 未配置 default_cwd。请使用 /new <cwd> 指定工作目录。"
	}
	req.Cwd = agent.DefaultCwd
	return req, "默认配置", ""
}

func (s *Service) nextDefaultSessionTitle(store *SessionStore, msg feishu.Message) string {
	next := 1
	if store != nil {
		next = len(store.ListByChat(msg.BotID, msg.ChatID)) + 1
	}
	if next < 1 {
		next = 1
	}
	return fmt.Sprintf("session#%d", next)
}

const maxSessionTitleRunes = 40

func normalizeSessionTitle(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return ""
	}
	runes := []rune(title)
	if len(runes) <= maxSessionTitleRunes {
		return title
	}
	return string(runes[:maxSessionTitleRunes]) + "..."
}

func titleFromPrompt(text string) string {
	return normalizeSessionTitle(text)
}

func (s *Service) updateAutomaticSessionTitle(ctx context.Context, msg feishu.Message, session Session, userText string) Session {
	if session.ManualTitle {
		return session
	}
	title := titleFromPrompt(userText)
	if title == "" || title == session.Title {
		return session
	}
	session.Title = title
	store := s.storeForMessage(msg)
	if store == nil {
		return session
	}
	if latest, ok := store.Get(session.Key); ok && latest.ACPSessionID == session.ACPSessionID {
		session = latest
		session.Title = title
	}
	if err := store.Upsert(session); err != nil {
		slog.WarnContext(ctx, "保存自动会话标题失败", "session", session.ACPSessionID, "错误", err)
	}
	return session
}

func displaySessionTitle(session Session) string {
	if strings.TrimSpace(session.Title) != "" {
		return session.Title
	}
	return "(未命名)"
}

func (s *Service) defaultAgentName() string {
	names := s.registry.Names()
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func (s *Service) storeForMessage(msg feishu.Message) *SessionStore {
	if s.stores == nil {
		return nil
	}
	if store := s.stores[msg.BotID]; store != nil {
		return store
	}
	return s.stores[""]
}

func sessionKeyFromMessage(msg feishu.Message) SessionKey {
	keys := sessionKeysFromMessage(msg)
	if len(keys) == 0 {
		return SessionKey{BotID: msg.BotID, ChatID: msg.ChatID}
	}
	return keys[0]
}

func chatKeyFromMessage(msg feishu.Message) ChatKey {
	return ChatKey{BotID: msg.BotID, ChatID: msg.ChatID}
}

func (s *Service) findSession(msg feishu.Message) (Session, bool) {
	store := s.storeForMessage(msg)
	if store == nil {
		return Session{}, false
	}
	for _, key := range sessionKeysFromMessage(msg) {
		if session, ok := store.Get(key); ok {
			return session, true
		}
	}
	return Session{}, false
}

func sessionKeysFromMessage(msg feishu.Message) []SessionKey {
	if msg.IsPrivateChat() {
		if msg.ChatID == "" {
			return nil
		}
		return []SessionKey{{BotID: msg.BotID, ChatID: msg.ChatID}}
	}
	if !msg.IsTopicThread() {
		if msg.ChatID == "" {
			return nil
		}
		return []SessionKey{{BotID: msg.BotID, ChatID: msg.ChatID}}
	}
	seen := make(map[string]bool)
	keys := make([]SessionKey, 0, 3)
	add := func(id string) {
		if msg.ChatID == "" || id == "" {
			return
		}
		key := msg.BotID + "\x00" + id
		if seen[key] {
			return
		}
		seen[key] = true
		keys = append(keys, SessionKey{BotID: msg.BotID, ChatID: msg.ChatID, ThreadID: id})
	}
	add(msg.ThreadID)
	add(msg.RootID)
	add(msg.ParentID)
	add(msg.MessageID)
	return keys
}

func sessionLabel(msg feishu.Message) string {
	if msg.IsPrivateChat() {
		return "当前私聊会话"
	}
	if !msg.IsTopicThread() {
		return "当前群聊会话"
	}
	return "当前话题会话"
}

func isTopicGroupMessage(msg feishu.Message) bool {
	return strings.EqualFold(msg.ChatType, "group") && msg.IsTopicThread()
}

func displayBotID(botID string) string {
	if strings.TrimSpace(botID) == "" {
		return "default"
	}
	return botID
}

func stripMentionNames(text string, mentions []feishu.Mention) string {
	for _, mention := range mentions {
		if mention.Name == "" {
			continue
		}
		text = strings.ReplaceAll(text, "@"+mention.Name, "")
	}
	return strings.TrimSpace(text)
}

func parseWikiInterval(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("wiki interval 不能为空")
	}
	if n, err := strconv.Atoi(raw); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("wiki interval 必须大于 0")
		}
		return time.Duration(n) * time.Minute, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("wiki interval 格式无效，可用 5m、30s 或纯数字分钟")
	}
	if d <= 0 {
		return 0, fmt.Errorf("wiki interval 必须大于 0")
	}
	return d, nil
}

func wikiInterval(chat ChatConfig) time.Duration {
	if chat.WikiIntervalSec > 0 {
		return time.Duration(chat.WikiIntervalSec) * time.Second
	}
	return defaultWikiInterval
}

func formatDuration(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return d.String()
}

func (s *Service) wikiStatus(msg feishu.Message, chat ChatConfig) string {
	enabled := !chat.WikiDisabled
	lines := []string{
		"当前聊天自动知识沉淀：" + map[bool]string{true: "开启", false: "关闭"}[enabled],
		"延迟：" + formatDuration(wikiInterval(chat)),
	}
	session, hasSession := s.findSession(msg)
	s.taskMu.Lock()
	var status wikiRunStatus
	var timerSet bool
	var task *runningTask
	wikiTaskRunning := false
	if hasSession {
		status = s.wikiStatuses[session.Key]
		_, timerSet = s.wikiTimers[session.Key]
		task = s.tasks[session.Key]
		for runtime := range s.wikiTasks {
			if runtime.SessionKey == session.Key {
				wikiTaskRunning = true
				break
			}
		}
	}
	s.taskMu.Unlock()
	if timerSet {
		lines = append(lines, "状态：等待定时触发")
	} else if status.running || wikiTaskRunning || (task != nil && task.kind == taskKindWiki) {
		lines = append(lines, "状态：正在反思")
	} else if !status.lastStarted.IsZero() {
		state := "成功"
		if !status.lastSuccess {
			state = "失败"
		}
		lines = append(lines, "最近一次："+state)
		lines = append(lines, "开始："+status.lastStarted.Format(time.RFC3339))
		if !status.lastEnded.IsZero() {
			lines = append(lines, "结束："+status.lastEnded.Format(time.RFC3339))
		}
		if status.lastError != "" {
			lines = append(lines, "错误："+status.lastError)
		}
	} else {
		lines = append(lines, "状态：尚未触发")
	}
	return strings.Join(lines, "\n")
}

func wikiReflectionPrompt(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		workspace = "$BOT_WORKSPACE"
	}
	return strings.Join([]string{
		"请对刚才的对话进行反思，根据需要更新你的知识体系。",
		"",
		"## 操作规范",
		"先阅读 `" + workspace + "/skills/wiki/SKILL.md` 了解完整的知识维护规范，然后按其中的流程执行。",
		"如果没有值得沉淀的新信息，不要修改文件。",
		"本轮是系统内部反思轮次，无需回复用户；如果必须输出文本，只输出 NoReply。",
	}, "\n")
}

func (s *Service) startTask(ctx context.Context, session Session, agent config.AgentConfig, kind taskKind) (context.Context, func()) {
	s.cancelWikiTimer(session.Key)
	ctx, cancel := context.WithCancel(ctx)
	task := &runningTask{
		kind:    kind,
		runtime: currentRuntimeKey(session.Key),
		cancel:  cancel,
		session: session,
		agent:   agent,
	}

	var previous *runningTask
	s.taskMu.Lock()
	previous = s.tasks[session.Key]
	s.tasks[session.Key] = task
	s.taskMu.Unlock()
	if previous != nil {
		previous.cancel()
		if previous.onCancel != nil {
			previous.onCancel(ctx, "已取消")
		}
		go s.cancelRuntimeTask(ctx, previous)
	}

	return ctx, func() {
		s.taskMu.Lock()
		if s.tasks[session.Key] == task {
			delete(s.tasks, session.Key)
		}
		s.taskMu.Unlock()
		cancel()
	}
}

func (s *Service) setTaskCancelHandler(key SessionKey, handler func(context.Context, string)) {
	if handler == nil {
		return
	}
	s.taskMu.Lock()
	if task := s.tasks[key]; task != nil {
		task.onCancel = handler
	}
	s.taskMu.Unlock()
}

func (s *Service) startWikiTask(ctx context.Context, session Session, agent config.AgentConfig, runtime runtimeKey) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	task := &runningTask{
		kind:    taskKindWiki,
		runtime: runtime,
		cancel:  cancel,
		session: session,
		agent:   agent,
	}

	s.taskMu.Lock()
	if s.wikiTasks == nil {
		s.wikiTasks = make(map[runtimeKey]*runningTask)
	}
	s.wikiTasks[runtime] = task
	s.taskMu.Unlock()

	return ctx, func() {
		s.taskMu.Lock()
		if s.wikiTasks[runtime] == task {
			delete(s.wikiTasks, runtime)
		}
		s.taskMu.Unlock()
		cancel()
	}
}

func (s *Service) cancelRuntimeTask(ctx context.Context, task *runningTask) {
	if task == nil || strings.TrimSpace(task.session.ACPSessionID) == "" {
		return
	}
	if err := s.runtime.CancelSession(ctx, task.runtime, task.session, task.agent); err != nil {
		slog.WarnContext(ctx, "取消 ACP session 失败", "session", task.session.ACPSessionID, "kind", task.kind, "错误", err)
	}
}

func (s *Service) cancelRunningSessionWork(ctx context.Context, key SessionKey) {
	s.taskMu.Lock()
	task := s.tasks[key]
	delete(s.tasks, key)
	s.taskMu.Unlock()
	if task != nil {
		task.cancel()
		if task.onCancel != nil {
			task.onCancel(ctx, "已取消")
		}
		go s.cancelRuntimeTask(ctx, task)
	}
}

func (s *Service) cancelSessionWork(ctx context.Context, key SessionKey) {
	s.cancelWikiTimer(key)
	s.cancelRunningSessionWork(ctx, key)
	s.cancelWikiTasks(ctx, key)
}

func (s *Service) cancelAllSessionWork(ctx context.Context) {
	s.taskMu.Lock()
	if s.wikiGenerations == nil {
		s.wikiGenerations = make(map[SessionKey]int64)
	}
	timers := make([]*time.Timer, 0, len(s.wikiTimers))
	for key, pending := range s.wikiTimers {
		s.wikiGenerations[key]++
		if pending != nil && pending.timer != nil {
			timers = append(timers, pending.timer)
		}
		delete(s.wikiTimers, key)
	}
	tasks := make([]*runningTask, 0, len(s.tasks)+len(s.wikiTasks))
	for key, task := range s.tasks {
		if task != nil {
			tasks = append(tasks, task)
		}
		delete(s.tasks, key)
	}
	for runtime, task := range s.wikiTasks {
		if task != nil {
			tasks = append(tasks, task)
		}
		delete(s.wikiTasks, runtime)
	}
	s.taskMu.Unlock()
	for _, timer := range timers {
		timer.Stop()
	}
	for _, task := range tasks {
		task.cancel()
		if task.onCancel != nil {
			task.onCancel(ctx, "已取消")
		}
		s.cancelRuntimeTask(ctx, task)
	}
}

func (s *Service) cancelWikiTasks(ctx context.Context, key SessionKey) {
	s.taskMu.Lock()
	tasks := make([]*runningTask, 0)
	for runtime, task := range s.wikiTasks {
		if runtime.SessionKey == key {
			tasks = append(tasks, task)
			delete(s.wikiTasks, runtime)
		}
	}
	s.taskMu.Unlock()
	for _, task := range tasks {
		task.cancel()
		if task.onCancel != nil {
			task.onCancel(ctx, "已取消")
		}
		go s.cancelRuntimeTask(ctx, task)
	}
}

func (s *Service) cancelWikiTimer(key SessionKey) {
	s.taskMu.Lock()
	if s.wikiGenerations == nil {
		s.wikiGenerations = make(map[SessionKey]int64)
	}
	s.wikiGenerations[key]++
	pending := s.wikiTimers[key]
	delete(s.wikiTimers, key)
	s.taskMu.Unlock()
	if pending != nil && pending.timer != nil {
		pending.timer.Stop()
	}
}

func (s *Service) hasWikiTimer(key SessionKey) bool {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	return s.wikiTimers[key] != nil
}

func (s *Service) takePendingWiki(key SessionKey) (pendingWikiRun, bool) {
	s.taskMu.Lock()
	if s.wikiGenerations == nil {
		s.wikiGenerations = make(map[SessionKey]int64)
	}
	s.wikiGenerations[key]++
	pending := s.wikiTimers[key]
	delete(s.wikiTimers, key)
	s.taskMu.Unlock()
	if pending == nil {
		return pendingWikiRun{}, false
	}
	if pending.timer != nil {
		pending.timer.Stop()
	}
	return *pending, true
}

func (s *Service) restorePendingWiki(pending pendingWikiRun) {
	if strings.TrimSpace(pending.session.ACPSessionID) == "" {
		return
	}
	interval := wikiInterval(s.wikiConfigForSession(pending.session))
	if interval <= 0 {
		interval = defaultWikiInterval
	}
	delay := time.Until(pending.scheduled.Add(interval))
	if delay <= 0 {
		delay = time.Millisecond
	}
	key := pending.session.Key
	s.taskMu.Lock()
	if s.wikiGenerations == nil {
		s.wikiGenerations = make(map[SessionKey]int64)
	}
	s.wikiGenerations[key]++
	generation := s.wikiGenerations[key]
	if old := s.wikiTimers[key]; old != nil {
		old.timer.Stop()
	}
	pending.generation = generation
	pending.timer = time.AfterFunc(delay, func() {
		s.runWikiTimer(key, generation, pending.session, pending.agent)
	})
	s.wikiTimers[key] = &pending
	s.taskMu.Unlock()
}

func (s *Service) scheduleWikiAfterUserPrompt(session Session, agent config.AgentConfig) {
	chat := s.wikiConfigForSession(session)
	if chat.WikiDisabled || strings.TrimSpace(session.ACPSessionID) == "" {
		s.cancelWikiTimer(session.Key)
		return
	}
	interval := wikiInterval(chat)
	if interval <= 0 {
		interval = defaultWikiInterval
	}
	key := session.Key
	s.taskMu.Lock()
	if s.wikiGenerations == nil {
		s.wikiGenerations = make(map[SessionKey]int64)
	}
	s.wikiGenerations[key]++
	generation := s.wikiGenerations[key]
	if old := s.wikiTimers[key]; old != nil {
		old.timer.Stop()
	}
	timer := time.AfterFunc(interval, func() {
		s.runWikiTimer(key, generation, session, agent)
	})
	s.wikiTimers[key] = &pendingWikiRun{
		timer:      timer,
		generation: generation,
		session:    session,
		agent:      agent,
		scheduled:  time.Now(),
	}
	s.taskMu.Unlock()
}

func (s *Service) wikiConfigForSession(session Session) ChatConfig {
	key := ChatKey{BotID: session.Key.BotID, ChatID: session.Key.ChatID}
	chat := ChatConfig{Key: key}
	store := s.storeForMessage(feishu.Message{BotID: session.Key.BotID})
	if store != nil {
		if existing, ok := store.GetChat(key); ok {
			return existing
		}
	}
	chat.WikiDisabled = session.WikiDisabled
	chat.WikiIntervalSec = session.WikiIntervalSec
	return chat
}

func (s *Service) runWikiTimer(key SessionKey, generation int64, session Session, agent config.AgentConfig) {
	s.taskMu.Lock()
	if s.wikiGenerations[key] != generation {
		s.taskMu.Unlock()
		return
	}
	delete(s.wikiTimers, key)
	if current := s.tasks[key]; current != nil {
		s.wikiGenerations[key]++
		s.taskMu.Unlock()
		s.scheduleWikiAfterUserPrompt(session, agent)
		return
	}
	s.taskMu.Unlock()

	ctx, finish := s.startTask(context.Background(), session, agent, taskKindWiki)
	s.markWikiStarted(key)
	_, err := s.runtime.Prompt(ctx, session, agent, wikiReflectionPrompt(sessionWorkspace(session, feishu.Message{})), acp.PromptOptions{})
	finish()
	s.markWikiFinished(key, session, err)
}

func (s *Service) runPendingWikiAsync(pending pendingWikiRun) {
	if strings.TrimSpace(pending.session.ACPSessionID) == "" {
		return
	}
	go s.runPendingWikiWithRuntimeKey(pending)
}

func (s *Service) runPendingWikiWithRuntimeKey(pending pendingWikiRun) {
	key := pending.session.Key
	runtime := wikiRuntimeKey(key, pending.generation, pending.session.ACPSessionID)
	ctx, finish := s.startWikiTask(context.Background(), pending.session, pending.agent, runtime)
	defer func() {
		finish()
		if err := s.runtime.CloseRuntimeKey(runtime); err != nil {
			slog.Warn("关闭 wiki ACP runtime 失败", "session", pending.session.ACPSessionID, "错误", err)
		}
	}()
	s.markWikiStarted(key)
	_, err := s.runtime.PromptWithRuntimeKey(ctx, runtime, pending.session, pending.agent, wikiReflectionPrompt(sessionWorkspace(pending.session, feishu.Message{})), acp.PromptOptions{})
	s.markWikiFinished(key, pending.session, err)
}

func (s *Service) markWikiStarted(key SessionKey) {
	s.taskMu.Lock()
	status := s.wikiStatuses[key]
	status.running = true
	status.lastStarted = time.Now()
	status.lastEnded = time.Time{}
	status.lastError = ""
	status.lastSuccess = false
	s.wikiStatuses[key] = status
	s.taskMu.Unlock()
}

func (s *Service) markWikiFinished(key SessionKey, session Session, err error) {
	s.taskMu.Lock()
	status := s.wikiStatuses[key]
	status.running = false
	status.lastEnded = time.Now()
	if err != nil && !errors.Is(err, context.Canceled) {
		status.lastError = err.Error()
		status.lastSuccess = false
	} else {
		status.lastError = ""
		status.lastSuccess = true
	}
	s.wikiStatuses[key] = status
	s.taskMu.Unlock()
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("wiki 自动知识沉淀失败", "session", session.ACPSessionID, "错误", err)
	}
}
