package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
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
	if !jsonContainsValue(card, streamCardInitialStatus) {
		t.Fatalf("initial stream card does not contain running status")
	}
	if !jsonElementFieldEquals(card, streamCardStatusElementID, "text_align", "left") {
		t.Fatalf("initial stream card status bar should align left: %#v", card)
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
	if jsonContainsValue(card, streamCardInitialStatus) {
		t.Fatalf("stream card should not contain running status")
	}
}

func TestNewStreamCardJSONCanIncludeHeaderAndFooter(t *testing.T) {
	var card any
	data := newStreamCardJSONFromState("", "", streamCardInitialStatus, "", true, true, false, true, StreamCardMeta{
		Title:    "定时任务执行结果",
		Subtitle: "task-id: daily",
		Footer:   "本消息的回复链将在本次执行会话中处理。",
	})
	if err := json.Unmarshal([]byte(data), &card); err != nil {
		t.Fatalf("newStreamCardJSONFromState() is not valid JSON: %v", err)
	}

	for _, want := range []string{"定时任务执行结果", "task-id: daily", "本消息的回复链将在本次执行会话中处理。", streamCardFooterElementID} {
		if !jsonContainsValue(card, want) {
			t.Fatalf("stream card meta JSON does not contain %q: %#v", want, card)
		}
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

func TestNewStreamCardJSONFromStateRendersNormalFinalSnapshot(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newStreamCardJSONFromState(
		"最终回复",
		"执行过程",
		"已完成 | 1K, 2K",
		"```json\n{}\n```",
		true,
		true,
		true,
		false,
		StreamCardMeta{},
	)), &card); err != nil {
		t.Fatalf("newStreamCardJSONFromState() is not valid JSON: %v", err)
	}

	for _, want := range []string{"最终回复", "执行过程", "已完成 | 1K, 2K", "用量明细"} {
		if !jsonContainsValue(card, want) {
			t.Fatalf("normal final card does not contain %q: %#v", want, card)
		}
	}
	if !jsonContainsSubstring(card, "```json") {
		t.Fatalf("normal final card does not contain usage detail content: %#v", card)
	}
	if !jsonContainsBool(card, "streaming_mode", false) {
		t.Fatalf("normal final card should disable streaming_mode: %#v", card)
	}
	if jsonContainsKey(card, "streaming_config") {
		t.Fatalf("normal final card should not contain streaming_config: %#v", card)
	}
}

func TestNewLoopStatusCardJSONUsesUpdatableNonStreamingElement(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newLoopStatusCardJSON(LoopStatusCardRequest{
		BotID:        "default",
		ChatID:       "oc_chat",
		ThreadID:     "omt_thread",
		ACPSessionID: "session-1",
		Text:         "已启动 loop。",
	}, false)), &card); err != nil {
		t.Fatalf("newLoopStatusCardJSON() is not valid JSON: %v", err)
	}

	if !jsonContainsValue(card, loopStatusCardTextElementID) {
		t.Fatalf("loop status card does not contain updatable element %q: %#v", loopStatusCardTextElementID, card)
	}
	if !jsonContainsValue(card, "已启动 loop。") {
		t.Fatalf("loop status card does not contain initial text: %#v", card)
	}
	for _, want := range []string{loopStatusCardAction, "取消 loop", "session-1"} {
		if !jsonContainsValue(card, want) {
			t.Fatalf("loop status card does not contain %q: %#v", want, card)
		}
	}
	if jsonContainsKey(card, "streaming_mode") {
		t.Fatalf("loop status card should be a normal non-streaming card: %#v", card)
	}
	if jsonContainsKey(card, "streaming_config") {
		t.Fatalf("loop status card should not contain streaming_config: %#v", card)
	}
}

func TestNewLoopStatusCardJSONFinishedHidesCancelButton(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newLoopStatusCardJSON(LoopStatusCardRequest{
		BotID:        "default",
		ChatID:       "oc_chat",
		ThreadID:     "omt_thread",
		ACPSessionID: "session-1",
		Text:         "loop 已结束。",
	}, true)), &card); err != nil {
		t.Fatalf("newLoopStatusCardJSON(finished) is not valid JSON: %v", err)
	}
	for _, want := range []string{"Loop 已结束", "loop 已结束。", loopStatusCardTextElementID} {
		if !jsonContainsValue(card, want) {
			t.Fatalf("finished loop status card does not contain %q: %#v", want, card)
		}
	}
	if jsonContainsTaggedElement(card, "button") || jsonContainsValue(card, loopStatusCardAction) {
		t.Fatalf("finished loop status card should hide cancel button: %#v", card)
	}
}

