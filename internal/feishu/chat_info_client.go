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
	resp, err := retryFeishuAPI(ctx, defaultFeishuRetryOptions(), func(ctx context.Context) (*larkim.GetChatResp, error) {
		return c.client.Im.Chat.Get(ctx, req)
	}, func(resp *larkim.GetChatResp) bool {
		return resp != nil && shouldRetryFeishuAPIResp(resp.ApiResp)
	})
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

func (a *Adapter) CreateDriveCommentTraceChat(ctx context.Context, request CreateDriveCommentTraceChatRequest) (CreatedChat, error) {
	if a.client == nil {
		return CreatedChat{}, fmt.Errorf("飞书客户端未初始化")
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		return CreatedChat{}, fmt.Errorf("飞书群名称为空")
	}
	ownerID := strings.TrimSpace(request.OwnerOpenID)
	if ownerID == "" {
		return CreatedChat{}, fmt.Errorf("飞书群主 open_id 为空")
	}
	userIDs := normalizeOpenIDList(request.UserOpenIDs)
	if len(userIDs) == 0 {
		userIDs = []string{ownerID}
	}
	body := larkim.NewCreateChatReqBodyBuilder().
		Name(name).
		OwnerId(ownerID).
		UserIdList(userIDs).
		GroupMessageType(larkim.CreateChatGroupMessageTypeThread).
		ChatMode("group").
		ChatType("private").
		Build()
	resp, err := a.client.Im.V1.Chat.Create(ctx, larkim.NewCreateChatReqBuilder().
		UserIdType(larkim.CreateChatUserIDTypeOpenId).
		SetBotManager(true).
		Body(body).
		Build())
	if err != nil {
		return CreatedChat{}, fmt.Errorf("调用飞书创建话题群接口: %w", err)
	}
	if !resp.Success() {
		return CreatedChat{}, fmt.Errorf("飞书创建话题群接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return CreatedChat{}, fmt.Errorf("飞书创建话题群接口未返回数据")
	}
	return CreatedChat{
		ChatID:           value(resp.Data.ChatId),
		ChatType:         value(resp.Data.ChatType),
		GroupMessageType: value(resp.Data.GroupMessageType),
	}, nil
}

func normalizeOpenIDList(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
