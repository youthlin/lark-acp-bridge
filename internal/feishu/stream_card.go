package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const (
	streamCardProcessPanelID   = "panel_process"
	streamCardProcessElementID = "md_process"
	streamCardTextElementID    = "md_stream"
)

type cardJSON map[string]any

func newStreamCardJSON() string {
	data, _ := json.Marshal(cardJSON{
		"schema": "2.0",
		"config": cardJSON{
			"streaming_mode":   true,
			"wide_screen_mode": true,
			"width_mode":       "fill",
			"summary":          cardJSON{"content": ""},
			"streaming_config": cardJSON{
				"print_frequency_ms": cardJSON{"default": 70},
				"print_step":         cardJSON{"default": 2},
				"print_strategy":     "fast",
			},
		},
		"body": cardJSON{
			"elements": []any{
				cardJSON{"tag": "markdown", "content": "", "element_id": streamCardTextElementID},
			},
		},
	})
	return string(data)
}

func newStreamCardProcessPanelJSON() string {
	data, _ := json.Marshal([]any{
		cardJSON{
			"tag":              "collapsible_panel",
			"expanded":         false,
			"element_id":       streamCardProcessPanelID,
			"background_color": "grey",
			"header": cardJSON{
				"title": cardJSON{"tag": "plain_text", "content": "执行过程"},
			},
			"border":           cardJSON{"color": "grey", "corner_radius": "8px"},
			"vertical_spacing": "4px",
			"padding":          "8px 12px 8px 12px",
			"elements": []any{
				cardJSON{"tag": "markdown", "content": "", "element_id": streamCardProcessElementID},
			},
		},
	})
	return string(data)
}

type sdkStreamCard struct {
	adapter *Adapter
	cardID  string

	mu             sync.Mutex
	sequence       int
	closed         bool
	processCreated bool
}

func (a *Adapter) StartStreamCard(ctx context.Context, msg Message) (StreamCard, error) {
	if a.client == nil {
		return nil, fmt.Errorf("飞书客户端未初始化")
	}
	cardResp, err := a.client.Cardkit.V1.Card.Create(ctx, larkcardkit.NewCreateCardReqBuilder().
		Body(larkcardkit.NewCreateCardReqBodyBuilder().
			Type("card_json").
			Data(newStreamCardJSON()).
			Build()).
		Build())
	if err != nil {
		return nil, fmt.Errorf("创建飞书流式卡片: %w", err)
	}
	if !cardResp.Success() {
		return nil, fmt.Errorf("创建飞书流式卡片返回错误: code=%d msg=%s", cardResp.Code, cardResp.Msg)
	}
	if cardResp.Data == nil || cardResp.Data.CardId == nil || *cardResp.Data.CardId == "" {
		return nil, fmt.Errorf("创建飞书流式卡片未返回 card_id")
	}
	cardID := *cardResp.Data.CardId
	if err := a.sendInteractiveCard(ctx, msg, cardID); err != nil {
		return nil, err
	}
	return &sdkStreamCard{adapter: a, cardID: cardID}, nil
}

func (a *Adapter) sendInteractiveCard(ctx context.Context, msg Message, cardID string) error {
	content, err := json.Marshal(map[string]any{
		"type": "card",
		"data": map[string]string{"card_id": cardID},
	})
	if err != nil {
		return fmt.Errorf("编码飞书卡片消息内容: %w", err)
	}
	if msg.IsPrivateChat() && strings.TrimSpace(msg.MessageID) == "" {
		if msg.ChatID == "" {
			return fmt.Errorf("飞书 chat_id 为空")
		}
		resp, err := a.client.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
			ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
			Body(larkim.NewCreateMessageReqBodyBuilder().
				ReceiveId(msg.ChatID).
				MsgType("interactive").
				Content(string(content)).
				Build()).
			Build())
		if err != nil {
			return fmt.Errorf("发送飞书流式卡片消息: %w", err)
		}
		if !resp.Success() {
			return fmt.Errorf("发送飞书流式卡片消息返回错误: code=%d msg=%s", resp.Code, resp.Msg)
		}
		return nil
	}
	if msg.MessageID == "" {
		return fmt.Errorf("飞书 message_id 为空")
	}
	replyInThread := replyInThreadForMessage(msg)
	resp, err := a.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(msg.MessageID).
		Body(&larkim.ReplyMessageReqBody{
			Content:       larkcore.StringPtr(string(content)),
			MsgType:       larkcore.StringPtr("interactive"),
			ReplyInThread: &replyInThread,
		}).
		Build())
	if err != nil {
		return fmt.Errorf("回复飞书流式卡片消息: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("回复飞书流式卡片消息返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *sdkStreamCard) UpdateProcess(ctx context.Context, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if !c.processCreated {
		if err := c.createProcessPanelLocked(ctx); err != nil {
			return err
		}
		c.processCreated = true
	}
	return c.updateElementLocked(ctx, streamCardProcessElementID, text)
}

func (c *sdkStreamCard) UpdateText(ctx context.Context, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	return c.updateElementLocked(ctx, streamCardTextElementID, text)
}

func (c *sdkStreamCard) createProcessPanelLocked(ctx context.Context) error {
	c.sequence++
	seq := c.sequence
	resp, err := c.adapter.client.Cardkit.V1.CardElement.Create(ctx, larkcardkit.NewCreateCardElementReqBuilder().
		CardId(c.cardID).
		Body(larkcardkit.NewCreateCardElementReqBodyBuilder().
			Type(larkcardkit.TypeInsertAfter).
			TargetElementId(streamCardTextElementID).
			Elements(newStreamCardProcessPanelJSON()).
			Sequence(seq).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("创建飞书流式卡片过程组件: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("创建飞书流式卡片过程组件返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *sdkStreamCard) updateElementLocked(ctx context.Context, elementID string, text string) error {
	c.sequence++
	seq := c.sequence
	resp, err := c.adapter.client.Cardkit.V1.CardElement.Content(ctx, larkcardkit.NewContentCardElementReqBuilder().
		CardId(c.cardID).
		ElementId(elementID).
		Body(larkcardkit.NewContentCardElementReqBodyBuilder().
			Content(text).
			Sequence(seq).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("更新飞书流式卡片组件: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("更新飞书流式卡片组件返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *sdkStreamCard) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.sequence++
	seq := c.sequence

	settings, _ := json.Marshal(cardJSON{"config": cardJSON{"streaming_mode": false}})
	resp, err := c.adapter.client.Cardkit.V1.Card.Settings(ctx, larkcardkit.NewSettingsCardReqBuilder().
		CardId(c.cardID).
		Body(larkcardkit.NewSettingsCardReqBodyBuilder().
			Settings(string(settings)).
			Sequence(seq).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("关闭飞书流式卡片: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("关闭飞书流式卡片返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}
