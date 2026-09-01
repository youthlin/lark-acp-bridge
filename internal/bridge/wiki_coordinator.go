package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

var wikiRetryDelays = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}

type wikiJob struct {
	SourceSessionID string
	FromSeq         uint64
	ToSeq           uint64
	AgentName       string
}

type wikiCoordinator struct {
	service   *Service
	botID     string
	workspace string
	trace     *traceStore
	state     *wikiStateStore

	mu          sync.Mutex
	timers      map[string]*time.Timer
	queue       []string
	queued      map[string]bool
	dirty       map[string]bool
	running     string
	runningJob  wikiJob
	runningAt   time.Time
	cancel      context.CancelFunc
	retryCounts map[string]int
	stopped     bool
	writeMu     sync.Mutex
}

func newWikiCoordinator(service *Service, bot config.BotConfig, trace *traceStore, state *wikiStateStore) *wikiCoordinator {
	workspace := strings.TrimSpace(bot.Workspace)
	if workspace == "" || trace == nil {
		return nil
	}
	if state == nil {
		state = newWikiStateStore(workspace)
		if err := state.Load(); err != nil {
			slog.Warn("加载 wiki 状态失败", "bot", displayBotID(bot.ID), "错误", err)
		}
	}
	return &wikiCoordinator{
		service:     service,
		botID:       strings.TrimSpace(bot.ID),
		workspace:   workspace,
		trace:       trace,
		state:       state,
		timers:      make(map[string]*time.Timer),
		queued:      make(map[string]bool),
		dirty:       make(map[string]bool),
		retryCounts: make(map[string]int),
	}
}

func (c *wikiCoordinator) restore() {
	if c == nil {
		return
	}
	now := time.Now()
	for sourceID, source := range c.state.snapshot().Sources {
		session := Session{Key: normalizeSessionKey(source.SessionKey), ACPSessionID: sourceID}
		if c.service.wikiConfigForSession(session).WikiDisabled {
			continue
		}
		if source.LastCompleteSeq <= source.CommittedSeq || source.CursorLost {
			continue
		}
		delay := time.Until(source.DueAt)
		if source.DueAt.IsZero() || delay < 0 {
			delay = 0
		}
		if source.LastActivityAt.After(now) {
			delay = 0
		}
		c.scheduleTimer(sourceID, delay)
	}
}

func (c *wikiCoordinator) onTurnCompleted(session Session, firstSeq, terminalSeq uint64, completedAt time.Time) {
	if c == nil || terminalSeq == 0 || strings.TrimSpace(session.ACPSessionID) == "" {
		return
	}
	if strings.EqualFold(session.Key.Source, "wiki") {
		return
	}
	chat := c.service.wikiConfigForSession(session)
	sourceID := wikiSourceID(session.ACPSessionID)
	dueAt := completedAt.Add(wikiInterval(chat))
	err := c.state.update(func(state *wikiState) {
		source, exists := state.Sources[sourceID]
		if !exists && firstSeq > 1 {
			// Wiki 2.0 升级后的首次新轮次以该轮起点建立基线，
			// 避免自动回灌没有 seq/cursor 语义的旧历史。
			source.CommittedSeq = firstSeq - 1
		}
		source.SessionKey = normalizeSessionKey(session.Key)
		source.AgentName = strings.TrimSpace(session.AgentName)
		if terminalSeq > source.LastCompleteSeq {
			source.LastCompleteSeq = terminalSeq
		}
		if chat.WikiDisabled && source.CommittedSeq < source.LastCompleteSeq {
			source.CommittedSeq = source.LastCompleteSeq
			source.LastError = ""
		}
		source.LastActivityAt = completedAt
		if chat.WikiDisabled {
			source.DueAt = time.Time{}
		} else {
			source.DueAt = dueAt
		}
		source.CursorLost = false
		state.Sources[sourceID] = source
	})
	if err != nil {
		slog.Warn("保存 wiki source 进度失败", "session", sourceID, "错误", err)
		return
	}
	if chat.WikiDisabled {
		c.cancelSource(sourceID, false)
		return
	}
	c.scheduleTimer(sourceID, time.Until(dueAt))
}