func TestNewLoopStatusCardJSONHidesCancelButtonWithoutCallbackTarget(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newLoopStatusCardJSON(LoopStatusCardRequest{
		BotID: "default",
		Text:  "已启动 loop。",
	}, false)), &card); err != nil {
		t.Fatalf("newLoopStatusCardJSON(missing target) is not valid JSON: %v", err)
	}
	if !jsonContainsValue(card, "已启动 loop。") {
		t.Fatalf("loop status card should still contain status text: %#v", card)
	}
	if jsonContainsTaggedElement(card, "button") || jsonContainsValue(card, loopStatusCardAction) {
		t.Fatalf("loop status card without callback target should hide cancel button: %#v", card)
	}
}

func TestLoopStatusCardTextPatchJSONOnlyUpdatesContent(t *testing.T) {
	var patch map[string]any
	if err := json.Unmarshal([]byte(loopStatusCardTextPatchJSON("第 1 轮运行中")), &patch); err != nil {
		t.Fatalf("loopStatusCardTextPatchJSON() is not valid JSON: %v", err)
	}
	if got := patch["content"]; got != "第 1 轮运行中" {
		t.Fatalf("content = %v, want updated loop status", got)
	}
	if _, ok := patch["tag"]; ok {
		t.Fatalf("patch should not include tag: %#v", patch)
	}
	if _, ok := patch["element_id"]; ok {
		t.Fatalf("patch should not include element_id: %#v", patch)
	}
}

func TestStreamCardUpdateContentNeverEmpty(t *testing.T) {
	if got := streamCardUpdateContent("  \n\t"); strings.TrimSpace(got) == "" {
		t.Fatalf("streamCardUpdateContent(empty) = %q, want non-empty content for Feishu field validation", got)
	}
	if got := streamCardUpdateContent("回复内容"); got != "回复内容" {
		t.Fatalf("streamCardUpdateContent(non-empty) = %q, want original text", got)
	}
}

func TestTruncateCardKitLogValue(t *testing.T) {
	if got := truncateCardKitLogValue(cardJSON{"content": "hello"}); !strings.Contains(got, `"content":"hello"`) {
		t.Fatalf("truncateCardKitLogValue(map) = %q, want JSON content", got)
	}

	got := truncateCardKitLogValue(strings.Repeat("你", 2001))
	if !strings.HasSuffix(got, "...<truncated>") {
		t.Fatalf("truncateCardKitLogValue(long) = %q, want truncated suffix", got)
	}
	if runes := len([]rune(strings.TrimSuffix(got, "...<truncated>"))); runes != 2000 {
		t.Fatalf("truncated rune count = %d, want 2000", runes)
	}
}

