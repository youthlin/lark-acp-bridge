package bridge

import "time"

type SessionKey struct {
	BotID    string `json:"bot_id"`
	ChatID   string `json:"chat_id"`
	ThreadID string `json:"thread_id"`
}

func (k SessionKey) Valid() bool {
	return k.ChatID != ""
}

type Session struct {
	Key                  SessionKey `json:"key"`
	Title                string     `json:"title,omitempty"`
	AgentName            string     `json:"agent_name"`
	ACPSessionID         string     `json:"acp_session_id,omitempty"`
	Cwd                  string     `json:"cwd"`
	Workspace            string     `json:"workspace,omitempty"`
	Status               string     `json:"status"`
	PendingInitialPrompt string     `json:"pending_initial_prompt,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}
