package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/youthlin/lark-acp-bridge/internal/arg"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

// handleMessage 处理Bot消息
func (a *Adapter) handleMessage(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	var body []byte
	if event != nil && event.EventReq != nil {
		body = event.Body
	}
	slog.InfoContext(ctx, "Bot收到原始消息", "body", eventLogBody(body, event))
	msg, err := ParseMessage(event)
	if err != nil {
		slog.Warn("解析飞书消息失败", "错误", err, "事件", larkcore.Prettify(event))
		return nil
	}
	msg.BotID = a.cfg.ID
	msg.Workspace = a.cfg.Workspace
	ctx = logging.CtxAddAttr(ctx, messageLogAttrs(msg)...)
	if a.deduper != nil {
		allowed, err := a.deduper.Allow(msg.BotID, msg.MessageID)
		if err != nil {
			slog.ErrorContext(ctx, "记录飞书消息去重状态失败", "错误", err)
		}
		if !allowed {
			slog.InfoContext(ctx, "跳过重复飞书消息")
			return nil
		}
	}
	if a.handler == nil {
		return nil
	}
	msg.Images = hydrateMessageImages(ctx, a.messages, msg.MessageID, msg.Workspace, msg.Images)
	setMessagePrimaryImage(&msg)
	msg = a.withReplyContext(ctx, msg)
	ctx = WithIntermediateReplySender(ctx, a.SendText)
	ctx = WithStreamCardStarter(ctx, a.StartStreamCard)
	reactionID := a.addProcessingReaction(ctx, msg)
	defer a.deleteProcessingReaction(ctx, msg, reactionID)
	reply, err := a.handler.HandleFeishuMessage(ctx, msg)
	if err != nil {
		slog.ErrorContext(ctx, "处理飞书消息失败", "错误", err)
		reply = "处理消息失败：" + err.Error()
	}
	if reply == "" {
		return nil
	}
	if err := a.SendText(ctx, msg, reply); err != nil {
		return fmt.Errorf("回复飞书消息: %w", err)
	}
	slog.InfoContext(ctx, "回复飞书消息成功")
	return nil
}

func (a *Adapter) withReplyContext(ctx context.Context, msg Message) Message {
	replyToID := replyToMessageID(msg)
	if replyToID == "" || a.messages == nil {
		return msg
	}
	reply, err := a.messages.GetMessage(ctx, replyToID, msg.Workspace)
	if err != nil {
		slog.WarnContext(ctx, "读取被回复飞书消息失败", "reply_to_message_id", replyToID, "错误", err)
		return msg
	}
	if reply == nil || strings.TrimSpace(reply.PromptText()) == "" {
		return msg
	}
	msg.Reply = reply
	return msg
}

func replyToMessageID(msg Message) string {
	for _, id := range []string{msg.ParentID, msg.RootID} {
		id = strings.TrimSpace(id)
		if id != "" && id != msg.MessageID {
			return id
		}
	}
	return ""
}

func (a *Adapter) handleReactionCreated(ctx context.Context, event *larkim.P2MessageReactionCreatedV1) error {
	var body []byte
	if event != nil && event.EventReq != nil {
		body = event.Body
	}
	slog.InfoContext(ctx, "Bot消息收到表情回应", "body", eventLogBody(body, event))
	return nil
}

func (a *Adapter) handleReactionDeleted(ctx context.Context, event *larkim.P2MessageReactionDeletedV1) error {
	var body []byte
	if event != nil && event.EventReq != nil {
		body = event.Body
	}
	slog.InfoContext(ctx, "Bot消息取消表情回应", "body", eventLogBody(body, event))
	return nil
}

func eventLogBody(body []byte, fallback any) any {
	if len(body) > 0 {
		return arg.RawJSON(body)
	}
	return arg.JSON(fallback)
}

func messageLogAttrs(msg Message) []slog.Attr {
	return []slog.Attr{
		slog.String("bot", msg.BotID),
		slog.String("chat_id", msg.ChatID),
		slog.String("chat_type", msg.ChatType),
		slog.String("message_id", msg.MessageID),
		slog.String("thread_id", msg.ThreadID),
		slog.String("root_id", msg.RootID),
		slog.String("parent_id", msg.ParentID),
		slog.String("sender_id", msg.SenderID),
	}
}
