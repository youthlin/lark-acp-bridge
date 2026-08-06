package feishu

import "context"

// Outbound 表示飞书侧可用的出站能力。具体能力由 bridge 侧按需做小接口断言，
// 避免把函数通过 context.WithValue 隐式传递到业务层。
type Outbound interface {
	Outbound()
}

type OutboundHandler interface {
	HandleFeishuMessageWithOutbound(context.Context, Message, Outbound) (string, error)
}

type OutboundDriveCommentHandler interface {
	HandleDriveCommentWithOutbound(context.Context, DriveComment, Outbound) error
}

type OutboundRenderContext struct {
	BaseDir string
}

type StreamCardMeta struct {
	Title     string
	Subtitle  string
	SourceURL string
	Footer    string
}

// LoopStatusCard 表示 /loop 启动后用于展示整体状态的卡片。
type LoopStatusCard interface {
	Message() SentMessage
	Update(context.Context, string) error
	Finish(context.Context, string) error
}

type LoopStatusCardRequest struct {
	BotID        string
	ChatID       string
	ThreadID     string
	ACPSessionID string
	Text         string
}

type LoopCancel struct {
	BotID        string
	ChatID       string
	ThreadID     string
	ACPSessionID string
	OperatorID   string
}

// StreamCard 表示一张可流式更新的飞书卡片。
type StreamCard interface {
	Message() SentMessage
	UpdateProcess(context.Context, string) error
	UpdateStatus(context.Context, string) error
	UpdateUsageDetail(context.Context, string) error
	UpdateText(context.Context, string) error
	SetFinalText(context.Context, string, OutboundRenderContext) error
	UpdateMeta(context.Context, StreamCardMeta) error
	Close(context.Context) error
}

type streamCardProcessPanelKey struct{}

type streamCardStatusBarKey struct{}

type streamCardMetaKey struct{}

type ModelOption struct {
	Value string
	Name  string
}

type ModeOption struct {
	Value string
	Name  string
}

type SessionOption struct {
	ACPSessionID string
	Title        string
	Cwd          string
}

type ConfigOptionValue struct {
	Value       string
	Name        string
	Description string
	Current     bool
}

type ConfigDetailCard struct {
	ID           string
	Name         string
	Category     string
	Description  string
	Type         string
	CurrentValue string
	Options      []ConfigOptionValue
	SetCommand   string
}

type ModelSelectionCard struct {
	BotID            string
	ChatID           string
	ThreadID         string
	GroupMessageType string
	ACPSessionID     string
	RequesterID      string
	CurrentModel     string
	Options          []ModelOption
}

type ModeSelectionCard struct {
	BotID            string
	ChatID           string
	ThreadID         string
	GroupMessageType string
	ACPSessionID     string
	RequesterID      string
	CurrentMode      string
	Options          []ModeOption
}

type SessionSelectionCard struct {
	BotID               string
	ChatID              string
	ThreadID            string
	GroupMessageType    string
	RequesterID         string
	CurrentACPSessionID string
	Options             []SessionOption
}

type OverviewOption struct {
	Value   string
	Text    string
	Current bool
}

type OverviewShowOptions struct {
	Step    bool
	Plan    bool
	Thought bool
	Tool    bool
	Status  bool
	Used    bool
}

type OverviewCard struct {
	BotID               string
	ChatID              string
	ChatType            string
	ThreadID            string
	GroupMessageType    string
	RequesterID         string
	CurrentACPSessionID string
	HasSession          bool
	SessionTitle        string
	AgentName           string
	ChatAgentName       string
	Cwd                 string
	Model               string
	Mode                string
	ContextUsage        string
	CompactStatus       string
	RuntimeStatus       string
	QueueStatus         string
	WikiStatus          string
	LoopStatus          string
	ACPErrorStatus      string
	AtStatus            string
	Show                OverviewShowOptions
	WikiEnabled         bool
	AgentOptions        []OverviewOption
	SessionOptions      []SessionOption
	AtOptions           []OverviewOption
	ModelOptions        []ModelOption
	ModeOptions         []ModeOption
	CommandHints        []string
	CommandNotes        []string
}

type OverviewAction struct {
	BotID               string
	ChatID              string
	ChatType            string
	ThreadID            string
	GroupMessageType    string
	RequesterID         string
	OperatorID          string
	CurrentACPSessionID string
	Action              string
	Target              string
	Value               string
}

type OverviewActionResult struct {
	ToastType string
	Toast     string
	Overview  *OverviewCard
	Model     *ModelSelectionCard
	Mode      *ModeSelectionCard
	Session   *SessionSelectionCard
}

func WithStreamCardProcessPanel(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, streamCardProcessPanelKey{}, enabled)
}

func StreamCardProcessPanelEnabled(ctx context.Context) bool {
	enabled, ok := ctx.Value(streamCardProcessPanelKey{}).(bool)
	if !ok {
		return true
	}
	return enabled
}

func WithStreamCardStatusBar(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, streamCardStatusBarKey{}, enabled)
}

func StreamCardStatusBarEnabled(ctx context.Context) bool {
	enabled, ok := ctx.Value(streamCardStatusBarKey{}).(bool)
	if !ok {
		return true
	}
	return enabled
}

func WithStreamCardMeta(ctx context.Context, meta StreamCardMeta) context.Context {
	return context.WithValue(ctx, streamCardMetaKey{}, meta)
}

func StreamCardMetaFromContext(ctx context.Context) StreamCardMeta {
	meta, _ := ctx.Value(streamCardMetaKey{}).(StreamCardMeta)
	return meta
}
