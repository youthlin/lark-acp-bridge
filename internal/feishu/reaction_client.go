package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// processingReactionEmojis 收到消息时随机选一个表情加在消息上
// 表情列表: https://open.larkoffice.com/document/server-docs/im-v1/message-reaction/emojis-introduce
var processingReactionEmojis = []string{
	"OK",         // OK
	"Get",        // 收到
	"WINK",       // Wink 眨眼
	"WITTY",      // 灵光一闪
	"DIZZY",      // 头晕
	"MeMeMe",     // 举手, 我我我
	"THINKING",   // 思考
	"Typing",     // 打字
	"OnIt",       // 在看了
	"OneSecond",  // 再等一下
	"GoGoGo",     // 开始干
	"SaluteFace", // 敬礼
}

type larkReactionClient struct {
	client *lark.Client
}

func (c larkReactionClient) AddReaction(ctx context.Context, messageID string) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("飞书客户端未初始化")
	}
	if messageID == "" {
		return "", fmt.Errorf("飞书 message_id 为空")
	}
	emojiType := randomProcessingReactionEmoji()
	req := larkim.NewCreateMessageReactionReqBuilder().
		MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(emojiType).Build()).
			Build()).
		Build()
	resp, err := c.client.Im.V1.MessageReaction.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("调用飞书添加 reaction 接口: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("飞书添加 reaction 接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.ReactionId == nil || strings.TrimSpace(*resp.Data.ReactionId) == "" {
		return "", fmt.Errorf("飞书添加 reaction 接口未返回 reaction_id")
	}
	return strings.TrimSpace(*resp.Data.ReactionId), nil
}

func randomProcessingReactionEmoji() string {
	if len(processingReactionEmojis) == 0 {
		return "OnIt"
	}
	return processingReactionEmojis[rand.Intn(len(processingReactionEmojis))]
}

func (c larkReactionClient) DeleteReaction(ctx context.Context, messageID string, reactionID string) error {
	if c.client == nil {
		return fmt.Errorf("飞书客户端未初始化")
	}
	if messageID == "" {
		return fmt.Errorf("飞书 message_id 为空")
	}
	if reactionID == "" {
		return fmt.Errorf("飞书 reaction_id 为空")
	}
	req := larkim.NewDeleteMessageReactionReqBuilder().
		MessageId(messageID).
		ReactionId(reactionID).
		Build()
	resp, err := retryFeishuAPI(ctx, defaultFeishuRetryOptions(), func(ctx context.Context) (*larkim.DeleteMessageReactionResp, error) {
		return c.client.Im.V1.MessageReaction.Delete(ctx, req)
	}, func(resp *larkim.DeleteMessageReactionResp) bool {
		return resp != nil && shouldRetryFeishuAPIResp(resp.ApiResp)
	})
	if err != nil {
		return fmt.Errorf("调用飞书删除 reaction 接口: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("飞书删除 reaction 接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (a *Adapter) addProcessingReaction(ctx context.Context, msg Message) string {
	if a.reaction == nil || strings.TrimSpace(msg.MessageID) == "" {
		return ""
	}
	reactionID, err := a.reaction.AddReaction(ctx, msg.MessageID)
	if err != nil {
		slog.WarnContext(ctx, "添加飞书处理中 reaction 失败", "错误", err)
		return ""
	}
	slog.InfoContext(ctx, "已添加飞书处理中 reaction", "reaction_id", reactionID)
	return reactionID
}

func (a *Adapter) StartProcessingReaction(ctx context.Context, msg Message) func() {
	reactionID := a.addProcessingReaction(ctx, msg)
	if strings.TrimSpace(reactionID) == "" {
		return func() {}
	}
	return func() {
		a.deleteProcessingReaction(ctx, msg, reactionID)
	}
}

func (a *Adapter) deleteProcessingReaction(ctx context.Context, msg Message, reactionID string) {
	if a.reaction == nil || strings.TrimSpace(reactionID) == "" {
		return
	}
	if err := a.reaction.DeleteReaction(ctx, msg.MessageID, reactionID); err != nil {
		slog.WarnContext(ctx, "删除飞书处理中 reaction 失败", "reaction_id", reactionID, "错误", err)
		return
	}
	slog.InfoContext(ctx, "已删除飞书处理中 reaction", "reaction_id", reactionID)
}
