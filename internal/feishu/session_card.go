package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
)

const sessionSelectionCardAction = "acp_session_selection"

type SessionSelection struct {
	BotID        string
	ChatID       string
	ThreadID     string
	RequesterID  string
	OperatorID   string
	ACPSessionID string
}

func (a *Adapter) SendSessionSelectionCard(ctx context.Context, msg Message, card SessionSelectionCard) error {
	if a.client == nil {
		return fmt.Errorf("飞书客户端未初始化")
	}
	cardResp, err := a.client.Cardkit.V1.Card.Create(ctx, larkcardkit.NewCreateCardReqBuilder().
		Body(larkcardkit.NewCreateCardReqBodyBuilder().
			Type("card_json").
			Data(newSessionSelectionCardJSON(card)).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("创建飞书会话选择卡片: %w", err)
	}
	if !cardResp.Success() {
		return fmt.Errorf("创建飞书会话选择卡片返回错误: code=%d msg=%s", cardResp.Code, cardResp.Msg)
	}
	cardID := ""
	if cardResp.Data != nil {
		cardID = normalizedCardID(cardResp.Data.CardId)
	}
	if cardID == "" {
		return fmt.Errorf("创建飞书会话选择卡片未返回 card_id")
	}
	if _, err := a.sendInteractiveCard(ctx, msg, cardID); err != nil {
		return fmt.Errorf("发送飞书会话选择卡片: %w", err)
	}
	return nil
}

func (a *Adapter) handleSessionSelectionAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return permissionCardToast("error", "无效的会话选择操作"), nil
	}
	action := event.Event.Action
	value := action.Value
	sessionID := strings.TrimSpace(action.Option)
	if sessionID == "" {
		sessionID = stringValue(value, "acp_session_id")
	}
	selection := SessionSelection{
		BotID:        stringValue(value, "bot_id"),
		ChatID:       stringValue(value, "chat_id"),
		ThreadID:     stringValue(value, "thread_id"),
		RequesterID:  stringValue(value, "requester_id"),
		ACPSessionID: sessionID,
	}
	if event.Event.Operator != nil {
		selection.OperatorID = strings.TrimSpace(event.Event.Operator.OpenID)
	}
	if selection.ChatID == "" || selection.ACPSessionID == "" {
		return permissionCardToast("error", "会话选择参数无效"), nil
	}
	handler, ok := a.handler.(SessionSelectionHandler)
	if !ok {
		return permissionCardToast("error", "会话恢复服务未初始化"), nil
	}
	display, err := handler.HandleSessionSelection(ctx, selection)
	if err != nil {
		return permissionCardToast("error", err.Error()), nil
	}
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: "会话已恢复"},
		Card: &callback.Card{
			Type: "raw",
			Data: completedSessionSelectionCard(display),
		},
	}, nil
}

func newSessionSelectionCardJSON(card SessionSelectionCard) string {
	options := make([]any, 0, len(card.Options))
	hasCurrent := false
	for _, option := range card.Options {
		value := strings.TrimSpace(option.ACPSessionID)
		if value == "" {
			continue
		}
		if value == strings.TrimSpace(card.CurrentACPSessionID) {
			hasCurrent = true
		}
		options = append(options, cardJSON{
			"text":  cardJSON{"tag": "plain_text", "content": sessionOptionDisplayName(option)},
			"value": value,
		})
	}
	selectElement := cardJSON{
		"tag":         "select_static",
		"element_id":  "session_select",
		"width":       "fill",
		"placeholder": cardJSON{"tag": "plain_text", "content": "请选择要恢复的会话"},
		"options":     options,
		"behaviors": []any{
			cardJSON{
				"type": "callback",
				"value": cardJSON{
					"action":       sessionSelectionCardAction,
					"bot_id":       card.BotID,
					"chat_id":      card.ChatID,
					"thread_id":    card.ThreadID,
					"requester_id": card.RequesterID,
				},
			},
		},
	}
	if hasCurrent {
		selectElement["initial_option"] = strings.TrimSpace(card.CurrentACPSessionID)
	}
	data, _ := json.Marshal(cardJSON{
		"schema": "2.0",
		"config": cardJSON{
			"update_multi": true,
			"width_mode":   "default",
			"summary":      cardJSON{"content": "选择当前聊天的历史 ACP 会话"},
		},
		"header": cardJSON{
			"title":    cardJSON{"tag": "plain_text", "content": "选择历史会话"},
			"subtitle": cardJSON{"tag": "plain_text", "content": "仅 bot owner 可恢复会话"},
			"template": "blue",
			"icon":     cardJSON{"tag": "standard_icon", "token": "history_colorful"},
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
						cardJSON{"tag": "markdown", "content": "**历史会话**"},
						cardJSON{"tag": "markdown", "content": fmt.Sprintf("显示最近 %d 个会话", len(options))},
					},
				},
				selectElement,
			},
		},
	})
	return string(data)
}

func completedSessionSelectionCard(title string) map[string]any {
	return cardJSON{
		"schema": "2.0",
		"config": cardJSON{
			"update_multi": true,
			"width_mode":   "default",
			"summary":      cardJSON{"content": "会话已恢复"},
		},
		"header": cardJSON{
			"title":    cardJSON{"tag": "plain_text", "content": "会话已恢复"},
			"template": "green",
			"icon":     cardJSON{"tag": "standard_icon", "token": "history_colorful"},
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
						cardJSON{"tag": "markdown", "content": "✅ **已恢复 " + sessionCardInline(title) + "**"},
					},
				},
			},
		},
	}
}

func sessionOptionDisplayName(option SessionOption) string {
	title := sessionCardInline(option.Title)
	cwd := sessionCardInline(option.Cwd)
	if cwd == "" || cwd == "未知" {
		return title
	}
	return title + " | " + cwd
}

func sessionCardInline(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "未知"
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "`", "'"), "\n", " ")
	runes := []rune(text)
	if len(runes) <= 60 {
		return text
	}
	return string(runes[:60]) + "..."
}
