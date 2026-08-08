package bridge

import (
	"context"
	"log/slog"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func (s *Service) scheduledTaskSink(task ScheduledTask) TriggerSink {
	task = normalizeScheduledTask(task)
	if !strings.EqualFold(task.ResultSink.Type, "im") || strings.TrimSpace(task.ResultSink.ChatID) == "" {
		return nil
	}
	return &scheduledTaskIMSink{message: feishu.Message{
		BotID:     task.BotID,
		ChatID:    task.ResultSink.ChatID,
		Workspace: task.Cwd,
	},
		cwd:           task.Cwd,
		taskID:        task.ID,
		store:         s.storeForBotID(task.BotID),
		sender:        s.scheduleIMSender(task.BotID),
		messageSender: s.scheduleMessageSender(task.BotID),
		starter:       s.scheduleStreamStarter(task.BotID),
	}
}

func (s *Service) scheduleIMSender(botID string) scheduledTaskIMSender {
	if s == nil {
		return nil
	}
	botID = strings.TrimSpace(botID)
	if sender := s.scheduleSenders[botID]; sender != nil {
		return sender
	}
	return s.scheduleSenders[""]
}

func (s *Service) scheduleMessageSender(botID string) scheduledTaskMessageSender {
	if s == nil {
		return nil
	}
	botID = strings.TrimSpace(botID)
	if sender, ok := s.outboundForBot(botID).(sentMessageSender); ok && sender != nil {
		return sender.SendTextMessage
	}
	if sender := s.scheduleMessageSenders[botID]; sender != nil {
		return sender
	}
	return s.scheduleMessageSenders[""]
}

func (s *Service) scheduleStreamStarter(botID string) scheduledTaskStreamStarter {
	if s == nil {
		return nil
	}
	botID = strings.TrimSpace(botID)
	if starter, ok := s.outboundForBot(botID).(streamCardStarter); ok && starter != nil {
		return starter.StartStreamCard
	}
	if starter := s.scheduleStreams[botID]; starter != nil {
		return starter
	}
	return s.scheduleStreams[""]
}

func (s *scheduledTaskIMSink) OnUpdate(ctx context.Context, result TriggerResult) error {
	stream := s.ensureStream(ctx, result)
	if stream == nil {
		return nil
	}
	stream.updatePromptStatusFromUpdate(result.Update)
	if chunk, ok := promptUpdateChunk(result.Update); ok {
		if chunk.ToolBoundary {
			s.chunks.markToolBoundary()
		}
		s.chunks.add(chunk)
		return nil
	}
	if isToolBoundaryUpdateKind(promptUpdateKind(result.Update)) {
		s.chunks.markToolBoundary()
	} else {
		s.chunks.finishStream()
	}
	stream.updatePromptUpdate(result.Update)
	return nil
}

func (s *scheduledTaskIMSink) OnComplete(ctx context.Context, result TriggerResult) error {
	text := strings.TrimSpace(result.Text)
	if text == "" {
		text = "定时任务已完成，但没有返回文本。"
	}
	if stream := s.ensureStream(ctx, result); stream != nil {
		finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptCardFinalUpdateLimit)
		defer cancel()
		if s.chunks != nil {
			s.chunks.close()
		}
		stream.setFinalTextWithContext(finalCtx, text)
		stream.updatePromptStatusFromResultWithContext(finalCtx, result.ACPResult)
		stream.updatePromptResult(result.ACPResult)
		stream.finishPromptStatusWithContext(finalCtx, result.ACPResult.StopReason)
		stream.updateMetaWithContext(finalCtx, s.streamCardMetaWithTitle(result, scheduleStreamCardCompleted))
		stream.closeWithContext(finalCtx)
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
	ctx = feishu.WithStreamCardMeta(ctx, s.streamCardMeta(result))
	session := result.Session
	if strings.TrimSpace(session.Cwd) == "" {
		session.Cwd = s.cwd
	}
	stream := newPromptCardStream(ctx, s.message, session, ChatConfig{}, streamCardStarterFunc(s.starter))
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
