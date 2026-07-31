package bridge

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

// Service 本项目核心服务
type Service struct {
	cfg             config.Config            // 配置文件
	configPath      string                   // 配置文件路径，用于内置后台重启
	registry        *acp.Registry            // 对接的 acp, 比如 traex -> "traex acp serve"
	runtime         acpRuntime               // acp client 运行时
	feishu          []*feishu.Adapter        // Bots 实例
	stores          map[string]*SessionStore // 会话存储, key=bot.id, 默认store用 "" 作为 key
	scheduleStores  map[string]*ScheduledTaskStore
	scheduleSenders map[string]scheduledTaskIMSender
	usageStores     map[string]*TokenUsageStore
	restartCommand  func(context.Context) error
	builtinRestart  bool // 是否允许使用内置后台 restart

	taskMu          sync.Mutex
	tasks           map[SessionKey]*runningTask
	wikiTasks       map[runtimeKey]*runningTask
	wikiTimers      map[SessionKey]*pendingWikiRun
	wikiGenerations map[SessionKey]int64
	wikiStatuses    map[SessionKey]wikiRunStatus
	loopStatuses    map[SessionKey]loopRunStatus
	scheduleRuns    map[string]scheduleRunStatus
	scheduleJobs    map[string]*scheduledTaskJob
	pendingAtTexts  map[ChatKey][]pendingAtMessage
	pendingAtAuto   map[SessionKey][]pendingAtMessage
	promptQueues    map[SessionKey]*promptQueue

	acpUpdateMu    sync.Mutex
	acpUpdateUnsub map[SessionKey]func()
}

// NewService 创建服务实例
//
//	@param cfg 服务配置
//	@param store 会话储存, 实际使用传nil即可, 单元测试可传 [NewSessionStore] 实例进来避免污染真实会话
func NewService(cfg config.Config, store *SessionStore) *Service {
	s := &Service{
		cfg:             cfg,
		registry:        acp.NewRegistry(cfg),
		stores:          make(map[string]*SessionStore),
		scheduleStores:  make(map[string]*ScheduledTaskStore),
		scheduleSenders: make(map[string]scheduledTaskIMSender),
		usageStores:     make(map[string]*TokenUsageStore),
		runtime:         newRuntimeManager(),
		tasks:           make(map[SessionKey]*runningTask),
		wikiTasks:       make(map[runtimeKey]*runningTask),
		wikiTimers:      make(map[SessionKey]*pendingWikiRun),
		wikiGenerations: make(map[SessionKey]int64),
		wikiStatuses:    make(map[SessionKey]wikiRunStatus),
		loopStatuses:    make(map[SessionKey]loopRunStatus),
		scheduleRuns:    make(map[string]scheduleRunStatus),
		scheduleJobs:    make(map[string]*scheduledTaskJob),
		pendingAtTexts:  make(map[ChatKey][]pendingAtMessage),
		pendingAtAuto:   make(map[SessionKey][]pendingAtMessage),
		promptQueues:    make(map[SessionKey]*promptQueue),
		acpUpdateUnsub:  make(map[SessionKey]func()),
	}
	for _, bot := range cfg.Bots {
		// 见 [Service.HandleFeishuMessage], s 实现了 [feishu.Handler]
		adapter := feishu.NewAdapter(bot, s)
		s.feishu = append(s.feishu, adapter)
		s.scheduleSenders[strings.TrimSpace(bot.ID)] = adapter.SendTextWithRenderContext
		if store != nil {
			s.stores[bot.ID] = store
		} else if strings.TrimSpace(bot.Workspace) != "" {
			s.stores[bot.ID] = NewSessionStore(filepath.Join(bot.Workspace, "sessions.json"))
		}
		if strings.TrimSpace(bot.Workspace) != "" {
			s.scheduleStores[bot.ID] = NewScheduledTaskStore(filepath.Join(bot.Workspace, "scheduled_tasks.json"))
			s.usageStores[bot.ID] = NewTokenUsageStore(filepath.Join(bot.Workspace, "token_usage.json"))
		}
	}
	if store != nil {
		s.stores[""] = store
		if strings.TrimSpace(store.path) != "" {
			s.usageStores[""] = NewTokenUsageStore(filepath.Join(filepath.Dir(store.path), "token_usage.json"))
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

// setRuntime 用于单元测试设置 fakeRuntime
func (s *Service) setRuntime(runtime acpRuntime) {
	if runtime != nil {
		s.runtime = runtime
	}
}

func (s *Service) setRestartCommand(command func(context.Context) error) {
	s.restartCommand = command
}

var _ feishu.Handler = (*Service)(nil)
var _ feishu.ModelSelectionHandler = (*Service)(nil)
var _ feishu.ModeSelectionHandler = (*Service)(nil)
var _ feishu.SessionSelectionHandler = (*Service)(nil)

const maxSessionHistoryPerChat = 10

const mentionOnlyPromptText = "（用户提及你，但本次无消息内容，请按历史消息，引用上下文回复）"

// HandleFeishuMessage 消息处理
// 实现 [feishu.Handler], 在 [NewService] 时将 [Service] 实例传入给了 [feishu.NewAdapter]
func (s *Service) HandleFeishuMessage(ctx context.Context, msg feishu.Message) (string, error) {
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
	slog.DebugContext(ctx, "处理解析后的消息", "text", text, "prompt_text", promptText)

	if s.shouldIgnoreMessage(msg, text) {
		if !strings.HasPrefix(text, "/") {
			s.cachePendingAtText(msg)
		}
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
	promptText = s.promptTextWithPendingAtTexts(msg, promptText)
	if s.shouldQueueAtAutoMessage(msg) {
		if s.queueAtAutoMessageIfBusy(msg) {
			return "", nil
		}
		promptText = s.promptTextWithAtAuto(msg, promptText)
	}
	return s.prompt(ctx, msg, promptText)
}
