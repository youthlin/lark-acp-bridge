package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

type loopRunStatus struct {
	running     bool
	started     time.Time
	ended       time.Time
	round       int
	maxRounds   int
	maxDuration time.Duration
	interval    time.Duration
	prompt      string
	pendingAdd  string
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
	started time.Time
}

func (s *Service) handleLoopCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) >= 2 {
		switch strings.ToLower(strings.TrimSpace(fields[1])) {
		case "add":
			return s.handleLoopAddCommand(ctx, msg, strings.TrimSpace(commandRemainder(text, 2)))
		case "how":
			// /loop how is intentionally handled as a normal user prompt so the
			// model can use the current ACP session context to refine the command.
			prompt, err := loopHowPrompt(strings.TrimSpace(commandRemainder(text, 2)))
			if err != nil {
				return err.Error()
			}
			reply, err := s.prompt(ctx, msg, prompt)
			if err != nil {
				return "生成 loop 命令失败：" + err.Error()
			}
			return reply
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
	started := time.Now()
	startText := loopAnchorText(req, loopProgressStarted, 0, "", started, started)
	cardReq := feishu.LoopStatusCardRequest{
		BotID:        session.Key.BotID,
		ChatID:       sessionKeyMainID(session.Key),
		ThreadID:     session.Key.SubID,
		ACPSessionID: session.ACPSessionID,
		Text:         startText,
	}
	if card, ok, err := s.sendLoopStatusCard(ctx, msg, cardReq); err != nil {
		return "启动 loop 失败：" + err.Error()
	} else if ok {
		anchor := loopAnchor{message: loopAnchorMessage(msg, card.Message()), request: req, card: card, started: started}
		s.startLoop(ctx, msg, anchor, session, prepared.agent, req, started)
		return ""
	}
	s.startLoop(ctx, msg, loopAnchor{started: started}, session, prepared.agent, req, started)
	return startText
}

func (s *Service) startLoop(ctx context.Context, msg feishu.Message, anchor loopAnchor, session Session, agent config.AgentConfig, req loopRequest, started time.Time) {
	if started.IsZero() {
		started = time.Now()
	}
	if anchor.started.IsZero() {
		anchor.started = started
	}
	ctx, finish := s.startLoopTask(context.WithoutCancel(ctx), session, agent)
	s.markLoopStarted(session.Key, started, req)
	s.setTaskCancelHandler(session.Key, func(cancelCtx context.Context, reason string) {
		s.updateLoopAnchor(cancelCtx, anchor, loopProgressFinished, 0, reason)
	})
	s.goBackground("loop:"+string(session.Key.MainID), func() {
		defer finish()
		reason, err := s.runLoop(ctx, msg, anchor, session, agent, req, started)
		s.markLoopFinished(session.Key, started, reason, err)
		s.updateLoopFinished(ctx, msg, anchor, reason, err)
	})
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
		roundPrompt := s.promptTextWithWorkspaceContextForSession(session, msg, loopPrompt(basePrompt, s.takeLoopPendingAdd(session.Key, started), req, round, started, deadline))
		includedWorkspaceContext := shouldIncludeWorkspaceContextPrompt(session, sessionWorkspace(session, msg))
		run := s.promptRuntimeWithProgressRawStatusPrefix(ctx, cardMsg, session, agent, roundPrompt, loopStatusPrefix(round))
		result := run.result
		rawResult := run.rawResult
		streamedReply := run.reply
		if includedWorkspaceContext && (run.err == nil || run.sentProgress) {
			session = s.markWorkspacePrompted(ctx, msg, session)
		}
		s.updateLoopAnchor(ctx, anchor, loopProgressCompleted, round, "")
		if run.err != nil {
			if errors.Is(run.err, context.Canceled) {
				return "已取消", context.Canceled
			}
			if strings.TrimSpace(rawResult.Text) != "" || strings.TrimSpace(result.Text) != "" {
				return "执行失败：" + run.err.Error(), run.err
			}
			return "执行失败：" + run.err.Error(), run.err
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

func loopPrompt(promptText string, pendingAdd string, req loopRequest, round int, started time.Time, deadline time.Time) string {
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
	userMessage := strings.TrimSpace(promptText)
	if strings.TrimSpace(pendingAdd) != "" {
		userMessage = strings.Join([]string{
			userMessage,
			"",
			"## 本轮补充消息",
			strings.TrimSpace(pendingAdd),
		}, "\n")
	}
	return promptWithUserMessage(prefixes, userMessage)
}

func loopStatusPrefix(round int) string {
	if round <= 0 {
		return ""
	}
	return "L" + strconv.Itoa(round)
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
	text := loopAnchorText(anchor.request, state, round, reason, anchor.started, time.Now())
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

func loopAnchorText(req loopRequest, state loopProgressState, round int, reason string, started time.Time, now time.Time) string {
	lines := []string{
		"已启动 loop。",
		formatLoopRequest(req),
		"",
		"状态：" + loopProgressText(state, round),
	}
	if strings.TrimSpace(reason) != "" {
		lines = append(lines, "结束原因："+strings.TrimSpace(reason))
	}
	if !started.IsZero() && !now.IsZero() {
		lines = append(lines, "已运行："+nonNegativeDuration(now.Sub(started)).String())
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

func nonNegativeDuration(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

func (s *Service) markLoopStarted(key SessionKey, started time.Time, req loopRequest) {
	key = normalizeSessionKey(key)
	s.startLoopStatus(key, started, req)
}

func (s *Service) startLoopStatus(key SessionKey, started time.Time, req loopRequest) {
	key = normalizeSessionKey(key)
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
	key = normalizeSessionKey(key)
	s.updateLoopRoundStatus(key, started, round)
}

func (s *Service) updateLoopRoundStatus(key SessionKey, started time.Time, round int) bool {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	status := s.loopStatuses[key]
	if !status.started.Equal(started) {
		return false
	}
	status.running = true
	status.round = round
	s.loopStatuses[key] = status
	return true
}

func (s *Service) handleLoopAddCommand(ctx context.Context, msg feishu.Message, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "请提供要追加到下一轮 loop prompt 的补充消息，例如 /loop add 补充说明。"
	}
	session, ok := s.findSession(msg)
	if !ok {
		return "当前会话没有正在运行的 loop。"
	}
	if !s.addLoopPendingMessage(session.Key, text) {
		return "当前会话没有正在运行的 loop。"
	}
	return "已追加到下一轮 loop prompt，下一轮执行完成后自动清空。"
}

func (s *Service) addLoopPendingMessage(key SessionKey, text string) bool {
	key = normalizeSessionKey(key)
	return s.appendLoopPendingMessage(key, text)
}

func (s *Service) appendLoopPendingMessage(key SessionKey, text string) bool {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	status, ok := s.loopStatuses[key]
	if !ok || !status.running {
		return false
	}
	text = strings.TrimSpace(text)
	if status.pendingAdd == "" {
		status.pendingAdd = text
	} else {
		status.pendingAdd = strings.TrimSpace(status.pendingAdd + "\n\n" + text)
	}
	s.loopStatuses[key] = status
	return true
}

func (s *Service) takeLoopPendingAdd(key SessionKey, started time.Time) string {
	key = normalizeSessionKey(key)
	return s.consumeLoopPendingMessage(key, started)
}

func (s *Service) consumeLoopPendingMessage(key SessionKey, started time.Time) string {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	status, ok := s.loopStatuses[key]
	if !ok || status.started != started || status.pendingAdd == "" {
		return ""
	}
	pending := status.pendingAdd
	status.pendingAdd = ""
	s.loopStatuses[key] = status
	return pending
}

func (s *Service) markLoopFinished(key SessionKey, started time.Time, reason string, err error) {
	key = normalizeSessionKey(key)
	s.finishLoopStatus(key, started, reason, err)
}

func (s *Service) finishLoopStatus(key SessionKey, started time.Time, reason string, err error) bool {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	status := s.loopStatuses[key]
	if !status.started.Equal(started) {
		return false
	}
	if errors.Is(err, context.Canceled) && !status.running && status.reason != "" {
		return false
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
	return true
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
	key := imSessionKey(msg.BotID, msg.ChatID, msg.ThreadID)
	store := s.storeForMessage(msg)
	if store != nil {
		var session Session
		ok := false
		for _, candidate := range callbackSessionKeys(msg) {
			session, ok = store.Get(candidate)
			if ok {
				break
			}
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
	key = normalizeSessionKey(key)
	task := s.takeRunningTaskOfKind(key, taskKindLoop)
	if task == nil {
		return false
	}
	s.markLoopTaskCanceled(key, reason)
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
	status, ok := s.loopStatusSnapshot(session.Key)
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

func (s *Service) loopStatusSnapshot(key SessionKey) (loopRunStatus, bool) {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	status, ok := s.loopStatuses[key]
	return status, ok
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
