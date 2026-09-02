package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const (
	defaultWikiInterval = 5 * time.Minute
	minWikiInterval     = time.Second
)

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
	lastSuccess bool
	lastError   string
	lastSummary string
}

type wikiStatusSnapshot struct {
	status         wikiRunStatus
	timerSet       bool
	foregroundTask *runningTask
	backgroundTask bool
}

type wikiTimerRunState int

const (
	wikiTimerRunStale wikiTimerRunState = iota
	wikiTimerRunReady
	wikiTimerRunBusy
)

func (s *Service) handleWikiCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return wikiCommandUsage()
	}
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	chat := s.chatConfigForMessage(msg)
	switch strings.ToLower(strings.TrimSpace(fields[1])) {
	case "on":
		wasDisabled := chat.WikiDisabled
		_, err := store.UpdateChat(chat, func(current *ChatConfig) {
			current.WikiDisabled = false
		})
		if err != nil {
			slog.ErrorContext(ctx, "保存 wiki 配置失败", "错误", err)
			return "保存 wiki 配置失败：" + err.Error()
		}
		if wasDisabled {
			if session, ok := s.findSession(msg); ok {
				if coordinator := s.wikiCoordinator(session.Key.BotID); coordinator != nil {
					coordinator.enableSource(session)
				}
			}
		}
		return "已开启当前聊天的自动知识沉淀。"
	case "off":
		_, err := store.UpdateChat(chat, func(current *ChatConfig) {
			current.WikiDisabled = true
		})
		if err != nil {
			slog.ErrorContext(ctx, "保存 wiki 配置失败", "错误", err)
			return "保存 wiki 配置失败：" + err.Error()
		}
		if session, ok := s.findSession(msg); ok {
			if coordinator := s.wikiCoordinator(session.Key.BotID); coordinator != nil {
				coordinator.cancelChat(session.Key, true)
			} else {
				// 兼容未配置 bot workspace 的旧 1.0 内存状态。
				s.cancelWikiTimer(session.Key)
				s.cancelWikiTasks(ctx, session.Key)
			}
		}
		return "已关闭当前聊天的自动知识沉淀。"
	case "status":
		return s.wikiStatus(msg, chat)
	case "trace":
		return s.handleWikiTraceCommand(ctx, fields, msg)
	case "lint":
		return s.runWikiLint(ctx, msg)
	case "upgrade":
		return s.runWikiUpgrade(ctx, msg)
	case "interval":
		if len(fields) < 3 {
			return "请使用 /wiki interval <duration> 指定时间，例如 /wiki interval 5m。"
		}
		interval, err := parseWikiInterval(fields[2])
		if err != nil {
			return err.Error()
		}
		_, err = store.UpdateChat(chat, func(current *ChatConfig) {
			current.WikiIntervalSec = int(interval.Seconds())
		})
		if err != nil {
			slog.ErrorContext(ctx, "保存 wiki interval 失败", "错误", err)
			return "保存 wiki interval 失败：" + err.Error()
		}
		if session, ok := s.findSession(msg); ok {
			if coordinator := s.wikiCoordinator(session.Key.BotID); coordinator != nil {
				coordinator.rescheduleSource(session)
			} else if s.hasWikiTimer(session.Key) {
				// 兼容未配置 bot workspace 的旧 1.0 内存 timer。
				if agent, ok := s.registry.Get(session.AgentName); ok {
					s.scheduleWikiAfterUserPrompt(session, agent)
				}
			}
		}
		return "已设置当前聊天自动知识沉淀延迟：" + formatDuration(interval) + "。"
	default:
		return "暂不支持这个 wiki 命令。" + wikiCommandUsage()
	}
}

func isWikiUpgradeCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) >= 2 && strings.EqualFold(fields[0], "/wiki") && strings.EqualFold(fields[1], "upgrade")
}

