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
	switch strings.ToLower(strings.TrimSpace(fields[1])) {
	case "on":
		_, err := store.UpdateChat(chat, func(current *ChatConfig) {
			current.WikiDisabled = false
		})
		if err != nil {
			slog.ErrorContext(ctx, "保存 wiki 配置失败", "错误", err)
			return "保存 wiki 配置失败：" + err.Error()
		}
		return "已开启当前聊天的自动知识沉淀。"
	case "off":
		if session, ok := s.findSession(msg); ok {
			s.cancelWikiTimer(session.Key)
			s.cancelWikiTasks(ctx, session.Key)
		}
		_, err := store.UpdateChat(chat, func(current *ChatConfig) {
			current.WikiDisabled = true
		})
		if err != nil {
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
		_, err = store.UpdateChat(chat, func(current *ChatConfig) {
			current.WikiIntervalSec = int(interval.Seconds())
		})
		if err != nil {
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
