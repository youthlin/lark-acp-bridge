package acp

import (
	"encoding/json"
	"strings"
	"time"
)

type UpdateHandler func(sessionID string, update SessionUpdate)

type PromptUpdateHandler func(update PromptUpdate)

type PromptLifecycleHandler func(event PromptLifecycleEvent)

type PromptOptions struct {
	OnUpdate            PromptUpdateHandler
	OnPermissionRequest PermissionRequestHandler
	OnLifecycle         PromptLifecycleHandler
}

type PromptLifecycleEvent struct {
	Stage        string
	SessionID    string
	Method       string
	RequestID    string
	Err          error
	Cause        error
	At           time.Time
	Elapsed      time.Duration
	WaitDuration time.Duration
}

type SessionListOptions struct {
	Cwd    string
	Cursor string
}

type SessionListResult struct {
	Sessions   []SessionInfo `json:"sessions"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

type PromptUpdate struct {
	SessionID string
	Update    SessionUpdate
}

type PromptResult struct {
	Text       string           `json:"-"`
	StopReason string           `json:"stopReason,omitempty"`
	Usage      TokenUsage       `json:"usage,omitempty"`
	Meta       PromptResultMeta `json:"_meta,omitempty"`
	Raw        json.RawMessage  `json:"-"`
}

type PromptResultMeta struct {
	TraeTokenUsage *TraeTokenUsage `json:"_trae/tokenUsage,omitempty"`
}

type TraeTokenUsage struct {
	TurnDisplay    TokenUsage         `json:"turnDisplay,omitempty"`
	SessionDisplay TokenUsage         `json:"sessionDisplay,omitempty"`
	ContextWindow  ContextWindowUsage `json:"contextWindow,omitempty"`
}

type TokenUsage struct {
	TotalTokens       int64 `json:"totalTokens,omitempty"`
	InputTokens       int64 `json:"inputTokens,omitempty"`
	OutputTokens      int64 `json:"outputTokens,omitempty"`
	ThoughtTokens     int64 `json:"thoughtTokens,omitempty"`
	CachedReadTokens  int64 `json:"cachedReadTokens,omitempty"`
	CachedWriteTokens int64 `json:"cachedWriteTokens,omitempty"`
}

type ContextWindowUsage struct {
	Used                  int64 `json:"used,omitempty"`
	Size                  int64 `json:"size,omitempty"`
	AutoCompactTokenLimit int64 `json:"autoCompactTokenLimit,omitempty"`
}

type UsageCost struct {
	Amount   float64 `json:"amount,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

type SessionUpdate struct {
	SessionUpdate     string                `json:"sessionUpdate"`
	Content           *ContentBlock         `json:"content,omitempty"`
	Message           string                `json:"message,omitempty"`
	Name              string                `json:"name,omitempty"`
	Status            string                `json:"status,omitempty"`
	Title             string                `json:"title,omitempty"`
	UpdatedAt         string                `json:"updatedAt,omitempty"`
	Meta              map[string]any        `json:"_meta,omitempty"`
	TitleSet          bool                  `json:"-"`
	UpdatedAtSet      bool                  `json:"-"`
	ModeID            string                `json:"modeId,omitempty"`
	ToolCallID        string                `json:"toolCallId,omitempty"`
	Kind              string                `json:"kind,omitempty"`
	StopReason        string                `json:"stopReason,omitempty"`
	ToolCallContent   []ToolCallContent     `json:"-"`
	ContentRaw        json.RawMessage       `json:"-"`
	Locations         json.RawMessage       `json:"locations,omitempty"`
	RawInput          json.RawMessage       `json:"rawInput,omitempty"`
	RawOutput         json.RawMessage       `json:"rawOutput,omitempty"`
	AvailableCommands []AvailableCommand    `json:"availableCommands,omitempty"`
	ConfigOptions     []SessionConfigOption `json:"configOptions,omitempty"`
	Models            *SessionModelState    `json:"models,omitempty"`
	Mode              *SessionModeState     `json:"mode,omitempty"`
	PlanEntries       []PlanEntry           `json:"entries,omitempty"`
	Used              int64                 `json:"used,omitempty"`
	Size              int64                 `json:"size,omitempty"`
	Cost              *UsageCost            `json:"cost,omitempty"`
	Raw               json.RawMessage       `json:"-"`
}

type PlanEntry struct {
	Content    string         `json:"content,omitempty"`
	Status     string         `json:"status,omitempty"`
	ActiveForm string         `json:"activeForm,omitempty"`
	Meta       map[string]any `json:"_meta,omitempty"`
}

func (u *SessionUpdate) UnmarshalJSON(data []byte) error {
	type sessionUpdateAlias SessionUpdate
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	// ACP agents use polymorphic update.content values: ordinary updates carry
	// a ContentBlock object, while tool-call deltas can carry an array of edit
	// records. Keep the raw bytes so renderers can still inspect unknown shapes.
	contentRaw := append(json.RawMessage(nil), fields["content"]...)
	delete(fields, "content")
	withoutContent, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	*u = SessionUpdate{}
	aux := struct {
		*sessionUpdateAlias
		LegacyModes *SessionModeState `json:"modes,omitempty"`
	}{
		sessionUpdateAlias: (*sessionUpdateAlias)(u),
	}
	if err := json.Unmarshal(withoutContent, &aux); err != nil {
		return err
	}
	if _, ok := fields["title"]; ok {
		u.TitleSet = true
	}
	if _, ok := fields["updatedAt"]; ok {
		u.UpdatedAtSet = true
	}
	if u.Mode == nil {
		u.Mode = aux.LegacyModes
	}
	u.ToolCallID = strings.TrimSpace(u.ToolCallID)
	u.AvailableCommands = normalizeAvailableCommands(u.AvailableCommands)
	u.ConfigOptions = filterSupportedConfigOptions(u.ConfigOptions)
	if u.SessionUpdate == "tool_call" && strings.TrimSpace(u.Status) == "" {
		u.Status = "pending"
	}
	if u.SessionUpdate == "tool_call" && strings.TrimSpace(u.Kind) == "" {
		u.Kind = "other"
	}
	if len(contentRaw) > 0 && string(contentRaw) != "null" {
		u.ContentRaw = append(json.RawMessage(nil), contentRaw...)
		switch firstJSONToken(contentRaw) {
		case '[':
			var content []ToolCallContent
			if err := json.Unmarshal(contentRaw, &content); err != nil {
				return err
			}
			u.ToolCallContent = content
		case '{':
			var content ContentBlock
			if err := json.Unmarshal(contentRaw, &content); err != nil {
				return err
			}
			u.Content = &content
		}
	}
	return nil
}

func firstJSONToken(raw json.RawMessage) byte {
	for _, b := range raw {
		switch b {
		case ' ', '\n', '\r', '\t':
			continue
		default:
			return b
		}
	}
	return 0
}

type ContentBlock struct {
	Type        string         `json:"type"`
	Text        string         `json:"text,omitempty"`
	Data        string         `json:"data,omitempty"`
	Resource    *Resource      `json:"resource,omitempty"`
	URI         string         `json:"uri,omitempty"`
	Name        string         `json:"name,omitempty"`
	MIMEType    string         `json:"mimeType,omitempty"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	Size        int64          `json:"size,omitempty"`
	Annotations map[string]any `json:"annotations,omitempty"`
	Meta        map[string]any `json:"_meta,omitempty"`
}

type Resource struct {
	URI      string `json:"uri"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
}

type ToolCallContent struct {
	Type       string        `json:"type"`
	Content    *ContentBlock `json:"content,omitempty"`
	Path       string        `json:"path,omitempty"`
	OldText    *string       `json:"oldText,omitempty"`
	NewText    string        `json:"newText,omitempty"`
	TerminalID string        `json:"terminalId,omitempty"`
}

type ToolCallInfo struct {
	ToolCallID string          `json:"toolCallId,omitempty"`
	Title      string          `json:"title,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Status     string          `json:"status,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"`
	Locations  json.RawMessage `json:"locations,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage `json:"rawOutput,omitempty"`
	generation int64
}

type SetConfigOptionResult struct {
	ConfigOptions []SessionConfigOption `json:"configOptions"`
}

func (r *SetConfigOptionResult) UnmarshalJSON(data []byte) error {
	type setConfigOptionResultAlias SetConfigOptionResult
	var parsed setConfigOptionResultAlias
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*r = SetConfigOptionResult(parsed)
	r.ConfigOptions = filterSupportedConfigOptions(r.ConfigOptions)
	return nil
}

type InitializeResult struct {
	ProtocolVersion   int                `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities  `json:"agentCapabilities"`
	AgentInfo         ImplementationInfo `json:"agentInfo"`
	AuthMethods       []AuthMethod       `json:"authMethods,omitempty"`
}

type AgentCapabilities struct {
	LoadSession         bool                `json:"loadSession,omitempty"`
	PromptCapabilities  PromptCapabilities  `json:"promptCapabilities,omitempty"`
	MCPCapabilities     MCPCapabilities     `json:"mcpCapabilities,omitempty"`
	SessionCapabilities SessionCapabilities `json:"sessionCapabilities,omitempty"`
	Auth                AuthCapabilities    `json:"auth,omitempty"`
	Meta                map[string]any      `json:"_meta,omitempty"`
}

type PromptCapabilities struct {
	Image           bool `json:"image,omitempty"`
	Audio           bool `json:"audio,omitempty"`
	EmbeddedContext bool `json:"embeddedContext,omitempty"`
}

type MCPCapabilities struct {
	HTTP bool `json:"http,omitempty"`
	SSE  bool `json:"sse,omitempty"`
}

type SessionCapabilities struct {
	Resume                any `json:"resume,omitempty"`
	Close                 any `json:"close,omitempty"`
	Delete                any `json:"delete,omitempty"`
	List                  any `json:"list,omitempty"`
	AdditionalDirectories any `json:"additionalDirectories,omitempty"`
}

func (c SessionCapabilities) SupportsResume() bool {
	return capabilityEnabled(c.Resume)
}

func (c SessionCapabilities) SupportsClose() bool {
	return capabilityEnabled(c.Close)
}

func (c SessionCapabilities) SupportsDelete() bool {
	return capabilityEnabled(c.Delete)
}

func (c SessionCapabilities) SupportsList() bool {
	return capabilityEnabled(c.List)
}

func (c SessionCapabilities) SupportsAdditionalDirectories() bool {
	return capabilityEnabled(c.AdditionalDirectories)
}

type AuthCapabilities struct {
	Logout any `json:"logout,omitempty"`
}

func (c AuthCapabilities) SupportsLogout() bool {
	return capabilityEnabled(c.Logout)
}

func capabilityEnabled(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	default:
		// Some ACP capabilities are advertised as detail objects rather than
		// booleans. Any non-nil, non-false value means the feature is supported.
		return true
	}
}

type ImplementationInfo struct {
	Name    string `json:"name,omitempty"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

type AuthMethod struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"`
}

func (m *AuthMethod) UnmarshalJSON(data []byte) error {
	type authMethodAlias AuthMethod
	var parsed authMethodAlias
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*m = AuthMethod(parsed)
	if strings.TrimSpace(m.Type) == "" {
		m.Type = "agent"
	}
	return nil
}
