package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

const permissionCardAction = "acp_permission"

type permissionCardWaiter struct {
	once chan acp.PermissionOutcome
}

func newPermissionCardWaiter() *permissionCardWaiter {
	return &permissionCardWaiter{once: make(chan acp.PermissionOutcome, 1)}
}

type permissionCardRegistry struct {
	mu      sync.Mutex
	waiters map[string]*permissionCardWaiter
}

func newPermissionCardRegistry() *permissionCardRegistry {
	return &permissionCardRegistry{waiters: make(map[string]*permissionCardWaiter)}
}

func (r *permissionCardRegistry) add(id string, waiter *permissionCardWaiter) {
	if r == nil || strings.TrimSpace(id) == "" || waiter == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waiters[id] = waiter
}

func (r *permissionCardRegistry) take(id string) (*permissionCardWaiter, bool) {
	if r == nil || strings.TrimSpace(id) == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	waiter, ok := r.waiters[id]
	if ok {
		delete(r.waiters, id)
	}
	return waiter, ok
}

func (r *permissionCardRegistry) remove(id string) {
	if r == nil || strings.TrimSpace(id) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.waiters, id)
}

func (a *Adapter) RequestPermission(ctx context.Context, msg Message, req acp.PermissionRequest) (acp.PermissionOutcome, error) {
	if a.client == nil {
		return acp.PermissionOutcome{}, fmt.Errorf("飞书客户端未初始化")
	}
	requestID := permissionRequestID(req)
	if requestID == "" {
		return acp.PermissionOutcome{Outcome: "cancelled"}, nil
	}
	cardResp, err := a.client.Cardkit.V1.Card.Create(ctx, larkcardkit.NewCreateCardReqBuilder().
		Body(larkcardkit.NewCreateCardReqBodyBuilder().
			Type("card_json").
			Data(newPermissionCardJSON(requestID, req, "")).
			Build()).
		Build())
	if err != nil {
		return acp.PermissionOutcome{}, fmt.Errorf("创建飞书权限卡片: %w", err)
	}
	if !cardResp.Success() {
		return acp.PermissionOutcome{}, fmt.Errorf("创建飞书权限卡片返回错误: code=%d msg=%s", cardResp.Code, cardResp.Msg)
	}
	if cardResp.Data == nil || cardResp.Data.CardId == nil || *cardResp.Data.CardId == "" {
		return acp.PermissionOutcome{}, fmt.Errorf("创建飞书权限卡片未返回 card_id")
	}
	waiter := newPermissionCardWaiter()
	a.permissionCards.add(requestID, waiter)
	defer a.permissionCards.remove(requestID)
	if err := a.sendInteractiveCard(ctx, msg, *cardResp.Data.CardId); err != nil {
		return acp.PermissionOutcome{}, err
	}
	select {
	case <-ctx.Done():
		return acp.PermissionOutcome{Outcome: "cancelled"}, nil
	case outcome := <-waiter.once:
		return outcome, nil
	}
}

func (a *Adapter) handleCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return permissionCardToast("error", "无效的卡片操作"), nil
	}
	value := event.Event.Action.Value
	if stringValue(value, "action") != permissionCardAction {
		return permissionCardToast("error", "未知的卡片操作"), nil
	}
	requestID := stringValue(value, "request_id")
	optionID := stringValue(value, "option_id")
	if requestID == "" || optionID == "" {
		return permissionCardToast("error", "权限选项无效"), nil
	}
	waiter, ok := a.permissionCards.take(requestID)
	if !ok {
		return permissionCardToast("warning", "该权限请求已处理或已过期"), nil
	}
	outcome := acp.PermissionOutcome{Outcome: "selected", OptionID: optionID}
	select {
	case waiter.once <- outcome:
	default:
	}
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: "已提交选择"},
		Card: &callback.Card{
			Type: "raw",
			Data: map[string]any{
				"schema": "2.0",
				"config": map[string]any{
					"wide_screen_mode": true,
					"width_mode":       "fill",
				},
				"header": map[string]any{
					"title": map[string]string{"tag": "plain_text", "content": "权限请求已处理"},
				},
				"body": map[string]any{
					"elements": []any{
						map[string]any{"tag": "markdown", "content": "已选择：" + permissionOptionDisplayName(optionID, stringValue(value, "option_name"))},
					},
				},
			},
		},
	}, nil
}

func permissionCardToast(kind, content string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: kind, Content: content},
	}
}

func newPermissionCardJSON(requestID string, req acp.PermissionRequest, selectedOptionID string) string {
	data, _ := json.Marshal(cardJSON{
		"schema": "2.0",
		"config": cardJSON{
			"wide_screen_mode": true,
			"width_mode":       "fill",
			"summary":          cardJSON{"content": "ACP 权限请求"},
		},
		"header": cardJSON{
			"title": cardJSON{"tag": "plain_text", "content": "需要确认权限"},
		},
		"body": cardJSON{
			"elements": permissionCardElements(requestID, req, selectedOptionID),
		},
	})
	return string(data)
}

