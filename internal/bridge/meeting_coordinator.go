package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const (
	defaultMeetingFlushInterval = 60 * time.Second
	defaultMeetingFlushChars    = 2000
	defaultMeetingRestoreGrace  = 24 * time.Hour
	defaultMeetingFinalizeGrace = 15 * time.Second
	defaultMeetingJoinMaxAge    = 30 * time.Minute
	defaultMeetingJoinMaxTries  = 6
	defaultMeetingBackfillTries = 3
	meetingRetryBase            = 5 * time.Second
	meetingRetryMax             = 2 * time.Minute
)

type meetingCoordinator struct {
	service *Service
	store   *MeetingStore
	key     meetingKey
	wake    chan struct{}
}

func (s *Service) ensureMeetingCoordinator(parent context.Context, botID, meetingID string, restored bool) {
	key := meetingKey{BotID: strings.TrimSpace(botID), MeetingID: strings.TrimSpace(meetingID)}
	if key.BotID == "" || key.MeetingID == "" {
		return
	}
	store := s.meetingStore(key.BotID)
	if store == nil {
		return
	}
	s.meetingMu.Lock()
	if _, exists := s.meetingCoordinators[key]; exists {
		s.meetingMu.Unlock()
		return
	}
	coordinator := &meetingCoordinator{service: s, store: store, key: key, wake: make(chan struct{}, 1)}
	s.meetingCoordinators[key] = coordinator
	s.meetingMu.Unlock()
	base := context.WithoutCancel(parent)
	if !s.goBackground(base, "meeting:"+key.MeetingID, func(ctx context.Context) { coordinator.run(ctx, restored) }) {
		s.meetingMu.Lock()
		delete(s.meetingCoordinators, key)
		s.meetingMu.Unlock()
	}
}

func (s *Service) wakeMeetingCoordinator(key meetingKey) {
	s.meetingMu.Lock()
	coordinator := s.meetingCoordinators[key]
	s.meetingMu.Unlock()
	if coordinator == nil {
		return
	}
	select {
	case coordinator.wake <- struct{}{}:
	default:
	}
}

