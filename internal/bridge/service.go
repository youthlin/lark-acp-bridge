package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

// Service 本项目核心服务
type Service struct {
	cfg      config.Config            // 配置文件
	registry *acp.Registry            // 对接的 acp, 比如 traex -> "traex acp serve"
	feishu   []*feishu.Adapter        // Bots 实例
	stores   map[string]*SessionStore // 会话存储, key=bot.id, 默认store用 "" 作为 key
	runtime  acpRuntime               // acp client 运行时

	taskMu          sync.Mutex
	tasks           map[SessionKey]*runningTask
	wikiTimers      map[SessionKey]*time.Timer
	wikiGenerations map[SessionKey]int64
	wikiStatuses    map[SessionKey]wikiRunStatus
}

type taskKind string

const (
	taskKindUser taskKind = "user"
	taskKindWiki taskKind = "wiki"

	defaultWikiInterval = 5 * time.Minute
)

type runningTask struct {
	kind    taskKind
	cancel  context.CancelFunc
	session Session
	agent   config.AgentConfig
}

type wikiRunStatus struct {
	running     bool
	lastStarted time.Time
	lastEnded   time.Time
	lastError   string
	lastSuccess bool
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
		wikiTimers:      make(map[SessionKey]*time.Timer),
		wikiGenerations: make(map[SessionKey]int64),
		wikiStatuses:    make(map[SessionKey]wikiRunStatus),
	}
	for _, bot := range cfg.Bots {
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

// setRuntime 用于单元测试设置 fakeRuntime
func (s *Service) setRuntime(runtime acpRuntime) {
	if runtime != nil {
		s.runtime = runtime
	}
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
	for _, adapter := range s.feishu {
		if err := adapter.Start(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	slog.Info("关闭 ACP 桥接服务")
	for _, adapter := range s.feishu {
		if err := adapter.Shutdown(ctx); err != nil {
			return err
		}
	}
	return s.runtime.Shutdown(ctx)
}

var _ feishu.Handler = (*Service)(nil)

// HandleFeishuMessage 消息处理
// 实现 [feishu.Handler]
func (s *Service) HandleFeishuMessage(ctx context.Context, msg feishu.Message) (string, error) {
	text := strings.TrimSpace(msg.Text)
	text = stripMentionNames(text, msg.Mentions)
	slog.InfoContext(ctx, "处理解析后的消息", "text", text)

	workspaceStatus, err := ensureWorkspace(msg.Workspace)
	if err != nil {
		slog.ErrorContext(ctx, "初始化 workspace 失败", "workspace", msg.Workspace, "错误", err)
		return "初始化 workspace 失败：" + err.Error(), nil
	}
	if text == "" && !workspaceStatus.Ready {
		return workspaceGuide(workspaceStatus), nil
	}

	// 斜杠命令
	if strings.HasPrefix(text, "/") {
		s.cancelRunningMessageWork(msg)
		return s.handleCommand(ctx, text, msg), nil
	}
	// 普通消息
	if strings.TrimSpace(text) != "" {
		s.cancelMessageWork(msg)
	}
	return s.prompt(ctx, msg, text)
}

func (s *Service) handleCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "" // text以/开头才会进来 这里不可能走到
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
			"/wiki on|off|status|interval <duration> - 管理当前会话的自动知识沉淀",
			"/status - 查看服务状态",
			"",
			"普通文本消息会发送到当前会话的 ACP session；当前会话没有 session 时会自动创建。",
		}, "\n")
	case "/new":
		return s.newSession(ctx, fields, msg)
	case "/session":
		return s.handleSessionCommand(ctx, text, msg)
	case "/wiki":
		return s.handleWikiCommand(ctx, text, msg)
	case "/status":
		return s.status(msg)
	default:
		return "暂不支持这个命令。发送 /help 查看当前支持的命令。"
	}
}

