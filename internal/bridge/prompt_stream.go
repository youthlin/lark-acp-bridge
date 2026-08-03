package bridge

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

type promptCardStream struct {
	ctx     context.Context
	msg     feishu.Message
	session Session
	starter streamCardStarter

	mu                sync.Mutex
	card              feishu.StreamCard
	available         bool
	delayed           bool
	creating          bool
	ready             chan struct{}
	started           bool
	showStepMessages  bool
	showPlans         bool
	showThoughts      bool
	showTools         bool
	showStatusBar     bool
	showUsageDetail   bool
	text              string
	process           []string
	processUpdates    promptProcessUpdateThrottler
	streaming         bool
	activeStreamClass promptProcessClass
	tools             []promptToolRow
	status            promptStatusBar
}

type promptToolRow struct {
	id     string
	title  string
	line   int
	active bool
}

func newPromptCardStream(ctx context.Context, msg feishu.Message, session Session, show ChatConfig, starter streamCardStarter) *promptCardStream {
	return newPromptCardStreamWithStatusPrefix(ctx, msg, session, show, "", starter)
}

func newPromptCardStreamWithStatusPrefix(ctx context.Context, msg feishu.Message, session Session, show ChatConfig, statusPrefix string, starter streamCardStarter) *promptCardStream {
	return &promptCardStream{
		ctx:              ctx,
		msg:              msg,
		session:          session,
		starter:          starter,
		available:        true,
		showStepMessages: !show.HideStepMessages,
		showPlans:        !show.HidePlans,
		showThoughts:     chatThoughtsVisible(show),
		showTools:        !show.HideTools,
		showStatusBar:    !show.HideStatusBar,
		showUsageDetail:  !show.HideUsageDetail,
		processUpdates:   promptProcessUpdateThrottler{interval: promptProcessFlushInterval},
		status:           promptStatusBar{state: promptStatusRunning, prefix: strings.TrimSpace(statusPrefix), startedAt: time.Now()},
	}
}

func (s *promptCardStream) delayCardCreation() {
	s.mu.Lock()
	s.delayed = true
	s.mu.Unlock()
}

func (s *promptCardStream) flushDelayedWithContext(ctx context.Context, result acp.PromptResult, stopReason string) {
	s.mu.Lock()
	s.delayed = false
	text := s.text
	processText := processPanelText(s.process)
	showStatus := s.showStatusBar
	status := s.status
	showUsage := s.showUsageDetail
	s.mu.Unlock()
	if strings.TrimSpace(text) != "" {
		s.setFinalTextWithContext(ctx, text)
	}
	if strings.TrimSpace(processText) != "" {
		card := s.ensureCardWithContext(ctx)
		if card != nil {
			s.queueProcessUpdateWithContext(ctx, card, processText, true)
		}
	}
	if showStatus {
		s.mu.Lock()
		s.status = status
		s.status.applyPromptResult(result)
		s.status.finish(promptStatusFromStopReason(stopReason), stopReason, time.Now())
		statusText := s.status.text()
		s.mu.Unlock()
		if card := s.ensureCardWithContext(ctx); card != nil {
			if err := card.UpdateStatus(ctx, statusText); err != nil {
				slog.ErrorContext(ctx, "更新 ACP 流式卡片状态栏失败", "session", s.session.ACPSessionID, "错误", err)
			}
		}
	}
	if showUsage {
		s.updatePromptResult(result)
	}
	s.closeWithContext(ctx)
}

func (s *promptCardStream) hasStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *promptCardStream) updateText(text string) {
	s.updateTextWithContext(s.ctx, text)
}