func wikiCommandUsage() string {
	return "可用命令：/wiki on、/wiki off、/wiki status、/wiki lint、/wiki upgrade、/wiki interval <duration> 或 /wiki trace on|off|new。"
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
	if d < minWikiInterval {
		return 0, fmt.Errorf("wiki interval 不能小于 1s")
	}
	if d%time.Second != 0 {
		return 0, fmt.Errorf("wiki interval 必须是整秒，例如 30s、5m 或纯数字分钟")
	}
	return d, nil
}

func wikiInterval(chat ChatConfig) time.Duration {
	if chat.WikiIntervalSec > 0 {
		return time.Duration(chat.WikiIntervalSec) * time.Second
	}
	return defaultWikiInterval
}

func (s *Service) wikiStatus(msg feishu.Message, chat ChatConfig) string {
	enabled := !chat.WikiDisabled
	lines := []string{
		"当前聊天自动知识沉淀：" + map[bool]string{true: "开启", false: "关闭"}[enabled],
		"延迟：" + formatDuration(wikiInterval(chat)),
	}
	session, hasSession := s.findSession(msg)
	var snapshot wikiCoordinatorSnapshot
	if hasSession {
		if coordinator := s.wikiCoordinator(session.Key.BotID); coordinator != nil {
			snapshot = coordinator.snapshotForSession(session.ACPSessionID)
		}
	}
	if hasSession && snapshot.Source == (wikiSourceState{}) && !snapshot.Waiting && !snapshot.Queued && !snapshot.Running {
		legacy := s.wikiStatusSnapshot(session.Key)
		if legacy.timerSet {
			snapshot.Waiting = true
		} else if legacy.status.running || legacy.backgroundTask || (legacy.foregroundTask != nil && legacy.foregroundTask.kind == taskKindWiki) {
			snapshot.Running = true
		} else if !legacy.status.lastStarted.IsZero() {
			snapshot.Source.LastSuccessAt = legacy.status.lastEnded
			snapshot.Source.LastSummary = legacy.status.lastSummary
			snapshot.Source.LastError = legacy.status.lastError
		}
	}
	if snapshot.Waiting {
		lines = append(lines, "状态：等待定时触发")
	} else if snapshot.Queued {
		lines = append(lines, "状态：已排队")
	} else if snapshot.Running {
		lines = append(lines, "状态：正在反思")
		lines = append(lines, fmt.Sprintf("处理区间：(%d, %d]", snapshot.Job.FromSeq, snapshot.Job.ToSeq))
	} else if snapshot.Source.CursorLost {
		lines = append(lines, "状态：游标数据丢失")
	} else if snapshot.Source.LastError != "" {
		lines = append(lines, "最近一次：失败", "错误："+snapshot.Source.LastError)
	} else if !snapshot.Source.LastSuccessAt.IsZero() {
		lines = append(lines, "最近一次：成功", "结束："+snapshot.Source.LastSuccessAt.Format(time.RFC3339))
		if snapshot.Source.LastSummary != "" {
			lines = append(lines, "最近摘要："+snapshot.Source.LastSummary)
		}
	} else {
		lines = append(lines, "状态：尚未触发")
	}
	if bot, ok := s.botConfig(msg.BotID); ok {
		if s.wikiCoordinator(msg.BotID) == nil {
			lines = append(lines, "告警：本地 trace 未启用，自动知识沉淀不会运行")
		}
		lines = append(lines, formatWikiTraceStatus(bot.WikiTrace))
	}
	return strings.Join(lines, "\n")
}

func (s *Service) wikiStatusSnapshot(key SessionKey) wikiStatusSnapshot {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	snapshot := wikiStatusSnapshot{
		status:         s.wikiStatuses[key],
		foregroundTask: s.tasks[key],
	}
	_, snapshot.timerSet = s.wikiTimers[key]
	for runtime := range s.wikiTasks {
		if normalizeSessionKey(runtime.SessionKey) == key {
			snapshot.backgroundTask = true
			break
		}
	}
	return snapshot
}

