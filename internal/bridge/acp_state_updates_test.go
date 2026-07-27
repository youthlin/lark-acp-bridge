package bridge

import (
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

func TestApplyACPStateUpdateAllowsEmptyLists(t *testing.T) {
	session := Session{
		AvailableCommands: []acp.AvailableCommand{{Name: "review"}},
		ConfigOptions: []acp.SessionConfigOption{
			{ID: "model", Name: "Model", Type: "select", CurrentValue: "gpt-5.5"},
		},
	}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{SessionUpdate: "available_commands_update"}) {
		t.Fatal("available_commands_update should be treated as changed even when empty")
	}
	if len(session.AvailableCommands) != 0 {
		t.Fatalf("AvailableCommands = %+v, want cleared", session.AvailableCommands)
	}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{SessionUpdate: "config_option_update"}) {
		t.Fatal("config_option_update should be treated as changed even when empty")
	}
	if len(session.ConfigOptions) != 0 {
		t.Fatalf("ConfigOptions = %+v, want cleared", session.ConfigOptions)
	}
}

func TestApplyACPStateUpdateReplacesConfigOptions(t *testing.T) {
	session := Session{
		ConfigOptions: []acp.SessionConfigOption{
			{ID: "model", Name: "Model", Type: "select", CurrentValue: "gpt-5.5"},
			{ID: "mode", Name: "Mode", Type: "select", CurrentValue: "ask"},
		},
	}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "config_option_update",
		ConfigOptions: []acp.SessionConfigOption{
			{ID: "reasoning", Name: "Reasoning", Type: "select", CurrentValue: "high"},
			{ID: "model", Name: "Model", Type: "select", CurrentValue: "gpt-5.6"},
		},
	}) {
		t.Fatal("config_option_update should be treated as changed")
	}
	if len(session.ConfigOptions) != 2 {
		t.Fatalf("ConfigOptions = %+v, want full replacement with two options", session.ConfigOptions)
	}
	if session.ConfigOptions[0].ID != "reasoning" || session.ConfigOptions[0].CurrentValue != "high" {
		t.Fatalf("first config option = %+v, want reasoning high", session.ConfigOptions[0])
	}
	if session.ConfigOptions[1].ID != "model" || session.ConfigOptions[1].CurrentValue != "gpt-5.6" {
		t.Fatalf("second config option = %+v, want model gpt-5.6", session.ConfigOptions[1])
	}
}

func TestApplyACPStateUpdatePersistsModeAndModelState(t *testing.T) {
	session := Session{}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_state_update",
		Models: &acp.SessionModelState{
			CurrentModelID: "gpt-5.5",
			AvailableModels: []acp.SessionModel{
				{ModelID: "gpt-5.5", Name: "GPT-5.5"},
			},
		},
		Mode: &acp.SessionModeState{
			CurrentModeID: "agent",
			AvailableModes: []acp.SessionMode{
				{ModeID: "agent", Name: "Agent"},
			},
		},
	}) {
		t.Fatal("session_state_update should update mode/model state")
	}
	if got := currentModelDisplay(session); got != "gpt-5.5" {
		t.Fatalf("currentModelDisplay = %q, want gpt-5.5", got)
	}
	if got := currentModeDisplay(session); got != "agent" {
		t.Fatalf("currentModeDisplay = %q, want agent", got)
	}
}

func TestApplyACPStateUpdatePersistsCurrentModeUpdate(t *testing.T) {
	session := Session{
		Mode: &acp.SessionModeState{
			CurrentModeID: "default",
			AvailableModes: []acp.SessionMode{
				{ModeID: "default", Name: "Default"},
				{ModeID: "plan", Name: "Plan"},
			},
		},
	}
	if !isACPStateUpdate(acp.SessionUpdate{SessionUpdate: "current_mode_update", ModeID: "plan"}) {
		t.Fatal("current_mode_update should be treated as state update")
	}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{SessionUpdate: "current_mode_update", ModeID: "plan"}) {
		t.Fatal("current_mode_update should update current mode")
	}
	if session.Mode == nil || session.Mode.CurrentModeID != "plan" {
		t.Fatalf("Mode = %+v, want current mode plan", session.Mode)
	}
	if len(session.Mode.AvailableModes) != 2 {
		t.Fatalf("AvailableModes = %+v, want preserved mode list", session.Mode.AvailableModes)
	}
}

func TestApplyACPStateUpdatePersistsSessionInfoTitle(t *testing.T) {
	session := Session{Title: "旧标题"}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		Title:         "新标题",
		TitleSet:      true,
	}) {
		t.Fatal("session_info_update should update title")
	}
	if session.Title != "新标题" {
		t.Fatalf("Title = %q, want 新标题", session.Title)
	}

	session.ManualTitle = true
	if applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		Title:         "自动标题",
		TitleSet:      true,
	}) {
		t.Fatal("manual title should not be overwritten by session_info_update")
	}
	if session.Title != "新标题" {
		t.Fatalf("Title = %q, want manual title preserved", session.Title)
	}
}

func TestApplyACPStateUpdateClearsSessionInfoTitle(t *testing.T) {
	session := Session{Title: "旧标题"}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		TitleSet:      true,
	}) {
		t.Fatal("session_info_update should clear title when title is null")
	}
	if session.Title != "" {
		t.Fatalf("Title = %q, want cleared", session.Title)
	}

	session = Session{Title: "手动标题", ManualTitle: true}
	if applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		TitleSet:      true,
	}) {
		t.Fatal("manual title should not be cleared by session_info_update")
	}
	if session.Title != "手动标题" {
		t.Fatalf("Title = %q, want manual title preserved", session.Title)
	}
}

func TestApplyACPStateUpdatePersistsACPUpdatedAt(t *testing.T) {
	session := Session{ACPUpdatedAt: "2025-10-29T14:22:15Z"}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		UpdatedAt:     "2025-10-29T15:00:00Z",
		UpdatedAtSet:  true,
	}) {
		t.Fatal("session_info_update should update ACP updatedAt")
	}
	if session.ACPUpdatedAt != "2025-10-29T15:00:00Z" {
		t.Fatalf("ACPUpdatedAt = %q, want updated timestamp", session.ACPUpdatedAt)
	}

	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		UpdatedAtSet:  true,
	}) {
		t.Fatal("session_info_update should clear ACP updatedAt")
	}
	if session.ACPUpdatedAt != "" {
		t.Fatalf("ACPUpdatedAt = %q, want cleared", session.ACPUpdatedAt)
	}
}

func TestApplyACPStateUpdatePersistsACPMeta(t *testing.T) {
	session := Session{ACPMeta: map[string]any{"old": true}}
	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		Meta: map[string]any{
			"priority": "high",
		},
	}) {
		t.Fatal("session_info_update should update ACP meta")
	}
	if got, ok := session.ACPMeta["priority"]; !ok || got != "high" {
		t.Fatalf("ACPMeta[priority] = %v, want high", got)
	}
	if _, ok := session.ACPMeta["old"]; ok {
		t.Fatalf("ACPMeta retained old key: %+v", session.ACPMeta)
	}

	if !applyACPStateUpdate(&session, acp.SessionUpdate{
		SessionUpdate: "session_info_update",
		Meta:          map[string]any{},
	}) {
		t.Fatal("session_info_update should clear ACP meta with empty object")
	}
	if len(session.ACPMeta) != 0 {
		t.Fatalf("ACPMeta = %+v, want empty", session.ACPMeta)
	}
}
