package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
)

const modeSelectionCardAction = "acp_mode_selection"

type ModeSelection struct {
	BotID        string
	ChatID       string
	ThreadID     string
	ACPSessionID string
	RequesterID  string
	OperatorID   string
	Mode         string
}

func (a *Adapter) SendModeSelectionCard(ctx context.Context, msg Message, card ModeSelectionCard) error {
	if a.client == nil {
		return fmt.Errorf("飞书客户端未初始化")
	}
	cardResp, err := a.client.Cardkit.V1.Card.Create(ctx, larkcardkit.NewCreateCardReqBuilder().
		Body(larkcardkit.NewCreateCardReqBodyBuilder().
			Type("card_json").
			Data(newModeSelectionCardJSON(card)).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("创建飞书模式选择卡片: %w", err)
	}
	if !cardResp.Success() {
		return fmt.Errorf("创建飞书模式选择卡片返回错误: code=%d msg=%s", cardResp.Code, cardResp.Msg)
	}
	cardID := ""
	if cardResp.Data != nil {
		cardID = normalizedCardID(cardResp.Data.CardId)
	}
	if cardID == "" {
		return fmt.Errorf("创建飞书模式选择卡片未返回 card_id")
	}
	if _, err := a.sendInteractiveCard(ctx, msg, cardID); err != nil {
		return fmt.Errorf("发送飞书模式选择卡片: %w", err)
	}
	return nil
}

func (a *Adapter) handleModeSelectionAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return permissionCardToast("error", "无效的模式选择操作"), nil
	}
	action := event.Event.Action
	value := action.Value
	mode := strings.TrimSpace(action.Option)
	if mode == "" {
		mode = stringValue(value, "mode")
	}
	selection := ModeSelection{
		BotID:        stringValue(value, "bot_id"),
		ChatID:       stringValue(value, "chat_id"),
		ThreadID:     stringValue(value, "thread_id"),
		ACPSessionID: stringValue(value, "acp_session_id"),
		RequesterID:  stringValue(value, "requester_id"),
		Mode:         mode,
	}
	if event.Event.Operator != nil {
		selection.OperatorID = strings.TrimSpace(event.Event.Operator.OpenID)
	}
	if selection.Mode == "" || selection.ChatID == "" || selection.ACPSessionID == "" {
		return permissionCardToast("error", "模式选择参数无效"), nil
	}
	handler, ok := a.handler.(ModeSelectionHandler)
	if !ok {
		return permissionCardToast("error", "模式设置服务未初始化"), nil
	}
	display, err := handler.HandleModeSelection(ctx, selection)
	if err != nil {
		return permissionCardToast("error", err.Error()), nil
	}
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: "模式设置成功"},
		Card: &callback.Card{
			Type: "raw",
			Data: completedModeSelectionCard(display),
		},
	}, nil
}

func newModeSelectionCardJSON(card ModeSelectionCard) string {
	options := make([]any, 0, len(card.Options))
	hasCurrentMode := false
	for _, option := range card.Options {
		value := strings.TrimSpace(option.Value)
		if value == "" {
			continue
		}
		if value == strings.TrimSpace(card.CurrentMode) {
			hasCurrentMode = true
		}
		options = append(options, cardJSON{
			"text":  cardJSON{"tag": "plain_text", "content": modeOptionDisplayName(option)},
			"value": value,
		})
	}
	selectElement := cardJSON{
		"tag":         "select_static",
		"element_id":  "mode_select",
		"width":       "fill",
		"placeholder": cardJSON{"tag": "plain_text", "content": "请选择要设置的模式"},
		"options":     options,
		"behaviors": []any{
			cardJSON{
				"type": "callback",
				"value": cardJSON{
					"action":         modeSelectionCardAction,
					"bot_id":         card.BotID,
					"chat_id":        card.ChatID,
					"thread_id":      card.ThreadID,
					"acp_session_id": card.ACPSessionID,
					"requester_id":   card.RequesterID,
				},
			},
		},
	}
	if hasCurrentMode {
		selectElement["initial_option"] = strings.TrimSpace(card.CurrentMode)
	}
	data, _ := json.Marshal(cardJSON{
		"schema": "2.0",
		"config": cardJSON{
			"update_multi": true,
			"width_mode":   "default",
			"summary":      cardJSON{"content": "选择当前 ACP 会话模式"},
		},
		"header": cardJSON{
			"title":    cardJSON{"tag": "plain_text", "content": "选择会话模式"},
			"subtitle": cardJSON{"tag": "plain_text", "content": "选择后立即对当前 ACP 会话生效"},
			"template": "blue",
			"icon":     cardJSON{"tag": "standard_icon", "token": "notice_colorful"},
		},
		"body": cardJSON{
			"direction":        "vertical",
			"padding":          "12px 12px 20px 12px",
			"vertical_spacing": "12px",
			"elements": []any{
				cardJSON{
					"tag":              "interactive_container",
					"width":            "fill",
					"has_border":       true,
					"border_color":     "blue-100",
					"corner_radius":    "8px",
					"background_style": "blue-50",
					"padding":          "12px 12px 12px 12px",
					"vertical_spacing": "4px",
					"disabled":         true,
					"elements": []any{
						cardJSON{"tag": "markdown", "content": "**当前模式**"},
						cardJSON{"tag": "markdown", "content": modeCardInline(card.CurrentMode)},
					},
				},
				selectElement,
			},
		},
	})
	return string(data)
}

func completedModeSelectionCard(mode string) map[string]any {
	return cardJSON{
		"schema": "2.0",
		"config": cardJSON{
			"update_multi": true,
			"width_mode":   "default",
			"summary":      cardJSON{"content": "模式设置成功"},
		},
		"header": cardJSON{
			"title":    cardJSON{"tag": "plain_text", "content": "模式设置完成"},
			"template": "green",
			"icon":     cardJSON{"tag": "standard_icon", "token": "notice_colorful"},
		},
		"body": cardJSON{
			"direction": "vertical",
			"padding":   "12px 12px 20px 12px",
			"elements": []any{
				cardJSON{
					"tag":              "interactive_container",
					"width":            "fill",
					"has_border":       true,
					"border_color":     "green-100",
					"corner_radius":    "8px",
					"background_style": "green-50",
					"padding":          "12px 12px 12px 12px",
					"disabled":         true,
					"elements": []any{
						cardJSON{"tag": "markdown", "content": "✅ **已设置为 " + modeCardInline(mode) + "**"},
					},
				},
			},
		},
	}
}

func modeOptionDisplayName(option ModeOption) string {
	name := strings.TrimSpace(option.Name)
	value := strings.TrimSpace(option.Value)
	if name == "" || name == value {
		return value
	}
	return name + "（" + value + "）"
}

func modeCardInline(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "未知"
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "`", "'"), "\n", " ")
}
