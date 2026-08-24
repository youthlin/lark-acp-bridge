package feishu

import (
	"context"
	"path/filepath"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

type Handler interface {
	// Message 是一次事件解析后的轻量值对象，按值传递可以让 handler 安全持有快照。
	HandleFeishuMessage(context.Context, Message) (string, error)
}

type ModelSelectionHandler interface {
	HandleModelSelection(context.Context, ModelSelection) (string, error)
}

type ModeSelectionHandler interface {
	HandleModeSelection(context.Context, ModeSelection) (string, error)
}

type SessionSelectionHandler interface {
	HandleSessionSelection(context.Context, SessionSelection) (string, error)
}

type OverviewActionHandler interface {
	HandleOverviewAction(context.Context, OverviewAction) (OverviewActionResult, error)
}

type LoopCancelHandler interface {
	HandleLoopCancel(context.Context, LoopCancel) (string, error)
}

type DriveCommentHandler interface {
	HandleDriveComment(context.Context, DriveComment) error
}

type DriveCommentCapabilityHandler interface {
	DriveCommentEnabled(botID string) bool
}

type Adapter struct {
	cfg             config.BotConfig        // Bot配置
	handler         Handler                 // 消息处理
	client          *lark.Client            // lark sdk Client 用于给用户发送消息
	ws              *larkws.Client          // WebSocket 客户端 用于监听上行消息
	deduper         *messageDeduper         // 消息去重
	reaction        reactionClient          // 消息处理期间添加/移除 reaction
	messages        messageClient           // 消息读取
	chatInfo        chatInfoClient          // 群信息读取
	applications    applicationClient       // 应用信息读取
	driveComments   driveCommentClient      // 云文档评论读取和回复
	permissionCards *permissionCardRegistry // ACP 权限卡片等待表
	chatInfoCache   *chatInfoCache          // 群信息缓存
}

var _ Outbound = (*Adapter)(nil)

type reactionClient interface {
	AddReaction(ctx context.Context, messageID string) (string, error)
	DeleteReaction(ctx context.Context, messageID string, reactionID string) error
}

type messageClient interface {
	GetMessage(ctx context.Context, messageID string, workspace string) (*Message, error)
	DownloadImage(ctx context.Context, messageID string, imageKey string, workspace string) (string, error)
	UploadImage(ctx context.Context, path string) (string, error)
}

type chatInfoClient interface {
	GetChatInfo(ctx context.Context, chatID string) (chatInfo, error)
}

type applicationClient interface {
	GetApplication(ctx context.Context) (applicationOwnerCandidates, error)
	GetCollaborators(ctx context.Context) ([]applicationCollaborator, error)
	GetBotInfo(ctx context.Context) (BotInfo, error)
}

type driveCommentClient interface {
	GetComment(ctx context.Context, fileToken, fileType, commentID string) (DriveCommentDetail, error)
	ReplyComment(ctx context.Context, comment DriveComment, text string) error
}

type BotInfo struct {
	OpenID string
	Name   string
}

type applicationOwnerCandidates struct {
	CreatorID string
	OwnerID   string
	AppName   string
}

type applicationCollaborator struct {
	Type   string
	UserID string
}

type chatInfo struct {
	Name             string
	ChatMode         string
	ChatType         string
	GroupMessageType string
}

type CreateDriveCommentTraceChatRequest struct {
	Name        string
	OwnerOpenID string
	UserOpenIDs []string
}

type CreateChatRequest struct {
	Name             string
	Mode             string
	ChatType         string
	GroupMessageType string
	OwnerOpenID      string
	UserOpenIDs      []string
	SetBotManager    bool
}

type CreatedChat struct {
	ChatID           string
	Name             string
	OwnerOpenID      string
	ChatMode         string
	ChatType         string
	GroupMessageType string
}

type AddChatMembersRequest struct {
	ChatID      string
	UserOpenIDs []string
}

type AddChatMembersResult struct {
	InvalidOpenIDs         []string
	NotExistedOpenIDs      []string
	PendingApprovalOpenIDs []string
}

// NewAdapter 创建飞书bot处理器
func NewAdapter(cfg config.BotConfig, handler Handler) *Adapter {
	deduper := newMessageDeduper(defaultMessageDeduperTTL, defaultMessageDeduperMax)
	if strings.TrimSpace(cfg.Workspace) != "" {
		deduper.WithPath(filepath.Join(cfg.Workspace, ".local", "processed_messages.json"))
	}
	return &Adapter{
		cfg:             cfg,
		handler:         handler,
		deduper:         deduper,
		chatInfoCache:   newChatInfoCache(defaultChatInfoCacheTTL),
		permissionCards: newPermissionCardRegistry(),
	}
}

func (a *Adapter) BotOpenID() string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.cfg.BotOpenID)
}

func (a *Adapter) OwnerOpenIDs() []string {
	if a == nil || len(a.cfg.OwnerOpenIDs) == 0 {
		return nil
	}
	out := make([]string, 0, len(a.cfg.OwnerOpenIDs))
	seen := make(map[string]struct{}, len(a.cfg.OwnerOpenIDs))
	for _, id := range a.cfg.OwnerOpenIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (a *Adapter) Outbound() {}
