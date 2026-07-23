package feishu

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

func TestNewStreamCardJSONStartsWithoutProcessPanel(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newStreamCardJSON()), &card); err != nil {
		t.Fatalf("newStreamCardJSON() is not valid JSON: %v", err)
	}

	if !jsonContainsValue(card, streamCardTextElementID) {
		t.Fatalf("initial stream card does not contain text element %q", streamCardTextElementID)
	}
	if jsonContainsValue(card, streamCardProcessPanelID) {
		t.Fatalf("initial stream card contains process panel %q", streamCardProcessPanelID)
	}
	if jsonContainsValue(card, streamCardProcessElementID) {
		t.Fatalf("initial stream card contains process element %q", streamCardProcessElementID)
	}
}

func TestNewStreamCardProcessPanelJSONContainsProcessElements(t *testing.T) {
	var elements any
	if err := json.Unmarshal([]byte(newStreamCardProcessPanelJSON()), &elements); err != nil {
		t.Fatalf("newStreamCardProcessPanelJSON() is not valid JSON: %v", err)
	}

	if !jsonContainsValue(elements, streamCardProcessPanelID) {
		t.Fatalf("process panel JSON does not contain panel %q", streamCardProcessPanelID)
	}
	if !jsonContainsValue(elements, streamCardProcessElementID) {
		t.Fatalf("process panel JSON does not contain process element %q", streamCardProcessElementID)
	}
}

func TestNewPermissionCardJSONShowsToolAndOptions(t *testing.T) {
	req := acp.PermissionRequest{
		RequestID: "perm-1",
		SessionID: "session-1",
		ToolCall:  acp.PermissionToolCallRef{ToolCallID: "call-1"},
		Options: []acp.PermissionOption{
			{OptionID: "allow-once", Kind: "allow_once"},
			{OptionID: "allow-always", Kind: "allow_always"},
			{OptionID: "reject-once", Kind: "reject_once"},
		},
		ToolCallState: &acp.ToolCallInfo{
			ToolCallID: "call-1",
			Title:      "Run tests",
			Kind:       "execute",
			Status:     "pending",
			RawInput:   json.RawMessage(`{"command":"go test ./..."}`),
		},
	}
	var card any
	if err := json.Unmarshal([]byte(newPermissionCardJSON("perm-1", req, "")), &card); err != nil {
		t.Fatalf("newPermissionCardJSON() is not valid JSON: %v", err)
	}

	for _, want := range []string{"Run tests", "go test ./...", "允许", "本会话总是允许", "拒绝"} {
		if !jsonContainsSubstring(card, want) {
			t.Fatalf("permission card does not contain %q: %#v", want, card)
		}
	}
}

func TestPermissionCardActionCompletesWaiter(t *testing.T) {
	adapter := &Adapter{permissionCards: newPermissionCardRegistry()}
	waiter := newPermissionCardWaiter()
	adapter.permissionCards.add("perm-1", waiter)

	resp, err := adapter.handleCardAction(nil, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Action: &callback.CallBackAction{
				Value: map[string]interface{}{
					"action":     permissionCardAction,
					"request_id": "perm-1",
					"option_id":  "allow-once",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("handleCardAction() error = %v", err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "success" {
		t.Fatalf("response = %+v, want success toast", resp)
	}
	select {
	case outcome := <-waiter.once:
		if outcome.Outcome != "selected" || outcome.OptionID != "allow-once" {
			t.Fatalf("outcome = %+v, want allow-once", outcome)
		}
	default:
		t.Fatal("waiter did not receive outcome")
	}
	if _, ok := adapter.permissionCards.take("perm-1"); ok {
		t.Fatal("waiter should be removed after action")
	}
}

func jsonContainsValue(v any, want string) bool {
	switch value := v.(type) {
	case map[string]any:
		for _, child := range value {
			if jsonContainsValue(child, want) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if jsonContainsValue(child, want) {
				return true
			}
		}
	case string:
		return value == want
	}
	return false
}

func jsonContainsSubstring(v any, want string) bool {
	switch value := v.(type) {
	case map[string]any:
		for _, child := range value {
			if jsonContainsSubstring(child, want) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if jsonContainsSubstring(child, want) {
				return true
			}
		}
	case string:
		return strings.Contains(value, want)
	}
	return false
}
