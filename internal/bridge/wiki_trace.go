package bridge

import (
	"context"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const (
	wikiTraceCardRunning   = "知识沉淀处理中"
	wikiTraceCardCompleted = "知识沉淀完成"
	wikiTraceCardFailed    = "知识沉淀失败"
	wikiTraceCardFooter    = "本卡片仅展示后台知识沉淀过程，不会向来源聊天发送回复。"
)

type wikiTraceObserver struct {
	message feishu.Message
	session Session
	show    ChatConfig
	starter scheduledTaskStreamStarter

	stream *promptCardStream
	chunks *promptChunkAccumulator
}

func wikiTracePromptOptions(observer *wikiTraceObserver) acp.PromptOptions {
	if observer == nil {
		return acp.PromptOptions{}
	}
	return acp.PromptOptions{
		OnUpdate: observer.onUpdate,
	}
}

func (s *Service) wikiTraceObserver(session Session) *wikiTraceObserver {
	bot, ok := s.botConfig(session.Key.BotID)
	if !ok || !bot.WikiTrace.Enabled || strings.TrimSpace(bot.WikiTrace.ChatID) == "" {
		return nil
	}
	msg := feishu.Message{
		BotID:     session.Key.BotID,
		ChatID:    strings.TrimSpace(bot.WikiTrace.ChatID),
		Workspace: strings.TrimSpace(session.Workspace),
	}
	return &wikiTraceObserver{
		message: msg,
		session: session,
		show:    s.chatConfigForMessage(msg),
		starter: s.scheduleStreamStarter(session.Key.BotID),
	}
}

func (o *wikiTraceObserver) start(ctx context.Context) {
	o.ensureStream(ctx)
}

func (o *wikiTraceObserver) onUpdate(update acp.PromptUpdate) {
	if o == nil {
		return
	}
	stream := o.ensureStream(o.streamContext())
	if stream == nil {
		return
	}
	stream.updatePromptStatusFromUpdate(update)
	kind := promptUpdateKind(update)
	if chunk, ok := promptUpdateChunk(update); ok {
		if chunk.FinalBoundary {
			o.chunks.markFinalBoundary()
		}
		o.chunks.add(chunk)
		return
	}
	if isFinalTextBoundaryUpdateKind(kind) {
		o.chunks.markFinalBoundary()
	} else {
		o.chunks.finishStream()
	}
	stream.updatePromptUpdate(update)
}

func (o *wikiTraceObserver) streamContext() context.Context {
	if o != nil && o.stream != nil && o.stream.ctx != nil {
		return o.stream.ctx
	}
	return context.Background()
}

func (o *wikiTraceObserver) complete(ctx context.Context, result acp.PromptResult, err error) {
	stream := o.ensureStream(ctx)
	if stream == nil {
		return
	}
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), promptCardFinalUpdateLimit)
	defer cancel()
	if o.chunks != nil {
		o.chunks.close()
	}
	if err != nil {
		stream.updateProcessMessageWithContext(finalCtx, "知识沉淀失败："+err.Error())
		stream.failPromptStatusWithContext(finalCtx)
		stream.updateMetaWithContext(finalCtx, o.streamCardMeta(wikiTraceCardFailed))
		stream.closeWithContext(finalCtx)
		return
	}
	finalText := o.finalCardText(result)
	if finalText == "" {
		finalText = "检查完成，无需沉淀。"
	}
	stream.setFinalTextWithContext(finalCtx, finalText)
	stream.updatePromptStatusFromResultWithContext(finalCtx, result)
	stream.updatePromptResult(result)
	stream.finishPromptStatusWithContext(finalCtx, result.StopReason)
	stream.updateMetaWithContext(finalCtx, o.streamCardMeta(wikiTraceCardCompleted))
	stream.closeWithContext(finalCtx)
}

func (o *wikiTraceObserver) finalCardText(result acp.PromptResult) string {
	if o == nil || o.chunks == nil {
		return wikiResultSummary(result)
	}
	text := strings.TrimSpace(o.chunks.finalText())
	if o.chunks.hasFinalBoundary() {
		return text
	}
	if text != "" {
		return text
	}
	return wikiResultSummary(result)
}

func (o *wikiTraceObserver) ensureStream(ctx context.Context) *promptCardStream {
	if o == nil || o.stream != nil {
		if o == nil {
			return nil
		}
		return o.stream
	}
	if o.starter == nil || strings.TrimSpace(o.message.ChatID) == "" {
		return nil
	}
	stream := newPromptCardStream(ctx, o.message, o.session, o.show, streamCardStarterFunc(o.starter))
	stream.setInitialMeta(o.streamCardMeta(wikiTraceCardRunning))
	if stream.ensureCardWithContext(ctx) == nil {
		return nil
	}
	o.stream = stream
	o.chunks = newPromptChunkAccumulator(stream)
	return stream
}

func (o *wikiTraceObserver) streamCardMeta(title string) feishu.StreamCardMeta {
	return feishu.StreamCardMeta{
		Title:          title,
		Metadata:       wikiTraceMetadata(o.session),
		Footer:         wikiTraceCardFooter,
		HideHeaderIcon: true,
	}
}

func wikiTraceMetadata(session Session) string {
	key := normalizeSessionKey(session.Key)
	lines := make([]string, 0, 3)
	if title := strings.TrimSpace(session.Title); title != "" {
		lines = append(lines, "**来源会话：**"+truncateRunes(title, 80))
	}
	if mainID := strings.TrimSpace(sessionKeyMainID(key)); mainID != "" {
		lines = append(lines, "**来源聊天：**"+mainID)
	}
	if subID := strings.TrimSpace(key.SubID); subID != "" {
		lines = append(lines, "**来源话题：**"+subID)
	}
	return strings.Join(lines, "\n")
}