func (s *Service) handleSessionCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return "可用命令：/session list 或 /session resume <index>"
	}
	switch fields[1] {
	case "list":
		return s.listSessions(msg)
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

func (s *Service) handleWikiCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return "可用命令：/wiki on、/wiki off、/wiki status 或 /wiki interval <duration>。"
	}
	session, ok := s.findSession(msg)
	if !ok {
		return "当前会话还没有 ACP session，发送普通文本或 /new 后再配置 wiki。"
	}
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	switch fields[1] {
	case "on":
		session.WikiDisabled = false
		if err := store.Upsert(session); err != nil {
			slog.ErrorContext(ctx, "保存 wiki 配置失败", "错误", err)
			return "保存 wiki 配置失败：" + err.Error()
		}
		return "已开启当前会话的自动知识沉淀。"
	case "off":
		session.WikiDisabled = true
		s.cancelWikiTimer(session.Key)
		if err := store.Upsert(session); err != nil {
			slog.ErrorContext(ctx, "保存 wiki 配置失败", "错误", err)
			return "保存 wiki 配置失败：" + err.Error()
		}
		return "已关闭当前会话的自动知识沉淀。"
	case "status":
		return s.wikiStatus(session)
	case "interval":
		if len(fields) < 3 {
			return "请使用 /wiki interval <duration> 指定时间，例如 /wiki interval 5m。"
		}
		interval, err := parseWikiInterval(fields[2])
		if err != nil {
			return err.Error()
		}
		session.WikiIntervalSec = int(interval.Seconds())
		if err := store.Upsert(session); err != nil {
			slog.ErrorContext(ctx, "保存 wiki interval 失败", "错误", err)
			return "保存 wiki interval 失败：" + err.Error()
		}
		if s.hasWikiTimer(session.Key) {
			if agent, ok := s.registry.Get(session.AgentName); ok {
				s.scheduleWikiAfterUserPrompt(session, agent)
			}
		}
		return "已设置当前会话自动知识沉淀延迟：" + formatDuration(interval) + "。"
	default:
		return "暂不支持这个 wiki 命令。可用 /wiki on、/wiki off、/wiki status 或 /wiki interval <duration>。"
	}
}

func (s *Service) listSessions(msg feishu.Message) string {
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	items := store.ListByChat(msg.BotID, msg.ChatID)
	if len(items) == 0 {
		return "当前聊天还没有历史 ACP 会话。发送普通文本会自动创建，或用 /new <cwd> 指定工作目录。"
	}
	current, hasCurrent := s.findSession(msg)
	lines := []string{"当前聊天的历史 ACP 会话："}
	for i, item := range items {
		marker := ""
		if hasCurrent && item.ACPSessionID == current.ACPSessionID {
			marker = " *"
		}
		lines = append(lines, fmt.Sprintf("%d. %s%s\n   标题：%s\n   cwd：%s\n   状态：%s", i+1, item.ACPSessionID, marker, displaySessionTitle(item), item.Cwd, item.Status))
	}
	lines = append(lines, "使用 /session resume <index> 恢复指定会话。")
	return strings.Join(lines, "\n")
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
	session := items[index-1]
	session.Key = sessionKeyFromMessage(msg)
	if err := store.Upsert(session); err != nil {
		slog.ErrorContext(ctx, "恢复会话映射失败", "错误", err)
		return "恢复会话失败：" + err.Error()
	}
	if err := s.runtime.CloseSession(session.Key); err != nil {
		slog.WarnContext(ctx, "关闭旧 ACP runtime 失败", "key", session.Key, "错误", err)
	}
	return fmt.Sprintf("已恢复会话 %d。\n标题：%s\nagent：%s\ncwd：%s\nsession：%s", index, displaySessionTitle(session), session.AgentName, session.Cwd, session.ACPSessionID)
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
	if err := store.Upsert(session); err != nil {
		slog.ErrorContext(ctx, "设置会话标题失败", "错误", err)
		return "设置会话标题失败：" + err.Error()
	}
	return "已设置当前会话标题：" + session.Title
}

