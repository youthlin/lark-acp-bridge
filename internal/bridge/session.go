package bridge

import (
	"encoding/json"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

type SessionKey struct {
	BotID    string `json:"bot_id"`
	ChatID   string `json:"chat_id"`
	ThreadID string `json:"thread_id"`
}

func (k SessionKey) Valid() bool {
	return k.ChatID != ""
}

type ChatKey struct {
	BotID  string `json:"bot_id"`
	ChatID string `json:"chat_id"`
}

func (k ChatKey) Valid() bool {
	return k.ChatID != ""
}

type ChatConfig struct {
	Key              ChatKey   `json:"key"`
	MentionOptional  bool      `json:"mention_optional,omitempty"`
	HideStepMessages bool      `json:"hide_step_messages,omitempty"`
	HideThoughts     bool      `json:"hide_thoughts,omitempty"`
	HideTools        bool      `json:"hide_tools,omitempty"`
	HideStatusBar    bool      `json:"hide_status_bar,omitempty"`
	HideUsageDetail  bool      `json:"hide_usage_detail,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type Session struct {
	Key               SessionKey                `json:"key"`
	Title             string                    `json:"title,omitempty"`
	AgentName         string                    `json:"agent_name"`
	ACPSessionID      string                    `json:"acp_session_id,omitempty"`
	Cwd               string                    `json:"cwd"`
	Workspace         string                    `json:"workspace,omitempty"`
	WikiDisabled      bool                      `json:"wiki_disabled,omitempty"`
	WikiIntervalSec   int                       `json:"wiki_interval_sec,omitempty"`
	HideStepMessages  bool                      `json:"hide_step_messages,omitempty"`
	HideThoughts      bool                      `json:"hide_thoughts,omitempty"`
	HideTools         bool                      `json:"hide_tools,omitempty"`
	HideStatusBar     bool                      `json:"hide_status_bar,omitempty"`
	HideUsageDetail   bool                      `json:"hide_usage_detail,omitempty"`
	AvailableCommands []acp.AvailableCommand    `json:"available_commands,omitempty"`
	ConfigOptions     []acp.SessionConfigOption `json:"config_options,omitempty"`
	Models            *acp.SessionModelState    `json:"models,omitempty"`
	Mode              *acp.SessionModeState     `json:"mode,omitempty"`
	CreatedAt         time.Time                 `json:"created_at"`
	UpdatedAt         time.Time                 `json:"updated_at"`
}

func (s *Session) UnmarshalJSON(data []byte) error {
	type sessionAlias Session
	aux := struct {
		*sessionAlias
		LegacyModes *acp.SessionModeState `json:"modes,omitempty"`
	}{
		sessionAlias: (*sessionAlias)(s),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if s.Mode == nil {
		s.Mode = aux.LegacyModes
	}
	return nil
}
