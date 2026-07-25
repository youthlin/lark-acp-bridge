package feishu

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

func TestNewStreamCardJSONStartsWithProcessPanel(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newStreamCardJSON()), &card); err != nil {
		t.Fatalf("newStreamCardJSON() is not valid JSON: %v", err)
	}

	if !jsonContainsValue(card, streamCardTextElementID) {
		t.Fatalf("initial stream card does not contain text element %q", streamCardTextElementID)
	}
	if !jsonContainsValue(card, streamCardStatusElementID) {
		t.Fatalf("initial stream card does not contain status element %q", streamCardStatusElementID)
	}
	if jsonContainsValue(card, streamCardUsagePanelID) {
		t.Fatalf("initial stream card should not contain usage detail panel %q", streamCardUsagePanelID)
	}
	if jsonContainsValue(card, streamCardUsageDetailID) {
		t.Fatalf("initial stream card should not contain usage detail element %q", streamCardUsageDetailID)
	}
	if jsonContainsValue(card, "结果明细") {
		t.Fatalf("initial stream card should not contain separate result title")
	}
	if !jsonContainsValue(card, "执行中") {
		t.Fatalf("initial stream card does not contain running status")
	}
	if !jsonContainsValue(card, streamCardProcessPanelID) {
		t.Fatalf("initial stream card does not contain process panel %q", streamCardProcessPanelID)
	}
	if !jsonContainsValue(card, streamCardProcessElementID) {
		t.Fatalf("initial stream card does not contain process element %q", streamCardProcessElementID)
	}
	if !jsonContainsValue(card, "执行过程") {
		t.Fatalf("initial stream card does not contain execution process title")
	}
}

func TestNewStreamCardJSONCanOmitProcessPanel(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newStreamCardJSONWithProcessPanel(false)), &card); err != nil {
		t.Fatalf("newStreamCardJSONWithProcessPanel(false) is not valid JSON: %v", err)
	}

	if !jsonContainsValue(card, streamCardTextElementID) {
		t.Fatalf("stream card does not contain text element %q", streamCardTextElementID)
	}
	if !jsonContainsValue(card, streamCardStatusElementID) {
		t.Fatalf("stream card does not contain status element %q", streamCardStatusElementID)
	}
	if jsonContainsValue(card, streamCardUsagePanelID) {
		t.Fatalf("stream card should not contain usage detail panel %q before prompt result", streamCardUsagePanelID)
	}
	if jsonContainsValue(card, streamCardUsageDetailID) {
		t.Fatalf("stream card should not contain usage detail element %q before prompt result", streamCardUsageDetailID)
	}
	if jsonContainsValue(card, streamCardProcessPanelID) {
		t.Fatalf("stream card should not contain process panel %q", streamCardProcessPanelID)
	}
	if jsonContainsValue(card, streamCardProcessElementID) {
		t.Fatalf("stream card should not contain process element %q", streamCardProcessElementID)
	}
	if jsonContainsValue(card, "结果明细") {
		t.Fatalf("stream card should not contain separate result title")
	}
	if jsonContainsValue(card, "执行过程") {
		t.Fatalf("stream card should not contain execution process title")
	}
}

func TestNewStreamCardJSONCanOmitStatusBar(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newStreamCardJSONWithPanels(true, false)), &card); err != nil {
		t.Fatalf("newStreamCardJSONWithPanels(true, false) is not valid JSON: %v", err)
	}

	if !jsonContainsValue(card, streamCardTextElementID) {
		t.Fatalf("stream card does not contain text element %q", streamCardTextElementID)
	}
	if !jsonContainsValue(card, streamCardProcessPanelID) {
		t.Fatalf("stream card does not contain process panel %q", streamCardProcessPanelID)
	}
	if jsonContainsValue(card, streamCardStatusElementID) {
		t.Fatalf("stream card should not contain status element %q", streamCardStatusElementID)
	}
	if jsonContainsValue(card, "执行中") {
		t.Fatalf("stream card should not contain running status")
	}
}