func (c *wikiCoordinator) scheduleTimer(sourceID string, delay time.Duration) {
	if c == nil || sourceID == "" {
		return
	}
	if delay < 0 {
		delay = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	if old := c.timers[sourceID]; old != nil {
		old.Stop()
	}
	c.timers[sourceID] = time.AfterFunc(delay, func() { c.enqueue(sourceID) })
}

func (c *wikiCoordinator) enqueue(sourceID string) {
	c.mu.Lock()
	if timer := c.timers[sourceID]; timer != nil {
		delete(c.timers, sourceID)
	}
	if c.stopped || c.queued[sourceID] {
		c.mu.Unlock()
		return
	}
	if c.running == sourceID {
		c.dirty[sourceID] = true
		c.mu.Unlock()
		return
	}
	c.queued[sourceID] = true
	c.queue = append(c.queue, sourceID)
	start := c.running == ""
	c.mu.Unlock()
	if start {
		c.service.goBackground("wiki-companion", c.drain)
	}
}

func (c *wikiCoordinator) drain() {
	for {
		c.mu.Lock()
		if c.stopped || c.running != "" || len(c.queue) == 0 {
			c.mu.Unlock()
			return
		}
		sourceID := c.queue[0]
		c.queue = c.queue[1:]
		delete(c.queued, sourceID)
		c.running = sourceID
		c.runningAt = time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		c.cancel = cancel
		c.mu.Unlock()

		err := c.runSource(ctx, sourceID)
		c.mu.Lock()
		c.running = ""
		c.runningJob = wikiJob{}
		c.runningAt = time.Time{}
		c.cancel = nil
		if c.dirty[sourceID] {
			delete(c.dirty, sourceID)
			if !c.queued[sourceID] {
				c.queued[sourceID] = true
				c.queue = append(c.queue, sourceID)
			}
		}
		stopped := c.stopped
		c.mu.Unlock()
		c.afterRun(sourceID, err)
		if stopped {
			return
		}
	}
}

func (c *wikiCoordinator) runSource(ctx context.Context, sourceID string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	state := c.state.snapshot()
	source, ok := state.Sources[sourceID]
	if !ok || source.LastCompleteSeq <= source.CommittedSeq || source.CursorLost {
		return nil
	}
	session := Session{
		Key:          normalizeSessionKey(source.SessionKey),
		AgentName:    source.AgentName,
		ACPSessionID: sourceID,
		Workspace:    c.workspace,
	}
	tracePath := c.trace.sessionPath(session)
	rangeInfo, err := readWikiTraceRange(tracePath, source.CommittedSeq, source.LastCompleteSeq)
	if err != nil {
		c.markCursorLost(sourceID, err)
		return err
	}
	if rangeInfo.ToSeq <= source.CommittedSeq {
		return nil
	}
	job := wikiJob{SourceSessionID: sourceID, FromSeq: source.CommittedSeq, ToSeq: rangeInfo.ToSeq, AgentName: source.AgentName}
	c.mu.Lock()
	c.runningJob = job
	c.mu.Unlock()
	result, err := c.runCompanionPrompt(ctx, session, job, tracePath)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			c.service.markWikiFinished(session.Key, session, result, err)
			return err
		}
		_ = c.state.update(func(state *wikiState) {
			s := state.Sources[sourceID]
			s.LastError = err.Error()
			state.Sources[sourceID] = s
		})
		c.service.markWikiFinished(session.Key, session, result, err)
		return err
	}
	now := time.Now()
	err = c.state.update(func(state *wikiState) {
		s := state.Sources[sourceID]
		s.CommittedSeq = job.ToSeq
		s.CommittedTS = time.Time(rangeInfo.Terminal.TS)
		s.LastSuccessAt = now
		s.LastError = ""
		s.LastSummary = wikiResultSummary(result)
		state.Sources[sourceID] = s
	})
	c.service.markWikiFinished(session.Key, session, result, err)
	return err
}

func (c *wikiCoordinator) runCompanionPrompt(ctx context.Context, source Session, job wikiJob, tracePath string) (acp.PromptResult, error) {
	agent, ok := c.service.registry.Get(job.AgentName)
	if !ok {
		return acp.PromptResult{}, fmt.Errorf("wiki companion agent 不存在: %s", job.AgentName)
	}
	companion, runtime, err := c.companionSession(ctx, agent, job.AgentName)
	if err != nil {
		return acp.PromptResult{}, err
	}
	c.service.markWikiStarted(source.Key)
	prompt := wikiCompanionPrompt(c.workspace, tracePath, job)
	observer := c.service.wikiTraceObserverForJob(source, companion, job)
	observer.start(ctx)
	recorder := newTraceRecorderWithMessageID(c.trace, companion, prompt, wikiJobTraceMessageID(job))
	result, err := c.service.runtime.PromptWithRuntimeKey(ctx, runtime, companion, agent, prompt, tracePromptOptions(recorder, wikiTracePromptOptions(observer)))
	if recorder != nil {
		recorder.Complete(result, err)
	}
	if errors.Is(err, errACPSessionUnavailable) {
		companion, runtime, err = c.createCompanionSession(ctx, agent, job.AgentName)
		if err == nil {
			observer.setStreamSession(ctx, companion)
			recorder = newTraceRecorderWithMessageID(c.trace, companion, prompt, wikiJobTraceMessageID(job))
			result, err = c.service.runtime.PromptWithRuntimeKey(ctx, runtime, companion, agent, prompt, tracePromptOptions(recorder, wikiTracePromptOptions(observer)))
			if recorder != nil {
				recorder.Complete(result, err)
			}
		}
	}
	observer.complete(ctx, result, err)
	return result, err
}