func (s *Service) newSession(ctx context.Context, fields []string, msg feishu.Message) string {
	session, _, source, _, errText := s.createSession(ctx, fields, msg)
	if errText != "" {
		return errText
	}
	return formatNewSessionReply(session, source)
}

func (s *Service) createSession(ctx context.Context, fields []string, msg feishu.Message) (Session, config.AgentConfig, string, string, string) {
	slog.InfoContext(ctx, "准备创建ACP会话", "cmd", fields)
	store := s.storeForMessage(msg)
	if store == nil {
		return Session{}, config.AgentConfig{}, "", "", "会话持久化未初始化。"
	}
	req, source, errText := s.resolveNewSessionRequest(fields, msg)
	if errText != "" {
		return Session{}, config.AgentConfig{}, "", "", errText
	}
	cwd := req.Cwd
	if !filepath.IsAbs(cwd) {
		return Session{}, config.AgentConfig{}, "", "", "工作目录必须是绝对路径，可使用 /absolute/path 或 ~/path。"
	}
	if info, err := os.Stat(cwd); err != nil {
		return Session{}, config.AgentConfig{}, "", "", "工作目录不可访问：" + err.Error()
	} else if !info.IsDir() {
		return Session{}, config.AgentConfig{}, "", "", "工作目录不是目录：" + cwd
	}
	agentName := s.defaultAgentName()
	agent, ok := s.registry.Get(agentName)
	if !ok {
		return Session{}, config.AgentConfig{}, "", "", "未找到默认 agent 配置。"
	}
	workspaceStatus, err := ensureWorkspace(msg.Workspace)
	if err != nil {
		slog.ErrorContext(ctx, "初始化 workspace 失败", "workspace", msg.Workspace, "错误", err)
		return Session{}, config.AgentConfig{}, "", "", "初始化 workspace 失败：" + err.Error()
	}
	key := sessionKeyFromMessage(msg)
	s.cancelSessionWork(key)
	acpSessionID, err := s.runtime.NewSession(ctx, key, agentName, agent, filepath.Clean(cwd), msg.Workspace)
	if err != nil {
		slog.ErrorContext(ctx, "创建 ACP session 失败", "agent", agentName, "cwd", cwd, "错误", err)
		return Session{}, config.AgentConfig{}, "", "", "创建 ACP session 失败：" + err.Error()
	}
	session := Session{
		Key:                  key,
		Title:                req.Title,
		AgentName:            agentName,
		ACPSessionID:         acpSessionID,
		Cwd:                  filepath.Clean(cwd),
		Workspace:            msg.Workspace,
		Status:               "ready",
		PendingInitialPrompt: "ready",
	}
	initialPrompt := ""
	if workspaceStatus.Ready {
		initialPrompt = workspaceReadyPrompt(msg.Workspace)
	} else {
		session.Status = "setup"
		session.PendingInitialPrompt = "setup"
		initialPrompt = workspaceSetupPrompt(msg.Workspace)
	}
	if strings.TrimSpace(initialPrompt) == "" {
		session.PendingInitialPrompt = ""
	}
	if err := store.Upsert(session); err != nil {
		slog.ErrorContext(ctx, "保存会话映射失败", "错误", err)
		return Session{}, config.AgentConfig{}, "", "", "保存会话映射失败：" + err.Error()
	}
	slog.InfoContext(ctx, "创建 ACP session 成功", "agent", agentName, "cwd", cwd, "ready", workspaceStatus.Ready)
	return session, agent, source, initialPrompt, ""
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
			"状态："+session.Status,
		)
	} else {
		lines = append(lines, sessionLabel(msg)+"还没有会话映射；发送普通文本会自动创建，或用 /new <cwd> 指定工作目录。")
	}
	return strings.Join(lines, "\n")
}

