package acp

type SessionInfo struct {
	SessionID         string                `json:"sessionId,omitempty"`
	AvailableCommands []AvailableCommand    `json:"availableCommands,omitempty"`
	ConfigOptions     []SessionConfigOption `json:"configOptions,omitempty"`
	Models            *SessionModelState    `json:"models,omitempty"`
	Modes             *SessionModeState     `json:"modes,omitempty"`
	Mode              any                   `json:"mode,omitempty"`
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

type SessionMode struct {
	ModeID      string `json:"modeId"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}