func (c *wikiCoordinator) companionSession(ctx context.Context, agent config.AgentConfig, agentName string) (Session, runtimeKey, error) {
	runtime := wikiCompanionRuntimeKey(c.botID, agentName)
	companion := c.state.snapshot().Companions[agentName]
	if strings.TrimSpace(companion.ACPSessionID) == "" {
		return c.createCompanionSession(ctx, agent, agentName)
	}
	return wikiCompanionSession(c.botID, c.workspace, companion), runtime, nil
}

func (c *wikiCoordinator) createCompanionSession(ctx context.Context, agent config.AgentConfig, agentName string) (Session, runtimeKey, error) {
	runtime := wikiCompanionRuntimeKey(c.botID, agentName)
	key := runtime.SessionKey
	candidate, err := c.service.runtime.NewSessionWithRuntimeKey(ctx, runtime, key, agentName, agent, c.workspace, c.workspace)
	if err != nil {
		return Session{}, runtime, fmt.Errorf("创建 wiki companion session: %w", err)
	}
	defer candidate.Abort()
	info := candidate.Info()
	now := time.Now()
	companion := wikiCompanionState{AgentName: agentName, ACPSessionID: info.SessionID, CreatedAt: now, UpdatedAt: now}
	err = candidate.Commit(func() error {
		return c.state.update(func(state *wikiState) { state.Companions[agentName] = companion })
	})
	if err != nil {
		return Session{}, runtime, fmt.Errorf("保存 wiki companion session: %w", err)
	}
	return wikiCompanionSession(c.botID, c.workspace, companion), runtime, nil
}

func wikiCompanionSession(botID, workspace string, companion wikiCompanionState) Session {
	return Session{
		Key:          SessionKey{BotID: botID, Source: "wiki", MainID: "companion:" + companion.AgentName},
		Title:        "wiki companion " + companion.AgentName,
		AgentName:    companion.AgentName,
		ACPSessionID: companion.ACPSessionID,
		Cwd:          workspace,
		Workspace:    workspace,
	}
}

func wikiCompanionRuntimeKey(botID, agentName string) runtimeKey {
	return runtimeKey{
		SessionKey: SessionKey{BotID: botID, Source: "wiki", MainID: "companion:" + strings.TrimSpace(agentName)},
		Scope:      runtimeScopeWikiCompanion,
		RunID:      strings.TrimSpace(agentName),
	}
}

func wikiCompanionPrompt(workspace, tracePath string, job wikiJob) string {
	return strings.Join([]string{
		"# 知识沉淀",
		"",
		"你是当前 bot workspace 的伴生知识维护会话。",
		"先完整阅读 `" + workspace + "/skills/wiki/SKILL.md`，再按其中流程执行。",
		"",
		"本次只处理下面这个已冻结的 trace 区间：",
		"- source session: " + job.SourceSessionID,
		"- trace file: " + tracePath,
		fmt.Sprintf("- seq: (%d, %d]", job.FromSeq, job.ToSeq),
		"",
		"按 JSONL 解析，只使用 type=user、type=assistant 且 is_final=true 的完整正常轮次；用 message_id 聚合同一轮，并以 turn_result 判断正常完成。error 终止的轮次跳过但视为已检查。",
		"忽略 source=wiki 或 message_id 以 wiki_ 开头的记录。不要处理 to_seq 之后的记录。",
		"根据技能规范更新知识文件；没有值得沉淀的信息时不要修改文件。",
		"若修改了文件，最后按以下格式输出审计摘要：",
		"```text",
		"**changed:** yes",
		"",
		"**files:**",
		"- path/to/file.md",
		"",
		"**summary:** <本次沉淀的内容>",
		"**reason:** <为什么值得长期保留>",
		"```",
		"若没有修改，只输出 NoReply。",
	}, "\n")
}

func wikiJobTraceMessageID(job wikiJob) string {
	return traceMessageID("wiki", job.SourceSessionID, fmt.Sprintf("%d_%d", job.FromSeq, job.ToSeq))
}

func (c *wikiCoordinator) afterRun(sourceID string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		c.mu.Lock()
		delete(c.retryCounts, sourceID)
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	retry := c.retryCounts[sourceID]
	if retry >= len(wikiRetryDelays) {
		c.mu.Unlock()
		return
	}
	c.retryCounts[sourceID] = retry + 1
	c.mu.Unlock()
	c.scheduleTimer(sourceID, wikiRetryDelays[retry])
}