func wikiReflectionPrompt(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		workspace = "$BOT_WORKSPACE"
	}
	return strings.Join([]string{
		"# 知识沉淀",
		"请对刚才的对话进行反思，根据需要更新你的知识体系。",
		"",
		"## 操作规范",
		"先阅读 `" + workspace + "/skills/wiki/SKILL.md` 了解完整的知识维护规范，然后按其中的流程执行。",
		"如果没有值得沉淀的新信息，不要修改文件。",
		"若修改了文件，请用下面的结构输出简短审计摘要；若没有修改，只输出 NoReply",
		"",
		"```text",
		"**changed:** yes",
		"",
		"**files:**",
		"- path/to/file.md",
		"",
		"**summary:** <本次沉淀的内容>",
		"**reason:** <为什么值得长期保留>",
		"```",
		"",
		"<system_reminder>",
		"本轮知识沉淀是系统自动下发任务，对用户不可见；输出仅供系统记录审计摘要。",
		"如果本轮知识沉淀被新消息打断，直接处理新消息即可，且无需在新消息轮次中执行知识沉淀或解释本规则，系统将在后续自动补发知识沉淀任务。",
		"</system_reminder>",
	}, "\n")
}

func wikiLintPrompt(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		workspace = "$BOT_WORKSPACE"
	}
	return strings.Join([]string{
		"请检查并修复当前 workspace 知识库的一致性。",
		"",
		"## 操作规范",
		"先阅读 `" + workspace + "/skills/wiki/SKILL.md` 和 `" + workspace + "/knowledge/lint.md`。",
		"检查 `knowledge/index.md` 列出的文件是否存在、实际 knowledge/skills 文件是否都被索引、`knowledge/core.md` 引用清单是否同步、`knowledge/log.md` 是否记录新增/删除/重命名、frontmatter 是否齐全，以及是否存在明显重复或冲突。",
		"如发现可直接修复的问题，请修改文件并同步索引和日志；如仅发现需要用户判断的问题，请不要臆断，列为待确认。",
		"",
		"## 输出格式",
		"请用下面结构输出简短结果：",
		"",
		"```text",
		"**changed:** yes/no",
		"",
		"**files:**",
		"- path/to/file.md",
		"",
		"**summary:** <检查和修复摘要>",
		"**reason:** <主要依据或待确认事项>",
		"```",
	}, "\n")
}

func (s *Service) runWikiLint(ctx context.Context, msg feishu.Message) string {
	session, ok := s.findSession(msg)
	if !ok || strings.TrimSpace(session.ACPSessionID) == "" {
		return "当前聊天还没有可用于 wiki lint 的 ACP 会话；先发送普通消息或使用 /new 创建会话。"
	}
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		return "当前会话的 ACP agent 不存在：" + session.AgentName
	}
	session.Workspace = sessionWorkspace(session, msg)
	if s.wikiWorkspaceBusy(session.Key, session.Workspace) {
		return "当前会话正在忙碌，稍后再执行 /wiki lint。"
	}
	taskCtx, finish, err := s.startTaskWithOptions(context.WithoutCancel(ctx), session, agent, taskKindWiki, wikiLintTaskOptions())
	if err != nil {
		if errors.Is(err, errSessionTaskBusy) {
			return "当前会话正在忙碌，稍后再执行 /wiki lint。"
		}
		return "启动 wiki lint 失败：" + err.Error()
	}
	ack := "wiki lint 已开始，完成后会回复检查摘要。"
	replyCtx := context.WithoutCancel(ctx)
	if ok, sendErr := s.sendIntermediateReply(replyCtx, msg, ack); sendErr != nil {
		finish()
		return "启动 wiki lint 失败：发送开始通知失败：" + sendErr.Error()
	} else if ok {
		if !s.goBackground(
			replyCtx,
			"wiki-lint",
			func(ctx context.Context) {
				s.runWikiLintTask(ctx, taskCtx, finish, msg, session, agent)
			},
		) {
			finish()
		}
		return ""
	}
	if !s.goBackground(
		context.WithoutCancel(ctx),
		"wiki-lint",
		func(ctx context.Context) {
			s.runWikiLintTask(ctx, taskCtx, finish, msg, session, agent)
		},
	) {
		finish()
	}
	return ack
}