func (c *meetingCoordinator) run(ctx context.Context, restored bool) {
	defer func() {
		c.service.meetingMu.Lock()
		if c.service.meetingCoordinators[c.key] == c {
			delete(c.service.meetingCoordinators, c.key)
		}
		c.service.meetingMu.Unlock()
	}()
	if restored {
		c.backfill(ctx)
	}
	for {
		state, ok := c.store.Get(c.key.MeetingID)
		if !ok {
			return
		}
		now := time.Now()
		if state.Status == meetingStatusJoining && meetingJoinRetryExhausted(state, now) {
			c.failJoinPermanently(ctx, state, fmt.Errorf("机器人入会重试已达到上限"))
			continue
		}
		if state.Status == meetingStatusJoining && c.shouldRetry(state, now) {
			c.retryJoin(ctx, state)
			continue
		}
		if (state.Card.Dirty || state.Card.CardID == "") && c.shouldSyncCard(state, now) {
			c.syncCard(ctx, state)
		}
		state, ok = c.store.Get(c.key.MeetingID)
		if !ok {
			return
		}
		if state.Status == meetingStatusCompleted && !state.Card.Dirty {
			if meetingLateEventDeadline(state).After(now) {
				// 在迟到事件窗口内保留 coordinator 和去重键，收到新事件时可重新进入最终整理。
			} else {
				if _, _, err := c.store.Update(state.MeetingID, nil); err != nil {
					slog.WarnContext(ctx, "压缩已完成会议状态失败", "meeting_id", state.MeetingID, "错误", err)
				}
				return
			}
		}
		if state.Status == meetingStatusFailed {
			return
		}
		if state.Status == meetingStatusEnding && c.shouldFinalize(state, now) && c.needsFinalBackfill(state, now) {
			c.finalBackfill(ctx, state)
			continue
		}
		if c.shouldFlush(state, time.Now()) {
			c.flush(ctx, state)
			continue
		}
		wait := c.nextWait(state, time.Now())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (c *meetingCoordinator) shouldFlush(state MeetingState, now time.Time) bool {
	if state.Status == meetingStatusEnding {
		return c.shouldFinalize(state, now) &&
			!state.FinalBackfillAt.IsZero() &&
			(state.BackfillAttempts == 0 || state.BackfillAttempts >= defaultMeetingBackfillTries)
	}
	if state.Status == meetingStatusFinalFailed {
		return !now.Before(state.LastFlushAt.Add(meetingRetryDelay(state.RetryCount)))
	}
	if len(state.PendingEvents) == 0 {
		return false
	}
	if state.RetryCount > 0 {
		return c.shouldRetry(state, now)
	}
	if meetingPendingTextLength(state.PendingEvents) >= defaultMeetingFlushChars {
		return true
	}
	return state.LastFlushAt.IsZero() || !now.Before(state.LastFlushAt.Add(defaultMeetingFlushInterval))
}

func (c *meetingCoordinator) nextWait(state MeetingState, now time.Time) time.Duration {
	waits := make([]time.Duration, 0, 2)
	if state.Card.Dirty {
		waits = append(waits, durationUntilRetry(state.Card.LastAttemptAt, state.Card.RetryCount, now))
	}
	if state.Status == meetingStatusJoining {
		waits = append(waits, durationUntilRetry(state.LastFlushAt, state.RetryCount, now))
	} else if state.Status != meetingStatusEnding && len(state.PendingEvents) > 0 {
		if state.RetryCount > 0 {
			waits = append(waits, durationUntilRetry(state.LastFlushAt, state.RetryCount, now))
		} else {
			waits = append(waits, durationUntil(state.LastFlushAt.Add(defaultMeetingFlushInterval), now))
		}
	}
	if state.Status == meetingStatusEnding {
		if !c.shouldFinalize(state, now) {
			waits = append(waits, durationUntil(meetingFinalizeDeadline(state), now))
		} else if state.FinalBackfillAt.IsZero() {
			waits = append(waits, time.Millisecond)
		} else if state.BackfillAttempts > 0 && state.BackfillAttempts < defaultMeetingBackfillTries {
			waits = append(waits, durationUntilRetry(state.FinalBackfillAt, state.BackfillAttempts, now))
		}
	}
	if state.Status == meetingStatusCompleted && !state.Card.Dirty {
		waits = append(waits, durationUntil(meetingLateEventDeadline(state), now))
	}
	if len(waits) == 0 {
		return defaultMeetingFlushInterval
	}
	wait := waits[0]
	for _, candidate := range waits[1:] {
		if candidate < wait {
			wait = candidate
		}
	}
	return wait
}

func (c *meetingCoordinator) shouldRetry(state MeetingState, now time.Time) bool {
	return !now.Before(state.LastFlushAt.Add(meetingRetryDelay(state.RetryCount)))
}

func (c *meetingCoordinator) shouldSyncCard(state MeetingState, now time.Time) bool {
	if state.Card.LastAttemptAt.IsZero() {
		return true
	}
	return !now.Before(state.Card.LastAttemptAt.Add(meetingRetryDelay(state.Card.RetryCount)))
}

func durationUntilRetry(lastAttempt time.Time, retries int, now time.Time) time.Duration {
	if lastAttempt.IsZero() {
		return time.Millisecond
	}
	return durationUntil(lastAttempt.Add(meetingRetryDelay(retries)), now)
}

func durationUntil(deadline time.Time, now time.Time) time.Duration {
	if delay := deadline.Sub(now); delay > 0 {
		return delay
	}
	return time.Millisecond
}

func (c *meetingCoordinator) retryJoin(ctx context.Context, state MeetingState) {
	joiner, ok := c.service.outboundForBot(state.BotID).(feishu.MeetingJoiner)
	if !ok || joiner == nil {
		c.recordJoinFailure(ctx, state, fmt.Errorf("飞书出站不支持机器人入会"))
		return
	}
	joined, err := joiner.JoinMeeting(ctx, feishu.MeetingJoinRequest{MeetingNo: state.MeetingNo, CallID: state.CallID})
	if err != nil {
		c.recordJoinFailure(ctx, state, err)
		return
	}
	_, _, err = c.store.Update(state.MeetingID, func(current *MeetingState) error {
		current.Status = meetingStatusActive
		current.RetryCount = 0
		current.LastError = ""
		mergeMeetingInfo(current, joined.Meeting)
		if joined.BotUser.ID != "" {
			current.Participants[joined.BotUser.ID] = joined.BotUser.Name
		}
		current.Card.Dirty = true
		return nil
	})
	if err != nil {
		slog.ErrorContext(ctx, "保存恢复入会状态失败", "meeting_id", state.MeetingID, "错误", err)
	}
}

func (c *meetingCoordinator) recordJoinFailure(ctx context.Context, state MeetingState, cause error) {
	updated, _, err := c.store.Update(state.MeetingID, func(current *MeetingState) error {
		current.RetryCount++
		current.LastError = cause.Error()
		current.LastFlushAt = time.Now()
		current.Card.Dirty = true
		if meetingJoinRetryExhausted(*current, time.Now()) {
			current.Status = meetingStatusFailed
		}
		return nil
	})
	if err != nil {
		slog.ErrorContext(ctx, "保存恢复入会失败状态失败", "meeting_id", state.MeetingID, "错误", err)
	}
	if updated.Status == meetingStatusFailed {
		slog.ErrorContext(ctx, "机器人入会重试已终止", "meeting_id", state.MeetingID, "重试次数", updated.RetryCount, "错误", cause)
	}
}

func (c *meetingCoordinator) failJoinPermanently(ctx context.Context, state MeetingState, cause error) {
	_, _, err := c.store.Update(state.MeetingID, func(current *MeetingState) error {
		current.Status = meetingStatusFailed
		current.LastError = cause.Error()
		current.Card.Dirty = true
		return nil
	})
	if err != nil {
		slog.ErrorContext(ctx, "保存机器人入会终止状态失败", "meeting_id", state.MeetingID, "错误", err)
	}
}

func (c *meetingCoordinator) flush(ctx context.Context, state MeetingState) {
	if len(state.PendingEvents) == 0 {
		if state.Status == meetingStatusEnding || state.Status == meetingStatusFinalFailed {
			c.complete(ctx, state)
		}
		return
	}
	batch := append([]MeetingEvent(nil), state.PendingEvents...)
	final := state.Status == meetingStatusEnding || state.Status == meetingStatusFinalFailed
	request, err := c.triggerRequest(state, batch, final)
	if err != nil {
		c.recordFailure(ctx, state, err, final)
		return
	}
	result, err := c.service.runTriggerPrompt(ctx, request)
	if err != nil {
		c.recordFailure(ctx, state, err, final)
		return
	}
	minutes, err := parseMeetingMinutes(result.Text)
	if err != nil {
		c.recordFailure(ctx, state, err, final)
		return
	}
	if err := validateMeetingMinutes(state.Minutes, batch, minutes); err != nil {
		c.recordFailure(ctx, state, err, final)
		return
	}
	keys := make(map[string]struct{}, len(batch))
	for _, event := range batch {
		keys[event.Key] = struct{}{}
	}
	updated, _, err := c.store.Update(state.MeetingID, func(current *MeetingState) error {
		current.Minutes = minutes
		current.ACPSessionID = strings.TrimSpace(result.ACPSessionID)
		current.LastFlushAt = time.Now()
		current.RetryCount = 0
		current.LastError = ""
		pending := current.PendingEvents[:0]
		for _, event := range current.PendingEvents {
			if _, consumed := keys[event.Key]; !consumed {
				pending = append(pending, event)
			}
		}
		current.PendingEvents = pending
		current.Card.Dirty = true
		if final && len(current.PendingEvents) == 0 {
			current.Status = meetingStatusCompleted
			current.CompletedAt = time.Now()
		}
		return nil
	})
	if err != nil {
		slog.ErrorContext(ctx, "保存会议纪要失败", "meeting_id", state.MeetingID, "错误", err)
		return
	}
	c.syncCard(ctx, updated)
}

func (c *meetingCoordinator) triggerRequest(state MeetingState, batch []MeetingEvent, final bool) (TriggerRequest, error) {
	agentName := c.service.defaultAgentName()
	agent, ok := c.service.registry.Get(agentName)
	if !ok || agentName == "" {
		return TriggerRequest{}, fmt.Errorf("未配置默认 agent")
	}
	cwd := strings.TrimSpace(agent.DefaultCwd)
	if cwd == "" {
		return TriggerRequest{}, fmt.Errorf("默认 agent %s 未配置 default_cwd", agentName)
	}
	bot, ok := c.service.botConfig(state.BotID)
	if !ok {
		return TriggerRequest{}, fmt.Errorf("未找到 bot 配置")
	}
	return TriggerRequest{
		BotID:        state.BotID,
		Key:          meetingSessionKey(state.BotID, state.MeetingID),
		Workspace:    bot.Workspace,
		AgentName:    agentName,
		Cwd:          cwd,
		Title:        "会议纪要 " + firstNonEmptyBridge(state.Topic, state.MeetingNo, state.MeetingID),
		Prompt:       meetingPrompt(state, batch, final),
		Metadata:     map[string]string{"meeting_id": state.MeetingID, "final": strconvFormatBool(final)},
		Sink:         noopTriggerSink{},
		DisableTrace: !bot.Meeting.TraceEnabled,
		// 会议处理, 权限请求全部拒绝
		DisableToolPermissions: true,
	}, nil
}

func (c *meetingCoordinator) recordFailure(ctx context.Context, state MeetingState, cause error, final bool) {
	updated, _, err := c.store.Update(state.MeetingID, func(current *MeetingState) error {
		current.RetryCount++
		current.LastError = cause.Error()
		current.LastFlushAt = time.Now()
		current.Card.Dirty = true
		if final {
			current.Status = meetingStatusFinalFailed
		}
		return nil
	})
	if err != nil {
		slog.ErrorContext(ctx, "保存会议整理失败状态失败", "错误", err)
		return
	}
	c.syncCard(ctx, updated)
}

func (c *meetingCoordinator) complete(ctx context.Context, state MeetingState) {
	updated, _, err := c.store.Update(state.MeetingID, func(current *MeetingState) error {
		current.Status = meetingStatusCompleted
		current.CompletedAt = time.Now()
		current.Card.Dirty = true
		return nil
	})
	if err == nil {
		c.syncCard(ctx, updated)
	}
}

func (c *meetingCoordinator) syncCard(ctx context.Context, state MeetingState) {
	sender, ok := c.service.outboundForBot(state.BotID).(feishu.MeetingCardSender)
	if !ok || sender == nil {
		c.recordCardFailure(ctx, state, fmt.Errorf("飞书出站不支持会议纪要卡片"), feishu.MeetingCardSnapshot{})
		return
	}
	view := meetingCardView(state)
	var card feishu.MeetingCard
	var err error
	if state.Card.CardID == "" {
		card, err = sender.StartMeetingCard(ctx, state.RecipientOpenID, view)
	} else {
		card = sender.RestoreMeetingCard(feishu.MeetingCardSnapshot{
			CardID:    state.Card.CardID,
			MessageID: state.Card.MessageID,
			ChatID:    state.Card.ChatID,
			Sequence:  state.Card.Sequence,
		})
		if card == nil {
			err = fmt.Errorf("恢复会议纪要卡片失败")
		} else {
			err = card.Update(ctx, view)
		}
	}
	if card == nil && err == nil {
		err = fmt.Errorf("会议纪要卡片为空")
	}
	var snapshot feishu.MeetingCardSnapshot
	if card != nil {
		snapshot = card.Snapshot()
	}
	c.recordCardResult(ctx, state, snapshot, err)
}

func (c *meetingCoordinator) recordCardFailure(ctx context.Context, state MeetingState, cause error, snapshot feishu.MeetingCardSnapshot) {
	c.recordCardResult(ctx, state, snapshot, cause)
}

func (c *meetingCoordinator) recordCardResult(ctx context.Context, state MeetingState, snapshot feishu.MeetingCardSnapshot, cardErr error) {
	_, _, persistErr := c.store.Update(state.MeetingID, func(current *MeetingState) error {
		if snapshot.CardID != "" {
			current.Card.CardID = snapshot.CardID
			current.Card.MessageID = snapshot.MessageID
			current.Card.ChatID = snapshot.ChatID
			current.Card.Sequence = snapshot.Sequence
		}
		current.Card.Dirty = cardErr != nil
		current.Card.LastAttemptAt = time.Now()
		if cardErr == nil {
			current.Card.RetryCount = 0
			current.LastCardUpdateAt = time.Now()
		} else {
			current.Card.RetryCount++
		}
		return nil
	})
	if cardErr != nil {
		slog.WarnContext(ctx, "更新会议纪要卡片失败", "meeting_id", state.MeetingID, "错误", cardErr)
	}
	if persistErr != nil {
		slog.ErrorContext(ctx, "保存会议卡片状态失败", "meeting_id", state.MeetingID, "错误", persistErr)
	}
}

func (c *meetingCoordinator) backfill(ctx context.Context) {
	lister, ok := c.service.outboundForBot(c.key.BotID).(interface {
		ListMeetingActivities(context.Context, string, time.Time) ([]feishu.MeetingActivity, error)
	})
	if !ok || lister == nil {
		return
	}
	state, exists := c.store.Get(c.key.MeetingID)
	if !exists {
		return
	}
	items, err := lister.ListMeetingActivities(ctx, state.MeetingID, state.StartedAt)
	if err != nil {
		slog.WarnContext(ctx, "补拉会议事件失败", "meeting_id", state.MeetingID, "错误", err)
		return
	}
	if len(items) > 0 {
		_ = c.service.HandleMeetingActivities(
			ctx,
			feishu.MeetingActivities{BotID: state.BotID, Items: items},
			c.service.outboundForBot(state.BotID),
		)
	}
	current, exists := c.store.Get(c.key.MeetingID)
	if !exists || current.Status != meetingStatusJoining && current.Status != meetingStatusActive {
		return
	}
	startedAt := current.StartedAt
	if startedAt.IsZero() {
		startedAt = current.CreatedAt
	}
	if startedAt.IsZero() || time.Since(startedAt) < defaultMeetingRestoreGrace {
		return
	}
	_, _, err = c.store.Update(current.MeetingID, func(state *MeetingState) error {
		state.Status = meetingStatusEnding
		if state.EndedAt.IsZero() {
			state.EndedAt = time.Now()
		}
		state.FinalizeAfter = time.Now()
		state.FinalBackfillAt = time.Now()
		state.BackfillAttempts = 0
		state.Card.Dirty = true
		return nil
	})
	if err != nil {
		slog.WarnContext(ctx, "保存超时会议恢复状态失败", "meeting_id", current.MeetingID, "错误", err)
	}
}

func (c *meetingCoordinator) shouldFinalize(state MeetingState, now time.Time) bool {
	return !now.Before(meetingFinalizeDeadline(state))
}

func (c *meetingCoordinator) needsFinalBackfill(state MeetingState, now time.Time) bool {
	if state.FinalBackfillAt.IsZero() {
		return true
	}
	return state.BackfillAttempts > 0 &&
		state.BackfillAttempts < defaultMeetingBackfillTries &&
		!now.Before(state.FinalBackfillAt.Add(meetingRetryDelay(state.BackfillAttempts)))
}

func (c *meetingCoordinator) finalBackfill(ctx context.Context, state MeetingState) {
	lister, ok := c.service.outboundForBot(c.key.BotID).(interface {
		ListMeetingActivities(context.Context, string, time.Time) ([]feishu.MeetingActivity, error)
	})
	if !ok || lister == nil {
		c.recordFinalBackfill(ctx, state, nil)
		return
	}
	items, err := lister.ListMeetingActivities(ctx, state.MeetingID, state.StartedAt)
	if err == nil && len(items) > 0 {
		err = c.service.HandleMeetingActivities(
			ctx,
			feishu.MeetingActivities{BotID: state.BotID, Items: items},
			c.service.outboundForBot(state.BotID),
		)
	}
	c.recordFinalBackfill(ctx, state, err)
}

func (c *meetingCoordinator) recordFinalBackfill(ctx context.Context, state MeetingState, cause error) {
	_, _, err := c.store.Update(state.MeetingID, func(current *MeetingState) error {
		current.FinalBackfillAt = time.Now()
		if cause == nil {
			current.BackfillAttempts = 0
			if current.Status == meetingStatusEnding {
				current.LastError = ""
			}
		} else {
			current.BackfillAttempts++
			current.LastError = "补拉会议结束事件失败: " + cause.Error()
			current.Card.Dirty = true
		}
		return nil
	})
	if cause != nil {
		slog.WarnContext(ctx, "补拉会议结束事件失败", "meeting_id", state.MeetingID, "错误", cause)
	}
	if err != nil {
		slog.ErrorContext(ctx, "保存会议结束补拉状态失败", "meeting_id", state.MeetingID, "错误", err)
	}
}

func meetingFinalizeDeadline(state MeetingState) time.Time {
	if !state.FinalizeAfter.IsZero() {
		return state.FinalizeAfter
	}
	if !state.EndedAt.IsZero() {
		return state.EndedAt.Add(defaultMeetingFinalizeGrace)
	}
	return time.Now()
}

func meetingLateEventDeadline(state MeetingState) time.Time {
	if state.CompletedAt.IsZero() {
		return time.Time{}
	}
	return state.CompletedAt.Add(meetingLateEventWindow)
}

func meetingJoinRetryExhausted(state MeetingState, now time.Time) bool {
	if state.RetryCount >= defaultMeetingJoinMaxTries {
		return true
	}
	started := state.CreatedAt
	if started.IsZero() {
		started = state.StartedAt
	}
	return !started.IsZero() && now.Sub(started) >= defaultMeetingJoinMaxAge
}

func meetingCardView(state MeetingState) feishu.MeetingCardView {
	view := feishu.MeetingCardView{
		Topic:         state.Topic,
		MeetingNo:     state.MeetingNo,
		Status:        state.Status,
		Summary:       state.Minutes.Summary,
		Decisions:     state.Minutes.Decisions,
		Risks:         state.Minutes.Risks,
		OpenQuestions: state.Minutes.OpenQuestions,
		UpdatedAt:     state.UpdatedAt.Local().Format("2006-01-02 15:04:05"),
		Error:         state.LastError,
	}
	if !state.StartedAt.IsZero() {
		view.StartedAt = state.StartedAt.Local().Format("2006-01-02 15:04")
	}
	if !state.EndedAt.IsZero() {
		view.EndedAt = state.EndedAt.Local().Format("2006-01-02 15:04")
	}
	for _, todo := range state.Minutes.Todos {
		if todo.Confidence == "explicit" {
			view.Todos = append(view.Todos, feishu.MeetingCardTodo{
				Content:  todo.Content,
				Assignee: todo.Assignee,
				DueAt:    todo.DueAt,
			})
		}
	}
	for _, doc := range state.Minutes.SharedDocuments {
		view.SharedDocuments = append(view.SharedDocuments, feishu.MeetingDocument{Title: doc.Title, URL: doc.URL})
	}
	return view
}

func meetingPendingTextLength(events []MeetingEvent) int {
	total := 0
	for _, event := range events {
		total += len([]rune(event.Text))
	}
	return total
}

func meetingRetryDelay(retries int) time.Duration {
	if retries < 1 {
		return meetingRetryBase
	}
	delay := meetingRetryBase
	for i := 1; i < retries && delay < meetingRetryMax; i++ {
		delay *= 2
	}
	if delay > meetingRetryMax {
		return meetingRetryMax
	}
	return delay
}

func firstNonEmptyBridge(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
func strconvFormatBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