func TestCardNameForError(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "chinese", in: "流式", want: "流式"},
		{name: "ascii", in: "loop 状态", want: " loop 状态"},
		{name: "blank", in: " \t\n", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cardNameForError(tt.in); got != tt.want {
				t.Fatalf("cardNameForError(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizedCardIDTrimsAndRejectsBlank(t *testing.T) {
	if got := normalizedCardID(nil); got != "" {
		t.Fatalf("normalizedCardID(nil) = %q, want empty", got)
	}
	blank := " \t\n "
	if got := normalizedCardID(&blank); got != "" {
		t.Fatalf("normalizedCardID(blank) = %q, want empty", got)
	}
	cardID := "  card-1 \n"
	if got := normalizedCardID(&cardID); got != "card-1" {
		t.Fatalf("normalizedCardID() = %q, want trimmed card id", got)
	}
}

func TestCardSequencesIncrease(t *testing.T) {
	stream := &sdkStreamCard{}
	if got := []int{stream.nextSequenceLocked(), stream.nextSequenceLocked(), stream.nextSequenceLocked()}; got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("stream sequence = %v, want 1,2,3", got)
	}

	loop := &sdkLoopStatusCard{}
	if got := []int{loop.nextSequenceLocked(), loop.nextSequenceLocked()}; got[0] != 1 || got[1] != 2 {
		t.Fatalf("loop sequence = %v, want 1,2", got)
	}
}

func TestSDKStreamCardFullCardJSONIncludesSnapshotPanels(t *testing.T) {
	card := &sdkStreamCard{
		text:        "最终回复",
		process:     "执行过程",
		status:      "已完成",
		usageDetail: "usage detail",
	}
	var payload any
	if err := json.Unmarshal([]byte(card.fullCardJSONLocked()), &payload); err != nil {
		t.Fatalf("fullCardJSONLocked() is not valid JSON: %v", err)
	}

	for _, want := range []string{"最终回复", "执行过程", "已完成", "usage detail", streamCardProcessPanelID, streamCardUsagePanelID} {
		if !jsonContainsValue(payload, want) {
			t.Fatalf("full card snapshot does not contain %q: %#v", want, payload)
		}
	}
	if !jsonContainsBool(payload, "streaming_mode", false) {
		t.Fatalf("full card snapshot should disable streaming_mode: %#v", payload)
	}
}

func TestSDKStreamCardFullCardJSONUsesUpdatedMeta(t *testing.T) {
	card := &sdkStreamCard{
		text: "最终回复",
		meta: StreamCardMeta{
			Title:    "定时任务已完成",
			Subtitle: "task-id: daily",
			Footer:   "本消息的回复链将在本次执行会话中处理。",
		},
	}
	var payload any
	if err := json.Unmarshal([]byte(card.fullCardJSONLocked()), &payload); err != nil {
		t.Fatalf("fullCardJSONLocked() is not valid JSON: %v", err)
	}

	for _, want := range []string{"定时任务已完成", "task-id: daily", "本消息的回复链将在本次执行会话中处理。"} {
		if !jsonContainsValue(payload, want) {
			t.Fatalf("full card snapshot does not contain updated meta %q: %#v", want, payload)
		}
	}
}

func TestSDKStreamCardFullCardJSONCanOmitHiddenStatusBar(t *testing.T) {
	card := &sdkStreamCard{
		text:           "最终回复",
		processCreated: true,
		process:        "执行过程",
	}
	var payload any
	if err := json.Unmarshal([]byte(card.fullCardJSONLocked()), &payload); err != nil {
		t.Fatalf("fullCardJSONLocked() is not valid JSON: %v", err)
	}

	if jsonContainsValue(payload, streamCardStatusElementID) || jsonContainsValue(payload, streamCardInitialStatus) {
		t.Fatalf("full card snapshot should keep hidden status bar omitted: %#v", payload)
	}
	if !jsonContainsValue(payload, streamCardProcessPanelID) {
		t.Fatalf("full card snapshot should keep process panel: %#v", payload)
	}
}

func TestIsStreamCardStreamingClosedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "timeout code text",
			err:  errors.New("更新飞书流式卡片组件返回错误: code=200850 msg=card streaming timeout"),
			want: true,
		},
		{
			name: "closed code error",
			err:  &larkcore.CodeError{Code: 300309, Msg: "streaming mode is closed"},
			want: true,
		},
		{
			name: "other error",
			err:  errors.New("code=999999 msg=other"),
		},
		{
			name: "nil",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStreamCardStreamingClosedError(tt.err); got != tt.want {
				t.Fatalf("isStreamCardStreamingClosedError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
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

func TestPermissionCardActionRejectsUnknownOption(t *testing.T) {
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
					"option_id":  "allow-always",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("handleCardAction() error = %v", err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "error" {
		t.Fatalf("response = %+v, want error toast", resp)
	}
	select {
	case outcome := <-waiter.once:
		t.Fatalf("waiter received unknown option outcome: %+v", outcome)
	default:
	}
	if _, ok := adapter.permissionCards.get("perm-1"); !ok {
		t.Fatal("unknown option should not remove permission request")
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

func TestNewSessionSelectionCardJSONContainsDropdownAndCallbackContext(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newSessionSelectionCardJSON(SessionSelectionCard{
		BotID:               "default",
		ChatID:              "oc_chat",
		ThreadID:            "omt_thread",
		GroupMessageType:    "thread",
		RequesterID:         "ou_requester",
		CurrentACPSessionID: "session-2",
		Options: []SessionOption{
			{ACPSessionID: "session-2", Title: "当前会话", Cwd: "/repo"},
			{ACPSessionID: "session-1", Title: "旧会话", Cwd: "/old"},
		},
	})), &card); err != nil {
		t.Fatalf("newSessionSelectionCardJSON() is not valid JSON: %v", err)
	}

	for _, want := range []string{
		"select_static",
		"当前会话 | /repo",
		sessionSelectionCardAction,
		"session-2",
		"ou_requester",
	} {
		if !jsonContainsValue(card, want) {
			t.Fatalf("session card does not contain %q: %#v", want, card)
		}
	}
	cardData, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal session card: %v", err)
	}
	for _, want := range []string{
		`"current_acp_session_id":"session-2"`,
		`"group_message_type":"thread"`,
	} {
		if !strings.Contains(string(cardData), want) {
			t.Fatalf("session card does not contain %s callback context: %s", want, cardData)
		}
	}
}

func TestNewConfigDetailCardJSONContainsConfigFieldsAndOptions(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newConfigDetailCardJSON(ConfigDetailCard{
		ID:           "model",
		Name:         "Model",
		Category:     "model",
		Description:  "Choose which model TRAE CLI should use",
		Type:         "select",
		CurrentValue: "gpt-5.5",
		Options: []ConfigOptionValue{
			{Value: "Doubao-Seed-2.1-Pro", Name: "Doubao-Seed-2.1-Pro", Description: "184K context window, support reasoning."},
			{Value: "gpt-5.5", Name: "GPT-5.5", Description: "support reasoning, beta.", Current: true},
		},
		SetCommand: "/config model <value>",
	})), &card); err != nil {
		t.Fatalf("newConfigDetailCardJSON() is not valid JSON: %v", err)
	}

	for _, want := range []string{
		"ACP 配置项：model",
		"Model | model | select",
		"Choose which model TRAE CLI should use",
		"GPT-5.5（gpt-5.5）",
		"**当前**",
		"/config model <value>",
	} {
		if !jsonContainsSubstring(card, want) {
			t.Fatalf("config detail card does not contain %q: %#v", want, card)
		}
	}
}

func TestNewOverviewCardJSONContainsActionsAndCallbackContext(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newOverviewCardJSON(OverviewCard{
		BotID:               "default",
		ChatID:              "oc_chat",
		ChatType:            "topic_group",
		ThreadID:            "omt_thread",
		GroupMessageType:    "thread",
		RequesterID:         "ou_requester",
		CurrentACPSessionID: "session-1",
		HasSession:          true,
		SessionTitle:        "当前会话",
		AgentName:           "traex",
		ChatAgentName:       "traex",
		Cwd:                 "/repo",
		Model:               "gpt-5.5",
		Mode:                "default",
		ContextUsage:        "160K/200K",
		CompactStatus:       "开启，阈值 80%",
		RuntimeStatus:       "空闲",
		QueueStatus:         "待执行 0 条",
		WikiStatus:          "开启",
		LoopStatus:          "尚无状态",
		ACPErrorStatus:      "无",
		AtStatus:            "需要 at",
		Show:                OverviewShowOptions{Step: true, Plan: true, Tool: true, Status: true, Used: true},
		WikiEnabled:         true,
		AgentOptions: []OverviewOption{
			{Value: "traex", Text: "traex", Current: true},
			{Value: "codex", Text: "codex"},
		},
		SessionOptions: []SessionOption{
			{ACPSessionID: "session-1", Title: "当前会话", Cwd: "/repo"},
			{ACPSessionID: "session-2", Title: "旧会话", Cwd: "/old"},
		},
		AtOptions: []OverviewOption{
			{Value: "on", Text: "需要@才响应 /at on", Current: true},
			{Value: "every", Text: "无需@每次响应 /at off every"},
			{Value: "auto", Text: "无需@且静默自动判断 /at off auto"},
			{Value: "auto-reaction", Text: "无需@自动判断带表情 /at off auto-reaction"},
		},
		ModelOptions: []ModelOption{
			{Value: "gpt-5.5", Name: "GPT-5.5"},
			{Value: "gpt-5.6", Name: "GPT-5.6"},
		},
		ModeOptions: []ModeOption{
			{Value: "default", Name: "Default"},
			{Value: "plan", Name: "Plan"},
		},
		CommandHints: []string{
			"/new [cwd] [title]",
			"/schedule how 定时任务描述",
			"/loop how 循环任务描述",
			"/compact on 80%",
			"/queue <下一轮执行>",
		},
		CommandNotes: []string{
			"直接发消息即可打断当前执行轮次。",
		},
	})), &card); err != nil {
		t.Fatalf("newOverviewCardJSON() is not valid JSON: %v", err)
	}

	for _, want := range []string{
		"当前聊天全览",
		"当前会话",
		"gpt-5.5",
		"切换当前会话模型",
		"切换当前会话模式",
		"切换群聊响应策略",
		"需要@才响应 /at on",
		"无需@自动判断带表情 /at off auto-reaction",
		"旧会话",
		"compact",
		"知识库：开启",
		"知识沉淀已开启",
		"/schedule how 定时任务描述",
		"/loop how 循环任务描述",
		"/compact on 80%",
		"/queue <下一轮执行>",
		"直接发消息即可打断当前执行轮次。",
		"新会话",
		"用量",
		"帮助",
	} {
		if !jsonContainsSubstring(card, want) {
			t.Fatalf("overview card does not contain %q: %#v", want, card)
		}
	}
	for _, want := range []string{
		overviewCardAction,
		"set_model",
		"set_mode",
		"set_at",
		"toggle_show",
		"toggle_wiki",
		"new_session",
		"show_usage",
		"show_help",
		"set_agent",
		"set_session",
		"session-1",
		"session-2",
		"ou_requester",
	} {
		if !jsonContainsValue(card, want) {
			t.Fatalf("overview card callback does not contain %q: %#v", want, card)
		}
	}
	for _, unexpected := range []string{"**聊天配置**", "上下文：", "过程 开启", "计划 关闭", "open_model", "open_mode", "wiki 关闭", "状态栏"} {
		if jsonContainsSubstring(card, unexpected) {
			t.Fatalf("overview card should not contain %q: %#v", unexpected, card)
		}
	}
	for text, wantType := range map[string]string{
		"过程":      "primary",
		"计划":      "primary",
		"思考":      "default",
		"状态":      "primary",
		"知识沉淀已开启": "primary",
	} {
		if got := jsonButtonTypeByText(card, text); got != wantType {
			t.Fatalf("button %q type = %q, want %q", text, got, wantType)
		}
	}
	cardData, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal overview card: %v", err)
	}
	for _, want := range []string{
		`"chat_type":"topic_group"`,
		`"group_message_type":"thread"`,
		`"current_acp_session_id":"session-1"`,
	} {
		if !strings.Contains(string(cardData), want) {
			t.Fatalf("overview card does not contain %s callback context: %s", want, cardData)
		}
	}
}