func (s *Service) runWikiLintTask(replyCtx context.Context, taskCtx context.Context, finish func(), msg feishu.Message, session Session, agent config.AgentConfig) {
	defer finish()
	if coordinator := s.wikiCoordinator(session.Key.BotID); coordinator != nil {
		coordinator.writeMu.Lock()
		defer coordinator.writeMu.Unlock()
	}
	key := normalizeSessionKey(session.Key)
	s.markWikiStarted(key)
	result, sentProgress, rawResult, _, err := s.promptRuntimeWithProgressRaw(taskCtx, msg, session, agent, wikiLintPrompt(sessionWorkspace(session, msg)))
	statusResult := result
	if strings.TrimSpace(rawResult.Text) != "" {
		statusResult = rawResult
	}
	s.markWikiFinished(key, session, statusResult, err)
	if sentProgress {
		return
	}
	reply := ""
	if err != nil {
		reply = "wiki lint 执行失败：" + err.Error()
	} else if summary := wikiResultSummary(result); summary != "" {
		reply = "wiki lint 完成：\n" + summary
	} else {
		reply = "wiki lint 完成：未返回摘要。"
	}
	if ok, sendErr := s.sendIntermediateReply(replyCtx, msg, reply); sendErr != nil {
		slog.WarnContext(replyCtx, "发送 wiki lint 结果失败", "session", session.ACPSessionID, "错误", sendErr)
	} else if !ok {
		slog.WarnContext(replyCtx, "缺少 wiki lint 结果回复发送器", "session", session.ACPSessionID)
	}
}

func (s *Service) runWikiUpgrade(ctx context.Context, msg feishu.Message) string {
	workspace := s.workspaceForWikiUpgrade(msg)
	if strings.TrimSpace(workspace) == "" {
		return "当前 bot workspace 未初始化，无法执行 wiki upgrade。"
	}
	key := sessionKeyFromMessage(msg)
	finish, ok := s.beginWikiUpgradeTask(key, workspace)
	if !ok {
		return "当前会话正在忙碌，稍后再执行 /wiki upgrade。"
	}
	defer finish()
	if coordinator := s.wikiCoordinator(msg.BotID); coordinator != nil {
		coordinator.writeMu.Lock()
		defer coordinator.writeMu.Unlock()
	}
	workspaceStatus, err := ensureWorkspaceWithOptions(workspace, msg.BotID, ensureWorkspaceOptions{})
	if err != nil {
		slog.ErrorContext(ctx, "初始化 workspace 失败", "workspace", workspace, "错误", err)
		return "初始化 workspace 失败：" + err.Error()
	}
	status, err := upgradeWorkspaceWikiPolicy(workspace)
	if err != nil {
		slog.ErrorContext(ctx, "升级 workspace wiki 规则失败", "workspace", workspace, "错误", err)
		return "wiki upgrade 失败：" + err.Error()
	}
	for _, name := range workspaceStatus.UpgradedFiles {
		status.UpdatedFiles = appendUniqueString(status.UpdatedFiles, name)
	}
	if err := appendWorkspaceUpgradeLog(workspace, status); err != nil {
		slog.ErrorContext(ctx, "记录 workspace wiki upgrade 日志失败", "workspace", workspace, "错误", err)
		return "wiki upgrade 写入日志失败：" + err.Error()
	}
	if len(status.UpdatedFiles) == 0 {
		return "wiki upgrade 完成：当前 workspace 已包含最新 wiki 维护规则。"
	}
	return "wiki upgrade 完成：已更新\n- " + strings.Join(status.UpdatedFiles, "\n- ")
}

