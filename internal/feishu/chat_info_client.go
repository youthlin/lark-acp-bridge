package feishu

import (
	"context"
	"fmt"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type larkChatInfoClient struct {
	client *lark.Client
}

func (c larkChatInfoClient) GetChatInfo(ctx context.Context, chatID string) (chatInfo, error) {
	if c.client == nil {
		return chatInfo{}, fmt.Errorf("飞书客户端未初始化")
	}
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return chatInfo{}, fmt.Errorf("飞书 chat_id 为空")
	}
	req := larkim.NewGetChatReqBuilder().
		ChatId(chatID).
		UserIdType(larkim.GetChatUserIDTypeOpenId).
		Build()
	resp, err := c.client.Im.Chat.Get(ctx, req)
	if err != nil {
		return chatInfo{}, fmt.Errorf("调用飞书获取群信息接口: %w", err)
	}
	if !resp.Success() {
		return chatInfo{}, fmt.Errorf("飞书获取群信息接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return chatInfo{}, fmt.Errorf("飞书获取群信息接口未返回数据")
	}
	return chatInfo{
		Name:             value(resp.Data.Name),
		ChatMode:         value(resp.Data.ChatMode),
		ChatType:         value(resp.Data.ChatType),
		GroupMessageType: value(resp.Data.GroupMessageType),
	}, nil
}