func (c *wikiCoordinator) markCursorLost(sourceID string, cause error) {
	_ = c.state.update(func(state *wikiState) {
		source := state.Sources[sourceID]
		source.CursorLost = true
		source.LastError = cause.Error()
		state.Sources[sourceID] = source
	})
	slog.Warn("wiki source trace 不可用", "session", sourceID, "错误", cause)
}

func (c *wikiCoordinator) cancelSource(sourceID string, cancelRunning bool) {
	c.mu.Lock()
	if timer := c.timers[sourceID]; timer != nil {
		timer.Stop()
		delete(c.timers, sourceID)
	}
	if c.queued[sourceID] {
		delete(c.queued, sourceID)
		queue := c.queue[:0]
		for _, current := range c.queue {
			if current != sourceID {
				queue = append(queue, current)
			}
		}
		c.queue = queue
	}
	delete(c.dirty, sourceID)
	if cancelRunning && c.running == sourceID && c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
}

func (c *wikiCoordinator) cancelChat(key SessionKey, cancelRunning bool) {
	if c == nil {
		return
	}
	key = normalizeSessionKey(key)
	for sourceID, source := range c.state.snapshot().Sources {
		if normalizeSessionKey(source.SessionKey) == key {
			c.cancelSource(sourceID, cancelRunning)
		}
	}
	_ = c.state.update(func(state *wikiState) {
		for sourceID, source := range state.Sources {
			if normalizeSessionKey(source.SessionKey) != key {
				continue
			}
			if source.LastCompleteSeq > source.CommittedSeq {
				source.CommittedSeq = source.LastCompleteSeq
				source.LastError = ""
			}
			source.DueAt = time.Time{}
			state.Sources[sourceID] = source
		}
	})
}

func (c *wikiCoordinator) enableSource(session Session) {
	if c == nil {
		return
	}
	sourceID := wikiSourceID(session.ACPSessionID)
	if sourceID == "" {
		return
	}
	_ = c.state.update(func(state *wikiState) {
		source, ok := state.Sources[sourceID]
		if !ok {
			return
		}
		if source.LastCompleteSeq > source.CommittedSeq {
			source.CommittedSeq = source.LastCompleteSeq
			source.LastError = ""
		}
		source.SessionKey = normalizeSessionKey(session.Key)
		if agentName := strings.TrimSpace(session.AgentName); agentName != "" {
			source.AgentName = agentName
		}
		state.Sources[sourceID] = source
	})
	c.cancelSource(sourceID, false)
}

func (c *wikiCoordinator) rescheduleSource(session Session) {
	if c == nil {
		return
	}
	sourceID := wikiSourceID(session.ACPSessionID)
	if sourceID == "" {
		return
	}
	c.cancelSource(sourceID, false)
	source, ok := c.state.snapshot().Sources[sourceID]
	if !ok || source.LastCompleteSeq <= source.CommittedSeq || source.CursorLost {
		return
	}
	dueAt := source.LastActivityAt.Add(wikiInterval(c.service.wikiConfigForSession(session)))
	_ = c.state.update(func(state *wikiState) {
		current := state.Sources[sourceID]
		current.DueAt = dueAt
		state.Sources[sourceID] = current
	})
	c.scheduleTimer(sourceID, time.Until(dueAt))
}

func (c *wikiCoordinator) stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.stopped = true
	for sourceID, timer := range c.timers {
		timer.Stop()
		delete(c.timers, sourceID)
	}
	if c.cancel != nil {
		c.cancel()
	}
	c.queue = nil
	c.queued = make(map[string]bool)
	c.dirty = make(map[string]bool)
	c.mu.Unlock()
}

type wikiCoordinatorSnapshot struct {
	Source  wikiSourceState
	Waiting bool
	Queued  bool
	Running bool
	Job     wikiJob
	Started time.Time
}

func (c *wikiCoordinator) snapshotForSession(sessionID string) wikiCoordinatorSnapshot {
	if c == nil {
		return wikiCoordinatorSnapshot{}
	}
	snapshot := wikiCoordinatorSnapshot{Source: c.state.snapshot().Sources[sessionID]}
	c.mu.Lock()
	snapshot.Waiting = c.timers[sessionID] != nil
	snapshot.Queued = c.queued[sessionID]
	snapshot.Running = c.running == sessionID
	if snapshot.Running {
		snapshot.Job = c.runningJob
		snapshot.Started = c.runningAt
	}
	c.mu.Unlock()
	return snapshot
}

func (c *wikiCoordinator) canPruneTrace(path string) bool {
	if c == nil {
		return true
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	state := c.state.snapshot()
	for sourceID, source := range state.Sources {
		if traceSafeFileName(sourceID) != name {
			continue
		}
		return !source.CursorLost && source.CommittedSeq >= source.LastCompleteSeq
	}
	return true
}