func (s *Service) beginWikiUpgradeTask(key SessionKey, workspace string) (func(), bool) {
	key = normalizeSessionKey(key)
	task := &runningTask{
		kind:    taskKindWiki,
		runtime: currentRuntimeKey(key),
		done:    make(chan struct{}),
		session: Session{
			Key:       key,
			Workspace: workspace,
		},
	}
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.hasWorkspaceTaskLocked(key, workspace) {
		task.closeDone()
		return func() {}, false
	}
	if s.tasks == nil {
		s.tasks = make(map[SessionKey]*runningTask)
	}
	s.tasks[key] = task
	s.workspaceLocks.set(workspace, task)
	return func() {
		s.taskMu.Lock()
		if s.tasks[key] == task {
			delete(s.tasks, key)
		}
		s.workspaceLocks.clear(workspace, task)
		s.taskMu.Unlock()
		task.closeDone()
	}, true
}

func (s *Service) wikiWorkspaceBusy(key SessionKey, workspace string) bool {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	return s.hasWorkspaceTaskLocked(key, workspace)
}

func (s *Service) workspaceForWikiUpgrade(msg feishu.Message) string {
	if session, ok := s.findSession(msg); ok {
		if workspace := sessionWorkspace(session, msg); strings.TrimSpace(workspace) != "" {
			return workspace
		}
	}
	if workspace := strings.TrimSpace(msg.Workspace); workspace != "" {
		return workspace
	}
	return s.botWorkspace(msg.BotID)
}

func (s *Service) startWikiTask(ctx context.Context, session Session, agent config.AgentConfig, runtime runtimeKey) (context.Context, func(), bool) {
	session.Key = normalizeSessionKey(session.Key)
	session.Workspace = s.workspaceForSessionTask(session)
	runtime = normalizeRuntimeKey(runtime)
	ctx, cancel := context.WithCancel(ctx)
	task := &runningTask{
		kind:    taskKindWiki,
		runtime: runtime,
		cancel:  cancel,
		done:    make(chan struct{}),
		session: session,
		agent:   agent,
	}

	if !s.beginWikiTask(runtime, task) {
		cancel()
		task.closeDone()
		return ctx, func() {}, false
	}

	return ctx, func() {
		s.finishWikiTask(runtime, task)
		cancel()
		task.closeDone()
	}, true
}

func (s *Service) beginWikiTask(runtime runtimeKey, task *runningTask) bool {
	runtime = normalizeRuntimeKey(runtime)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.hasWorkspaceTaskLocked(task.session.Key, task.session.Workspace) {
		return false
	}
	if s.wikiTasks == nil {
		s.wikiTasks = make(map[runtimeKey]*runningTask)
	}
	s.wikiTasks[runtime] = task
	s.workspaceLocks.set(task.session.Workspace, task)
	return true
}

func (s *Service) finishWikiTask(runtime runtimeKey, task *runningTask) bool {
	runtime = normalizeRuntimeKey(runtime)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.wikiTasks[runtime] != task {
		if !s.taskRegisteredLocked(task) {
			s.workspaceLocks.clear(task.session.Workspace, task)
		}
		return false
	}
	delete(s.wikiTasks, runtime)
	s.workspaceLocks.clear(task.session.Workspace, task)
	return true
}

func (s *Service) cancelWikiTasks(ctx context.Context, key SessionKey) {
	tasks := s.takeWikiTasks(key)
	for _, task := range tasks {
		task.cancel()
		if task.onCancel != nil {
			task.onCancel(ctx, "已取消")
		}
		go s.cancelRuntimeTask(ctx, task)
	}
}

