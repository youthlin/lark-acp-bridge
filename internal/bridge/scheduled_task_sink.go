package bridge

import (
	"context"
	"log/slog"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func (s *scheduledTaskIMSink) OnUpdate(ctx context.Context, result TriggerResult) error {
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

func (s *scheduledTaskIMSink) OnComplete(ctx context.Context, result TriggerResult) error {
	text := strings.TrimSpace(result.Text)
	explicitEmpty := result.TextSet && text == ""
	if text == "" && !explicitEmpty {
		text = "定时任务已完成，但没有返回文本。"
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
		stream.updatePromptResultWithContext(finalCtx, result.ACPResult)
		stream.finishPromptStatusWithContext(finalCtx, result.ACPResult.StopReason)
		stream.updateMetaWithContext(finalCtx, s.streamCardMetaWithTitle(result, scheduleStreamCardCompleted))
		stream.closeWithContext(finalCtx)
		return nil
	}
	if explicitEmpty {
		return nil
	}
	return s.send(ctx, result, text)
}

func (s *scheduledTaskIMSink) OnError(ctx context.Context, result TriggerResult) error {
	text := "定时任务执行失败"
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
		stream.updateMetaWithContext(finalCtx, s.streamCardMetaWithTitle(result, scheduleStreamCardResult))
		stream.closeWithContext(finalCtx)
		return nil
	}
	return s.send(ctx, result, text)
}

func (s *scheduledTaskIMSink) ensureStream(ctx context.Context, result TriggerResult) *promptCardStream {
	if s.stream != nil {
		return s.stream
	}
	if s.starter == nil || strings.TrimSpace(s.message.ChatID) == "" {
		return nil
	}
	session := result.Session
	if strings.TrimSpace(session.Cwd) == "" {
		session.Cwd = s.cwd
	}
	stream := newPromptCardStream(ctx, s.message, session, ChatConfig{}, streamCardStarterFunc(s.starter))
	stream.setProcessMessageID(result.Request.TraceMessageID)
	stream.setInitialMeta(s.streamCardMeta(result))
	card := stream.ensureCardWithContext(ctx)
	if card == nil {
		return nil
	}
	s.stream = stream
	s.chunks = newPromptChunkAccumulator(stream)
	s.bindStreamMessage(ctx, result, card.Message())
	return stream
}

func (s *scheduledTaskIMSink) streamCardMeta(result TriggerResult) feishu.StreamCardMeta {
	return s.streamCardMetaWithTitle(result, scheduleStreamCardRunning)
}

func (s *scheduledTaskIMSink) streamCardMetaWithTitle(result TriggerResult, title string) feishu.StreamCardMeta {
	taskID := firstNonEmpty(s.taskID, result.Request.Metadata["task_id"])
	subtitle := ""
	if taskID != "" {
		subtitle = "task-id: " + taskID
	}
	return feishu.StreamCardMeta{
		Title:    title,
		Subtitle: subtitle,
		Footer:   "本消息的回复链将在本次执行会话中处理。",
	}
}

func (s *scheduledTaskIMSink) bindStreamMessage(ctx context.Context, result TriggerResult, sent feishu.SentMessage) {
	s.bindSentMessage(ctx, result, sent)
}

func (s *scheduledTaskIMSink) bindSentMessage(ctx context.Context, result TriggerResult, sent feishu.SentMessage) {
	if s.store == nil || strings.TrimSpace(sent.MessageID) == "" {
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
		slog.WarnContext(ctx, "保存定时任务结果消息会话绑定失败", "message_id", sent.MessageID, "session", result.Session.ACPSessionID, "错误", err)
	}
}

func (s *scheduledTaskIMSink) send(ctx context.Context, result TriggerResult, text string) error {
	if s.messageSender != nil {
		sent, err := s.messageSender(ctx, s.message, text)
		if err == nil {
			s.bindSentMessage(ctx, result, sent)
			return nil
		}
		slog.WarnContext(ctx, "定时任务 IM result sink 发送新消息失败，尝试降级发送", "chat_id", s.message.ChatID, "错误", err)
	}
	if s.sender != nil {
		return s.sender(ctx, s.message, text, feishu.OutboundRenderContext{BaseDir: s.cwd})
	}
	slog.WarnContext(ctx, "缺少定时任务 IM result sink 发送器", "chat_id", s.message.ChatID, "thread_id", s.message.ThreadID)
	return nil
}
