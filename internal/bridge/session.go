package bridge

import (
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

type Session struct {
	Key               SessionKey                `json:"key"`
	Title             string                    `json:"title,omitempty"`
	AgentName         string                    `json:"agent_name"`
	ACPSessionID      string                    `json:"acp_session_id,omitempty"`
	Cwd               string                    `json:"cwd"`
	Workspace         string                    `json:"workspace,omitempty"`
	WikiDisabled      bool                      `json:"wiki_disabled,omitempty"`
	WikiIntervalSec   int                       `json:"wiki_interval_sec,omitempty"`
	AvailableCommands []acp.AvailableCommand    `json:"available_commands,omitempty"`
	ConfigOptions     []acp.SessionConfigOption `json:"config_options,omitempty"`
	Models            *acp.SessionModelState    `json:"models,omitempty"`
	CreatedAt         time.Time                 `json:"created_at"`
	UpdatedAt         time.Time                 `json:"updated_at"`
}
