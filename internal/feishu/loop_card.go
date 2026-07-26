package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
)

const (
	loopStatusCardAction        = "acp_loop_cancel"
	loopStatusCardTextElementID = "md_loop_status"
	loopStatusCardActionID      = "loop_cancel_action"
)

type sdkLoopStatusCard struct {
	adapter *Adapter
	cardID  string
	message SentMessage
	request LoopStatusCardRequest

	mu       sync.Mutex
	sequence int
	text     string
	finished bool
}

func (a *Adapter) SendLoopStatusCard(ctx context.Context, msg Message, request LoopStatusCardRequest) (LoopStatusCard, error) {
	if a.client == nil {
		return nil, fmt.Errorf("飞书客户端未初始化")
	}
	request.Text = strings.TrimSpace(request.Text)
	cardResp, err := a.client.Cardkit.V1.Card.Create(ctx, larkcardkit.NewCreateCardReqBuilder().
		Body(larkcardkit.NewCreateCardReqBodyBuilder().
			Type("card_json").
			Data(newLoopStatusCardJSON(request, false)).
			Build()).
		Build())
	if err != nil {
		return nil, fmt.Errorf("创建飞书 loop 状态卡片: %w", err)
	}
	if !cardResp.Success() {
		return nil, fmt.Errorf("创建飞书 loop 状态卡片返回错误: code=%d msg=%s", cardResp.Code, cardResp.Msg)
	}
	if cardResp.Data == nil || cardResp.Data.CardId == nil || strings.TrimSpace(*cardResp.Data.CardId) == "" {
		return nil, fmt.Errorf("创建飞书 loop 状态卡片未返回 card_id")
	}
	cardID := strings.TrimSpace(*cardResp.Data.CardId)
	sent, err := a.sendInteractiveCard(ctx, msg, cardID)
	if err != nil {
		return nil, fmt.Errorf("发送飞书 loop 状态卡片: %w", err)
	}
	return &sdkLoopStatusCard{
		adapter: a,
		cardID:  cardID,
		message: sent,
		request: request,
		text:    request.Text,
	}, nil
}

func (c *sdkLoopStatusCard) Message() SentMessage {
	if c == nil {
		return SentMessage{}
	}
	return c.message
}

func (c *sdkLoopStatusCard) Update(ctx context.Context, text string) error {
	if c == nil || c.adapter == nil || c.adapter.client == nil {
		return fmt.Errorf("飞书 loop 状态卡片未初始化")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return nil
	}
	c.text = strings.TrimSpace(text)
	c.request.Text = c.text
	return c.patchTextLocked(ctx)
}

func (c *sdkLoopStatusCard) Finish(ctx context.Context, text string) error {
	if c == nil || c.adapter == nil || c.adapter.client == nil {
		return fmt.Errorf("飞书 loop 状态卡片未初始化")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return nil
	}
	c.text = strings.TrimSpace(text)
	c.request.Text = c.text
	if err := c.patchTextLocked(ctx); err != nil {
		return err
	}
	c.finished = true
	// 按钮移除是收尾展示优化；状态文本已经更新成功时，不让按钮移除失败反向污染 loop 结果。
	_ = c.deleteActionLocked(ctx)
	return nil
}

