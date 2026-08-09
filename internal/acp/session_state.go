package acp

import (
	"encoding/json"
	"strings"
)

type SessionInfo struct {
	SessionID             string                `json:"sessionId,omitempty"`
	Cwd                   string                `json:"cwd,omitempty"`
	AdditionalDirectories []string              `json:"additionalDirectories,omitempty"`
	Title                 string                `json:"title,omitempty"`
	UpdatedAt             string                `json:"updatedAt,omitempty"`
	Meta                  map[string]any        `json:"_meta,omitempty"`
	AvailableCommands     []AvailableCommand    `json:"availableCommands,omitempty"`
	ConfigOptions         []SessionConfigOption `json:"configOptions,omitempty"`
	Models                *SessionModelState    `json:"models,omitempty"`
	Mode                  *SessionModeState     `json:"mode,omitempty"`
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
	s.AvailableCommands = normalizeAvailableCommands(s.AvailableCommands)
	s.ConfigOptions = filterSupportedConfigOptions(s.ConfigOptions)
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

func normalizeAvailableCommands(commands []AvailableCommand) []AvailableCommand {
	if len(commands) == 0 {
		return commands
	}
	for i := range commands {
		commands[i].Name = strings.TrimSpace(commands[i].Name)
		commands[i].Description = strings.TrimSpace(commands[i].Description)
		if commands[i].Input != nil {
			commands[i].Input.Hint = strings.TrimSpace(commands[i].Input.Hint)
		}
	}
	return commands
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
	Value       string         `json:"value"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Meta        map[string]any `json:"_meta,omitempty"`
}

func filterSupportedConfigOptions(options []SessionConfigOption) []SessionConfigOption {
	if len(options) == 0 {
		return options
	}
	filtered := options[:0]
	for _, opt := range options {
		opt = normalizeConfigOption(opt)
		if isSupportedConfigOptionType(opt.Type) {
			filtered = append(filtered, opt)
		}
	}
	return filtered
}

func normalizeConfigOption(opt SessionConfigOption) SessionConfigOption {
	opt.ID = strings.TrimSpace(opt.ID)
	opt.Category = strings.TrimSpace(opt.Category)
	opt.Type = strings.TrimSpace(opt.Type)
	for i := range opt.Options {
		opt.Options[i].Value = strings.TrimSpace(opt.Options[i].Value)
		opt.Options[i].Name = strings.TrimSpace(opt.Options[i].Name)
	}
	return opt
}

func isSupportedConfigOptionType(optionType string) bool {
	switch strings.TrimSpace(optionType) {
	case "select", "boolean":
		return true
	default:
		return false
	}
}

type SessionModelState struct {
	CurrentModelID  string         `json:"currentModelId"`
	AvailableModels []SessionModel `json:"availableModels,omitempty"`
}

func (s *SessionModelState) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	var parsed struct {
		CurrentModelID  string         `json:"currentModelId"`
		AvailableModels []SessionModel `json:"availableModels,omitempty"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	s.CurrentModelID = strings.TrimSpace(parsed.CurrentModelID)
	s.AvailableModels = append([]SessionModel(nil), parsed.AvailableModels...)
	return nil
}

type SessionModel struct {
	ModelID     string         `json:"modelId"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Meta        map[string]any `json:"_meta,omitempty"`
}

func (m *SessionModel) UnmarshalJSON(data []byte) error {
	var parsed struct {
		ModelID     string         `json:"modelId"`
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Meta        map[string]any `json:"_meta,omitempty"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	m.ModelID = strings.TrimSpace(parsed.ModelID)
	m.Name = strings.TrimSpace(parsed.Name)
	m.Description = strings.TrimSpace(parsed.Description)
	m.Meta = parsed.Meta
	return nil
}

func TraeModelLoadPercent(meta map[string]any) (int, bool) {
	if len(meta) == 0 {
		return 0, false
	}
	trae, ok := meta["trae"].(map[string]any)
	if !ok {
		return 0, false
	}
	load, ok := trae["load"].(map[string]any)
	if !ok {
		return 0, false
	}
	percent, ok := numberToInt(load["percent"])
	if !ok || percent < 0 || percent > 100 {
		return 0, false
	}
	return percent, true
}

func numberToInt(value any) (int, bool) {
	switch value := value.(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		if value != float64(int(value)) {
			return 0, false
		}
		return int(value), true
	case json.Number:
		parsed, err := value.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	default:
		return 0, false
	}
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
	ModeID      string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func (m *SessionMode) UnmarshalJSON(data []byte) error {
	var parsed struct {
		ID          string `json:"id"`
		LegacyID    string `json:"modeId"`
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	m.ModeID = strings.TrimSpace(parsed.ID)
	if m.ModeID == "" {
		m.ModeID = strings.TrimSpace(parsed.LegacyID)
	}
	m.Name = parsed.Name
	m.Description = parsed.Description
	return nil
}
