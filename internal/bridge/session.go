package bridge

import (
	"encoding/json"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

type SessionKey struct {
	BotID  string `json:"bot_id"`            // config.json 中配置的 bot id
	Source string `json:"source,omitempty"`  // 消息来源 im/schedule/drive_comment
	MainID string `json:"main_id,omitempty"` // chatid
	SubID  string `json:"sub_id,omitempty"`  // thread_id
}

func (k SessionKey) Valid() bool {
	return sessionKeySource(k) != "" && sessionKeyMainID(k) != ""
}

type ChatKey struct {
	BotID  string `json:"bot_id"`
	ChatID string `json:"chat_id"`
}

func (k ChatKey) Valid() bool {
	return k.ChatID != ""
}

type ChatConfig struct {
	Key              ChatKey                    `json:"key"`
	AgentName        string                     `json:"agent_name,omitempty"`
	AgentConfigs     map[string]ChatAgentConfig `json:"agent_configs,omitempty"`
	Rules            string                     `json:"rules,omitempty"`
	RulesRevision    uint64                     `json:"rules_revision,omitempty"`
	AtMode           string                     `json:"at_mode,omitempty"`
	MentionOptional  bool                       `json:"mention_optional,omitempty"`
	WikiDisabled     bool                       `json:"wiki_disabled,omitempty"`
	WikiIntervalSec  int                        `json:"wiki_interval_sec,omitempty"`
	HideStepMessages bool                       `json:"hide_step_messages,omitempty"`
	HidePlans        bool                       `json:"hide_plans,omitempty"`
	ShowThoughts     bool                       `json:"show_thoughts,omitempty"`
	HideThoughts     bool                       `json:"hide_thoughts,omitempty"`
	HideTools        bool                       `json:"hide_tools,omitempty"`
	HideStatusBar    bool                       `json:"hide_status_bar,omitempty"`
	HideUsageDetail  bool                       `json:"hide_usage_detail,omitempty"`
	NextSessionSeq   int                        `json:"next_session_seq,omitempty"`
	CreatedAt        time.Time                  `json:"created_at"`
	UpdatedAt        time.Time                  `json:"updated_at"`
}

type ChatAgentConfig struct {
	Mode  string `json:"mode,omitempty"`
	Model string `json:"model,omitempty"`
}

type MessageSessionBinding struct {
	BotID      string     `json:"bot_id"`
	ChatID     string     `json:"chat_id"`
	MessageID  string     `json:"message_id"`
	SessionKey SessionKey `json:"session_key"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type Session struct {
	Key               SessionKey                `json:"key"`
	Title             string                    `json:"title,omitempty"`
	ManualTitle       bool                      `json:"manual_title,omitempty"`
	AgentName         string                    `json:"agent_name"`
	ACPSessionID      string                    `json:"acp_session_id,omitempty"`
	ACPUpdatedAt      string                    `json:"acp_updated_at,omitempty"`
	ACPMeta           map[string]any            `json:"acp_meta,omitempty"`
	Cwd               string                    `json:"cwd"`
	Workspace         string                    `json:"workspace,omitempty"`
	WorkspacePrompted bool                      `json:"workspace_prompted,omitempty"`
	WikiDisabled      bool                      `json:"wiki_disabled,omitempty"`
	WikiIntervalSec   int                       `json:"wiki_interval_sec,omitempty"`
	HideStepMessages  bool                      `json:"hide_step_messages,omitempty"`
	HidePlans         bool                      `json:"hide_plans,omitempty"`
	ShowThoughts      bool                      `json:"show_thoughts,omitempty"`
	HideThoughts      bool                      `json:"hide_thoughts,omitempty"`
	HideTools         bool                      `json:"hide_tools,omitempty"`
	HideStatusBar     bool                      `json:"hide_status_bar,omitempty"`
	HideUsageDetail   bool                      `json:"hide_usage_detail,omitempty"`
	ContextWindow     *acp.ContextWindowUsage   `json:"context_window,omitempty"`
	AutoCompact       bool                      `json:"auto_compact,omitempty"`
	AutoCompactPct    int                       `json:"auto_compact_pct,omitempty"`
	AutoCompacting    bool                      `json:"auto_compacting,omitempty"`
	LastAutoCompactAt *time.Time                `json:"last_auto_compact_at,omitempty"`
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