func TestNewStreamCardUsagePanelJSONContainsUsageDetail(t *testing.T) {
	var elements any
	if err := json.Unmarshal([]byte(newStreamCardUsagePanelJSON("```json\n{}\n```")), &elements); err != nil {
		t.Fatalf("newStreamCardUsagePanelJSON() is not valid JSON: %v", err)
	}

	if !jsonContainsValue(elements, streamCardUsagePanelID) {
		t.Fatalf("usage panel JSON does not contain panel %q", streamCardUsagePanelID)
	}
	if !jsonContainsValue(elements, streamCardUsageDetailID) {
		t.Fatalf("usage panel JSON does not contain detail element %q", streamCardUsageDetailID)
	}
	if !jsonContainsValue(elements, "用量明细") {
		t.Fatalf("usage panel JSON does not contain usage detail title")
	}
	if jsonContainsValue(elements, "结果明细") {
		t.Fatalf("usage panel JSON should not contain separate result title")
	}
	if !jsonContainsSubstring(elements, "```json") {
		t.Fatalf("usage panel JSON does not contain detail content")
	}
}

func TestStreamCardUsageTargetID(t *testing.T) {
	tests := []struct {
		name                string
		processPanelEnabled bool
		statusBarEnabled    bool
		want                string
	}{
		{
			name:                "after status bar",
			processPanelEnabled: true,
			statusBarEnabled:    true,
			want:                streamCardStatusElementID,
		},
		{
			name:                "after process panel without status bar",
			processPanelEnabled: true,
			want:                streamCardProcessPanelID,
		},
		{
			name: "after text only",
			want: streamCardTextElementID,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := streamCardUsageTargetID(tt.processPanelEnabled, tt.statusBarEnabled); got != tt.want {
				t.Fatalf("streamCardUsageTargetID() = %q, want %q", got, tt.want)
			}
		})
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
	if !jsonContainsValue(elements, "执行过程") {
		t.Fatalf("process panel JSON does not contain execution process title")
	}
	if jsonContainsValue(elements, "思考与工具调用") {
		t.Fatalf("process panel JSON should not use old title")
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
			Content:    json.RawMessage(`[{"type":"diff","path":"go.mod"}]`),
			Locations:  json.RawMessage(`[{"path":"go.mod"}]`),
		},
	}
	var card any
	if err := json.Unmarshal([]byte(newPermissionCardJSON("perm-1", req, "")), &card); err != nil {
		t.Fatalf("newPermissionCardJSON() is not valid JSON: %v", err)
	}

	for _, want := range []string{"Run tests", "go.mod", "allow-once", "allow-always", "reject-once", "选择 1", "选择 2", "选择 3"} {
		if !jsonContainsSubstring(card, want) {
			t.Fatalf("permission card does not contain %q: %#v", want, card)
		}
	}
	if !jsonContainsBool(card, "update_multi", true) {
		t.Fatalf("permission card should enable update_multi: %#v", card)
	}
	for _, unwanted := range []string{"go test ./...", "type\":\"diff", "Tool Call ID", "pending"} {
		if jsonContainsSubstring(card, unwanted) {
			t.Fatalf("permission card contains verbose field %q: %#v", unwanted, card)
		}
	}
	if jsonContainsTaggedElement(card, "action") {
		t.Fatalf("permission card contains Card 1.0 action container: %#v", card)
	}
	if jsonButtonContainsTopLevelValue(card) {
		t.Fatalf("permission card button contains Card 1.0 top-level value: %#v", card)
	}
	if !jsonContainsTaggedElement(card, "column_set") || !jsonContainsKey(card, "behaviors") || !jsonContainsValue(card, "callback") {
		t.Fatalf("permission card does not contain Card 2.0 callback button structure: %#v", card)
	}
}