func (s *Service) takeWikiTasks(key SessionKey) []*runningTask {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	tasks := make([]*runningTask, 0)
	for runtime, task := range s.wikiTasks {
		if runtime.SessionKey == key {
			tasks = append(tasks, task)
			delete(s.wikiTasks, runtime)
			s.workspaceLocks.clear(task.session.Workspace, task)
		}
	}
	return tasks
}

func (s *Service) cancelWikiTimer(key SessionKey) {
	pending, _ := s.takeWikiTimer(key)
	if pending != nil && pending.timer != nil {
		pending.timer.Stop()
	}
}

func (s *Service) takeWikiTimer(key SessionKey) (*pendingWikiRun, bool) {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	if s.wikiGenerations == nil {
		s.wikiGenerations = make(map[SessionKey]int64)
	}
	s.wikiGenerations[key]++
	pending := s.wikiTimers[key]
	delete(s.wikiTimers, key)
	s.taskMu.Unlock()
	return pending, pending != nil
}

func (s *Service) hasWikiTimer(key SessionKey) bool {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	return s.wikiTimers[key] != nil
}

func (s *Service) takePendingWiki(key SessionKey) (pendingWikiRun, bool) {
	pending, ok := s.takeWikiTimer(key)
	if !ok {
		return pendingWikiRun{}, false
	}
	if pending.timer != nil {
		pending.timer.Stop()
	}
	return *pending, true
}

func (s *Service) restorePendingWiki(pending pendingWikiRun) {
	pending.session.Key = normalizeSessionKey(pending.session.Key)
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
	s.scheduleWikiTimer(key, delay, pending)
}

func (s *Service) scheduleWikiAfterUserPrompt(session Session, agent config.AgentConfig) {
	session.Key = normalizeSessionKey(session.Key)
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
	s.scheduleWikiTimer(key, interval, pendingWikiRun{
		session:   session,
		agent:     agent,
		scheduled: time.Now(),
	})
}

func (s *Service) scheduleWikiTimer(key SessionKey, delay time.Duration, pending pendingWikiRun) {
	key = normalizeSessionKey(key)
	pending.session.Key = normalizeSessionKey(pending.session.Key)
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
		if !s.goBackground(
			context.Background(),
			"wiki-timer",
			func(ctx context.Context) {
				s.runWikiTimer(
					ctx,
					key,
					generation,
					pending.session,
					pending.agent,
				)
			},
		) {
			s.cancelWikiTimer(key)
		}
	})
	s.wikiTimers[key] = &pending
	s.taskMu.Unlock()
}