func (s *Service) prompt(ctx context.Context, msg feishu.Message, text string) (string, error) {
	session, ok := s.findSession(msg)
	clearInitialPrompt := false
	if !ok {
		created, agent, _, initialPrompt, errText := s.createSession(ctx, []string{"/new", "--title", titleFromPrompt(text)}, msg)
		if errText != "" {
			return errText, nil
		}
		session = created
		if strings.TrimSpace(initialPrompt) != "" {
			if session.Status == "ready" {
				text = promptWithUserMessage([]string{
					initialPrompt,
					workspaceMemoryPolicyPrompt(sessionWorkspace(session, msg)),
				}, text)
			} else {
				text = initialPrompt
			}
			clearInitialPrompt = true
		} else if session.Status == "ready" {
			text = promptWithUserMessage([]string{workspaceMemoryPolicyPrompt(sessionWorkspace(session, msg))}, text)
		}
		return s.promptSession(ctx, msg, session, agent, text, clearInitialPrompt)
	}
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		return "", fmt.Errorf("未找到 agent 配置: %s", session.AgentName)
	}
	if session.Status == "setup" && strings.TrimSpace(session.PendingInitialPrompt) == "setup" {
		text = promptWithUserMessage([]string{workspaceSetupPrompt(sessionWorkspace(session, msg))}, text)
		clearInitialPrompt = true
	} else if session.Status == "ready" && strings.TrimSpace(session.ACPSessionID) == "" {
		created, _, _, initialPrompt, errText := s.createSession(ctx, []string{"/new", session.Cwd, titleFromPrompt(text)}, msg)
		if errText != "" {
			return errText, nil
		}
		session = created
		if strings.TrimSpace(initialPrompt) != "" {
			text = promptWithUserMessage([]string{
				initialPrompt,
				workspaceMemoryPolicyPrompt(sessionWorkspace(session, msg)),
			}, text)
			clearInitialPrompt = true
		} else {
			text = promptWithUserMessage([]string{workspaceMemoryPolicyPrompt(sessionWorkspace(session, msg))}, text)
		}
	} else if session.Status == "ready" && strings.TrimSpace(session.PendingInitialPrompt) == "ready" {
		workspace := sessionWorkspace(session, msg)
		text = promptWithUserMessage([]string{
			workspaceReadyPrompt(workspace),
			workspaceMemoryPolicyPrompt(workspace),
		}, text)
		clearInitialPrompt = true
	} else if session.Status == "ready" {
		if memoryPolicy := workspaceMemoryPolicyPrompt(sessionWorkspace(session, msg)); strings.TrimSpace(memoryPolicy) != "" {
			text = promptWithUserMessage([]string{memoryPolicy}, text)
		}
	}
	return s.promptSession(ctx, msg, session, agent, text, clearInitialPrompt)
}

func (s *Service) clearPendingInitialPrompt(ctx context.Context, msg feishu.Message, session Session) Session {
	if strings.TrimSpace(session.PendingInitialPrompt) == "" {
		return session
	}
	session.PendingInitialPrompt = ""
	store := s.storeForMessage(msg)
	if store == nil {
		return session
	}
	if err := store.Upsert(session); err != nil {
		slog.ErrorContext(ctx, "清除待发送初始 prompt 标记失败", "错误", err)
	}
	return session
}

func sessionWorkspace(session Session, msg feishu.Message) string {
	if strings.TrimSpace(session.Workspace) != "" {
		return session.Workspace
	}
	return msg.Workspace
}

func (s *Service) promptSession(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, clearInitialPrompt bool) (string, error) {
	reply, sentProgress, err := s.runUserPrompt(ctx, msg, session, agent, text)
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
	if clearInitialPrompt {
		s.clearPendingInitialPrompt(ctx, msg, session)
	}
	if strings.TrimSpace(reply) == "" {
		if sentProgress {
			return "", nil
		}
		return "ACP session 已完成，但没有返回文本。", nil
	}
	return reply, nil
}

func (s *Service) runUserPrompt(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string) (string, bool, error) {
	ctx, finish := s.startTask(ctx, session, agent, taskKindUser)
	defer finish()
	reply, sentProgress, err := s.promptRuntimeWithProgress(ctx, msg, session, agent, text)
	if errors.Is(err, context.Canceled) {
		return reply, sentProgress, err
	}
	if err == nil {
		s.scheduleWikiAfterUserPrompt(session, agent)
	}
	return reply, sentProgress, err
}

