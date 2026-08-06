package bridge

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

// Service 本项目核心服务
type Service struct {
	configMu       sync.RWMutex
	cfg            config.Config     // 配置文件
	configPath     string            // 配置文件路径，用于内置后台重启
	version        string            // bridge 自身版本，用于 /update
	registry       *acp.Registry     // 对接的 acp, 比如 traex -> "traex acp serve"
	runtime        acpRuntime        // acp client 运行时
	feishu         []*feishu.Adapter // Bots 实例
	restartCommand func(context.Context) error
	builtinRestart bool // 是否允许使用内置后台 restart

	serviceStores
	serviceOutbounds
	serviceTasks
	serviceScheduleRuns
	serviceACPUpdates
	backgroundWg backgroundGoroutines
}

type serviceStores struct {
	stores                 map[string]*SessionStore // 会话存储, key=bot.id, 默认store用 "" 作为 key
	scheduleStores         map[string]*ScheduledTaskStore
	scheduleSenders        map[string]scheduledTaskIMSender
	scheduleMessageSenders map[string]scheduledTaskMessageSender
	scheduleStreams        map[string]scheduledTaskStreamStarter
	usageStores            map[string]*TokenUsageStore
}

type serviceOutbounds struct {
	outboundMu sync.Mutex
	outbounds  map[string]feishu.Outbound
}

type serviceTasks struct {
	taskMu          sync.Mutex
	tasks           map[SessionKey]*runningTask
	wikiTasks       map[runtimeKey]*runningTask
	wikiTimers      map[SessionKey]*pendingWikiRun
	wikiGenerations map[SessionKey]int64
	wikiStatuses    map[SessionKey]wikiRunStatus
	workspaceLocks  workspaceTaskLocks
	loopStatuses    map[SessionKey]loopRunStatus
	acpErrors       map[SessionKey]acpErrorSnapshot
	pendingAtTexts  map[ChatKey][]pendingAtMessage
	pendingAtAuto   map[SessionKey][]pendingAtMessage
	promptQueues    map[SessionKey]*promptQueue
}

type serviceScheduleRuns struct {
	scheduleRuns map[string]scheduleRunStatus
	scheduleJobs map[string]*scheduledTaskJob
}

type serviceACPUpdates struct {
	acpUpdateMu    sync.Mutex
	acpUpdateUnsub map[SessionKey]func()
}