func TestNewOverviewCardJSONOmitsUnsupportedModelAndModeDropdowns(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newOverviewCardJSON(OverviewCard{
		BotID:               "default",
		ChatID:              "oc_chat",
		RequesterID:         "ou_requester",
		CurrentACPSessionID: "session-1",
		HasSession:          true,
		SessionTitle:        "当前会话",
		Model:               "gpt-5.5",
		Mode:                "default",
		WikiEnabled:         false,
	})), &card); err != nil {
		t.Fatalf("newOverviewCardJSON() is not valid JSON: %v", err)
	}
	for _, unexpected := range []string{"overview_model", "overview_mode", "set_model", "set_mode"} {
		if jsonContainsValue(card, unexpected) {
			t.Fatalf("overview card should omit unsupported model/mode dropdown %q: %#v", unexpected, card)
		}
	}
	if got := jsonButtonTypeByText(card, "知识沉淀已关闭"); got != "default" {
		t.Fatalf("wiki disabled button type = %q, want default", got)
	}
}

func TestNewOverviewCardJSONElementIDsFollowCardKitRules(t *testing.T) {
	var card any
	if err := json.Unmarshal([]byte(newOverviewCardJSON(OverviewCard{
		BotID:               "default",
		ChatID:              "oc_chat",
		RequesterID:         "ou_requester",
		CurrentACPSessionID: "session-1",
		AgentOptions: []OverviewOption{
			{Value: "traex", Text: "traex", Current: true},
			{Value: "codex", Text: "codex"},
		},
		SessionOptions: []SessionOption{
			{ACPSessionID: "session-1", Title: "当前会话", Cwd: "/repo"},
			{ACPSessionID: "session-2", Title: "旧会话", Cwd: "/old"},
		},
		ModelOptions: []ModelOption{
			{Value: "gpt-5.5", Name: "GPT-5.5"},
			{Value: "gpt-5.6", Name: "GPT-5.6"},
		},
		ModeOptions: []ModeOption{
			{Value: "default", Name: "Default"},
			{Value: "plan", Name: "Plan"},
		},
		AtOptions: []OverviewOption{
			{Value: "on", Text: "需要@才响应 /at on", Current: true},
			{Value: "auto", Text: "无需@且静默自动判断 /at off auto"},
		},
	})), &card); err != nil {
		t.Fatalf("newOverviewCardJSON() is not valid JSON: %v", err)
	}

	assertCardElementIDFormat(t, card)
	if !jsonContainsValue(card, "overview_agent") {
		t.Fatalf("overview card does not contain agent select element id: %#v", card)
	}
	if jsonContainsValue(card, "overview_agent_select") {
		t.Fatalf("overview card contains invalid old agent select element id: %#v", card)
	}
	if !jsonContainsValue(card, "overview_session") {
		t.Fatalf("overview card does not contain session select element id: %#v", card)
	}
	if !jsonContainsValue(card, "overview_at") {
		t.Fatalf("overview card does not contain at select element id: %#v", card)
	}
	if !jsonContainsValue(card, "overview_model") {
		t.Fatalf("overview card does not contain model select element id: %#v", card)
	}
	if !jsonContainsValue(card, "overview_mode") {
		t.Fatalf("overview card does not contain mode select element id: %#v", card)
	}
}

