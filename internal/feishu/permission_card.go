package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
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
	entries map[string]permissionCardEntry
}

type permissionCardEntry struct {
	waiter       *permissionCardWaiter
	request      acp.PermissionRequest
	cardID       string
	requesterID  string
	ownerOpenIDs []string
	groupChat    bool
}

func newPermissionCardRegistry() *permissionCardRegistry {
	return &permissionCardRegistry{entries: make(map[string]permissionCardEntry)}
}

func (r *permissionCardRegistry) add(id string, entry permissionCardEntry) {
	if r == nil || strings.TrimSpace(id) == "" || entry.waiter == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[id] = entry
}

func (r *permissionCardRegistry) get(id string) (permissionCardEntry, bool) {
	if r == nil || strings.TrimSpace(id) == "" {
		return permissionCardEntry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[id]
	return entry, ok
}

func (r *permissionCardRegistry) take(id string) (permissionCardEntry, bool) {
	if r == nil || strings.TrimSpace(id) == "" {
		return permissionCardEntry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[id]
	if ok {
		delete(r.entries, id)
	}
	return entry, ok
}

func (r *permissionCardRegistry) remove(id string) {
	if r == nil || strings.TrimSpace(id) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, id)
}

func (a *Adapter) RequestPermission(ctx context.Context, msg Message, req acp.PermissionRequest) (acp.PermissionOutcome, error) {
	if a.client == nil {
		return acp.PermissionOutcome{}, fmt.Errorf("飞书客户端未初始化")
	}
	requestID := permissionRequestID(req)
	if requestID == "" {
		return acp.PermissionOutcome{Outcome: "cancelled"}, nil
	}
	cardID, err := a.createCardJSON(ctx, newPermissionCardJSON(requestID, req, ""), "权限")
	if err != nil {
		return acp.PermissionOutcome{}, err
	}
	waiter := newPermissionCardWaiter()
	a.permissionCards.add(requestID, permissionCardEntry{
		waiter:       waiter,
		request:      req,
		cardID:       cardID,
		requesterID:  msg.SenderID,
		ownerOpenIDs: a.cfg.OwnerOpenIDs,
		groupChat:    !msg.IsPrivateChat(),
	})
	defer a.permissionCards.remove(requestID)
	if _, err := a.sendInteractiveCard(ctx, msg, cardID, "权限"); err != nil {
		return acp.PermissionOutcome{}, err
	}
	select {
	case <-ctx.Done():
		if entry, ok := a.permissionCards.take(requestID); ok {
			a.markPermissionCardCancelled(requestID, entry)
		}
		return acp.PermissionOutcome{Outcome: "cancelled"}, nil
	case outcome := <-waiter.once:
		return outcome, nil
	}
}

func (a *Adapter) markPermissionCardCancelled(requestID string, entry permissionCardEntry) {
	cardID := strings.TrimSpace(entry.cardID)
	if a == nil || a.client == nil || cardID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.updatePermissionCard(ctx, cardID, newPermissionCardCancelledJSON(requestID, entry.request)); err != nil {
		slog.Warn("更新飞书权限卡片取消状态失败", "request_id", requestID, "card_id", cardID, "err", err)
	}
}

func (a *Adapter) updatePermissionCard(ctx context.Context, cardID string, data string) error {
	return a.updateCardJSON(ctx, cardUpdateRequest{
		cardID: cardID,
		data:   data,
		action: "更新飞书权限卡片",
	})
}

func (a *Adapter) handleCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return permissionCardToast("error", "无效的卡片操作"), nil
	}
	value := event.Event.Action.Value
	switch stringValue(value, "action") {
	case modelSelectionCardAction:
		return a.handleModelSelectionAction(ctx, event)
	case modeSelectionCardAction:
		return a.handleModeSelectionAction(ctx, event)
	case sessionSelectionCardAction:
		return a.handleSessionSelectionAction(ctx, event)
	case loopStatusCardAction:
		return a.handleLoopCancelAction(ctx, event)
	case overviewCardAction:
		return a.handleOverviewAction(ctx, event)
	case permissionCardAction:
	default:
		return permissionCardToast("error", "未知的卡片操作"), nil
	}
	requestID := stringValue(value, "request_id")
	optionID := stringValue(value, "option_id")
	if requestID == "" || optionID == "" {
		return permissionCardToast("error", "权限选项无效"), nil
	}
	entry, ok := a.permissionCards.get(requestID)
	if !ok {
		return permissionCardToast("warning", "该权限请求已处理或已过期"), nil
	}
	if !permissionCardOperatorAllowed(entry, cardOperatorOpenID(event)) {
		return permissionCardToast("warning", permissionCardUnauthorizedMessage(entry)), nil
	}
	if !permissionCardOptionAllowed(entry, optionID) {
		return permissionCardToast("error", "权限选项已失效"), nil
	}
	entry, ok = a.permissionCards.take(requestID)
	if !ok {
		return permissionCardToast("warning", "该权限请求已处理或已过期"), nil
	}
	outcome := acp.PermissionOutcome{Outcome: "selected", OptionID: optionID}
	select {
	case entry.waiter.once <- outcome:
	default:
	}
	selectedOption := permissionOptionDisplayName(optionID, stringValue(value, "option_name"))
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: "已提交选择"},
		Card: &callback.Card{
			Type: "raw",
			Data: newPermissionCardData(requestID, entry.request, selectedOption),
		},
	}, nil
}

