package acp

import (
	"encoding/json"
	"strings"
)

type SessionInfo struct {
	SessionID         string                `json:"sessionId,omitempty"`
	AvailableCommands []AvailableCommand    `json:"availableCommands,omitempty"`
	ConfigOptions     []SessionConfigOption `json:"configOptions,omitempty"`
	Models            *SessionModelState    `json:"models,omitempty"`
	Mode              *SessionModeState     `json:"mode,omitempty"`
}

func (s *SessionInfo) UnmarshalJSON(data []byte) error {
	type sessionInfoAlias SessionInfo
	aux := struct {
		*sessionInfoAlias
		LegacyModes *SessionModeState `json:"modes,omitempty"`
	}{
		sessionInfoAlias: (*sessionInfoAlias)(s),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if s.Mode == nil {
		s.Mode = aux.LegacyModes
	}
	return nil
}

type AvailableCommand struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Input       *AvailableCommandInput `json:"input,omitempty"`
}

type AvailableCommandInput struct {
	Hint string `json:"hint"`
}

type SessionConfigOption struct {
	ID           string                     `json:"id"`
	Name         string                     `json:"name"`
	Description  string                     `json:"description,omitempty"`
	Category     string                     `json:"category,omitempty"`
	Type         string                     `json:"type"`
	CurrentValue any                        `json:"currentValue"`
	Options      []SessionConfigOptionValue `json:"options,omitempty"`
}

type SessionConfigOptionValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type SessionModelState struct {
	CurrentModelID  string         `json:"currentModelId"`
	AvailableModels []SessionModel `json:"availableModels,omitempty"`
}

type SessionModel struct {
	ModelID     string `json:"modelId"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type SessionModeState struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes,omitempty"`
}

func (s *SessionModeState) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	var current string
	if err := json.Unmarshal(data, &current); err == nil {
		s.CurrentModeID = strings.TrimSpace(current)
		s.AvailableModes = nil
		return nil
	}
	var parsed struct {
		CurrentModeID  string        `json:"currentModeId"`
		AvailableModes []SessionMode `json:"availableModes,omitempty"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	s.CurrentModeID = strings.TrimSpace(parsed.CurrentModeID)
	s.AvailableModes = append([]SessionMode(nil), parsed.AvailableModes...)
	return nil
}

type SessionMode struct {
	ModeID      string `json:"modeId"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}