func permissionCardElements(requestID string, req acp.PermissionRequest, selectedOptionID string) []any {
	tool := req.ToolCallState
	title := strings.TrimSpace(req.ToolCall.ToolCallID)
	if tool != nil && strings.TrimSpace(tool.Title) != "" {
		title = strings.TrimSpace(tool.Title)
	}
	if title == "" {
		title = "未命名工具调用"
	}
	lines := []string{"**工具调用**：" + markdownInline(title)}
	if req.ToolCall.ToolCallID != "" {
		lines = append(lines, "**Tool Call ID**：`"+markdownInline(req.ToolCall.ToolCallID)+"`")
	}
	if tool != nil {
		if strings.TrimSpace(tool.Kind) != "" {
			lines = append(lines, "**类型**：`"+markdownInline(tool.Kind)+"`")
		}
		if strings.TrimSpace(tool.Status) != "" {
			lines = append(lines, "**状态**：`"+markdownInline(tool.Status)+"`")
		}
		lines = appendJSONDetail(lines, "输入", tool.RawInput)
		lines = appendJSONDetail(lines, "内容", tool.Content)
		lines = appendJSONDetail(lines, "位置", tool.Locations)
	}
	elements := []any{
		cardJSON{"tag": "markdown", "content": strings.Join(lines, "\n")},
	}
	if selectedOptionID != "" {
		elements = append(elements, cardJSON{"tag": "markdown", "content": "**已选择**：" + markdownInline(selectedOptionID)})
		return elements
	}
	actions := permissionCardActions(requestID, req.Options)
	if len(actions) > 0 {
		elements = append(elements, cardJSON{"tag": "action", "actions": actions})
	}
	return elements
}

func permissionCardActions(requestID string, options []acp.PermissionOption) []any {
	var actions []any
	for _, kind := range []string{"allow_once", "allow_always", "reject_once", "reject_always"} {
		option, ok := findPermissionOption(options, kind)
		if !ok || strings.TrimSpace(option.OptionID) == "" {
			continue
		}
		text, buttonType := permissionButtonStyle(option)
		actions = append(actions, cardJSON{
			"tag":  "button",
			"text": cardJSON{"tag": "plain_text", "content": text},
			"type": buttonType,
			"value": cardJSON{
				"action":      permissionCardAction,
				"request_id":  requestID,
				"option_id":   option.OptionID,
				"option_name": permissionOptionDisplayName(option.OptionID, option.Name),
			},
		})
	}
	return actions
}

func permissionButtonStyle(option acp.PermissionOption) (string, string) {
	name := strings.TrimSpace(option.Name)
	switch option.Kind {
	case "allow_once":
		if name == "" {
			name = "允许"
		}
		return name, "primary"
	case "allow_always":
		if name == "" {
			name = "本会话总是允许"
		}
		return name, "default"
	case "reject_once", "reject_always":
		if name == "" {
			name = "拒绝"
		}
		return name, "danger"
	default:
		if name == "" {
			name = option.OptionID
		}
		return name, "default"
	}
}

func findPermissionOption(options []acp.PermissionOption, kind string) (acp.PermissionOption, bool) {
	for _, option := range options {
		if option.Kind == kind {
			return option, true
		}
	}
	return acp.PermissionOption{}, false
}

func permissionRequestID(req acp.PermissionRequest) string {
	requestID := strings.TrimSpace(req.RequestID)
	sessionID := strings.TrimSpace(req.SessionID)
	toolCallID := strings.TrimSpace(req.ToolCall.ToolCallID)
	if requestID != "" {
		return requestID
	}
	if sessionID == "" || toolCallID == "" {
		return ""
	}
	return sessionID + ":" + toolCallID
}

func appendJSONDetail(lines []string, label string, raw json.RawMessage) []string {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return lines
	}
	text := compactJSONString(raw)
	if text == "" {
		return lines
	}
	return append(lines, "**"+label+"**：`"+markdownInline(text)+"`")
}

func compactJSONString(raw json.RawMessage) string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return string(raw)
	}
	text := string(data)
	const limit = 700
	if len(text) > limit {
		text = text[:limit] + "..."
	}
	return text
}

func markdownInline(text string) string {
	text = strings.ReplaceAll(text, "`", "'")
	return strings.ReplaceAll(text, "\n", " ")
}

func stringValue(value map[string]interface{}, key string) string {
	if value == nil {
		return ""
	}
	if s, ok := value[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func permissionOptionDisplayName(optionID, name string) string {
	if strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	return strings.TrimSpace(optionID)
}