func (s *Service) promptRuntime(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string) (string, error) {
	reply, _, err := s.promptRuntimeWithProgress(ctx, msg, session, agent, text)
	return reply, err
}

func (s *Service) promptRuntimeWithProgress(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string) (string, bool, error) {
	slog.InfoContext(ctx, "准备发送消息给ACP后端")
	stream := newPromptCardStream(ctx, msg, session)
	chunks := newPromptChunkAccumulator(stream)
	flushStreams := func() {
		chunks.finishStream()
	}
	opts := acp.PromptOptions{
		OnUpdate: func(update acp.PromptUpdate) {
			slog.InfoContext(ctx, "ACP|OnUpdate", "update", update)
			if chunk, ok := promptUpdateChunk(update); ok {
				chunks.add(chunk)
				return
			}
			flushStreams()
			stream.updatePromptUpdate(update)
		},
	}
	reply, err := s.runtime.Prompt(ctx, session, agent, text, opts)
	chunks.close()
	streamedReply := chunks.replyText()
	if stream.hasStarted() {
		if finalReply := strings.TrimSpace(reply); finalReply != "" && finalReply != streamedReply {
			stream.updateText(finalReply)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				stream.updateProcess("已取消")
			} else {
				stream.updateProcess("执行失败：" + err.Error())
			}
		}
		stream.close()
	}
	if stream.hasStarted() {
		reply = ""
	}
	return reply, stream.hasStarted(), err
}

const (
	promptChunkFlushRunes = 300

	promptChunkTargetText    = "text"
	promptChunkTargetProcess = "process"
)

type promptChunk struct {
	Target string
	Key    string
	Text   string
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
}