func (s *Service) wikiConfigForSession(session Session) ChatConfig {
	key := chatKeyFromSessionKey(session.Key)
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

func (s *Service) runWikiTimer(parent context.Context, key SessionKey, generation int64, session Session, agent config.AgentConfig) {
	key = normalizeSessionKey(key)
	session.Key = normalizeSessionKey(session.Key)
	switch s.beginWikiTimerRun(key, generation, session.Workspace) {
	case wikiTimerRunStale:
		return
	case wikiTimerRunBusy:
		s.scheduleWikiAfterUserPrompt(session, agent)
		return
	}

	// Timer-driven reflection is independent of any Feishu request; task
	// cancellation is controlled by the task manager and session lifecycle.
	ctx, finish, err := s.startTaskWithOptions(parent, session, agent, taskKindWiki, wikiReflectionTaskOptions())
	if err != nil {
		if errors.Is(err, errSessionTaskBusy) {
			s.scheduleWikiAfterUserPrompt(session, agent)
		} else {
			slog.Warn("启动 wiki 自动知识沉淀失败", "session", session.ACPSessionID, "错误", err)
		}
		return
	}
	defer finish()
	s.markWikiStarted(key)
	prompt := wikiReflectionPrompt(sessionWorkspace(session, feishu.Message{}))
	trace := s.wikiTraceObserver(session, generation)
	trace.start(ctx)
	recorder := s.newTraceRecorderWithMessageID(session, prompt, wikiTraceMessageID(session, generation))
	result, err := s.runtime.Prompt(ctx, session, agent, prompt, tracePromptOptions(recorder, wikiTracePromptOptions(trace)))
	if recorder != nil {
		recorder.Complete(result, err)
	}
	trace.complete(ctx, result, err)
	s.markWikiFinished(key, session, result, err)
}

func (s *Service) beginWikiTimerRun(key SessionKey, generation int64, workspace string) wikiTimerRunState {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.wikiGenerations[key] != generation {
		return wikiTimerRunStale
	}
	delete(s.wikiTimers, key)
	if s.hasWorkspaceTaskLocked(key, workspace) {
		s.wikiGenerations[key]++
		return wikiTimerRunBusy
	}
	return wikiTimerRunReady
}

func (s *Service) runPendingWikiAsync(pending pendingWikiRun) {
	if strings.TrimSpace(pending.session.ACPSessionID) == "" {
		return
	}
	s.goBackground(
		context.Background(),
		"pending-wiki",
		func(ctx context.Context) {
			s.runPendingWikiWithRuntimeKey(ctx, pending)
		},
	)
}

func (s *Service) runPendingWikiWithRuntimeKey(parent context.Context, pending pendingWikiRun) {
	pending.session.Key = normalizeSessionKey(pending.session.Key)
	key := pending.session.Key
	runtime := wikiRuntimeKey(key, pending.generation, pending.session.ACPSessionID)
	// Pending wiki runs resume after foreground work finishes, outside the
	// original request lifecycle.
	ctx, finish, ok := s.startWikiTask(parent, pending.session, pending.agent, runtime)
	if !ok {
		s.scheduleWikiAfterUserPrompt(pending.session, pending.agent)
		return
	}
	defer func() {
		finish()
		if err := s.runtime.CloseRuntimeKey(runtime); err != nil {
			slog.Warn("关闭 wiki ACP runtime 失败", "session", pending.session.ACPSessionID, "错误", err)
		}
	}()
	s.markWikiStarted(key)
	prompt := wikiReflectionPrompt(sessionWorkspace(pending.session, feishu.Message{}))
	trace := s.wikiTraceObserver(pending.session, pending.generation)
	trace.start(ctx)
	recorder := s.newTraceRecorderWithMessageID(pending.session, prompt, wikiTraceMessageID(pending.session, pending.generation))
	result, err := s.runtime.PromptWithRuntimeKey(ctx, runtime, pending.session, pending.agent, prompt, tracePromptOptions(recorder, wikiTracePromptOptions(trace)))
	if recorder != nil {
		recorder.Complete(result, err)
	}
	trace.complete(ctx, result, err)
	s.markWikiFinished(key, pending.session, result, err)
}

func wikiTraceMessageID(session Session, generation int64) string {
	return traceMessageID("wiki", session.ACPSessionID, fmt.Sprintf("generation_%d", generation))
}

func (s *Service) markWikiStarted(key SessionKey) {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	status := s.wikiStatuses[key]
	status.running = true
	status.lastStarted = time.Now()
	status.lastEnded = time.Time{}
	status.lastError = ""
	status.lastSummary = ""
	status.lastSuccess = false
	s.wikiStatuses[key] = status
	s.taskMu.Unlock()
}

func (s *Service) markWikiFinished(key SessionKey, session Session, result acp.PromptResult, err error) {
	key = normalizeSessionKey(key)
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
		status.lastSummary = wikiResultSummary(result)
	}
	s.wikiStatuses[key] = status
	s.taskMu.Unlock()
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("wiki 自动知识沉淀失败", "session", session.ACPSessionID, "错误", err)
	}
}

func wikiResultSummary(result acp.PromptResult) string {
	text := strings.TrimSpace(result.Text)
	if text == "" || strings.EqualFold(text, "NoReply") {
		return ""
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	return truncateRunes(strings.Join(fields, " "), 240)
}
