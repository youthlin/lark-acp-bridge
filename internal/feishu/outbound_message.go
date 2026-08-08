package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func (a *Adapter) SendText(ctx context.Context, msg Message, text string) error {
	return a.SendTextWithRenderContext(ctx, msg, text, OutboundRenderContext{BaseDir: msg.Workspace})
}

func (a *Adapter) SendTextWithRenderContext(ctx context.Context, msg Message, text string, render OutboundRenderContext) error {
	blocks, err := a.renderOutboundBlocks(ctx, text, outboundRenderContextFromPublic(render))
	if err != nil {
		slog.ErrorContext(ctx, "渲染飞书出站消息失败", "message_id", msg.MessageID, "chat_id", msg.ChatID, "base_dir", render.BaseDir, "错误", err)
		return err
	}
	if outboundBlocksHaveImage(blocks) {
		slog.InfoContext(ctx, "飞书出站消息包含图片，改用富文本发送", "message_id", msg.MessageID, "chat_id", msg.ChatID, "image_count", outboundBlocksImageCount(blocks), "base_dir", render.BaseDir)
		_, err := a.SendPostMessage(ctx, msg, blocks)
		if err != nil {
			slog.ErrorContext(ctx, "发送飞书富文本图片消息失败", "message_id", msg.MessageID, "chat_id", msg.ChatID, "image_count", outboundBlocksImageCount(blocks), "错误", err)
		}
		return err
	}
	_, err = a.SendTextMessage(ctx, msg, text)
	if err != nil {
		slog.ErrorContext(ctx, "发送飞书文本消息失败", "message_id", msg.MessageID, "chat_id", msg.ChatID, "错误", err)
	}
	return err
}

func (a *Adapter) SendTextMessage(ctx context.Context, msg Message, text string) (SentMessage, error) {
	if strings.TrimSpace(msg.MessageID) != "" {
		return a.ReplyTextMessage(ctx, msg, text)
	}
	if strings.TrimSpace(msg.ChatID) != "" {
		return a.SendChatTextMessage(ctx, msg.ChatID, msg.ChatType, text)
	}
	return SentMessage{}, fmt.Errorf("飞书 message_id 为空")
}

func (a *Adapter) SendChatText(ctx context.Context, chatID string, text string) error {
	_, err := a.SendChatTextMessage(ctx, chatID, "p2p", text)
	return err
}

