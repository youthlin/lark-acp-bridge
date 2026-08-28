package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestModelSelectionOptionsFallsBackToModels(t *testing.T) {
	session := Session{
		Models: &acp.SessionModelState{
			CurrentModelID: "gpt-5.5",
			AvailableModels: []acp.SessionModel{
				{ModelID: "gpt-5.5", Name: "GPT-5.5"},
				{ModelID: "gpt-5.6", Name: "GPT-5.6", Meta: traeLoadMeta(47)},
			},
		},
	}
	options := modelSelectionOptions(session, acp.SessionConfigOption{ID: "model", Category: "model"})
	if len(options) != 2 || options[1].Value != "gpt-5.6" || options[1].Name != "GPT-5.6" {
		t.Fatalf("options = %+v, want models fallback", options)
	}
	if options[1].LoadPercent == nil || *options[1].LoadPercent != 47 {
		t.Fatalf("options = %+v, want load percent 47", options)
	}
}

func TestModelSelectionOptionsIncludesTraeLoadFromConfigOptions(t *testing.T) {
	options := modelSelectionOptions(Session{}, acp.SessionConfigOption{
		ID:       "model",
		Category: "model",
		Options: []acp.SessionConfigOptionValue{
			{Value: "gpt-5.5", Name: "GPT-5.5", Meta: traeLoadMeta(10)},
		},
	})
	if len(options) != 1 || options[0].LoadPercent == nil || *options[0].LoadPercent != 10 {
		t.Fatalf("options = %+v, want config option load percent 10", options)
	}
}

func TestModelSelectionOptionsMergesTraeLoadFromModels(t *testing.T) {
	session := Session{
		Models: &acp.SessionModelState{
			CurrentModelID: "gpt-5.5/high",
			AvailableModels: []acp.SessionModel{
				{ModelID: "gpt-5.5", Name: "GPT-5.5", Meta: traeLoadMeta(10)},
				{ModelID: "gpt-5.6", Name: "GPT-5.6", Meta: traeLoadMeta(47)},
			},
		},
	}
	options := modelSelectionOptions(session, acp.SessionConfigOption{
		ID:           "model",
		Category:     "model",
		Type:         "select",
		CurrentValue: "gpt-5.5",
		Options: []acp.SessionConfigOptionValue{
			{Value: "gpt-5.5", Name: "GPT-5.5"},
			{Value: "gpt-5.6", Name: "GPT-5.6"},
		},
	})
	if len(options) != 2 {
		t.Fatalf("options = %+v, want 2 options", options)
	}
	if options[0].LoadPercent == nil || *options[0].LoadPercent != 10 {
		t.Fatalf("options = %+v, want gpt-5.5 load from models", options)
	}
	if options[1].LoadPercent == nil || *options[1].LoadPercent != 47 {
		t.Fatalf("options = %+v, want gpt-5.6 load from models", options)
	}
}

func traeLoadMeta(percent int) map[string]any {
	return map[string]any{
		"trae": map[string]any{
			"load": map[string]any{
				"percent": percent,
			},
		},
	}
}

func TestModeSelectionOptionsFallsBackToModeState(t *testing.T) {
	session := Session{
		Mode: &acp.SessionModeState{
			CurrentModeID: "default",
			AvailableModes: []acp.SessionMode{
				{ModeID: "default", Name: "Default"},
				{ModeID: "plan", Name: "Plan"},
			},
		},
	}
	options := modeSelectionOptions(session, acp.SessionConfigOption{ID: "mode", Category: "mode"})
	if len(options) != 2 || options[1].Value != "plan" || options[1].Name != "Plan" {
		t.Fatalf("options = %+v, want modes fallback", options)
	}
}