// NewService 创建服务实例
//
//	@param cfg 服务配置
//	@param store 会话储存, 实际使用传nil即可, 单元测试可传 [NewSessionStore] 实例进来避免污染真实会话
func NewService(cfg config.Config, store *SessionStore) *Service {
	s := &Service{
		cfg:      cfg,
		registry: acp.NewRegistry(cfg),
		runtime:  newRuntimeManager(),
		serviceStores: serviceStores{
			stores:                 make(map[string]*SessionStore),
			scheduleStores:         make(map[string]*ScheduledTaskStore),
			scheduleSenders:        make(map[string]scheduledTaskIMSender),
			scheduleMessageSenders: make(map[string]scheduledTaskMessageSender),
			scheduleStreams:        make(map[string]scheduledTaskStreamStarter),
			usageStores:            make(map[string]*TokenUsageStore),
		},
		serviceOutbounds: serviceOutbounds{
			outbounds: make(map[string]feishu.Outbound),
		},
		serviceTasks: serviceTasks{
			tasks:           make(map[SessionKey]*runningTask),
			wikiTasks:       make(map[runtimeKey]*runningTask),
			wikiTimers:      make(map[SessionKey]*pendingWikiRun),
			wikiGenerations: make(map[SessionKey]int64),
			wikiStatuses:    make(map[SessionKey]wikiRunStatus),
			workspaceLocks:  newWorkspaceTaskLocks(),
			loopStatuses:    make(map[SessionKey]loopRunStatus),
			acpErrors:       make(map[SessionKey]acpErrorSnapshot),
			pendingAtTexts:  make(map[ChatKey][]pendingAtMessage),
			pendingAtAuto:   make(map[SessionKey][]pendingAtMessage),
			promptQueues:    make(map[SessionKey]*promptQueue),
		},
		serviceScheduleRuns: serviceScheduleRuns{
			scheduleRuns: make(map[string]scheduleRunStatus),
			scheduleJobs: make(map[string]*scheduledTaskJob),
		},
		serviceACPUpdates: serviceACPUpdates{
			acpUpdateUnsub: make(map[SessionKey]func()),
		},
	}
	for _, bot := range cfg.Bots {
		// 见 [Service.HandleFeishuMessage], s 实现了 [feishu.Handler]
		adapter := feishu.NewAdapter(bot, s)
		s.feishu = append(s.feishu, adapter)
		s.scheduleSenders[strings.TrimSpace(bot.ID)] = adapter.SendTextWithRenderContext
		s.scheduleMessageSenders[strings.TrimSpace(bot.ID)] = adapter.SendTextMessage
		s.scheduleStreams[strings.TrimSpace(bot.ID)] = adapter.StartStreamCard
		if store != nil {
			s.stores[bot.ID] = store
		} else if strings.TrimSpace(bot.Workspace) != "" {
			s.stores[bot.ID] = NewSessionStoreWithFallback(
				workspaceLocalPath(bot.Workspace, "sessions.json"),
				workspaceLegacyPath(bot.Workspace, "sessions.json"),
			)
		}
		if strings.TrimSpace(bot.Workspace) != "" {
			s.scheduleStores[bot.ID] = NewScheduledTaskStoreWithFallback(
				workspaceLocalPath(bot.Workspace, "scheduled_tasks.json"),
				workspaceLegacyPath(bot.Workspace, "scheduled_tasks.json"),
			)
			s.usageStores[bot.ID] = NewTokenUsageStoreWithFallback(
				workspaceLocalPath(bot.Workspace, "token_usage.json"),
				workspaceLegacyPath(bot.Workspace, "token_usage.json"),
			)
		}
	}
	if store != nil {
		s.stores[""] = store
		if strings.TrimSpace(store.path) != "" {
			s.usageStores[""] = NewTokenUsageStore(sessionStoreSiblingPath(store.path, "token_usage.json"))
		}
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

// WithVersion 设置 bridge 自身版本号，供 /update 使用。
func (s *Service) WithVersion(version string) *Service {
	s.version = strings.TrimSpace(version)
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

func (s *Service) configBots() []config.BotConfig {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	bots := make([]config.BotConfig, len(s.cfg.Bots))
	for i, bot := range s.cfg.Bots {
		bots[i] = cloneBotConfig(bot)
	}
	return bots
}

func (s *Service) configRestartCommand() []string {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return append([]string(nil), s.cfg.RestartCommand...)
}

func (s *Service) messageReactionEnabled() bool {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.cfg.MessageReaction
}

var _ feishu.Handler = (*Service)(nil)
var _ feishu.ModelSelectionHandler = (*Service)(nil)
var _ feishu.ModeSelectionHandler = (*Service)(nil)
var _ feishu.SessionSelectionHandler = (*Service)(nil)
var _ feishu.OverviewActionHandler = (*Service)(nil)
var _ feishu.DriveCommentCapabilityHandler = (*Service)(nil)

const maxSessionHistoryPerChat = 10

const mentionOnlyPromptText = "（用户提及你，但本次无消息内容，请按历史消息，引用上下文回复）"

type incomingPromptMessage struct {
	msg        feishu.Message
	text       string
	promptText string
}

// HandleFeishuMessage 消息处理
// 实现 [feishu.Handler], 在 [NewService] 时将 [Service] 实例传入给了 [feishu.NewAdapter]
func (s *Service) HandleFeishuMessage(ctx context.Context, msg feishu.Message) (string, error) {
	incoming := s.normalizeIncomingMessage(msg)
	slog.DebugContext(ctx, "处理解析后的消息", "text", incoming.text, "prompt_text", incoming.promptText)

	if s.shouldSkipIncomingMessage(ctx, incoming) {
		return "", nil
	}
	if cleanup, ok := s.startIncomingProcessingReaction(ctx, incoming.msg); ok {
		defer cleanup()
	}
	if errText := s.ensureIncomingWorkspace(ctx, incoming.msg); errText != "" {
		return errText, nil
	}
	if incoming.promptText == "" {
		if s.shouldSilenceAtAutoUnsupported(incoming.msg) {
			slog.InfoContext(ctx, "at-auto 群消息内容为空或暂不支持，静默跳过")
			return "", nil
		}
		return "暂不支持的消息类型。", nil
	}
	if strings.HasPrefix(incoming.text, "/") {
		return s.handleSlashCommandMessage(ctx, incoming.msg, incoming.text), nil
	}
	return s.handlePromptMessage(ctx, incoming)
}

func (s *Service) normalizeIncomingMessage(msg feishu.Message) incomingPromptMessage {
	if strings.TrimSpace(msg.BotOpenID) == "" {
		msg.BotOpenID = s.botOpenID(msg.BotID)
	}
	text := strings.TrimSpace(msg.Text)
	text = stripCurrentBotMentionNames(text, msg)
	msg.Text = text
	promptText := strings.TrimSpace(msg.PromptText())
	if promptText == "" && messageMentionsBot(msg) {
		promptText = mentionOnlyPromptText
	}
	return incomingPromptMessage{msg: msg, text: text, promptText: promptText}
}

func (s *Service) shouldSkipIncomingMessage(ctx context.Context, incoming incomingPromptMessage) bool {
	if s.shouldIgnoreMessage(incoming.msg, incoming.text) {
		if !strings.HasPrefix(incoming.text, "/") {
			s.cachePendingAtText(incoming.msg)
		}
		slog.InfoContext(ctx, "群聊消息未 at bot，按当前 chat 配置跳过")
		return true
	}
	return false
}

func (s *Service) startIncomingProcessingReaction(ctx context.Context, msg feishu.Message) (func(), bool) {
	if !s.shouldStartProcessingReaction(msg) {
		return nil, false
	}
	return s.startProcessingReaction(ctx, msg)
}

func (s *Service) ensureIncomingWorkspace(ctx context.Context, msg feishu.Message) string {
	if _, err := ensureWorkspace(msg.Workspace, msg.BotID); err != nil {
		slog.ErrorContext(ctx, "初始化 workspace 失败", "workspace", msg.Workspace, "错误", err)
		return "初始化 workspace 失败：" + err.Error()
	}
	return ""
}

func (s *Service) handleSlashCommandMessage(ctx context.Context, msg feishu.Message, text string) string {
	if s.shouldSilenceAtAutoUnsupportedCommand(msg, text) {
		slog.InfoContext(ctx, "at-auto 群消息命中不支持的 slash 命令，静默跳过", "command", firstSlashCommandName(text))
		return ""
	}
	if !s.slashCommandAllowed(msg) {
		if len(s.ownerOpenIDs(msg.BotID)) == 0 {
			return "未配置 bot owner，不能执行斜杠命令。"
		}
		return "只有 bot owner 可以执行斜杠命令。"
	}
	return s.handleCommand(ctx, text, msg)
}

func (s *Service) shouldSilenceAtAutoUnsupported(msg feishu.Message) bool {
	return s.shouldHandleAtAutoMessage(msg)
}

func (s *Service) shouldSilenceAtAutoUnsupportedCommand(msg feishu.Message, text string) bool {
	if !s.shouldSilenceAtAutoUnsupported(msg) {
		return false
	}
	command := firstSlashCommandName(text)
	if command == "" || strings.HasPrefix(command, "//") {
		return false
	}
	_, ok := lookupSlashCommand(command)
	return !ok
}

func firstSlashCommandName(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimSpace(fields[0])
}

func (s *Service) handlePromptMessage(ctx context.Context, incoming incomingPromptMessage) (string, error) {
	promptText := s.promptTextWithPendingAtTexts(incoming.msg, incoming.promptText)
	if s.shouldQueueAtAutoMessage(incoming.msg) {
		if s.queueAtAutoMessageIfBusy(incoming.msg) {
			return "", nil
		}
		promptText = s.promptTextWithAtAuto(incoming.msg, promptText)
	}
	return s.prompt(ctx, incoming.msg, promptText)
}
