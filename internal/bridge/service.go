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
