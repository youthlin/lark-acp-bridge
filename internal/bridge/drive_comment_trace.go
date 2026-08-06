package bridge

import (
	"context"
	"log/slog"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const (
	driveCommentStreamCardRunning   = "云文档评论处理中"
	driveCommentStreamCardCompleted = "云文档评论处理完成"
	driveCommentStreamCardResult    = "云文档评论处理结果"
)

type driveCommentTraceSink struct {
	message feishu.Message
	cwd     string
	comment feishu.DriveComment
	store   *SessionStore
	starter scheduledTaskStreamStarter

	stream *promptCardStream
	chunks *promptChunkAccumulator
}

func (s *driveCommentTraceSink) OnUpdate(ctx context.Context, result TriggerResult) error {
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

func (s *driveCommentTraceSink) OnComplete(ctx context.Context, result TriggerResult) error {
	text := strings.TrimSpace(result.Text)
	if text == "" {
		text = "云文档评论已完成，但没有返回文本。"
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
		stream.updateMetaWithContext(finalCtx, s.streamCardMetaWithTitle(result, driveCommentStreamCardCompleted))
		stream.closeWithContext(finalCtx)
	}
	return nil
}

func (s *driveCommentTraceSink) OnError(ctx context.Context, result TriggerResult) error {
	text := "云文档评论处理失败"
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
		stream.updateMetaWithContext(finalCtx, s.streamCardMetaWithTitle(result, driveCommentStreamCardResult))
		stream.closeWithContext(finalCtx)
	}
	return nil
}

func (s *driveCommentTraceSink) ensureStream(ctx context.Context, result TriggerResult) *promptCardStream {
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

func (s *driveCommentTraceSink) streamCardMeta(result TriggerResult) feishu.StreamCardMeta {
	return s.streamCardMetaWithTitle(result, driveCommentStreamCardRunning)
}

func (s *driveCommentTraceSink) streamCardMetaWithTitle(result TriggerResult, title string) feishu.StreamCardMeta {
	subtitle := driveCommentTraceSubtitle(s.comment)
	if subtitle == "" {
		subtitle = strings.TrimSpace(result.Request.Metadata["comment_id"])
	}
	return feishu.StreamCardMeta{
		Title:    title,
		Subtitle: subtitle,
		Footer:   "本消息的回复链将在本次执行会话中处理；最终回复仍写回云文档评论。",
	}
}

func (s *driveCommentTraceSink) bindStreamMessage(ctx context.Context, result TriggerResult, sent feishu.SentMessage) {
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
		slog.WarnContext(ctx, "保存云文档评论处理过程消息会话绑定失败", "message_id", sent.MessageID, "session", result.Session.ACPSessionID, "错误", err)
	}
}

func driveCommentTraceSubtitle(comment feishu.DriveComment) string {
	comment = comment.Normalized()
	parts := make([]string, 0, 3)
	if comment.FileType != "" || comment.FileToken != "" {
		parts = append(parts, strings.TrimSpace(comment.FileType)+":"+strings.TrimSpace(comment.FileToken))
	}
	if strings.TrimSpace(comment.CommentID) != "" {
		parts = append(parts, "#"+strings.TrimSpace(comment.CommentID))
	}
	return strings.Join(parts, "")
}