type promptChunkAccumulator struct {
	mu              sync.Mutex
	stream          *promptCardStream
	current         *promptChunkStream
	reply           strings.Builder
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

func (a *promptChunkAccumulator) takeFlushLocked(finish bool) promptChunkFlush {
	if a.current == nil {
		return promptChunkFlush{}
	}
	current := a.current
	hasPending := current.pending.Len() > 0
	text := strings.TrimSpace(current.full.String())
	if current.target == promptChunkTargetText {
		text = strings.TrimSpace(a.reply.String())
	}
	if finish {
		a.current = nil
	} else {
		current.pending.Reset()
	}
	if !hasPending {
		return promptChunkFlush{target: current.target, finish: finish}
	}
	return promptChunkFlush{target: current.target, text: text, finish: finish}
}

func (a *promptChunkAccumulator) applyFlush(flush promptChunkFlush) {
	if flush.target == "" {
		return
	}
	switch flush.target {
	case promptChunkTargetText:
		if flush.text != "" {
			a.stream.updateText(flush.text)
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

	mu        sync.Mutex
	card      feishu.StreamCard
	available bool
	creating  bool
	ready     chan struct{}
	started   bool
	text      string
	process   []string
	streaming bool
	tools     []promptToolRow
}

type promptToolRow struct {
	title  string
	line   int
	active bool
}

func newPromptCardStream(ctx context.Context, msg feishu.Message, session Session) *promptCardStream {
	return &promptCardStream{
		ctx:       ctx,
		msg:       msg,
		session:   session,
		available: true,
	}
}

func (s *promptCardStream) hasStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *promptCardStream) updateText(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
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
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	card := s.ensureCard()
	if card == nil {
		return
	}
	s.mu.Lock()
	normalized := normalizeStreamMarkdown(text)
	if s.streaming && len(s.process) > 0 {
		s.process[len(s.process)-1] = normalized
	} else {
		s.process = append(s.process, normalized)
		s.streaming = true
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

	card, ok, err := feishu.StartStreamCard(s.ctx, s.msg)
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
		return truncateRunes(firstNonEmpty(u.Message, contentText(u.Content), rawText(u.Raw)), maxPromptUpdateRunes)
	case "plan", "thought", "reasoning":
		return truncateRunes(firstNonEmpty(u.Message, contentText(u.Content), rawText(u.Raw)), maxPromptUpdateRunes)
	case "status", "progress":
		text := firstNonEmpty(u.Message, u.Status, contentText(u.Content), rawText(u.Raw))
		return truncateRunes(text, maxPromptUpdateRunes)
	default:
		if text := firstNonEmpty(u.Message, contentText(u.Content), rawText(u.Raw)); text != "" {
			return truncateRunes(text, maxPromptUpdateRunes)
		}
		return ""
	}
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
	return fmt.Sprintf("已为当前会话创建 ACP 会话。\n标题：%s\nagent：%s\ncwd：%s\ncwd 来源：%s\nsession：%s",
		displaySessionTitle(session), session.AgentName, session.Cwd, source, session.ACPSessionID)
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
	Cwd   string
	Title string
}

func (s *Service) resolveNewSessionRequest(fields []string, msg feishu.Message) (newSessionRequest, string, string) {
	args := fields[1:]
	req := newSessionRequest{}
	if len(args) > 0 {
		if args[0] == "--title" || args[0] == "-t" {
			req.Title = normalizeSessionTitle(strings.Join(args[1:], " "))
		} else {
			candidate, err := config.ExpandPath(args[0])
			if err == nil {
				if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
					req.Cwd = candidate
					req.Title = normalizeSessionTitle(strings.Join(args[1:], " "))
				} else {
					req.Title = normalizeSessionTitle(strings.Join(args, " "))
				}
			} else {
				req.Title = normalizeSessionTitle(strings.Join(args, " "))
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

func (s *Service) findSession(msg feishu.Message) (Session, bool) {
	store := s.storeForMessage(msg)
	if store == nil {
		return Session{}, false
	}
	for _, key := range sessionKeysFromMessage(msg) {
		if session, ok := store.Get(key); ok {
			session = s.refreshReadySession(msg, store, session)
			return session, true
		}
	}
	return Session{}, false
}

func (s *Service) refreshReadySession(msg feishu.Message, store *SessionStore, session Session) Session {
	staleSetupACP := session.Status == "setup" || setupSessionReadyAfterCreated(session)
	if !staleSetupACP {
		return session
	}
	workspace := strings.TrimSpace(session.Workspace)
	if workspace == "" {
		workspace = strings.TrimSpace(msg.Workspace)
	}
	if workspace == "" {
		return session
	}
	ready, err := workspaceReady(workspace)
	if err != nil {
		slog.Warn("检查 setup session 的 workspace 状态失败", "workspace", workspace, "错误", err)
		return session
	}
	if !ready && staleSetupACP {
		setup, _, setupErr := loadWorkspaceSetup(workspace)
		if setupErr != nil {
			slog.Warn("检查 ACP session 上下文状态失败", "workspace", workspace, "错误", setupErr)
			return session
		}
		ready = setup.Ready
	}
	if !ready {
		return session
	}
	session.Workspace = workspace
	session.Status = "ready"
	if session.ACPSessionID != "" {
		if err := s.runtime.CloseSession(session.Key); err != nil {
			slog.Warn("关闭旧 setup ACP runtime 失败", "session", session.ACPSessionID, "错误", err)
		}
	}
	session.ACPSessionID = ""
	if err := store.Upsert(session); err != nil {
		slog.Error("刷新 session ready 状态失败", "workspace", workspace, "session", session.ACPSessionID, "错误", err)
	}
	return session
}

func setupSessionReadyAfterCreated(session Session) bool {
	if session.Status == "setup" {
		return true
	}
	if session.Status != "ready" || session.ACPSessionID == "" {
		return false
	}
	setup, _, err := loadWorkspaceSetup(session.Workspace)
	if err != nil || !setup.Ready || setup.Updated.IsZero() || session.CreatedAt.IsZero() {
		return false
	}
	return setup.Updated.After(session.CreatedAt)
}

func sessionKeysFromMessage(msg feishu.Message) []SessionKey {
	if msg.IsPrivateChat() {
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
	return "当前话题会话"
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

func wikiInterval(session Session) time.Duration {
	if session.WikiIntervalSec > 0 {
		return time.Duration(session.WikiIntervalSec) * time.Second
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

func (s *Service) wikiStatus(session Session) string {
	enabled := !session.WikiDisabled
	lines := []string{
		"当前会话自动知识沉淀：" + map[bool]string{true: "开启", false: "关闭"}[enabled],
		"延迟：" + formatDuration(wikiInterval(session)),
	}
	s.taskMu.Lock()
	status := s.wikiStatuses[session.Key]
	_, timerSet := s.wikiTimers[session.Key]
	task := s.tasks[session.Key]
	s.taskMu.Unlock()
	if timerSet {
		lines = append(lines, "状态：等待定时触发")
	} else if status.running || (task != nil && task.kind == taskKindWiki) {
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
		go s.cancelRuntimeTask(context.Background(), previous)
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

func (s *Service) cancelRuntimeTask(ctx context.Context, task *runningTask) {
	if task == nil || strings.TrimSpace(task.session.ACPSessionID) == "" {
		return
	}
	if err := s.runtime.CancelSession(ctx, task.session, task.agent); err != nil {
		slog.WarnContext(ctx, "取消 ACP session 失败", "session", task.session.ACPSessionID, "kind", task.kind, "错误", err)
	}
}

func (s *Service) cancelRunningSessionWork(key SessionKey) {
	s.taskMu.Lock()
	task := s.tasks[key]
	delete(s.tasks, key)
	s.taskMu.Unlock()
	if task != nil {
		task.cancel()
		go s.cancelRuntimeTask(context.Background(), task)
	}
}

func (s *Service) cancelSessionWork(key SessionKey) {
	s.cancelWikiTimer(key)
	s.cancelRunningSessionWork(key)
}

func (s *Service) cancelMessageWork(msg feishu.Message) {
	for _, key := range sessionKeysFromMessage(msg) {
		s.cancelSessionWork(key)
	}
}

func (s *Service) cancelRunningMessageWork(msg feishu.Message) {
	for _, key := range sessionKeysFromMessage(msg) {
		s.cancelRunningSessionWork(key)
	}
}

func (s *Service) cancelWikiTimer(key SessionKey) {
	s.taskMu.Lock()
	if s.wikiGenerations == nil {
		s.wikiGenerations = make(map[SessionKey]int64)
	}
	s.wikiGenerations[key]++
	timer := s.wikiTimers[key]
	delete(s.wikiTimers, key)
	s.taskMu.Unlock()
	if timer != nil {
		timer.Stop()
	}
}

func (s *Service) hasWikiTimer(key SessionKey) bool {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	return s.wikiTimers[key] != nil
}

func (s *Service) scheduleWikiAfterUserPrompt(session Session, agent config.AgentConfig) {
	if session.WikiDisabled || session.Status != "ready" || strings.TrimSpace(session.ACPSessionID) == "" {
		s.cancelWikiTimer(session.Key)
		return
	}
	interval := wikiInterval(session)
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
		old.Stop()
	}
	timer := time.AfterFunc(interval, func() {
		s.runWikiTimer(key, generation, session, agent)
	})
	s.wikiTimers[key] = timer
	s.taskMu.Unlock()
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
	status := s.wikiStatuses[key]
	status.running = true
	status.lastStarted = time.Now()
	status.lastEnded = time.Time{}
	status.lastError = ""
	status.lastSuccess = false
	s.wikiStatuses[key] = status
	s.taskMu.Unlock()

	ctx, finish := s.startTask(context.Background(), session, agent, taskKindWiki)
	_, err := s.runtime.Prompt(ctx, session, agent, wikiReflectionPrompt(sessionWorkspace(session, feishu.Message{})), acp.PromptOptions{})
	finish()

	s.taskMu.Lock()
	status = s.wikiStatuses[key]
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