func permissionCardOperatorAllowed(entry permissionCardEntry, operatorID string) bool {
	operatorID = strings.TrimSpace(operatorID)
	if len(entry.ownerOpenIDs) > 0 {
		return containsOpenID(entry.ownerOpenIDs, operatorID)
	}
	return false
}

func permissionCardOptionAllowed(entry permissionCardEntry, optionID string) bool {
	optionID = strings.TrimSpace(optionID)
	if optionID == "" {
		return false
	}
	for _, option := range entry.request.Options {
		if strings.TrimSpace(option.OptionID) == optionID {
			return true
		}
	}
	return false
}

func permissionCardUnauthorizedMessage(entry permissionCardEntry) string {
	if len(entry.ownerOpenIDs) == 0 {
		return "权限卡片需要先配置 bot owner"
	}
	return "只有 bot owner 可以操作该权限卡片"
}

func containsOpenID(ids []string, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return false
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == want {
			return true
		}
	}
	return false
}

func cardOperatorOpenID(event *callback.CardActionTriggerEvent) string {
	if event == nil || event.Event == nil || event.Event.Operator == nil {
		return ""
	}
	return strings.TrimSpace(event.Event.Operator.OpenID)
}

func permissionCardToast(kind, content string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: kind, Content: content},
	}
}

func newPermissionCardJSON(requestID string, req acp.PermissionRequest, selectedOptionID string) string {
	data, _ := json.Marshal(newPermissionCardData(requestID, req, selectedOptionID))
	return string(data)
}

func newPermissionCardCancelledJSON(requestID string, req acp.PermissionRequest) string {
	data, _ := json.Marshal(newPermissionCardDataWithState(requestID, req, permissionCardRenderState{cancelled: true}))
	return string(data)
}

type permissionCardRenderState struct {
	selectedOption string
	cancelled      bool
}

func newPermissionCardData(requestID string, req acp.PermissionRequest, selectedOptionID string) cardJSON {
	return newPermissionCardDataWithState(requestID, req, permissionCardRenderState{selectedOption: selectedOptionID})
}

func newPermissionCardDataWithState(requestID string, req acp.PermissionRequest, state permissionCardRenderState) cardJSON {
	title := "需要确认权限"
	if state.cancelled {
		title = "权限请求已取消"
	} else if state.selectedOption != "" {
		title = "权限请求已处理"
	}
	return cardJSON{
		"schema": "2.0",
		"config": cardJSON{
			"wide_screen_mode": true,
			"width_mode":       "fill",
			"update_multi":     true,
			"summary":          cardJSON{"content": "ACP 权限请求"},
		},
		"header": cardJSON{
			"title": cardJSON{"tag": "plain_text", "content": title},
		},
		"body": cardJSON{
			"elements": permissionCardElements(requestID, req, state),
		},
	}
}

