package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/youthlin/lark-acp-bridge/internal/arg"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

const maxIncomingMessageAge = 10 * time.Minute

// handleMessage 处理Bot消息
func (a *Adapter) handleMessage(ctx context.Context, event *larkim.P2MessageReceiveV1) (err error) {
	defer recoverEventHandler(ctx, "message", &err)
	var body []byte
	if event != nil && event.EventReq != nil {
		body = event.Body
	}
	slog.DebugContext(ctx, "Bot收到原始消息", "body", eventLogBody(body, event))
	msg, err := ParseMessage(event)
	if err != nil {
		slog.Warn("解析飞书消息失败", "错误", err)
		slog.Debug("解析飞书消息失败详情", "错误", err, "事件", larkcore.Prettify(event))
		return nil
	}
	msg.BotID = a.cfg.ID
	msg.BotOpenID = a.cfg.BotOpenID
	msg.Workspace = a.cfg.Workspace
	if strings.TrimSpace(msg.MessageID) == "" {
		slog.WarnContext(ctx, "跳过缺少 message_id 的飞书消息")
		return nil
	}
	msg = a.withChatInfo(ctx, msg)
	ctx = logging.CtxAddAttr(ctx, messageLogAttrs(msg)...)
	ctx, _ = logging.EnsureTraceID(ctx, incomingMessageTraceParts(msg)...)
	if isStaleIncomingMessage(msg.CreatedAt, time.Now(), maxIncomingMessageAge) {
		slog.InfoContext(ctx, "跳过过旧飞书消息",
			"message_create_time", msg.CreatedAt.Format(time.RFC3339Nano),
			"max_age", maxIncomingMessageAge.String(),
		)
		return nil
	}
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
	msg = a.withMergedForwardContent(ctx, msg)
	msg = a.withReplyContext(ctx, msg)

	handler := a.handler
	if outboundHandler, ok := handler.(OutboundHandler); ok {
		reply, err := outboundHandler.HandleFeishuMessageWithOutbound(ctx, msg, a)
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
	reply, err := handler.HandleFeishuMessage(ctx, msg)
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

func (a *Adapter) withMergedForwardContent(ctx context.Context, msg Message) Message {
	if !strings.EqualFold(msg.MsgType, "merge_forward") || a.messages == nil {
		return msg
	}
	detail, err := a.messages.GetMessage(ctx, msg.MessageID, msg.Workspace)
	if err != nil {
		slog.WarnContext(ctx, "读取合并转发飞书消息失败", "错误", err)
		return msg
	}
	if detail == nil {
		return msg
	}
	text := strings.TrimSpace(detail.Text)
	if text == "" {
		return msg
	}
	msg.Text = text
	msg.Images = append([]MessageImage(nil), detail.Images...)
	setMessagePrimaryImage(&msg)
	return msg
}

func isStaleIncomingMessage(createdAt, now time.Time, maxAge time.Duration) bool {
	if createdAt.IsZero() || maxAge <= 0 {
		return false
	}
	return createdAt.Before(now.Add(-maxAge))
}

func (a *Adapter) withChatInfo(ctx context.Context, msg Message) Message {
	if strings.TrimSpace(msg.ChatID) == "" || msg.IsPrivateChat() || a.chatInfo == nil {
		return msg
	}
	var info chatInfo
	var ok bool
	if a.chatInfoCache != nil {
		info, ok = a.chatInfoCache.Get(msg.ChatID)
	}
	if !ok {
		var err error
		info, err = a.chatInfo.GetChatInfo(ctx, msg.ChatID)
		if err != nil {
			slog.WarnContext(ctx, "读取飞书群信息失败", "chat_id", msg.ChatID, "错误", err)
			return msg
		}
		if a.chatInfoCache != nil {
			a.chatInfoCache.Set(msg.ChatID, info)
		}
	}
	msg.ChatMode = info.ChatMode
	msg.GroupMessageType = info.GroupMessageType
	return msg
}

func (a *Adapter) withReplyContext(ctx context.Context, msg Message) Message {
	replyToID := replyToMessageID(msg)
	if replyToID == "" || a.messages == nil {
		return msg
	}
	replyMessage, err := a.messages.GetMessage(ctx, replyToID, msg.Workspace)
	if err != nil {
		slog.WarnContext(ctx, "读取被回复飞书消息失败", "reply_to_message_id", replyToID, "错误", err)
		return msg
	}
	reply := replyContextFromMessage(replyMessage)
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
func (a *Adapter) handleReactionCreated(ctx context.Context, event *larkim.P2MessageReactionCreatedV1) (err error) {
	defer recoverEventHandler(ctx, "reaction_created", &err)
	var body []byte
	if event != nil && event.EventReq != nil {
		body = event.Body
	}
	slog.DebugContext(ctx, "有消息添加了表情回应", "body", eventLogBody(body, event))
	return nil
}

func (a *Adapter) handleReactionDeleted(ctx context.Context, event *larkim.P2MessageReactionDeletedV1) (err error) {
	defer recoverEventHandler(ctx, "reaction_deleted", &err)
	var body []byte
	if event != nil && event.EventReq != nil {
		body = event.Body
	}
	slog.DebugContext(ctx, "有消息移除了表情回应", "body", eventLogBody(body, event))
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
		slog.String("chat_mode", msg.ChatMode),
		slog.String("group_message_type", msg.GroupMessageType),
		slog.String("message_id", msg.MessageID),
		slog.String("thread_id", msg.ThreadID),
		slog.String("root_id", msg.RootID),
		slog.String("parent_id", msg.ParentID),
		slog.String("sender_id", msg.SenderID),
	}
}

func incomingMessageTraceParts(msg Message) []string {
	return []string{
		"feishu_message",
		msg.BotID,
		msg.MessageID,
		msg.ChatID,
		msg.ThreadID,
		msg.RootID,
		msg.ParentID,
		msg.SenderID,
	}
}