func TestNewPermissionCardJSONUsesToolCallTitleWithoutSnapshot(t *testing.T) {
	req := acp.PermissionRequest{
		RequestID: "perm-1",
		SessionID: "session-1",
		ToolCall: acp.PermissionToolCallRef{
			ToolCallID: "call-1",
			Title:      "git status --short",
			Kind:       "execute",
			Status:     "pending",
			Locations:  json.RawMessage(`[{"path":"internal/bridge/runtime.go"}]`),
		},
		Options: []acp.PermissionOption{
			{OptionID: "allow-once", Kind: "allow_once"},
		},
	}
	var card any
	if err := json.Unmarshal([]byte(newPermissionCardJSON("perm-1", req, "")), &card); err != nil {
		t.Fatalf("newPermissionCardJSON() is not valid JSON: %v", err)
	}

	for _, want := range []string{"git status --short", "execute", "internal/bridge/runtime.go"} {
		if !jsonContainsSubstring(card, want) {
			t.Fatalf("permission card does not contain %q: %#v", want, card)
		}
	}
	for _, unwanted := range []string{"Tool Call ID", "pending"} {
		if jsonContainsSubstring(card, unwanted) {
			t.Fatalf("permission card contains hidden field %q: %#v", unwanted, card)
		}
	}
	if jsonContainsSubstring(card, "工具调用 call-1") {
		t.Fatalf("permission card should prefer tool title over id fallback: %#v", card)
	}
}

func TestNewPermissionCardJSONMergesPartialSnapshotWithRequestToolCall(t *testing.T) {
	req := acp.PermissionRequest{
		RequestID: "perm-1",
		SessionID: "session-1",
		ToolCall: acp.PermissionToolCallRef{
			ToolCallID: "call-1",
			Title:      "git status --short",
			Kind:       "execute",
			Status:     "pending",
			Locations:  json.RawMessage(`[{"path":"internal/feishu/permission_card.go"}]`),
		},
		Options: []acp.PermissionOption{
			{OptionID: "allow-once", Kind: "allow_once"},
		},
		ToolCallState: &acp.ToolCallInfo{
			ToolCallID: "call-1",
			Kind:       "execute",
		},
	}
	var card any
	if err := json.Unmarshal([]byte(newPermissionCardJSON("perm-1", req, "")), &card); err != nil {
		t.Fatalf("newPermissionCardJSON() is not valid JSON: %v", err)
	}

	for _, want := range []string{"git status --short", "execute", "internal/feishu/permission_card.go"} {
		if !jsonContainsSubstring(card, want) {
			t.Fatalf("permission card does not contain %q: %#v", want, card)
		}
	}
	for _, unwanted := range []string{"Tool Call ID", "pending"} {
		if jsonContainsSubstring(card, unwanted) {
			t.Fatalf("permission card contains hidden field %q: %#v", unwanted, card)
		}
	}
}

func TestNewPermissionCardJSONShowsAgentOptionsWithShortButtons(t *testing.T) {
	req := acp.PermissionRequest{
		RequestID: "perm-1",
		SessionID: "session-1",
		ToolCall:  acp.PermissionToolCallRef{ToolCallID: "call-1"},
		Options: []acp.PermissionOption{
			{OptionID: "allow-once", Kind: "allow_once", Name: "允许本次工具调用并且继续执行后续所有步骤"},
			{OptionID: "allow-always", Kind: "allow_always", Name: "在当前会话中总是允许这个非常非常长的工具调用选项"},
			{OptionID: "reject-once", Kind: "reject_once", Name: "拒绝本次工具调用并解释原因"},
		},
		ToolCallState: &acp.ToolCallInfo{
			ToolCallID: "call-1",
			Title:      "Run tests",
		},
	}
	var card any
	if err := json.Unmarshal([]byte(newPermissionCardJSON("perm-1", req, "")), &card); err != nil {
		t.Fatalf("newPermissionCardJSON() is not valid JSON: %v", err)
	}

	for _, want := range []string{
		"允许本次工具调用并且继续执行后续所有步骤",
		"在当前会话中总是允许这个非常非常长的工具调用选项",
		"拒绝本次工具调用并解释原因",
	} {
		if !jsonContainsSubstring(card, want) {
			t.Fatalf("permission card does not contain agent option %q: %#v", want, card)
		}
	}
	texts := collectButtonTexts(card)
	for _, want := range []string{"选择 1", "选择 2", "选择 3"} {
		if !containsString(texts, want) {
			t.Fatalf("button texts = %+v, want %q", texts, want)
		}
	}
	for _, text := range texts {
		if strings.Contains(text, "非常非常长") || len([]rune(text)) > 8 {
			t.Fatalf("button text = %q, want short indexed label", text)
		}
	}
}