func TestOverviewCardActionUpdatesAndReplacesCard(t *testing.T) {
	handler := &fakeOverviewActionHandler{
		result: OverviewActionResult{
			ToastType: "success",
			Toast:     "展示配置已更新",
			Overview: &OverviewCard{
				BotID:               "default",
				ChatID:              "oc_chat",
				ThreadID:            "omt_thread",
				GroupMessageType:    "thread",
				RequesterID:         "ou_requester",
				CurrentACPSessionID: "session-1",
				ChatAgentName:       "traex",
				RuntimeStatus:       "空闲",
				QueueStatus:         "待执行 0 条",
				WikiStatus:          "开启",
				LoopStatus:          "尚无状态",
				ACPErrorStatus:      "无",
				AtStatus:            "需要 at",
				Show:                OverviewShowOptions{Plan: true, Tool: true, Status: true, Used: true},
				WikiEnabled:         true,
			},
		},
	}
	adapter := &Adapter{handler: handler}
	resp, err := adapter.handleCardAction(nil, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_requester"},
			Action: &callback.CallBackAction{
				Value: map[string]interface{}{
					"action":                 overviewCardAction,
					"overview_action":        "toggle_show",
					"target":                 "step",
					"value":                  "off",
					"bot_id":                 "default",
					"chat_id":                "oc_chat",
					"thread_id":              "omt_thread",
					"group_message_type":     "thread",
					"requester_id":           "ou_requester",
					"current_acp_session_id": "session-1",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("handleCardAction() error = %v", err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "展示配置已更新" || resp.Card == nil {
		t.Fatalf("response = %+v, want success card replacement", resp)
	}
	if handler.action.OperatorID != "ou_requester" ||
		handler.action.Action != "toggle_show" ||
		handler.action.Target != "step" ||
		handler.action.Value != "off" ||
		handler.action.CurrentACPSessionID != "session-1" ||
		handler.action.GroupMessageType != "thread" {
		t.Fatalf("overview action = %+v, want callback context", handler.action)
	}
	if !jsonContainsValue(resp.Card.Data, "当前聊天全览") || !jsonContainsValue(resp.Card.Data, overviewCardAction) {
		t.Fatalf("replacement card = %#v, want overview card", resp.Card.Data)
	}
}

func TestOverviewCardSessionDropdownUpdatesAndReplacesCard(t *testing.T) {
	handler := &fakeOverviewActionHandler{
		result: OverviewActionResult{
			ToastType: "success",
			Toast:     "会话已恢复",
			Overview: &OverviewCard{
				BotID:               "default",
				ChatID:              "oc_chat",
				ChatType:            "topic_group",
				ThreadID:            "omt_thread",
				GroupMessageType:    "thread",
				RequesterID:         "ou_requester",
				CurrentACPSessionID: "session-1",
				HasSession:          true,
				SessionTitle:        "旧会话",
				ChatAgentName:       "traex",
				RuntimeStatus:       "空闲",
				QueueStatus:         "待执行 0 条",
				WikiStatus:          "开启",
				LoopStatus:          "尚无状态",
				ACPErrorStatus:      "无",
				AtStatus:            "需要 at",
				Show:                OverviewShowOptions{Plan: true, Tool: true, Status: true, Used: true},
				WikiEnabled:         true,
			},
		},
	}
	adapter := &Adapter{handler: handler}
	resp, err := adapter.handleCardAction(nil, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_requester"},
			Action: &callback.CallBackAction{
				Tag:    "select_static",
				Option: "session-1",
				Value: map[string]interface{}{
					"action":                 overviewCardAction,
					"overview_action":        "set_session",
					"bot_id":                 "default",
					"chat_id":                "oc_chat",
					"chat_type":              "topic_group",
					"thread_id":              "omt_thread",
					"group_message_type":     "thread",
					"requester_id":           "ou_requester",
					"current_acp_session_id": "session-2",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("handleCardAction() error = %v", err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "会话已恢复" || resp.Card == nil {
		t.Fatalf("response = %+v, want success overview card replacement", resp)
	}
	if handler.action.Action != "set_session" ||
		handler.action.Value != "session-1" ||
		handler.action.CurrentACPSessionID != "session-2" ||
		handler.action.ChatType != "topic_group" ||
		handler.action.OperatorID != "ou_requester" {
		t.Fatalf("overview action = %+v, want session dropdown context", handler.action)
	}
	if !jsonContainsValue(resp.Card.Data, "当前聊天全览") || !jsonContainsSubstring(resp.Card.Data, "旧会话") {
		t.Fatalf("replacement card = %#v, want refreshed overview card", resp.Card.Data)
	}
}

func TestOverviewCardModelDropdownUpdatesAndReplacesCard(t *testing.T) {
	handler := &fakeOverviewActionHandler{
		result: OverviewActionResult{
			ToastType: "success",
			Toast:     "模型已设置：GPT-5.6（gpt-5.6）",
			Overview: &OverviewCard{
				BotID:               "default",
				ChatID:              "oc_chat",
				ChatType:            "topic_group",
				ThreadID:            "omt_thread",
				GroupMessageType:    "thread",
				RequesterID:         "ou_requester",
				CurrentACPSessionID: "session-1",
				HasSession:          true,
				SessionTitle:        "当前会话",
				ChatAgentName:       "traex",
				Model:               "gpt-5.6",
				ModelOptions: []ModelOption{
					{Value: "gpt-5.5", Name: "GPT-5.5"},
					{Value: "gpt-5.6", Name: "GPT-5.6"},
				},
				RuntimeStatus:  "空闲",
				QueueStatus:    "待执行 0 条",
				WikiStatus:     "开启",
				LoopStatus:     "尚无状态",
				ACPErrorStatus: "无",
				AtStatus:       "需要 at",
				Show:           OverviewShowOptions{Plan: true, Tool: true, Status: true, Used: true},
				WikiEnabled:    true,
			},
		},
	}
	adapter := &Adapter{handler: handler}
	resp, err := adapter.handleCardAction(nil, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_requester"},
			Action: &callback.CallBackAction{
				Tag:    "select_static",
				Option: "gpt-5.6",
				Value: map[string]interface{}{
					"action":                 overviewCardAction,
					"overview_action":        "set_model",
					"bot_id":                 "default",
					"chat_id":                "oc_chat",
					"chat_type":              "topic_group",
					"thread_id":              "omt_thread",
					"group_message_type":     "thread",
					"requester_id":           "ou_requester",
					"current_acp_session_id": "session-1",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("handleCardAction() error = %v", err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "模型已设置：GPT-5.6（gpt-5.6）" || resp.Card == nil {
		t.Fatalf("response = %+v, want success overview card replacement", resp)
	}
	if handler.action.Action != "set_model" ||
		handler.action.Value != "gpt-5.6" ||
		handler.action.CurrentACPSessionID != "session-1" ||
		handler.action.OperatorID != "ou_requester" {
		t.Fatalf("overview action = %+v, want model dropdown context", handler.action)
	}
	if !jsonContainsValue(resp.Card.Data, "overview_model") || !jsonContainsValue(resp.Card.Data, "set_model") {
		t.Fatalf("replacement card = %#v, want refreshed model dropdown", resp.Card.Data)
	}
}

func TestSessionSelectionCardActionResumesSessionAndReplacesDropdown(t *testing.T) {
	handler := &fakeSessionSelectionHandler{display: "旧会话"}
	adapter := &Adapter{handler: handler}
	resp, err := adapter.handleCardAction(nil, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_owner"},
			Action: &callback.CallBackAction{
				Tag:    "select_static",
				Option: "session-1",
				Value: map[string]interface{}{
					"action":                 sessionSelectionCardAction,
					"bot_id":                 "default",
					"chat_id":                "oc_chat",
					"thread_id":              "omt_thread",
					"group_message_type":     "thread",
					"requester_id":           "ou_requester",
					"current_acp_session_id": "session-2",
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
	if handler.selection.ACPSessionID != "session-1" ||
		handler.selection.CurrentACPSessionID != "session-2" ||
		handler.selection.GroupMessageType != "thread" ||
		handler.selection.OperatorID != "ou_owner" {
		t.Fatalf("selection = %+v, want selected session and operator", handler.selection)
	}
	if jsonContainsValue(resp.Card.Data, "select_static") {
		t.Fatalf("completed card still contains dropdown: %#v", resp.Card.Data)
	}
	completedData, err := json.Marshal(resp.Card.Data)
	if err != nil {
		t.Fatalf("marshal completed card: %v", err)
	}
	if !strings.Contains(string(completedData), "已恢复 旧会话") {
		t.Fatalf("completed card = %s, want selected session", completedData)
	}
}

func TestLoopStatusCardActionCancelsLoopAndHidesButton(t *testing.T) {
	handler := &fakeLoopCancelHandler{display: "loop 已结束。"}
	adapter := &Adapter{handler: handler}
	resp, err := adapter.handleCardAction(nil, &callback.CardActionTriggerEvent{
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_owner"},
			Action: &callback.CallBackAction{
				Value: map[string]interface{}{
					"action":         loopStatusCardAction,
					"bot_id":         "default",
					"chat_id":        "oc_chat",
					"thread_id":      "omt_thread",
					"acp_session_id": "session-1",
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
	if handler.cancel.OperatorID != "ou_owner" || handler.cancel.ACPSessionID != "session-1" {
		t.Fatalf("cancel = %+v, want callback context", handler.cancel)
	}
	if !jsonContainsValue(resp.Card.Data, "loop 已结束。") {
		t.Fatalf("completed loop card does not contain finished text: %#v", resp.Card.Data)
	}
	if jsonContainsTaggedElement(resp.Card.Data, "button") || jsonContainsValue(resp.Card.Data, loopStatusCardAction) {
		t.Fatalf("completed loop card should hide cancel button: %#v", resp.Card.Data)
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

type fakeSessionSelectionHandler struct {
	selection SessionSelection
	display   string
	err       error
}

func (f *fakeSessionSelectionHandler) HandleFeishuMessage(context.Context, Message) (string, error) {
	return "", nil
}

func (f *fakeSessionSelectionHandler) HandleSessionSelection(ctx context.Context, selection SessionSelection) (string, error) {
	f.selection = selection
	return f.display, f.err
}

type fakeOverviewActionHandler struct {
	action OverviewAction
	result OverviewActionResult
	err    error
}

func (f *fakeOverviewActionHandler) HandleFeishuMessage(context.Context, Message) (string, error) {
	return "", nil
}

func (f *fakeOverviewActionHandler) HandleOverviewAction(ctx context.Context, action OverviewAction) (OverviewActionResult, error) {
	f.action = action
	return f.result, f.err
}

type fakeLoopCancelHandler struct {
	cancel  LoopCancel
	display string
	err     error
}

func (f *fakeLoopCancelHandler) HandleFeishuMessage(context.Context, Message) (string, error) {
	return "", nil
}

func (f *fakeLoopCancelHandler) HandleLoopCancel(ctx context.Context, cancel LoopCancel) (string, error) {
	f.cancel = cancel
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

func jsonElementFieldEquals(v any, elementID string, field string, want string) bool {
	switch value := v.(type) {
	case cardJSON:
		if value["element_id"] == elementID {
			return value[field] == want
		}
		for _, child := range value {
			if jsonElementFieldEquals(child, elementID, field, want) {
				return true
			}
		}
	case map[string]any:
		if value["element_id"] == elementID {
			return value[field] == want
		}
		for _, child := range value {
			if jsonElementFieldEquals(child, elementID, field, want) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if jsonElementFieldEquals(child, elementID, field, want) {
				return true
			}
		}
	}
	return false
}

func jsonButtonTypeByText(v any, text string) string {
	switch value := v.(type) {
	case cardJSON:
		if value["tag"] == "button" && plainTextContent(value["text"]) == text {
			if buttonType, ok := value["type"].(string); ok {
				return buttonType
			}
			return ""
		}
		for _, child := range value {
			if buttonType := jsonButtonTypeByText(child, text); buttonType != "" {
				return buttonType
			}
		}
	case map[string]any:
		if value["tag"] == "button" && plainTextContent(value["text"]) == text {
			if buttonType, ok := value["type"].(string); ok {
				return buttonType
			}
			return ""
		}
		for _, child := range value {
			if buttonType := jsonButtonTypeByText(child, text); buttonType != "" {
				return buttonType
			}
		}
	case []any:
		for _, child := range value {
			if buttonType := jsonButtonTypeByText(child, text); buttonType != "" {
				return buttonType
			}
		}
	}
	return ""
}

func assertCardElementIDFormat(t *testing.T, v any) {
	t.Helper()

	validElementID := regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,19}$`)
	var walk func(any)
	walk = func(node any) {
		switch value := node.(type) {
		case cardJSON:
			if elementID, ok := value["element_id"].(string); ok && !validElementID.MatchString(elementID) {
				t.Fatalf("invalid element_id %q, must match %s", elementID, validElementID.String())
			}
			for _, child := range value {
				walk(child)
			}
		case map[string]any:
			if elementID, ok := value["element_id"].(string); ok && !validElementID.MatchString(elementID) {
				t.Fatalf("invalid element_id %q, must match %s", elementID, validElementID.String())
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(v)
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
