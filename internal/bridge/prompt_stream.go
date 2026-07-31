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
	processPending    string
	processDirty      bool
	processLastFlush  time.Time
	processTimer      *time.Timer
	processTimerGen   int64
	processFlushing   sync.WaitGroup
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
	return newPromptCardStreamWithStatusPrefix(ctx, msg, session, show, "")
}

func newPromptCardStreamWithStatusPrefix(ctx context.Context, msg feishu.Message, session Session, show ChatConfig, statusPrefix string) *promptCardStream {
	return &promptCardStream{
		ctx:              ctx,
		msg:              msg,
		session:          session,
		available:        true,
		showStepMessages: !show.HideStepMessages,
		showPlans:        !show.HidePlans,
		showThoughts:     chatThoughtsVisible(show),
		showTools:        !show.HideTools,
		showStatusBar:    !show.HideStatusBar,
		showUsageDetail:  !show.HideUsageDetail,
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

func (s *promptCardStream) updatePromptStatusFromResult(result acp.PromptResult) {
	s.updatePromptStatusFromResultWithContext(s.ctx, result)
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
	title := toolDisplayName(u)
	line, ok := s.toolProgressLine(status, title)
	if !ok {
		return true
	}
	s.mu.Lock()
	processText := s.applyToolProgressLineLocked(status, title, line)
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
		return processPanelText(s.process)
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
		return processPanelText(s.process)
	}
	s.process = append(s.process, normalizeStreamMarkdown(line))
	return processPanelText(s.process)
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
	s.processPending = text
	s.processDirty = true
	if force || s.processLastFlush.IsZero() || !now.Before(s.processLastFlush.Add(promptProcessFlushInterval)) {
		flushText, flushNow = s.takePendingProcessUpdateLocked(now)
		s.stopProcessFlushTimerLocked()
	} else {
		s.scheduleProcessFlushLocked(s.processLastFlush.Add(promptProcessFlushInterval).Sub(now))
	}
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
	s.stopProcessFlushTimerLocked()
	flushText, flushNow = s.takePendingProcessUpdateLocked(time.Now())
	s.mu.Unlock()
	if flushNow && card != nil {
		s.applyProcessUpdateWithContext(ctx, card, flushText)
	}
	s.processFlushing.Wait()
}

func (s *promptCardStream) scheduleProcessFlushLocked(delay time.Duration) {
	if s.processTimer != nil {
		return
	}
	if delay < 0 {
		delay = 0
	}
	s.processTimerGen++
	generation := s.processTimerGen
	s.processFlushing.Add(1)
	s.processTimer = time.AfterFunc(delay, func() {
		defer s.processFlushing.Done()
		var (
			card      feishu.StreamCard
			flushText string
			flushNow  bool
		)
		s.mu.Lock()
		if s.processTimerGen != generation {
			s.mu.Unlock()
			return
		}
		s.processTimer = nil
		card = s.card
		flushText, flushNow = s.takePendingProcessUpdateLocked(time.Now())
		s.mu.Unlock()
		if flushNow && card != nil {
			s.applyProcessUpdate(card, flushText)
		}
	})
}

func (s *promptCardStream) stopProcessFlushTimerLocked() {
	s.processTimerGen++
	if s.processTimer == nil {
		return
	}
	if s.processTimer.Stop() {
		s.processFlushing.Done()
	}
	s.processTimer = nil
}

func (s *promptCardStream) takePendingProcessUpdateLocked(now time.Time) (string, bool) {
	if !s.processDirty {
		return "", false
	}
	text := s.processPending
	s.processPending = ""
	s.processDirty = false
	s.processLastFlush = now
	return text, true
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