func (c *sdkLoopStatusCard) patchTextLocked(ctx context.Context) error {
	seq := c.nextSequenceLocked()
	partial := loopStatusCardTextPatchJSON(c.text)
	resp, err := c.adapter.client.Cardkit.V1.CardElement.Patch(ctx, larkcardkit.NewPatchCardElementReqBuilder().
		CardId(c.cardID).
		ElementId(loopStatusCardTextElementID).
		Body(larkcardkit.NewPatchCardElementReqBodyBuilder().
			PartialElement(partial).
			Sequence(seq).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("更新飞书 loop 状态卡片文本: %w", err)
	}
	if !resp.Success() {
		logCardKitFailure(ctx, "CardElement.Patch", c.cardID, loopStatusCardTextElementID, seq, cardJSON{
			"partial_element": partial,
		}, resp.ApiResp, resp.Code, resp.Msg)
		return fmt.Errorf("更新飞书 loop 状态卡片文本返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *sdkLoopStatusCard) deleteActionLocked(ctx context.Context) error {
	seq := c.nextSequenceLocked()
	resp, err := c.adapter.client.Cardkit.V1.CardElement.Delete(ctx, larkcardkit.NewDeleteCardElementReqBuilder().
		CardId(c.cardID).
		ElementId(loopStatusCardActionID).
		Body(larkcardkit.NewDeleteCardElementReqBodyBuilder().
			Sequence(seq).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("移除飞书 loop 状态卡片按钮: %w", err)
	}
	if !resp.Success() {
		logCardKitFailure(ctx, "CardElement.Delete", c.cardID, loopStatusCardActionID, seq, nil, resp.ApiResp, resp.Code, resp.Msg)
		return fmt.Errorf("移除飞书 loop 状态卡片按钮返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *sdkLoopStatusCard) nextSequenceLocked() int {
	c.sequence++
	return c.sequence
}

func (a *Adapter) handleLoopCancelAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return permissionCardToast("error", "无效的 loop 操作"), nil
	}
	value := event.Event.Action.Value
	cancel := LoopCancel{
		BotID:        stringValue(value, "bot_id"),
		ChatID:       stringValue(value, "chat_id"),
		ThreadID:     stringValue(value, "thread_id"),
		ACPSessionID: stringValue(value, "acp_session_id"),
		OperatorID:   cardOperatorOpenID(event),
	}
	if cancel.ChatID == "" || cancel.ACPSessionID == "" {
		return permissionCardToast("error", "loop 操作参数无效"), nil
	}
	handler, ok := a.handler.(LoopCancelHandler)
	if !ok {
		return permissionCardToast("error", "loop 服务未初始化"), nil
	}
	text, err := handler.HandleLoopCancel(ctx, cancel)
	if err != nil {
		return permissionCardToast("warning", err.Error()), nil
	}
	if strings.TrimSpace(text) == "" {
		text = "loop 已取消。"
	}
	request := LoopStatusCardRequest{
		BotID:        cancel.BotID,
		ChatID:       cancel.ChatID,
		ThreadID:     cancel.ThreadID,
		ACPSessionID: cancel.ACPSessionID,
		Text:         text,
	}
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: "已取消 loop"},
		Card: &callback.Card{
			Type: "raw",
			Data: newLoopStatusCardData(request, true),
		},
	}, nil
}

func newLoopStatusCardJSON(request LoopStatusCardRequest, finished bool) string {
	data, _ := json.Marshal(newLoopStatusCardData(request, finished))
	return string(data)
}

func loopStatusCardTextPatchJSON(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "loop 状态更新中。"
	}
	data, _ := json.Marshal(cardJSON{"content": text})
	return string(data)
}

func newLoopStatusCardData(request LoopStatusCardRequest, finished bool) map[string]any {
	text := strings.TrimSpace(request.Text)
	if text == "" {
		text = "loop 状态更新中。"
	}
	elements := []any{
		cardJSON{
			"tag":              "interactive_container",
			"width":            "fill",
			"has_border":       true,
			"border_color":     "blue-100",
			"corner_radius":    "8px",
			"background_style": "blue-50",
			"padding":          "12px 12px 12px 12px",
			"elements": []any{
				cardJSON{"tag": "markdown", "content": text, "element_id": loopStatusCardTextElementID},
			},
		},
	}
	if !finished {
		elements = append(elements, loopStatusCardActions(request))
	}
	template := "blue"
	title := "Loop 运行中"
	subtitle := "自动循环任务"
	if finished {
		template = "green"
		title = "Loop 已结束"
		subtitle = "按钮已移除"
	}
	return cardJSON{
		"schema": "2.0",
		"config": cardJSON{
			"update_multi":     true,
			"wide_screen_mode": true,
			"width_mode":       "fill",
			"summary":          cardJSON{"content": title},
		},
		"header": cardJSON{
			"title":    cardJSON{"tag": "plain_text", "content": title},
			"subtitle": cardJSON{"tag": "plain_text", "content": subtitle},
			"template": template,
			"icon":     cardJSON{"tag": "standard_icon", "token": "time_colorful"},
		},
		"body": cardJSON{
			"direction":        "vertical",
			"padding":          "12px 12px 16px 12px",
			"vertical_spacing": "12px",
			"elements":         elements,
		},
	}
}

func loopStatusCardActions(request LoopStatusCardRequest) any {
	return cardJSON{
		"tag":                "column_set",
		"element_id":         loopStatusCardActionID,
		"flex_mode":          "flow",
		"horizontal_spacing": "8px",
		"columns": []any{
			cardJSON{
				"tag":   "column",
				"width": "auto",
				"elements": []any{
					cardJSON{
						"tag":   "button",
						"text":  cardJSON{"tag": "plain_text", "content": "取消 loop"},
						"type":  "danger",
						"width": "fill",
						"behaviors": []any{
							cardJSON{
								"type": "callback",
								"value": cardJSON{
									"action":         loopStatusCardAction,
									"bot_id":         request.BotID,
									"chat_id":        request.ChatID,
									"thread_id":      request.ThreadID,
									"acp_session_id": request.ACPSessionID,
								},
							},
						},
					},
				},
			},
		},
	}
}
