package bridge

import (
	"context"
	"log/slog"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const (
	meetingTraceCardRunning   = "会议整理中"
	meetingTraceCardCompleted = "会议整理完成"
	meetingTraceCardResult    = "会议整理结果"
	meetingTraceCardFooter    = "本卡片仅展示后台会议整理过程，最终纪要将更新到会议纪要卡片。回复本消息会进入对应会议整理会话。"
)

type meetingTraceSink struct {
	message feishu.Message
	state   MeetingState
	show    ChatConfig
	store   *SessionStore
	starter scheduledTaskStreamStarter

	stream *promptCardStream
	chunks *promptChunkAccumulator
}

func (s *meetingTraceSink) OnUpdate(ctx context.Context, result TriggerResult) error {
	stream := s.ensureStream(ctx, result)
	if stream == nil {
		return nil
	}
	stream.updatePromptStatusFromUpdate(result.Update)
	if chunk, ok := promptUpdateChunk(result.Update); ok {
		if chunk.FinalBoundary {
			s.chunks.markFinalBoundary()
		}
		s.chunks.add(chunk)
		return nil
	}
	if isFinalTextBoundaryUpdateKind(promptUpdateKind(result.Update)) {
		s.chunks.markFinalBoundary()
	} else {
		s.chunks.finishStream()
	}
	stream.updatePromptUpdate(result.Update)
	return nil
}

func (s *meetingTraceSink) OnComplete(ctx context.Context, result TriggerResult) error {
	text := strings.TrimSpace(result.Text)
	explicitEmpty := result.TextSet && text == ""
	if text == "" && !explicitEmpty {
		text = "会议整理已完成，但没有返回文本。"
	}
	if stream := s.ensureStream(ctx, result); stream != nil {
		finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptCardFinalUpdateLimit)
		defer cancel()
		if s.chunks != nil {
			s.chunks.close()
		}
		if text != "" {
			stream.setFinalTextWithContext(finalCtx, text)
		}
		stream.updatePromptStatusFromResultWithContext(finalCtx, result.ACPResult)
		stream.updatePromptResult(result.ACPResult)
		stream.finishPromptStatusWithContext(finalCtx, result.ACPResult.StopReason)
		stream.updateMetaWithContext(finalCtx, s.streamCardMetaWithTitle(meetingTraceCardCompleted))
		stream.closeWithContext(finalCtx)
	}
	return nil
}

func (s *meetingTraceSink) OnError(ctx context.Context, result TriggerResult) error {
	text := "会议整理失败"
	if result.Err != nil {
		text += "：" + result.Err.Error()
	}
	if stream := s.ensureStream(ctx, result); stream != nil {
		finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptCardFinalUpdateLimit)
		defer cancel()
		if s.chunks != nil {
			s.chunks.close()
		}
		stream.updateProcessMessageWithContext(finalCtx, text)
		stream.failPromptStatusWithContext(finalCtx)
		stream.updateMetaWithContext(finalCtx, s.streamCardMetaWithTitle(meetingTraceCardResult))
		stream.closeWithContext(finalCtx)
	}
	return nil
}

func (s *meetingTraceSink) ensureStream(ctx context.Context, result TriggerResult) *promptCardStream {
	if s == nil {
		return nil
	}
	if s.stream != nil {
		return s.stream
	}
	if s.starter == nil || strings.TrimSpace(s.message.ChatID) == "" {
		return nil
	}
	session := result.Session
	if strings.TrimSpace(session.Cwd) == "" {
		session.Cwd = result.Request.Cwd
	}
	message := s.traceMessage(result.Session.Key)
	stream := newPromptCardStream(ctx, message, session, s.show, streamCardStarterFunc(s.starter))
	stream.setProcessMessageID(result.Request.TraceMessageID)
	stream.setInitialMeta(s.streamCardMetaWithTitle(meetingTraceCardRunning))
	card := stream.ensureCardWithContext(ctx)
	if card == nil {
		return nil
	}
	s.stream = stream
	s.chunks = newPromptChunkAccumulator(stream)
	s.bindStreamMessage(ctx, result, card.Message())
	return stream
}

func (s *meetingTraceSink) traceMessage(sessionKey SessionKey) feishu.Message {
	message := s.message
	if s.store == nil {
		return message
	}
	sessionKey = normalizeSessionKey(sessionKey)
	if !sessionKey.Valid() {
		sessionKey = meetingSessionKey(s.state.BotID, s.state.MeetingID)
	}
	binding, ok := s.store.FirstMessageForSession(message.BotID, message.ChatID, sessionKey)
	if !ok {
		return message
	}
	message.MessageID = binding.MessageID
	message.ForceReplyInThread = true
	return message
}

func (s *meetingTraceSink) streamCardMetaWithTitle(title string) feishu.StreamCardMeta {
	return feishu.StreamCardMeta{
		Title:          title,
		Metadata:       meetingTraceMetadata(s.state),
		Footer:         meetingTraceCardFooter,
		HideHeaderIcon: true,
	}
}

func meetingTraceMetadata(state MeetingState) string {
	lines := make([]string, 0, 4)
	if topic := strings.TrimSpace(state.Topic); topic != "" {
		lines = append(lines, "**会议主题：** "+truncateRunes(topic, 80))
	}
	if meetingNo := strings.TrimSpace(state.MeetingNo); meetingNo != "" {
		lines = append(lines, "**会议号：** "+meetingNo)
	}
	if meetingID := strings.TrimSpace(state.MeetingID); meetingID != "" {
		lines = append(lines, "**会议 ID：** "+meetingID)
	}
	if !state.StartedAt.IsZero() {
		lines = append(lines, "**开始时间：** "+state.StartedAt.Local().Format("2006-01-02 15:04:05"))
	}
	return strings.Join(lines, "\n")
}

func (s *meetingTraceSink) bindStreamMessage(ctx context.Context, result TriggerResult, sent feishu.SentMessage) {
	if s == nil || s.store == nil || strings.TrimSpace(sent.MessageID) == "" {
		return
	}
	chatID := firstNonEmpty(sent.ChatID, s.message.ChatID)
	if strings.TrimSpace(chatID) == "" {
		return
	}
	if _, err := s.store.BindMessageToSession(MessageSessionBinding{
		BotID:      result.Request.BotID,
		ChatID:     chatID,
		MessageID:  sent.MessageID,
		SessionKey: result.Session.Key,
	}); err != nil {
		slog.WarnContext(ctx, "保存会议整理过程卡片会话绑定失败", "message_id", sent.MessageID, "session", result.Session.ACPSessionID, "错误", err)
	}
}

func (s *Service) meetingTraceSink(state MeetingState) TriggerSink {
	bot, ok := s.botConfig(state.BotID)
	if !ok || !bot.Meeting.TraceEnabled || strings.TrimSpace(bot.Meeting.TraceChatID) == "" {
		return noopTriggerSink{}
	}
	message := feishu.Message{
		BotID:     state.BotID,
		ChatID:    strings.TrimSpace(bot.Meeting.TraceChatID),
		Workspace: strings.TrimSpace(bot.Workspace),
	}
	return &meetingTraceSink{
		message: message,
		state:   state,
		show:    s.chatConfigForMessage(message),
		store:   s.storeForBotID(state.BotID),
		starter: s.scheduleStreamStarter(state.BotID),
	}
}

func meetingTraceMessageID(state MeetingState) string {
	return triggerTraceMessageID(meetingSessionKey(state.BotID, state.MeetingID))
}