func (s *promptCardStream) updateTextWithContext(ctx context.Context, text string) {
	text = strings.TrimSpace(text)
	s.mu.Lock()
	s.text = normalizeStreamMarkdown(text)
	fullText := s.text
	delayed := s.delayed
	s.mu.Unlock()
	if delayed {
		return
	}
	card := s.ensureCardWithContext(ctx)
	if card == nil {
		return
	}
	if err := card.UpdateText(ctx, fullText); err != nil {
		slog.ErrorContext(ctx, "更新 ACP 流式卡片文本失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) setFinalTextWithContext(ctx context.Context, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.mu.Lock()
	s.text = normalizeStreamMarkdown(text)
	fullText := s.text
	delayed := s.delayed
	s.mu.Unlock()
	if delayed {
		return
	}
	card := s.ensureCardWithContext(ctx)
	if card == nil {
		return
	}
	if err := card.SetFinalText(ctx, fullText, feishu.OutboundRenderContext{BaseDir: s.session.Cwd}); err != nil {
		slog.ErrorContext(ctx, "更新 ACP 流式卡片最终文本失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) updateMetaWithContext(ctx context.Context, meta feishu.StreamCardMeta) {
	s.mu.Lock()
	delayed := s.delayed
	s.mu.Unlock()
	if delayed {
		return
	}
	card := s.ensureCardWithContext(ctx)
	if card == nil {
		return
	}
	if err := card.UpdateMeta(ctx, meta); err != nil {
		slog.ErrorContext(ctx, "更新 ACP 流式卡片元信息失败", "session", s.session.ACPSessionID, "错误", err)
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
	s.mu.Lock()
	s.status.Context = acp.ContextWindowUsage{Used: u.Used, Size: u.Size}
	statusText := s.status.text()
	delayed := s.delayed
	s.mu.Unlock()
	if delayed {
		return
	}
	card := s.ensureCardWithContext(s.ctx)
	if card == nil {
		return
	}
	if err := card.UpdateStatus(s.ctx, statusText); err != nil {
		slog.ErrorContext(s.ctx, "更新 ACP 流式卡片状态栏失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) updatePromptStatusFromResultWithContext(ctx context.Context, result acp.PromptResult) {
	if !s.showStatusBar {
		return
	}
	if result.Usage.InputTokens == 0 && result.Usage.OutputTokens == 0 && result.Meta.TraeTokenUsage == nil {
		return
	}
	s.mu.Lock()
	s.status.applyPromptResult(result)
	statusText := s.status.text()
	delayed := s.delayed
	s.mu.Unlock()
	if delayed {
		return
	}
	card := s.ensureCardWithContext(ctx)
	if card == nil {
		return
	}
	if err := card.UpdateStatus(ctx, statusText); err != nil {
		slog.ErrorContext(ctx, "更新 ACP 流式卡片状态栏失败", "session", s.session.ACPSessionID, "错误", err)
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
	s.mu.Lock()
	delayed := s.delayed
	s.mu.Unlock()
	if delayed {
		return
	}
	card := s.ensureCardWithContext(s.ctx)
	if card == nil {
		return
	}
	if err := card.UpdateUsageDetail(s.ctx, detail); err != nil {
		slog.ErrorContext(s.ctx, "更新 ACP 流式卡片用量明细失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) finishPromptStatus(stopReason string) {
	s.finishPromptStatusWithContext(s.ctx, stopReason)
}

func (s *promptCardStream) finishPromptStatusWithContext(ctx context.Context, stopReason string) {
	if !s.showStatusBar {
		return
	}
	s.mu.Lock()
	s.status.finish(promptStatusFromStopReason(stopReason), stopReason, time.Now())
	statusText := s.status.text()
	delayed := s.delayed
	s.mu.Unlock()
	if delayed {
		return
	}
	card := s.ensureCardWithContext(ctx)
	if card == nil {
		return
	}
	if err := card.UpdateStatus(ctx, statusText); err != nil {
		slog.ErrorContext(ctx, "更新 ACP 流式卡片状态栏失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) failPromptStatus() {
	s.failPromptStatusWithContext(s.ctx)
}

func (s *promptCardStream) failPromptStatusWithContext(ctx context.Context) {
	if !s.showStatusBar {
		return
	}
	card := s.ensureCardWithContext(ctx)
	if card == nil {
		return
	}
	s.mu.Lock()
	s.status.finish(promptStatusFailed, "", time.Now())
	statusText := s.status.text()
	s.mu.Unlock()
	if err := card.UpdateStatus(ctx, statusText); err != nil {
		slog.ErrorContext(ctx, "更新 ACP 流式卡片状态栏失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) updateProcessMessage(text string) {
	s.updateProcessMessageWithContext(s.ctx, text)
}

func (s *promptCardStream) updateProcessMessageWithContext(ctx context.Context, text string) {
	if !s.showStepMessages {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.updateProcessWithContext(ctx, formatProcessMessageText(text))
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

func (s *promptCardStream) updatePlanStream(text string) {
	if !s.showPlans {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.updateProcessStreamText(promptProcessPlan, "📌 "+text, false)
}

func (s *promptCardStream) updateToolStream(text string) {
	if !s.showTools {
		return
	}
	s.updateProcessStreamText(promptProcessTool, text, false)
}

func (s *promptCardStream) updateProcess(text string) {
	s.updateProcessWithContext(s.ctx, text)
}

func (s *promptCardStream) updateProcessWithContext(ctx context.Context, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.mu.Lock()
	s.process = append(s.process, normalizeStreamMarkdown(text))
	processText := processPanelText(s.process)
	delayed := s.delayed
	s.mu.Unlock()
	if delayed {
		return
	}
	card := s.ensureCardWithContext(ctx)
	if card == nil {
		return
	}
	s.queueProcessUpdateWithContext(ctx, card, processText, false)
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
	} else if isPlanUpdateKind(kind) {
		if !s.showPlans {
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
	id := strings.TrimSpace(u.ToolCallID)
	rawTitle := strings.TrimSpace(toolDisplayName(u))
	s.mu.Lock()
	processText := s.applyToolProgressLineLocked(status, id, rawTitle)
	delayed := s.delayed
	s.mu.Unlock()
	if delayed {
		return true
	}
	card := s.ensureCardWithContext(s.ctx)
	if card == nil {
		return true
	}
	s.queueProcessUpdate(card, processText, true)
	return true
}

func (s *promptCardStream) applyToolProgressLineLocked(status toolProgressStatus, id, rawTitle string) string {
	title := s.resolveToolTitleLocked(id, rawTitle)
	line := normalizeStreamMarkdown(toolStatusIcon(status) + " " + title)
	if status == toolProgressRunning {
		// 仅当带 toolCallId 且命中同一调用的已有行时才复用(更新 in_progress 进度);
		// 没有 id 时无法判定是否同一次调用,按原逻辑新增一行,避免覆盖上一个仍在运行的工具。
		if id != "" {
			if idx := s.findToolRowLocked(id, title); idx >= 0 && s.tools[idx].active {
				row := &s.tools[idx]
				if title != "" {
					row.title = title
				}
				if row.line >= 0 && row.line < len(s.process) {
					s.process[row.line] = line
				} else {
					s.process = append(s.process, line)
					row.line = len(s.process) - 1
				}
				return processPanelText(s.process)
			}
		}
		s.process = append(s.process, line)
		s.tools = append(s.tools, promptToolRow{
			id:     id,
			title:  title,
			line:   len(s.process) - 1,
			active: true,
		})
		return processPanelText(s.process)
	}
	if idx := s.findToolRowLocked(id, title); idx >= 0 {
		row := &s.tools[idx]
		if title != "" {
			row.title = title
		}
		if row.line >= 0 && row.line < len(s.process) {
			s.process[row.line] = line
		} else {
			s.process = append(s.process, line)
			row.line = len(s.process) - 1
		}
		row.active = false
		return processPanelText(s.process)
	}
	s.process = append(s.process, line)
	return processPanelText(s.process)
}

// resolveToolTitleLocked 确定一次工具更新展示的标题:
// 优先用本条 update 自带的标题;否则按 toolCallId 复用同一次调用起始事件记录的标题;
// 再退回到最近一个活跃工具;都没有时用占位文案。
func (s *promptCardStream) resolveToolTitleLocked(id, rawTitle string) string {
	if rawTitle != "" {
		return truncateRunes(rawTitle, 80)
	}
	if id != "" {
		if idx := s.findToolRowLocked(id, ""); idx >= 0 {
			if t := strings.TrimSpace(s.tools[idx].title); t != "" {
				return t
			}
		}
	}
	if t := strings.TrimSpace(s.latestActiveToolTitleLocked()); t != "" {
		return t
	}
	return "工具调用"
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

func (s *promptCardStream) findToolRowLocked(id, title string) int {
	id = strings.TrimSpace(id)
	title = strings.TrimSpace(title)
	if id != "" {
		for i := len(s.tools) - 1; i >= 0; i-- {
			if s.tools[i].active && s.tools[i].id == id {
				return i
			}
		}
		for i := len(s.tools) - 1; i >= 0; i-- {
			if s.tools[i].id == id {
				return i
			}
		}
	}
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
	promptProcessPlan    promptProcessClass = "plan"
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
	s.mu.Lock()
	normalized := normalizeStreamMarkdown(text)
	if s.streaming && s.activeStreamClass == class && len(s.process) > 0 {
		s.process[len(s.process)-1] = normalized
	} else {
		s.process = append(s.process, normalized)
		s.streaming = true
		s.activeStreamClass = class
	}
	processText := processPanelText(s.process)
	delayed := s.delayed
	s.mu.Unlock()
	if delayed {
		return
	}
	card := s.ensureCardWithContext(s.ctx)
	if card == nil {
		return
	}
	s.queueProcessUpdate(card, processText, false)
}

func processPanelText(entries []string) string {
	return truncateProcessText(strings.Join(entries, "\n"))
}

func (s *promptCardStream) finishProcessStream() {
	s.mu.Lock()
	s.streaming = false
	s.activeStreamClass = promptProcessNone
	s.mu.Unlock()
}

const (
	promptCardFlushDelay       = 180 * time.Millisecond
	promptProcessFlushInterval = time.Second
	maxPromptProcessRunes      = 6000
)

type promptProcessUpdateThrottler struct {
	interval  time.Duration
	pending   string
	dirty     bool
	lastFlush time.Time
	timer     *time.Timer
	timerGen  int64
	flushing  sync.WaitGroup
}

func (t *promptProcessUpdateThrottler) queueLocked(now time.Time, text string, force bool, onTimer func(int64)) (string, bool) {
	t.pending = text
	t.dirty = true
	interval := t.interval
	if interval <= 0 {
		interval = promptProcessFlushInterval
	}
	if force || t.lastFlush.IsZero() || !now.Before(t.lastFlush.Add(interval)) {
		flushText, flushNow := t.takeLocked(now)
		t.stopTimerLocked()
		return flushText, flushNow
	}
	t.scheduleLocked(t.lastFlush.Add(interval).Sub(now), onTimer)
	return "", false
}

func (t *promptProcessUpdateThrottler) scheduleLocked(delay time.Duration, onTimer func(int64)) {
	if t.timer != nil {
		return
	}
	if delay < 0 {
		delay = 0
	}
	t.timerGen++
	generation := t.timerGen
	t.flushing.Add(1)
	t.timer = time.AfterFunc(delay, func() {
		defer t.flushing.Done()
		if onTimer != nil {
			onTimer(generation)
		}
	})
}

func (t *promptProcessUpdateThrottler) stopTimerLocked() {
	t.timerGen++
	if t.timer == nil {
		return
	}
	if t.timer.Stop() {
		t.flushing.Done()
	}
	t.timer = nil
}

func (t *promptProcessUpdateThrottler) takeLocked(now time.Time) (string, bool) {
	if !t.dirty {
		return "", false
	}
	text := t.pending
	t.pending = ""
	t.dirty = false
	t.lastFlush = now
	return text, true
}

func (t *promptProcessUpdateThrottler) wait() {
	t.flushing.Wait()
}

func (s *promptCardStream) close() {
	s.closeWithContext(s.ctx)
}

func (s *promptCardStream) closeWithContext(ctx context.Context) {
	s.flushPendingProcessUpdateWithContext(ctx)
	s.mu.Lock()
	card := s.card
	s.mu.Unlock()
	if card == nil {
		return
	}
	if err := card.Close(ctx); err != nil {
		slog.ErrorContext(ctx, "关闭 ACP 流式卡片失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) queueProcessUpdate(card feishu.StreamCard, text string, force bool) {
	s.queueProcessUpdateWithContext(s.ctx, card, text, force)
}

func (s *promptCardStream) queueProcessUpdateWithContext(ctx context.Context, card feishu.StreamCard, text string, force bool) {
	if card == nil {
		return
	}
	now := time.Now()
	var flushText string
	flushNow := false
	s.mu.Lock()
	flushText, flushNow = s.processUpdates.queueLocked(now, text, force, func(generation int64) {
		s.flushProcessUpdateTimer(generation)
	})
	s.mu.Unlock()
	if flushNow {
		s.applyProcessUpdateWithContext(ctx, card, flushText)
	}
}

func (s *promptCardStream) flushPendingProcessUpdate() {
	s.flushPendingProcessUpdateWithContext(s.ctx)
}

func (s *promptCardStream) flushPendingProcessUpdateWithContext(ctx context.Context) {
	var (
		card      feishu.StreamCard
		flushText string
		flushNow  bool
	)
	s.mu.Lock()
	card = s.card
	s.processUpdates.stopTimerLocked()
	flushText, flushNow = s.processUpdates.takeLocked(time.Now())
	s.mu.Unlock()
	if flushNow && card != nil {
		s.applyProcessUpdateWithContext(ctx, card, flushText)
	}
	s.processUpdates.wait()
}

func (s *promptCardStream) flushProcessUpdateTimer(generation int64) {
	var (
		card      feishu.StreamCard
		flushText string
		flushNow  bool
	)
	s.mu.Lock()
	if s.processUpdates.timerGen != generation {
		s.mu.Unlock()
		return
	}
	s.processUpdates.timer = nil
	card = s.card
	flushText, flushNow = s.processUpdates.takeLocked(time.Now())
	s.mu.Unlock()
	if flushNow && card != nil {
		s.applyProcessUpdate(card, flushText)
	}
}

func (s *promptCardStream) applyProcessUpdate(card feishu.StreamCard, text string) {
	s.applyProcessUpdateWithContext(s.ctx, card, text)
}

func (s *promptCardStream) applyProcessUpdateWithContext(ctx context.Context, card feishu.StreamCard, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if err := card.UpdateProcess(ctx, text); err != nil {
		slog.ErrorContext(ctx, "更新 ACP 流式卡片过程失败", "session", s.session.ACPSessionID, "错误", err)
	}
	s.refreshPromptStatusWithContext(ctx, card)
}

func (s *promptCardStream) refreshPromptStatusWithContext(ctx context.Context, card feishu.StreamCard) {
	if !s.showStatusBar || card == nil {
		return
	}
	s.mu.Lock()
	statusText := s.status.text()
	s.mu.Unlock()
	if err := card.UpdateStatus(ctx, statusText); err != nil {
		slog.ErrorContext(ctx, "更新 ACP 流式卡片状态栏失败", "session", s.session.ACPSessionID, "错误", err)
	}
}

func (s *promptCardStream) ensureCard() feishu.StreamCard {
	return s.ensureCardWithContext(s.ctx)
}

func (s *promptCardStream) ensureCardWithContext(ctx context.Context) feishu.StreamCard {
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
		case <-ctx.Done():
			return nil
		}
		s.mu.Lock()
	}
	ready := make(chan struct{})
	s.creating = true
	s.ready = ready
	s.mu.Unlock()

	cardCtx := feishu.WithStreamCardProcessPanel(ctx, s.showStepMessages || s.showThoughts || s.showTools)
	cardCtx = feishu.WithStreamCardStatusBar(cardCtx, s.showStatusBar)
	starter := s.starter
	if starter == nil {
		s.mu.Lock()
		s.creating = false
		if s.ready == ready {
			s.ready = nil
		}
		close(ready)
		s.available = false
		s.mu.Unlock()
		return nil
	}
	card, err := starter.StartStreamCard(cardCtx, s.msg)
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
	if card == nil {
		s.available = false
		return nil
	}
	s.card = card
	s.started = true
	return s.card
}
