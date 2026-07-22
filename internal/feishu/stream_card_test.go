package feishu

import (
	"encoding/json"
	"testing"
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
