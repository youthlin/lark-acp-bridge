package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkapplication "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
)

type larkApplicationClient struct {
	client *lark.Client
}

func (c larkApplicationClient) GetApplication(ctx context.Context) (applicationOwnerCandidates, error) {
	resp, err := c.client.Application.Application.Get(ctx, larkapplication.NewGetApplicationReqBuilder().
		AppId("me").
		Lang("zh_cn").
		UserIdType("open_id").
		Build())
	if err != nil {
		return applicationOwnerCandidates{}, fmt.Errorf("调用飞书获取应用信息接口: %w", err)
	}
	if !resp.Success() {
		return applicationOwnerCandidates{}, fmt.Errorf("飞书获取应用信息接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.App == nil {
		return applicationOwnerCandidates{}, nil
	}
	app := resp.Data.App
	out := applicationOwnerCandidates{
		CreatorID: value(app.CreatorId),
		AppName:   value(app.AppName),
	}
	if app.Owner != nil {
		out.OwnerID = value(app.Owner.OwnerId)
	}
	return out, nil
}

func (c larkApplicationClient) GetCollaborators(ctx context.Context) ([]applicationCollaborator, error) {
	resp, err := c.client.Application.ApplicationCollaborators.Get(ctx, larkapplication.NewGetApplicationCollaboratorsReqBuilder().
		AppId("me").
		UserIdType("open_id").
		Build())
	if err != nil {
		return nil, fmt.Errorf("调用飞书获取应用协作者接口: %w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("飞书获取应用协作者接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return nil, nil
	}
	out := make([]applicationCollaborator, 0, len(resp.Data.Collaborators))
	for _, item := range resp.Data.Collaborators {
		if item == nil {
			continue
		}
		out = append(out, applicationCollaborator{
			Type:   value(item.Type),
			UserID: value(item.UserId),
		})
	}
	return out, nil
}

func (c larkApplicationClient) GetBotInfo(ctx context.Context) (BotInfo, error) {
	resp, err := c.client.Get(ctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return BotInfo{}, fmt.Errorf("调用飞书获取机器人信息接口: %w", err)
	}
	if resp == nil {
		return BotInfo{}, fmt.Errorf("飞书获取机器人信息接口返回为空")
	}
	if resp.StatusCode != 200 {
		return BotInfo{}, fmt.Errorf("飞书获取机器人信息接口 HTTP 状态异常: %d", resp.StatusCode)
	}
	info, err := parseBotInfoResponse(resp.RawBody)
	if err != nil {
		return BotInfo{}, fmt.Errorf("解析飞书机器人信息接口响应: %w", err)
	}
	return info, nil
}

func parseBotInfoResponse(raw []byte) (BotInfo, error) {
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID      string `json:"open_id"`
			Name        string `json:"name"`
			DisplayName string `json:"display_name"`
			BotName     string `json:"bot_name"`
			AppName     string `json:"app_name"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return BotInfo{}, err
	}
	if result.Code != 0 {
		return BotInfo{}, fmt.Errorf("飞书获取机器人信息接口返回错误: code=%d msg=%s", result.Code, result.Msg)
	}
	return BotInfo{
		OpenID: strings.TrimSpace(result.Bot.OpenID),
		Name:   strings.TrimSpace(firstNonEmpty(result.Bot.Name, result.Bot.DisplayName, result.Bot.BotName, result.Bot.AppName)),
	}, nil
}