func (a *Adapter) SendChatTextMessage(ctx context.Context, chatID string, chatType string, text string) (SentMessage, error) {
	if a.client == nil {
		return SentMessage{}, fmt.Errorf("飞书客户端未初始化")
	}
	if chatID == "" {
		return SentMessage{}, fmt.Errorf("飞书 chat_id 为空")
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return SentMessage{}, fmt.Errorf("编码飞书消息内容: %w", err)
	}
	msgType := "text"
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(msgType).
			Content(string(content)).
			Build()).
		Build()
	resp, err := a.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return SentMessage{}, fmt.Errorf("调用飞书发消息接口: %w", err)
	}
	if !resp.Success() {
		return SentMessage{}, fmt.Errorf("飞书发消息接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return sentMessageFromCreateResp(resp, chatID, chatType), nil
}

func (a *Adapter) SendPostMessage(ctx context.Context, msg Message, blocks []outboundBlock) (SentMessage, error) {
	if strings.TrimSpace(msg.MessageID) != "" {
		return a.ReplyPostMessage(ctx, msg, blocks)
	}
	if strings.TrimSpace(msg.ChatID) != "" {
		return a.SendChatPostMessage(ctx, msg.ChatID, msg.ChatType, blocks)
	}
	return SentMessage{}, fmt.Errorf("飞书 message_id 为空")
}

func (a *Adapter) SendChatPostMessage(ctx context.Context, chatID string, chatType string, blocks []outboundBlock) (SentMessage, error) {
	if a.client == nil {
		return SentMessage{}, fmt.Errorf("飞书客户端未初始化")
	}
	if chatID == "" {
		return SentMessage{}, fmt.Errorf("飞书 chat_id 为空")
	}
	content, err := outboundBlocksPostContent(blocks)
	if err != nil {
		return SentMessage{}, err
	}
	resp, err := a.client.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType("post").
			Content(content).
			Build()).
		Build())
	if err != nil {
		return SentMessage{}, fmt.Errorf("调用飞书发富文本消息接口: %w", err)
	}
	if !resp.Success() {
		return SentMessage{}, fmt.Errorf("飞书发富文本消息接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return sentMessageFromCreateResp(resp, chatID, chatType), nil
}

func (a *Adapter) ReplyText(ctx context.Context, msg Message, text string) error {
	_, err := a.ReplyTextMessage(ctx, msg, text)
	return err
}

func (a *Adapter) ReplyTextMessage(ctx context.Context, msg Message, text string) (SentMessage, error) {
	if a.client == nil {
		return SentMessage{}, fmt.Errorf("飞书客户端未初始化")
	}
	if msg.MessageID == "" {
		return SentMessage{}, fmt.Errorf("飞书 message_id 为空")
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return SentMessage{}, fmt.Errorf("编码飞书回复内容: %w", err)
	}
	msgType := "text"
	replyInThread := replyInThreadForMessage(msg)
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(msg.MessageID).
		Body(&larkim.ReplyMessageReqBody{
			Content:       larkcore.StringPtr(string(content)),
			MsgType:       larkcore.StringPtr(msgType),
			ReplyInThread: &replyInThread,
		}).
		Build()
	resp, err := a.client.Im.Message.Reply(ctx, req)
	if err != nil {
		return SentMessage{}, fmt.Errorf("调用飞书回复接口: %w", err)
	}
	if !resp.Success() {
		return SentMessage{}, fmt.Errorf("飞书回复接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return sentMessageFromReplyResp(resp, msg), nil
}

func (a *Adapter) ReplyPostMessage(ctx context.Context, msg Message, blocks []outboundBlock) (SentMessage, error) {
	if a.client == nil {
		return SentMessage{}, fmt.Errorf("飞书客户端未初始化")
	}
	if msg.MessageID == "" {
		return SentMessage{}, fmt.Errorf("飞书 message_id 为空")
	}
	content, err := outboundBlocksPostContent(blocks)
	if err != nil {
		return SentMessage{}, err
	}
	replyInThread := replyInThreadForMessage(msg)
	resp, err := a.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(msg.MessageID).
		Body(&larkim.ReplyMessageReqBody{
			Content:       larkcore.StringPtr(content),
			MsgType:       larkcore.StringPtr("post"),
			ReplyInThread: &replyInThread,
		}).
		Build())
	if err != nil {
		return SentMessage{}, fmt.Errorf("调用飞书回复富文本接口: %w", err)
	}
	if !resp.Success() {
		return SentMessage{}, fmt.Errorf("飞书回复富文本接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return sentMessageFromReplyResp(resp, msg), nil
}

func (a *Adapter) SendImageMessage(ctx context.Context, msg Message, path string) (SentMessage, error) {
	if strings.TrimSpace(msg.MessageID) != "" {
		return a.ReplyImageMessage(ctx, msg, path)
	}
	if strings.TrimSpace(msg.ChatID) != "" {
		return a.SendChatImageMessage(ctx, msg.ChatID, msg.ChatType, path)
	}
	return SentMessage{}, fmt.Errorf("飞书 message_id 为空")
}

func (a *Adapter) SendChatImageMessage(ctx context.Context, chatID string, chatType string, path string) (SentMessage, error) {
	if a.client == nil {
		return SentMessage{}, fmt.Errorf("飞书客户端未初始化")
	}
	if chatID == "" {
		return SentMessage{}, fmt.Errorf("飞书 chat_id 为空")
	}
	imageKey, err := a.uploadReplyImage(ctx, path)
	if err != nil {
		return SentMessage{}, err
	}
	content, err := json.Marshal(map[string]string{"image_key": imageKey})
	if err != nil {
		return SentMessage{}, fmt.Errorf("编码飞书图片消息内容: %w", err)
	}
	resp, err := a.client.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType("image").
			Content(string(content)).
			Build()).
		Build())
	if err != nil {
		return SentMessage{}, fmt.Errorf("调用飞书发图片消息接口: %w", err)
	}
	if !resp.Success() {
		return SentMessage{}, fmt.Errorf("飞书发图片消息接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return sentMessageFromCreateResp(resp, chatID, chatType), nil
}

func (a *Adapter) ReplyImageMessage(ctx context.Context, msg Message, path string) (SentMessage, error) {
	if a.client == nil {
		return SentMessage{}, fmt.Errorf("飞书客户端未初始化")
	}
	if msg.MessageID == "" {
		return SentMessage{}, fmt.Errorf("飞书 message_id 为空")
	}
	imageKey, err := a.uploadReplyImage(ctx, path)
	if err != nil {
		return SentMessage{}, err
	}
	content, err := json.Marshal(map[string]string{"image_key": imageKey})
	if err != nil {
		return SentMessage{}, fmt.Errorf("编码飞书图片回复内容: %w", err)
	}
	replyInThread := replyInThreadForMessage(msg)
	resp, err := a.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(msg.MessageID).
		Body(&larkim.ReplyMessageReqBody{
			Content:       larkcore.StringPtr(string(content)),
			MsgType:       larkcore.StringPtr("image"),
			ReplyInThread: &replyInThread,
		}).
		Build())
	if err != nil {
		return SentMessage{}, fmt.Errorf("调用飞书回复图片接口: %w", err)
	}
	if !resp.Success() {
		return SentMessage{}, fmt.Errorf("飞书回复图片接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return sentMessageFromReplyResp(resp, msg), nil
}

func (a *Adapter) uploadReplyImage(ctx context.Context, path string) (string, error) {
	path, err := normalizeReplyImagePath(path)
	if err != nil {
		return "", err
	}
	if a.messages == nil {
		if a.client == nil {
			return "", fmt.Errorf("飞书客户端未初始化")
		}
		a.messages = larkMessageClient{client: a.client}
	}
	imageKey, err := a.messages.UploadImage(ctx, path)
	if err != nil {
		return "", fmt.Errorf("上传飞书图片: %w", err)
	}
	return imageKey, nil
}

func replyInThreadForMessage(msg Message) bool {
	if msg.ForceReplyInThread {
		return true
	}
	return msg.IsTopicThread()
}

func (a *Adapter) UpdateText(ctx context.Context, messageID string, text string) error {
	if a.client == nil {
		return fmt.Errorf("飞书客户端未初始化")
	}
	if strings.TrimSpace(messageID) == "" {
		return fmt.Errorf("飞书 message_id 为空")
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("编码飞书消息内容: %w", err)
	}
	resp, err := a.client.Im.V1.Message.Update(ctx, larkim.NewUpdateMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewUpdateMessageReqBodyBuilder().
			MsgType("text").
			Content(string(content)).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("调用飞书更新消息接口: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("飞书更新消息接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func sentMessageFromCreateResp(resp *larkim.CreateMessageResp, chatID string, chatType string) SentMessage {
	sent := SentMessage{ChatID: chatID, ChatType: chatType}
	if resp == nil || resp.Data == nil {
		return sent
	}
	sent.MessageID = value(resp.Data.MessageId)
	sent.ChatID = firstNonEmpty(value(resp.Data.ChatId), sent.ChatID)
	sent.ThreadID = value(resp.Data.ThreadId)
	sent.RootID = value(resp.Data.RootId)
	sent.ParentID = value(resp.Data.ParentId)
	return sent
}

func sentMessageFromReplyResp(resp *larkim.ReplyMessageResp, msg Message) SentMessage {
	sent := SentMessage{ChatID: msg.ChatID, ChatType: msg.ChatType}
	if resp == nil || resp.Data == nil {
		return sent
	}
	sent.MessageID = value(resp.Data.MessageId)
	sent.ChatID = firstNonEmpty(value(resp.Data.ChatId), sent.ChatID)
	sent.ThreadID = firstNonEmpty(value(resp.Data.ThreadId), msg.ThreadID)
	sent.RootID = value(resp.Data.RootId)
	sent.ParentID = value(resp.Data.ParentId)
	return sent
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