func TestHandleModelSelectionSetsModelAndRejectsStaleOrOtherUser(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.ConfigOptions = []acp.SessionConfigOption{
		{
			ID:           "model",
			Name:         "Model",
			Category:     "model",
			Type:         "select",
			CurrentValue: "gpt-5.5",
			Options: []acp.SessionConfigOptionValue{
				{Value: "gpt-5.5", Name: "GPT-5.5"},
				{Value: "gpt-5.6", Name: "GPT-5.6"},
			},
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{
		configOptions: []acp.SessionConfigOption{
			{
				ID:           "model",
				Name:         "Model",
				Category:     "model",
				Type:         "select",
				CurrentValue: "gpt-5.6",
				Options: []acp.SessionConfigOptionValue{
					{Value: "gpt-5.5", Name: "GPT-5.5"},
					{Value: "gpt-5.6", Name: "GPT-5.6"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	selection := feishu.ModelSelection{
		BotID:        session.Key.BotID,
		ChatID:       sessionKeyMainID(session.Key),
		ThreadID:     session.Key.SubID,
		ACPSessionID: session.ACPSessionID,
		RequesterID:  "ou_requester",
		OperatorID:   "ou_requester",
		Model:        "gpt-5.6",
	}

	display, err := svc.HandleModelSelection(context.Background(), selection)
	if err != nil {
		t.Fatalf("HandleModelSelection() error = %v", err)
	}
	if display != "GPT-5.6（gpt-5.6）" {
		t.Fatalf("display = %q, want friendly model display", display)
	}
	if len(rt.configCalls) != 1 || rt.configCalls[0].Value != "gpt-5.6" {
		t.Fatalf("configCalls = %+v, want gpt-5.6", rt.configCalls)
	}

	selection.OperatorID = "ou_other"
	if _, err := svc.HandleModelSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "只有发起") {
		t.Fatalf("other user error = %v, want requester validation", err)
	}

	selection.OperatorID = selection.RequesterID
	selection.RequesterID = ""
	if _, err := svc.HandleModelSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "缺少发起人或操作者") {
		t.Fatalf("missing requester error = %v, want requester metadata validation", err)
	}

	selection.RequesterID = "ou_requester"
	selection.OperatorID = ""
	if _, err := svc.HandleModelSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "缺少发起人或操作者") {
		t.Fatalf("missing operator error = %v, want operator metadata validation", err)
	}

	selection.OperatorID = selection.RequesterID
	selection.ACPSessionID = "stale-session"
	if _, err := svc.HandleModelSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("stale card error = %v, want expired validation", err)
	}
}

func TestHandleModeSelectionSetsModeAndRejectsStaleOrOtherUser(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.ConfigOptions = []acp.SessionConfigOption{
		{
			ID:           "mode",
			Name:         "Mode",
			Category:     "mode",
			Type:         "select",
			CurrentValue: "default",
			Options: []acp.SessionConfigOptionValue{
				{Value: "default", Name: "Default"},
				{Value: "plan", Name: "Plan"},
			},
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{
		configOptions: []acp.SessionConfigOption{
			{
				ID:           "mode",
				Name:         "Mode",
				Category:     "mode",
				Type:         "select",
				CurrentValue: "plan",
				Options: []acp.SessionConfigOptionValue{
					{Value: "default", Name: "Default"},
					{Value: "plan", Name: "Plan"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	selection := feishu.ModeSelection{
		BotID:        session.Key.BotID,
		ChatID:       sessionKeyMainID(session.Key),
		ThreadID:     session.Key.SubID,
		ACPSessionID: session.ACPSessionID,
		RequesterID:  "ou_requester",
		OperatorID:   "ou_requester",
		Mode:         "plan",
	}

	display, err := svc.HandleModeSelection(context.Background(), selection)
	if err != nil {
		t.Fatalf("HandleModeSelection() error = %v", err)
	}
	if display != "Plan（plan）" {
		t.Fatalf("display = %q, want friendly mode display", display)
	}
	if len(rt.configCalls) != 1 || rt.configCalls[0].Value != "plan" {
		t.Fatalf("configCalls = %+v, want plan", rt.configCalls)
	}

	selection.OperatorID = "ou_other"
	if _, err := svc.HandleModeSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "只有发起") {
		t.Fatalf("other user error = %v, want requester validation", err)
	}

	selection.OperatorID = selection.RequesterID
	selection.RequesterID = ""
	if _, err := svc.HandleModeSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "缺少发起人或操作者") {
		t.Fatalf("missing requester error = %v, want requester metadata validation", err)
	}

	selection.RequesterID = "ou_requester"
	selection.OperatorID = ""
	if _, err := svc.HandleModeSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "缺少发起人或操作者") {
		t.Fatalf("missing operator error = %v, want operator metadata validation", err)
	}

	selection.OperatorID = selection.RequesterID
	selection.ACPSessionID = "stale-session"
	if _, err := svc.HandleModeSelection(context.Background(), selection); err == nil || !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("stale card error = %v, want expired validation", err)
	}
}

func TestHandleModeSelectionFallsBackToLegacySetMode(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.ConfigOptions = nil
	session.Mode = &acp.SessionModeState{
		CurrentModeID: "default",
		AvailableModes: []acp.SessionMode{
			{ModeID: "default", Name: "Default"},
			{ModeID: "plan", Name: "Plan"},
		},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	display, err := svc.HandleModeSelection(context.Background(), feishu.ModeSelection{
		BotID:        session.Key.BotID,
		ChatID:       sessionKeyMainID(session.Key),
		ThreadID:     session.Key.SubID,
		ACPSessionID: session.ACPSessionID,
		RequesterID:  "ou_requester",
		OperatorID:   "ou_requester",
		Mode:         "Plan",
	})
	if err != nil {
		t.Fatalf("HandleModeSelection() error = %v", err)
	}
	if display != "Plan（plan）" {
		t.Fatalf("display = %q, want friendly legacy mode display", display)
	}
	if len(rt.modeCalls) != 1 || rt.modeCalls[0].ModeID != "plan" {
		t.Fatalf("modeCalls = %+v, want legacy set_mode plan", rt.modeCalls)
	}
	if len(rt.configCalls) != 0 {
		t.Fatalf("configCalls = %+v, want no set_config_option fallback call", rt.configCalls)
	}
	updated, ok := store.Get(session.Key)
	if !ok || updated.Mode == nil || updated.Mode.CurrentModeID != "plan" {
		t.Fatalf("updated session = %+v, want legacy mode plan persisted", updated)
	}
}

func TestSelectionSessionUsesCallbackSessionKey(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	svc := newTestService(config.Default(), store)

	got, err := svc.selectionSession(feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
	}, session.ACPSessionID, "card expired")
	if err != nil {
		t.Fatalf("selectionSession() error = %v", err)
	}
	if got.Key != normalizeSessionKey(session.Key) {
		t.Fatalf("selectionSession() key = %+v, want %+v", got.Key, session.Key)
	}

	missingStore := newTestService(config.Default(), nil)
	_, err = missingStore.selectionSession(feishu.Message{BotID: "unknown-bot", ChatID: "oc_chat"}, "acp-session", "card expired")
	if err == nil || !strings.Contains(err.Error(), "会话持久化未初始化") {
		t.Fatalf("selectionSession() error = %v, want missing store error", err)
	}
}