func permissionCardElements(requestID string, req acp.PermissionRequest, state permissionCardRenderState) []any {
	title := permissionToolDisplayName(req)
	lines := []string{"**工具调用**：" + markdownInline(title)}
	if kind := permissionToolKind(req); kind != "" {
		lines = append(lines, "**类型**：`"+markdownInline(kind)+"`")
	}
	lines = appendJSONDetail(lines, "位置", permissionToolLocations(req))
	elements := []any{
		cardJSON{"tag": "markdown", "content": strings.Join(lines, "\n")},
	}
	if state.cancelled {
		return elements
	}
	if state.selectedOption != "" {
		elements = append(elements, cardJSON{"tag": "markdown", "content": "**已选择**：" + markdownInline(state.selectedOption)})
		return elements
	}
	if optionsText := permissionOptionsMarkdown(req.Options); optionsText != "" {
		elements = append(elements, cardJSON{"tag": "markdown", "content": optionsText})
	}
	actions := permissionCardActions(requestID, req.Options)
	if len(actions) > 0 {
		elements = append(elements, cardJSON{
			"tag":                "column_set",
			"flex_mode":          "flow",
			"horizontal_spacing": "8px",
			"columns":            actions,
		})
	}
	return elements
}

func permissionCardActions(requestID string, options []acp.PermissionOption) []any {
	var actions []any
	buttonIndex := 1
	for _, option := range options {
		if strings.TrimSpace(option.OptionID) == "" {
			continue
		}
		text, buttonType := permissionButtonStyle(option, buttonIndex)
		buttonIndex++
		actions = append(actions, cardJSON{
			"tag":   "column",
			"width": "auto",
			"elements": []any{
				cardJSON{
					"tag":   "button",
					"text":  cardJSON{"tag": "plain_text", "content": text},
					"type":  buttonType,
					"width": "fill",
					"behaviors": []any{
						cardJSON{
							"type": "callback",
							"value": cardJSON{
								"action":      permissionCardAction,
								"request_id":  requestID,
								"option_id":   option.OptionID,
								"option_name": permissionOptionDisplayName(option.OptionID, option.Name),
							},
						},
					},
				},
			},
		})
	}
	return actions
}

func permissionButtonStyle(option acp.PermissionOption, index int) (string, string) {
	text := fmt.Sprintf("选择 %d", index)
	switch option.Kind {
	case "allow_once":
		return text, "primary"
	case "allow_always":
		return text, "default"
	case "reject_once", "reject_always":
		return text, "danger"
	}
	return text, "default"
}

func permissionOptionsMarkdown(options []acp.PermissionOption) string {
	var lines []string
	displayIndex := 1
	for _, option := range options {
		if strings.TrimSpace(option.OptionID) == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%d. %s", displayIndex, markdownInline(permissionOptionDisplayName(option.OptionID, option.Name))))
		displayIndex++
	}
	if len(lines) == 0 {
		return ""
	}
	return "**选项**：\n" + strings.Join(lines, "\n")
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

func permissionToolDisplayName(req acp.PermissionRequest) string {
	tool := req.ToolCallState
	if tool != nil {
		for _, value := range []string{tool.Title, req.ToolCall.Title, tool.Kind, req.ToolCall.Kind} {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	} else if value := strings.TrimSpace(req.ToolCall.Title); value != "" {
		return value
	}
	for _, value := range []string{req.ToolCall.Kind} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	if toolCallID := strings.TrimSpace(req.ToolCall.ToolCallID); toolCallID != "" {
		return "工具调用 " + toolCallID
	}
	return "未命名工具调用"
}

func permissionToolKind(req acp.PermissionRequest) string {
	if req.ToolCallState != nil {
		if value := strings.TrimSpace(req.ToolCallState.Kind); value != "" {
			return value
		}
	}
	return strings.TrimSpace(req.ToolCall.Kind)
}

func permissionToolLocations(req acp.PermissionRequest) json.RawMessage {
	if req.ToolCallState != nil && jsonDetailPresent(req.ToolCallState.Locations) {
		return req.ToolCallState.Locations
	}
	return req.ToolCall.Locations
}

func jsonDetailPresent(raw json.RawMessage) bool {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	return len(raw) > 0 && string(raw) != "null"
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
