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
	return a.CreateChat(ctx, CreateChatRequest{
		Name:             request.Name,
		Mode:             "group",
		ChatType:         "private",
		GroupMessageType: "thread",
		OwnerOpenID:      request.OwnerOpenID,
		UserOpenIDs:      request.UserOpenIDs,
		SetBotManager:    true,
	})
}

func (a *Adapter) CreateChat(ctx context.Context, request CreateChatRequest) (CreatedChat, error) {
	if a.client == nil {
		return CreatedChat{}, fmt.Errorf("飞书客户端未初始化")
	}
	ownerID := strings.TrimSpace(request.OwnerOpenID)
	if ownerID == "" {
		return CreatedChat{}, fmt.Errorf("飞书群主 open_id 为空")
	}
	userIDs := normalizeOpenIDList(request.UserOpenIDs)
	if len(userIDs) == 0 {
		userIDs = []string{ownerID}
	}
	body := buildCreateChatReqBody(request, ownerID, userIDs)
	resp, err := a.client.Im.V1.Chat.Create(ctx, larkim.NewCreateChatReqBuilder().
		UserIdType(larkim.CreateChatUserIDTypeOpenId).
		SetBotManager(request.SetBotManager).
		Body(body).
		Build())
	if err != nil {
		return CreatedChat{}, fmt.Errorf("调用飞书创建群接口: %w", err)
	}
	if !resp.Success() {
		return CreatedChat{}, fmt.Errorf("飞书创建群接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return CreatedChat{}, fmt.Errorf("飞书创建群接口未返回数据")
	}
	return CreatedChat{
		ChatID:           value(resp.Data.ChatId),
		Name:             value(resp.Data.Name),
		OwnerOpenID:      value(resp.Data.OwnerId),
		ChatMode:         value(resp.Data.ChatMode),
		ChatType:         value(resp.Data.ChatType),
		GroupMessageType: value(resp.Data.GroupMessageType),
	}, nil
}

func buildCreateChatReqBody(request CreateChatRequest, ownerID string, userIDs []string) *larkim.CreateChatReqBody {
	name := strings.TrimSpace(request.Name)
	chatType := strings.TrimSpace(request.ChatType)
	if chatType == "" {
		chatType = "private"
	}
	body := larkim.NewCreateChatReqBodyBuilder().
		OwnerId(ownerID).
		UserIdList(userIDs).
		ChatMode(normalizeCreateChatMode(request.Mode)).
		ChatType(chatType)
	if name != "" {
		body.Name(name)
	}
	switch normalizeGroupMessageType(request.GroupMessageType) {
	case "thread":
		body.GroupMessageType(larkim.CreateChatGroupMessageTypeThread)
	case "chat":
		body.GroupMessageType(larkim.CreateChatGroupMessageTypeChat)
	}
	return body.Build()
}

func (a *Adapter) AddChatMembers(ctx context.Context, request AddChatMembersRequest) (AddChatMembersResult, error) {
	if a.client == nil {
		return AddChatMembersResult{}, fmt.Errorf("飞书客户端未初始化")
	}
	chatID := strings.TrimSpace(request.ChatID)
	if chatID == "" {
		return AddChatMembersResult{}, fmt.Errorf("飞书 chat_id 为空")
	}
	userIDs := normalizeOpenIDList(request.UserOpenIDs)
	if len(userIDs) == 0 {
		return AddChatMembersResult{}, nil
	}
	resp, err := a.client.Im.V1.ChatMembers.Create(ctx, larkim.NewCreateChatMembersReqBuilder().
		ChatId(chatID).
		MemberIdType("open_id").
		SucceedType(larkim.InviteMemberSucceedType1).
		Body(larkim.NewCreateChatMembersReqBodyBuilder().
			IdList(userIDs).
			Build()).
		Build())
	if err != nil {
		return AddChatMembersResult{}, fmt.Errorf("调用飞书拉人入群接口: %w", err)
	}
	if !resp.Success() {
		return AddChatMembersResult{}, fmt.Errorf("飞书拉人入群接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	result := AddChatMembersResult{}
	if resp.Data != nil {
		result.InvalidOpenIDs = normalizeOpenIDList(resp.Data.InvalidIdList)
		result.NotExistedOpenIDs = normalizeOpenIDList(resp.Data.NotExistedIdList)
		result.PendingApprovalOpenIDs = normalizeOpenIDList(resp.Data.PendingApprovalIdList)
	}
	return result, nil
}

func normalizeCreateChatMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "topic":
		return "topic"
	default:
		return "group"
	}
}

func normalizeGroupMessageType(groupMessageType string) string {
	switch strings.ToLower(strings.TrimSpace(groupMessageType)) {
	case "thread":
		return "thread"
	case "chat":
		return "chat"
	default:
		return ""
	}
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