func TestPermissionToolDisplayNameFallsBackAfterTitle(t *testing.T) {
	req := acp.PermissionRequest{
		ToolCall: acp.PermissionToolCallRef{ToolCallID: "call-1"},
		ToolCallState: &acp.ToolCallInfo{
			ToolCallID: "call-1",
			Kind:       "execute",
		},
	}
	if got := permissionToolDisplayName(req); got != "execute" {
		t.Fatalf("permissionToolDisplayName() = %q, want kind fallback", got)
	}
	req.ToolCallState.Title = "Run tests"
	if got := permissionToolDisplayName(req); got != "Run tests" {
		t.Fatalf("permissionToolDisplayName() = %q, want title", got)
	}
	req.ToolCallState = nil
	if got := permissionToolDisplayName(req); got != "工具调用 call-1" {
		t.Fatalf("permissionToolDisplayName() = %q, want labeled id fallback", got)
	}
}

func TestPermissionCardActionCompletesWaiter(t *testing.T) {
	adapter := &Adapter{permissionCards: newPermissionCardRegistry()}
	waiter := newPermissionCardWaiter()
	adapter.permissionCards.add("perm-1", permissionCardEntry{
		waiter:       waiter,
		requesterID:  "ou_requester",
		ownerOpenIDs: []string{"ou_owner"},
		groupChat:    true,
		request: acp.PermissionRequest{
			RequestID: "perm-1",
			ToolCall:  acp.PermissionToolCallRef{ToolCallID: "call-1"},
			ToolCallState: &acp.ToolCallInfo{
				ToolCallID: "call-1",
				Title:      "Run tests",
				Kind:       "execute",
				Status:     "pending",
				Locations:  json.RawMessage(`[{"path":"go.mod"}]`),
			},
			Options: []acp.PermissionOption{
				{OptionID: "allow-once", Kind: "allow_once"},
			},
		},
	})

	resp, err := adapter.handleCardAction(nil, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_owner"},
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
	if resp.Card == nil || jsonContainsTaggedElement(resp.Card.Data, "button") {
		t.Fatalf("completed permission card should hide buttons: %+v", resp.Card)
	}
	for _, want := range []string{"Run tests", "go.mod", "已选择", "allow-once"} {
		if !jsonContainsSubstring(resp.Card.Data, want) {
			t.Fatalf("completed permission card does not contain %q: %#v", want, resp.Card.Data)
		}
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

func TestPermissionCardActionRejectsNonOwnerInGroup(t *testing.T) {
	adapter := &Adapter{permissionCards: newPermissionCardRegistry()}
	waiter := newPermissionCardWaiter()
	adapter.permissionCards.add("perm-1", permissionCardEntry{
		waiter:       waiter,
		requesterID:  "ou_requester",
		ownerOpenIDs: []string{"ou_owner"},
		groupChat:    true,
		request: acp.PermissionRequest{
			RequestID: "perm-1",
			ToolCall:  acp.PermissionToolCallRef{ToolCallID: "call-1"},
			Options: []acp.PermissionOption{
				{OptionID: "allow-once", Kind: "allow_once"},
			},
		},
	})

	resp, err := adapter.handleCardAction(nil, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_other"},
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
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "warning" {
		t.Fatalf("response = %+v, want warning toast", resp)
	}
	select {
	case outcome := <-waiter.once:
		t.Fatalf("waiter received unauthorized outcome: %+v", outcome)
	default:
	}
	if _, ok := adapter.permissionCards.get("perm-1"); !ok {
		t.Fatal("unauthorized click should not remove permission request")
	}
}

func TestPermissionCardActionRejectsGroupWhenOwnerMissing(t *testing.T) {
	adapter := &Adapter{permissionCards: newPermissionCardRegistry()}
	waiter := newPermissionCardWaiter()
	adapter.permissionCards.add("perm-1", permissionCardEntry{
		waiter:      waiter,
		requesterID: "ou_requester",
		groupChat:   true,
		request: acp.PermissionRequest{
			RequestID: "perm-1",
			ToolCall:  acp.PermissionToolCallRef{ToolCallID: "call-1"},
			Options: []acp.PermissionOption{
				{OptionID: "allow-once", Kind: "allow_once"},
			},
		},
	})

	resp, err := adapter.handleCardAction(nil, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_requester"},
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
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "warning" {
		t.Fatalf("response = %+v, want warning toast", resp)
	}
	if resp.Toast.Content != "权限卡片需要先配置 bot owner" {
		t.Fatalf("toast = %q, want owner config warning", resp.Toast.Content)
	}
	select {
	case outcome := <-waiter.once:
		t.Fatalf("waiter received unauthorized outcome: %+v", outcome)
	default:
	}
	if _, ok := adapter.permissionCards.get("perm-1"); !ok {
		t.Fatal("unauthorized click should not remove permission request")
	}
}

func TestPermissionCardActionAllowsOwnerInGroup(t *testing.T) {
	adapter := &Adapter{permissionCards: newPermissionCardRegistry()}
	waiter := newPermissionCardWaiter()
	adapter.permissionCards.add("perm-1", permissionCardEntry{
		waiter:       waiter,
		requesterID:  "ou_requester",
		ownerOpenIDs: []string{"ou_owner"},
		groupChat:    true,
		request: acp.PermissionRequest{
			RequestID: "perm-1",
			ToolCall:  acp.PermissionToolCallRef{ToolCallID: "call-1"},
			Options: []acp.PermissionOption{
				{OptionID: "allow-once", Kind: "allow_once"},
			},
		},
	})

	resp, err := adapter.handleCardAction(nil, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_owner"},
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
	if _, ok := adapter.permissionCards.get("perm-1"); ok {
		t.Fatal("owner click should remove permission request")
	}
}

func TestPermissionCardActionRejectsPrivateWhenOwnerMissing(t *testing.T) {
	adapter := &Adapter{permissionCards: newPermissionCardRegistry()}
	waiter := newPermissionCardWaiter()
	adapter.permissionCards.add("perm-1", permissionCardEntry{
		waiter:      waiter,
		requesterID: "ou_requester",
		groupChat:   false,
		request: acp.PermissionRequest{
			RequestID: "perm-1",
			ToolCall:  acp.PermissionToolCallRef{ToolCallID: "call-1"},
			Options: []acp.PermissionOption{
				{OptionID: "allow-once", Kind: "allow_once"},
			},
		},
	})

	resp, err := adapter.handleCardAction(nil, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_requester"},
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
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "warning" {
		t.Fatalf("response = %+v, want warning toast", resp)
	}
	if resp.Toast.Content != "权限卡片需要先配置 bot owner" {
		t.Fatalf("toast = %q, want owner config warning", resp.Toast.Content)
	}
	select {
	case outcome := <-waiter.once:
		t.Fatalf("waiter received unauthorized outcome: %+v", outcome)
	default:
	}
	if _, ok := adapter.permissionCards.get("perm-1"); !ok {
		t.Fatal("unauthorized click should not remove permission request")
	}
}

func TestPermissionCardActionAllowsOwnerInPrivate(t *testing.T) {
	adapter := &Adapter{permissionCards: newPermissionCardRegistry()}
	waiter := newPermissionCardWaiter()
	adapter.permissionCards.add("perm-1", permissionCardEntry{
		waiter:       waiter,
		requesterID:  "ou_requester",
		ownerOpenIDs: []string{"ou_owner"},
		groupChat:    false,
		request: acp.PermissionRequest{
			RequestID: "perm-1",
			ToolCall:  acp.PermissionToolCallRef{ToolCallID: "call-1"},
			Options: []acp.PermissionOption{
				{OptionID: "allow-once", Kind: "allow_once"},
			},
		},
	})

	resp, err := adapter.handleCardAction(nil, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_owner"},
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
	if _, ok := adapter.permissionCards.get("perm-1"); ok {
		t.Fatal("owner click should remove permission request")
	}
}

func TestNewPermissionCardCancelledJSONHidesButtons(t *testing.T) {
	req := acp.PermissionRequest{
		RequestID: "perm-1",
		ToolCall:  acp.PermissionToolCallRef{ToolCallID: "call-1"},
		Options: []acp.PermissionOption{
			{OptionID: "allow-once", Kind: "allow_once"},
		},
		ToolCallState: &acp.ToolCallInfo{
			ToolCallID: "call-1",
			Title:      "Run tests",
		},
	}
	var card any
	if err := json.Unmarshal([]byte(newPermissionCardCancelledJSON("perm-1", req)), &card); err != nil {
		t.Fatalf("newPermissionCardCancelledJSON() is not valid JSON: %v", err)
	}
	for _, want := range []string{"权限请求已取消", "Run tests"} {
		if !jsonContainsSubstring(card, want) {
			t.Fatalf("cancelled permission card does not contain %q: %#v", want, card)
		}
	}
	if jsonContainsSubstring(card, "**状态**") {
		t.Fatalf("cancelled permission card should not contain status field: %#v", card)
	}
	if jsonContainsTaggedElement(card, "button") || jsonContainsValue(card, "选择 1") {
		t.Fatalf("cancelled permission card should hide buttons: %#v", card)
	}
}

func TestNewModelSelectionCardJSONContainsDropdownAndCallbackContext(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newModelSelectionCardJSON(ModelSelectionCard{
		BotID:        "default",
		ChatID:       "oc_chat",
		ThreadID:     "omt_thread",
		ACPSessionID: "session-1",
		RequesterID:  "ou_requester",
		CurrentModel: "gpt-5.5",
		Options: []ModelOption{
			{Value: "gpt-5.5", Name: "GPT-5.5"},
			{Value: "gpt-5.6", Name: "GPT-5.6"},
		},
	})), &card); err != nil {
		t.Fatalf("newModelSelectionCardJSON() is not valid JSON: %v", err)
	}

	for _, want := range []string{
		"select_static",
		"gpt-5.5",
		"GPT-5.6（gpt-5.6）",
		modelSelectionCardAction,
		"session-1",
		"ou_requester",
	} {
		if !jsonContainsValue(card, want) {
			t.Fatalf("model card does not contain %q: %#v", want, card)
		}
	}
}

func TestModelSelectionCardActionSetsModelAndReplacesDropdown(t *testing.T) {
	handler := &fakeModelSelectionHandler{display: "GPT-5.6（gpt-5.6）"}
	adapter := &Adapter{handler: handler}
	resp, err := adapter.handleCardAction(nil, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_requester"},
			Action: &callback.CallBackAction{
				Tag:    "select_static",
				Option: "gpt-5.6",
				Value: map[string]interface{}{
					"action":         modelSelectionCardAction,
					"bot_id":         "default",
					"chat_id":        "oc_chat",
					"thread_id":      "omt_thread",
					"acp_session_id": "session-1",
					"requester_id":   "ou_requester",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("handleCardAction() error = %v", err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "success" || resp.Card == nil {
		t.Fatalf("response = %+v, want success card replacement", resp)
	}
	if handler.selection.Model != "gpt-5.6" || handler.selection.OperatorID != "ou_requester" {
		t.Fatalf("selection = %+v, want selected model and operator", handler.selection)
	}
	if jsonContainsValue(resp.Card.Data, "select_static") {
		t.Fatalf("completed card still contains dropdown: %#v", resp.Card.Data)
	}
	completedData, err := json.Marshal(resp.Card.Data)
	if err != nil {
		t.Fatalf("marshal completed card: %v", err)
	}
	if !strings.Contains(string(completedData), "已设置为 GPT-5.6（gpt-5.6）") {
		t.Fatalf("completed card = %s, want selected model", completedData)
	}
}

func TestNewModeSelectionCardJSONContainsDropdownAndCallbackContext(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newModeSelectionCardJSON(ModeSelectionCard{
		BotID:        "default",
		ChatID:       "oc_chat",
		ThreadID:     "omt_thread",
		ACPSessionID: "session-1",
		RequesterID:  "ou_requester",
		CurrentMode:  "default",
		Options: []ModeOption{
			{Value: "default", Name: "Default"},
			{Value: "plan", Name: "Plan"},
		},
	})), &card); err != nil {
		t.Fatalf("newModeSelectionCardJSON() is not valid JSON: %v", err)
	}

	for _, want := range []string{
		"select_static",
		"default",
		"Plan（plan）",
		modeSelectionCardAction,
		"session-1",
		"ou_requester",
	} {
		if !jsonContainsValue(card, want) {
			t.Fatalf("mode card does not contain %q: %#v", want, card)
		}
	}
}

func TestModeSelectionCardActionSetsModeAndReplacesDropdown(t *testing.T) {
	handler := &fakeModeSelectionHandler{display: "Plan（plan）"}
	adapter := &Adapter{handler: handler}
	resp, err := adapter.handleCardAction(nil, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_requester"},
			Action: &callback.CallBackAction{
				Tag:    "select_static",
				Option: "plan",
				Value: map[string]interface{}{
					"action":         modeSelectionCardAction,
					"bot_id":         "default",
					"chat_id":        "oc_chat",
					"thread_id":      "omt_thread",
					"acp_session_id": "session-1",
					"requester_id":   "ou_requester",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("handleCardAction() error = %v", err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "success" || resp.Card == nil {
		t.Fatalf("response = %+v, want success card replacement", resp)
	}
	if handler.selection.Mode != "plan" || handler.selection.OperatorID != "ou_requester" {
		t.Fatalf("selection = %+v, want selected mode and operator", handler.selection)
	}
	if jsonContainsValue(resp.Card.Data, "select_static") {
		t.Fatalf("completed card still contains dropdown: %#v", resp.Card.Data)
	}
	completedData, err := json.Marshal(resp.Card.Data)
	if err != nil {
		t.Fatalf("marshal completed card: %v", err)
	}
	if !strings.Contains(string(completedData), "已设置为 Plan（plan）") {
		t.Fatalf("completed card = %s, want selected mode", completedData)
	}
}

type fakeModelSelectionHandler struct {
	selection ModelSelection
	display   string
	err       error
}

func (f *fakeModelSelectionHandler) HandleFeishuMessage(context.Context, Message) (string, error) {
	return "", nil
}

func (f *fakeModelSelectionHandler) HandleModelSelection(ctx context.Context, selection ModelSelection) (string, error) {
	f.selection = selection
	return f.display, f.err
}

type fakeModeSelectionHandler struct {
	selection ModeSelection
	display   string
	err       error
}

func (f *fakeModeSelectionHandler) HandleFeishuMessage(context.Context, Message) (string, error) {
	return "", nil
}

func (f *fakeModeSelectionHandler) HandleModeSelection(ctx context.Context, selection ModeSelection) (string, error) {
	f.selection = selection
	return f.display, f.err
}

func jsonContainsValue(v any, want string) bool {
	switch value := v.(type) {
	case cardJSON:
		for _, child := range value {
			if jsonContainsValue(child, want) {
				return true
			}
		}
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
	case cardJSON:
		for _, child := range value {
			if jsonContainsSubstring(child, want) {
				return true
			}
		}
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

func jsonContainsTaggedElement(v any, tag string) bool {
	switch value := v.(type) {
	case cardJSON:
		if value["tag"] == tag {
			return true
		}
		for _, child := range value {
			if jsonContainsTaggedElement(child, tag) {
				return true
			}
		}
	case map[string]any:
		if value["tag"] == tag {
			return true
		}
		for _, child := range value {
			if jsonContainsTaggedElement(child, tag) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if jsonContainsTaggedElement(child, tag) {
				return true
			}
		}
	}
	return false
}

func jsonButtonContainsTopLevelValue(v any) bool {
	switch value := v.(type) {
	case cardJSON:
		if value["tag"] == "button" {
			_, ok := value["value"]
			return ok
		}
		for _, child := range value {
			if jsonButtonContainsTopLevelValue(child) {
				return true
			}
		}
	case map[string]any:
		if value["tag"] == "button" {
			_, ok := value["value"]
			return ok
		}
		for _, child := range value {
			if jsonButtonContainsTopLevelValue(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if jsonButtonContainsTopLevelValue(child) {
				return true
			}
		}
	}
	return false
}

func jsonContainsKey(v any, key string) bool {
	switch value := v.(type) {
	case cardJSON:
		if _, ok := value[key]; ok {
			return true
		}
		for _, child := range value {
			if jsonContainsKey(child, key) {
				return true
			}
		}
	case map[string]any:
		if _, ok := value[key]; ok {
			return true
		}
		for _, child := range value {
			if jsonContainsKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if jsonContainsKey(child, key) {
				return true
			}
		}
	}
	return false
}

func jsonContainsBool(v any, key string, want bool) bool {
	switch value := v.(type) {
	case cardJSON:
		if got, ok := value[key].(bool); ok && got == want {
			return true
		}
		for _, child := range value {
			if jsonContainsBool(child, key, want) {
				return true
			}
		}
	case map[string]any:
		if got, ok := value[key].(bool); ok && got == want {
			return true
		}
		for _, child := range value {
			if jsonContainsBool(child, key, want) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if jsonContainsBool(child, key, want) {
				return true
			}
		}
	}
	return false
}

func collectButtonTexts(v any) []string {
	switch value := v.(type) {
	case cardJSON:
		if value["tag"] == "button" {
			if text := plainTextContent(value["text"]); text != "" {
				return []string{text}
			}
		}
		var out []string
		for _, child := range value {
			out = append(out, collectButtonTexts(child)...)
		}
		return out
	case map[string]any:
		if value["tag"] == "button" {
			if text := plainTextContent(value["text"]); text != "" {
				return []string{text}
			}
		}
		var out []string
		for _, child := range value {
			out = append(out, collectButtonTexts(child)...)
		}
		return out
	case []any:
		var out []string
		for _, child := range value {
			out = append(out, collectButtonTexts(child)...)
		}
		return out
	default:
		return nil
	}
}

func plainTextContent(v any) string {
	switch value := v.(type) {
	case cardJSON:
		if text, ok := value["content"].(string); ok {
			return text
		}
	case map[string]any:
		if text, ok := value["content"].(string); ok {
			return text
		}
	}
	return ""
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
