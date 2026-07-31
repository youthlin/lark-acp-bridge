package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkapplication "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
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

type LoopCancelHandler interface {
	HandleLoopCancel(context.Context, LoopCancel) (string, error)
}

type DriveCommentHandler interface {
	HandleDriveComment(context.Context, DriveComment) error
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
	GetBotOpenID(ctx context.Context) (string, error)
}

type driveCommentClient interface {
	GetComment(ctx context.Context, fileToken, fileType, commentID string) (DriveCommentDetail, error)
	ReplyComment(ctx context.Context, comment DriveComment, text string) error
}

type applicationOwnerCandidates struct {
	CreatorID string
	OwnerID   string
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

// NewAdapter 创建飞书bot处理器
func NewAdapter(cfg config.BotConfig, handler Handler) *Adapter {
	deduper := newMessageDeduper(defaultMessageDeduperTTL, defaultMessageDeduperMax)
	if strings.TrimSpace(cfg.Workspace) != "" {
		deduper.WithPath(filepath.Join(cfg.Workspace, "processed_messages.json"))
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

// Start 启动Bot监听
func (a *Adapter) Start(ctx context.Context) error {
	if a.cfg.AppID == "" || a.cfg.AppSecret == "" {
		slog.Warn("未配置飞书机器人凭证，消息监听未启动")
		return nil
	}
	if a.deduper != nil {
		if err := a.deduper.Load(); err != nil {
			slog.Warn("加载飞书消息去重状态失败", "bot", a.cfg.ID, "错误", err)
		}
	}

	clientOptions := []lark.ClientOptionFunc{lark.WithLogger(NewLogger(slog.LevelInfo, a.cfg.ID, "lark-sdk"))}
	a.client = lark.NewClient(a.cfg.AppID, a.cfg.AppSecret, clientOptions...)
	if a.reaction == nil {
		a.reaction = larkReactionClient{client: a.client}
	}
	if a.messages == nil {
		a.messages = larkMessageClient{client: a.client}
	}
	if a.chatInfo == nil {
		a.chatInfo = larkChatInfoClient{client: a.client}
	}
	if a.applications == nil {
		a.applications = larkApplicationClient{client: a.client}
	}
	if a.driveComments == nil {
		a.driveComments = larkDriveCommentClient{client: a.client}
	}
	a.resolveBotOpenID(ctx)
	a.resolveOwnerOpenIDs(ctx)

	handler := a.newEventDispatcher()

	a.ws = larkws.NewClient(
		a.cfg.AppID,
		a.cfg.AppSecret,
		larkws.WithEventHandler(handler),
		larkws.WithLogger(NewLogger(slog.LevelInfo, a.cfg.ID, "lark-ws")),
	)
	go func() {
		if err := a.ws.Start(ctx); err != nil {
			slog.Error("飞书 WebSocket 监听已停止", "err", err)
		}
	}()
	slog.Info("启动飞书 WebSocket 监听")
	return nil
}

func (a *Adapter) newEventDispatcher() *dispatcher.EventDispatcher {
	handler := dispatcher.NewEventDispatcher(a.cfg.AppID, a.cfg.AppSecret).
		OnP2MessageReceiveV1(a.handleMessage).
		OnP2MessageReactionCreatedV1(a.handleReactionCreated).
		OnP2MessageReactionDeletedV1(a.handleReactionDeleted).
		OnP2NoticeCommentAddV1(a.handleDriveCommentAdd).
		OnP2CardActionTrigger(a.handleCardAction)
	handler.InitConfig(larkevent.WithLogger(NewLogger(slog.LevelInfo, a.cfg.ID, "lark-handler")))
	return handler
}

func (a *Adapter) resolveBotOpenID(ctx context.Context) {
	if strings.TrimSpace(a.cfg.BotOpenID) != "" || a.applications == nil {
		a.cfg.BotOpenID = strings.TrimSpace(a.cfg.BotOpenID)
		return
	}
	openID, err := a.applications.GetBotOpenID(ctx)
	if err != nil {
		slog.Warn("获取飞书机器人 open_id 失败，群聊 at 过滤需要手动配置 bot_open_id", "bot", a.cfg.ID, "err", err)
		return
	}
	openID = strings.TrimSpace(openID)
	if openID == "" {
		slog.Warn("飞书机器人 open_id 为空，群聊 at 过滤需要手动配置 bot_open_id", "bot", a.cfg.ID)
		return
	}
	a.cfg.BotOpenID = openID
	slog.Info("已解析飞书机器人 open_id", "bot", a.cfg.ID)
}

func (a *Adapter) resolveOwnerOpenIDs(ctx context.Context) {
	if len(a.cfg.OwnerOpenIDs) > 0 || a.applications == nil {
		return
	}
	owners, err := a.fetchOwnerOpenIDs(ctx)
	if err != nil {
		slog.Warn("获取飞书应用协作者失败，群聊权限卡片需要手动配置 bot owner", "bot", a.cfg.ID, "err", err)
		return
	}
	if len(owners) == 0 {
		slog.Warn("飞书应用协作者中未解析到 bot owner，群聊权限卡片需要手动配置 bot owner", "bot", a.cfg.ID)
		return
	}
	a.cfg.OwnerOpenIDs = owners
	slog.Info("已从飞书应用协作者解析 bot owner", "bot", a.cfg.ID, "数量", len(owners))
}

func (a *Adapter) fetchOwnerOpenIDs(ctx context.Context) ([]string, error) {
	var ids []string
	app, appErr := a.applications.GetApplication(ctx)
	if appErr == nil {
		ids = append(ids, app.OwnerID, app.CreatorID)
	} else {
		slog.Warn("获取飞书应用信息失败，将继续尝试读取应用协作者", "bot", a.cfg.ID, "err", appErr)
	}
	collaborators, collabErr := a.applications.GetCollaborators(ctx)
	if collabErr == nil {
		for _, item := range collaborators {
			if applicationCollaboratorCanApprove(item.Type) {
				ids = append(ids, item.UserID)
			}
		}
	}
	ids = normalizeOpenIDs(ids)
	if len(ids) > 0 {
		return ids, nil
	}
	if appErr != nil && collabErr != nil {
		return nil, fmt.Errorf("获取应用信息: %w; 获取应用协作者: %w", appErr, collabErr)
	}
	if collabErr != nil {
		return nil, fmt.Errorf("获取应用协作者: %w", collabErr)
	}
	return nil, nil
}

func applicationCollaboratorCanApprove(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "owner", "administrator", "developer":
		return true
	default:
		return false
	}
}

func normalizeOpenIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
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

type larkApplicationClient struct {
	client *lark.Client
}

type larkDriveCommentClient struct {
	client *lark.Client
}

func (c larkApplicationClient) GetApplication(ctx context.Context) (applicationOwnerCandidates, error) {
	resp, err := c.client.Application.Application.Get(ctx, larkapplication.NewGetApplicationReqBuilder().
		AppId("me").
		Lang("zh_cn").
		UserIdType("open_id").
		Build())
	if err != nil {
		return applicationOwnerCandidates{}, fmt.Errorf("调用飞书获取应用信息接口: %w", err)
	}
	if !resp.Success() {
		return applicationOwnerCandidates{}, fmt.Errorf("飞书获取应用信息接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.App == nil {
		return applicationOwnerCandidates{}, nil
	}
	app := resp.Data.App
	out := applicationOwnerCandidates{CreatorID: value(app.CreatorId)}
	if app.Owner != nil {
		out.OwnerID = value(app.Owner.OwnerId)
	}
	return out, nil
}

func (c larkApplicationClient) GetCollaborators(ctx context.Context) ([]applicationCollaborator, error) {
	resp, err := c.client.Application.ApplicationCollaborators.Get(ctx, larkapplication.NewGetApplicationCollaboratorsReqBuilder().
		AppId("me").
		UserIdType("open_id").
		Build())
	if err != nil {
		return nil, fmt.Errorf("调用飞书获取应用协作者接口: %w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("飞书获取应用协作者接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return nil, nil
	}
	out := make([]applicationCollaborator, 0, len(resp.Data.Collaborators))
	for _, item := range resp.Data.Collaborators {
		if item == nil {
			continue
		}
		out = append(out, applicationCollaborator{
			Type:   value(item.Type),
			UserID: value(item.UserId),
		})
	}
	return out, nil
}

func (c larkApplicationClient) GetBotOpenID(ctx context.Context) (string, error) {
	resp, err := c.client.Get(ctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return "", fmt.Errorf("调用飞书获取机器人信息接口: %w", err)
	}
	if resp == nil {
		return "", fmt.Errorf("飞书获取机器人信息接口返回为空")
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("飞书获取机器人信息接口 HTTP 状态异常: %d", resp.StatusCode)
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(resp.RawBody, &result); err != nil {
		return "", fmt.Errorf("解析飞书机器人信息接口响应: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("飞书获取机器人信息接口返回错误: code=%d msg=%s", result.Code, result.Msg)
	}
	return strings.TrimSpace(result.Bot.OpenID), nil
}

func (c larkDriveCommentClient) GetComment(ctx context.Context, fileToken, fileType, commentID string) (DriveCommentDetail, error) {
	if c.client == nil {
		return DriveCommentDetail{}, fmt.Errorf("飞书客户端未初始化")
	}
	fileToken = strings.TrimSpace(fileToken)
	fileType = strings.TrimSpace(fileType)
	commentID = strings.TrimSpace(commentID)
	if fileToken == "" || fileType == "" || commentID == "" {
		return DriveCommentDetail{}, fmt.Errorf("云文档评论参数不完整")
	}
	body := larkdrive.NewBatchQueryFileCommentReqBodyBuilder().
		CommentIds([]string{commentID}).
		Build()
	req := larkdrive.NewBatchQueryFileCommentReqBuilder().
		FileToken(fileToken).
		FileType(fileType).
		UserIdType(larkdrive.UserIdTypeBatchQueryFileCommentOpenId).
		Body(body).
		Build()
	resp, err := c.client.Drive.V1.FileComment.BatchQuery(ctx, req)
	_ = c.client.Drive.V1.FileComment.Get // 这个Get是只能获取指定的「全文评论」 不是侧边栏局部评论
	if err != nil {
		return DriveCommentDetail{}, fmt.Errorf("调用飞书获取云文档评论接口: %w", err)
	}
	if resp == nil || !resp.Success() {
		code, msg := 0, ""
		if resp != nil {
			code, msg = resp.Code, resp.Msg
		}
		return DriveCommentDetail{}, fmt.Errorf("飞书获取云文档评论接口返回错误: code=%d msg=%s", code, msg)
	}
	if resp.Data == nil {
		return DriveCommentDetail{}, fmt.Errorf("飞书获取云文档评论接口未返回数据")
	}
	for _, item := range resp.Data.Items {
		if item == nil || strings.TrimSpace(value(item.CommentId)) != commentID {
			continue
		}
		detail := driveCommentDetailFromFileComment(item)
		if detail.HasMore {
			replies, err := c.listCommentReplies(ctx, fileToken, fileType, commentID)
			if err != nil {
				slog.WarnContext(ctx, "读取云文档评论回复列表失败，使用批量接口返回的部分回复", "错误", err)
				return detail, nil
			}
			detail.Replies = replies
			detail.HasMore = false
			detail.PageToken = ""
			detail.RepliesComplete = true
		}
		return detail, nil
	}
	return DriveCommentDetail{}, fmt.Errorf("飞书获取云文档评论接口未返回评论: comment_id=%s", commentID)
}

func (c larkDriveCommentClient) listCommentReplies(ctx context.Context, fileToken, fileType, commentID string) ([]DriveCommentReply, error) {
	var replies []DriveCommentReply
	pageToken := ""
	for {
		builder := larkdrive.NewListFileCommentReplyReqBuilder().
			FileToken(fileToken).
			CommentId(commentID).
			FileType(fileType).
			PageSize(100).
			UserIdType(larkdrive.UserIdTypeListFileCommentReplyOpenId)
		if pageToken != "" {
			builder.PageToken(pageToken)
		}
		resp, err := c.client.Drive.V1.FileCommentReply.List(ctx, builder.Build())
		if err != nil {
			return nil, fmt.Errorf("调用飞书获取云文档评论回复接口: %w", err)
		}
		if resp == nil || !resp.Success() {
			code, msg := 0, ""
			if resp != nil {
				code, msg = resp.Code, resp.Msg
			}
			return nil, fmt.Errorf("飞书获取云文档评论回复接口返回错误: code=%d msg=%s", code, msg)
		}
		if resp.Data == nil {
			return nil, fmt.Errorf("飞书获取云文档评论回复接口未返回数据")
		}
		for _, reply := range resp.Data.Items {
			replies = appendDriveCommentReply(replies, reply)
		}
		if !valueBool(resp.Data.HasMore) {
			return replies, nil
		}
		pageToken = value(resp.Data.PageToken)
		if pageToken == "" {
			return replies, nil
		}
	}
}

func (c larkDriveCommentClient) ReplyComment(ctx context.Context, comment DriveComment, text string) error {
	if c.client == nil {
		return fmt.Errorf("飞书客户端未初始化")
	}
	comment = comment.Normalized()
	text = strings.TrimSpace(text)
	if comment.FileToken == "" || comment.FileType == "" || comment.CommentID == "" {
		return fmt.Errorf("Drive 评论参数不完整")
	}
	if text == "" {
		return nil
	}
	body := larkdrive.NewCreateFileCommentReplyReqBodyBuilder().
		Content(driveReplyContentFromText(text)).
		Build()
	req := larkdrive.NewCreateFileCommentReplyReqBuilder().
		FileToken(comment.FileToken).
		CommentId(comment.CommentID).
		FileType(comment.FileType).
		UserIdType(larkdrive.UserIdTypeCreateFileCommentReplyOpenId).
		Body(body).
		Build()
	resp, err := c.client.Drive.V1.FileCommentReply.Create(ctx, req)
	if err != nil {
		return fmt.Errorf("调用飞书回复云文档评论接口: %w", err)
	}
	if resp == nil || !resp.Success() {
		code, msg := 0, ""
		if resp != nil {
			code, msg = resp.Code, resp.Msg
		}
		return fmt.Errorf("飞书回复云文档评论接口返回错误: code=%d msg=%s", code, msg)
	}
	return nil
}

type larkChatInfoClient struct {
	client *lark.Client
}

func (c larkChatInfoClient) GetChatInfo(ctx context.Context, chatID string) (chatInfo, error) {
	if c.client == nil {
		return chatInfo{}, fmt.Errorf("飞书客户端未初始化")
	}
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return chatInfo{}, fmt.Errorf("飞书 chat_id 为空")
	}
	req := larkim.NewGetChatReqBuilder().
		ChatId(chatID).
		UserIdType(larkim.GetChatUserIDTypeOpenId).
		Build()
	resp, err := c.client.Im.Chat.Get(ctx, req)
	if err != nil {
		return chatInfo{}, fmt.Errorf("调用飞书获取群信息接口: %w", err)
	}
	if !resp.Success() {
		return chatInfo{}, fmt.Errorf("飞书获取群信息接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil {
		return chatInfo{}, fmt.Errorf("飞书获取群信息接口未返回数据")
	}
	return chatInfo{
		Name:             value(resp.Data.Name),
		ChatMode:         value(resp.Data.ChatMode),
		ChatType:         value(resp.Data.ChatType),
		GroupMessageType: value(resp.Data.GroupMessageType),
	}, nil
}

func (a *Adapter) Shutdown(ctx context.Context) error {
	if a.ws != nil {
		a.ws.Close()
	}
	return nil
}

func (a *Adapter) SendText(ctx context.Context, msg Message, text string) error {
	return a.SendTextWithRenderContext(ctx, msg, text, OutboundRenderContext{BaseDir: msg.Workspace})
}

func (a *Adapter) SendTextWithRenderContext(ctx context.Context, msg Message, text string, render OutboundRenderContext) error {
	blocks, err := a.renderOutboundBlocks(ctx, text, outboundRenderContextFromPublic(render))
	if err != nil {
		return err
	}
	if outboundBlocksHaveImage(blocks) {
		_, err := a.SendPostMessage(ctx, msg, blocks)
		return err
	}
	_, err = a.SendTextMessage(ctx, msg, text)
	return err
}

func (a *Adapter) SendTextMessage(ctx context.Context, msg Message, text string) (SentMessage, error) {
	if strings.TrimSpace(msg.MessageID) != "" {
		return a.ReplyTextMessage(ctx, msg, text)
	}
	if strings.TrimSpace(msg.ChatID) != "" {
		return a.SendChatTextMessage(ctx, msg.ChatID, msg.ChatType, text)
	}
	return SentMessage{}, fmt.Errorf("飞书 message_id 为空")
}

func (a *Adapter) SendChatText(ctx context.Context, chatID string, text string) error {
	_, err := a.SendChatTextMessage(ctx, chatID, "p2p", text)
	return err
}

func (a *Adapter) SendChatTextMessage(ctx context.Context, chatID string, chatType string, text string) (SentMessage, error) {
	if a.client == nil {
		return SentMessage{}, fmt.Errorf("飞书客户端未初始化")
	}
	if chatID == "" {
		return SentMessage{}, fmt.Errorf("飞书 chat_id 为空")
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return SentMessage{}, fmt.Errorf("编码飞书消息内容: %w", err)
	}
	msgType := "text"
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(msgType).
			Content(string(content)).
			Build()).
		Build()
	resp, err := a.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return SentMessage{}, fmt.Errorf("调用飞书发消息接口: %w", err)
	}
	if !resp.Success() {
		return SentMessage{}, fmt.Errorf("飞书发消息接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return sentMessageFromCreateResp(resp, chatID, chatType), nil
}

func (a *Adapter) SendPostMessage(ctx context.Context, msg Message, blocks []outboundBlock) (SentMessage, error) {
	if strings.TrimSpace(msg.MessageID) != "" {
		return a.ReplyPostMessage(ctx, msg, blocks)
	}
	if strings.TrimSpace(msg.ChatID) != "" {
		return a.SendChatPostMessage(ctx, msg.ChatID, msg.ChatType, blocks)
	}
	return SentMessage{}, fmt.Errorf("飞书 message_id 为空")
}

func (a *Adapter) SendChatPostMessage(ctx context.Context, chatID string, chatType string, blocks []outboundBlock) (SentMessage, error) {
	if a.client == nil {
		return SentMessage{}, fmt.Errorf("飞书客户端未初始化")
	}
	if chatID == "" {
		return SentMessage{}, fmt.Errorf("飞书 chat_id 为空")
	}
	content, err := outboundBlocksPostContent(blocks)
	if err != nil {
		return SentMessage{}, err
	}
	resp, err := a.client.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType("post").
			Content(content).
			Build()).
		Build())
	if err != nil {
		return SentMessage{}, fmt.Errorf("调用飞书发富文本消息接口: %w", err)
	}
	if !resp.Success() {
		return SentMessage{}, fmt.Errorf("飞书发富文本消息接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return sentMessageFromCreateResp(resp, chatID, chatType), nil
}

func (a *Adapter) ReplyText(ctx context.Context, msg Message, text string) error {
	_, err := a.ReplyTextMessage(ctx, msg, text)
	return err
}

func (a *Adapter) ReplyTextMessage(ctx context.Context, msg Message, text string) (SentMessage, error) {
	if a.client == nil {
		return SentMessage{}, fmt.Errorf("飞书客户端未初始化")
	}
	if msg.MessageID == "" {
		return SentMessage{}, fmt.Errorf("飞书 message_id 为空")
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return SentMessage{}, fmt.Errorf("编码飞书回复内容: %w", err)
	}
	msgType := "text"
	replyInThread := replyInThreadForMessage(msg)
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(msg.MessageID).
		Body(&larkim.ReplyMessageReqBody{
			Content:       larkcore.StringPtr(string(content)),
			MsgType:       larkcore.StringPtr(msgType),
			ReplyInThread: &replyInThread,
		}).
		Build()
	resp, err := a.client.Im.Message.Reply(ctx, req)
	if err != nil {
		return SentMessage{}, fmt.Errorf("调用飞书回复接口: %w", err)
	}
	if !resp.Success() {
		return SentMessage{}, fmt.Errorf("飞书回复接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return sentMessageFromReplyResp(resp, msg), nil
}

func (a *Adapter) ReplyPostMessage(ctx context.Context, msg Message, blocks []outboundBlock) (SentMessage, error) {
	if a.client == nil {
		return SentMessage{}, fmt.Errorf("飞书客户端未初始化")
	}
	if msg.MessageID == "" {
		return SentMessage{}, fmt.Errorf("飞书 message_id 为空")
	}
	content, err := outboundBlocksPostContent(blocks)
	if err != nil {
		return SentMessage{}, err
	}
	replyInThread := replyInThreadForMessage(msg)
	resp, err := a.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(msg.MessageID).
		Body(&larkim.ReplyMessageReqBody{
			Content:       larkcore.StringPtr(content),
			MsgType:       larkcore.StringPtr("post"),
			ReplyInThread: &replyInThread,
		}).
		Build())
	if err != nil {
		return SentMessage{}, fmt.Errorf("调用飞书回复富文本接口: %w", err)
	}
	if !resp.Success() {
		return SentMessage{}, fmt.Errorf("飞书回复富文本接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return sentMessageFromReplyResp(resp, msg), nil
}

func (a *Adapter) SendImageMessage(ctx context.Context, msg Message, path string) (SentMessage, error) {
	if strings.TrimSpace(msg.MessageID) != "" {
		return a.ReplyImageMessage(ctx, msg, path)
	}
	if strings.TrimSpace(msg.ChatID) != "" {
		return a.SendChatImageMessage(ctx, msg.ChatID, msg.ChatType, path)
	}
	return SentMessage{}, fmt.Errorf("飞书 message_id 为空")
}

func (a *Adapter) SendChatImageMessage(ctx context.Context, chatID string, chatType string, path string) (SentMessage, error) {
	if a.client == nil {
		return SentMessage{}, fmt.Errorf("飞书客户端未初始化")
	}
	if chatID == "" {
		return SentMessage{}, fmt.Errorf("飞书 chat_id 为空")
	}
	imageKey, err := a.uploadReplyImage(ctx, path)
	if err != nil {
		return SentMessage{}, err
	}
	content, err := json.Marshal(map[string]string{"image_key": imageKey})
	if err != nil {
		return SentMessage{}, fmt.Errorf("编码飞书图片消息内容: %w", err)
	}
	resp, err := a.client.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType("image").
			Content(string(content)).
			Build()).
		Build())
	if err != nil {
		return SentMessage{}, fmt.Errorf("调用飞书发图片消息接口: %w", err)
	}
	if !resp.Success() {
		return SentMessage{}, fmt.Errorf("飞书发图片消息接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return sentMessageFromCreateResp(resp, chatID, chatType), nil
}

func (a *Adapter) ReplyImageMessage(ctx context.Context, msg Message, path string) (SentMessage, error) {
	if a.client == nil {
		return SentMessage{}, fmt.Errorf("飞书客户端未初始化")
	}
	if msg.MessageID == "" {
		return SentMessage{}, fmt.Errorf("飞书 message_id 为空")
	}
	imageKey, err := a.uploadReplyImage(ctx, path)
	if err != nil {
		return SentMessage{}, err
	}
	content, err := json.Marshal(map[string]string{"image_key": imageKey})
	if err != nil {
		return SentMessage{}, fmt.Errorf("编码飞书图片回复内容: %w", err)
	}
	replyInThread := replyInThreadForMessage(msg)
	resp, err := a.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(msg.MessageID).
		Body(&larkim.ReplyMessageReqBody{
			Content:       larkcore.StringPtr(string(content)),
			MsgType:       larkcore.StringPtr("image"),
			ReplyInThread: &replyInThread,
		}).
		Build())
	if err != nil {
		return SentMessage{}, fmt.Errorf("调用飞书回复图片接口: %w", err)
	}
	if !resp.Success() {
		return SentMessage{}, fmt.Errorf("飞书回复图片接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return sentMessageFromReplyResp(resp, msg), nil
}

func (a *Adapter) uploadReplyImage(ctx context.Context, path string) (string, error) {
	path, err := normalizeReplyImagePath(path)
	if err != nil {
		return "", err
	}
	if a.messages == nil {
		if a.client == nil {
			return "", fmt.Errorf("飞书客户端未初始化")
		}
		a.messages = larkMessageClient{client: a.client}
	}
	imageKey, err := a.messages.UploadImage(ctx, path)
	if err != nil {
		return "", fmt.Errorf("上传飞书图片: %w", err)
	}
	return imageKey, nil
}

func replyInThreadForMessage(msg Message) bool {
	if msg.ForceReplyInThread {
		return true
	}
	return msg.IsTopicThread()
}

func (a *Adapter) UpdateText(ctx context.Context, messageID string, text string) error {
	if a.client == nil {
		return fmt.Errorf("飞书客户端未初始化")
	}
	if strings.TrimSpace(messageID) == "" {
		return fmt.Errorf("飞书 message_id 为空")
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("编码飞书消息内容: %w", err)
	}
	resp, err := a.client.Im.V1.Message.Update(ctx, larkim.NewUpdateMessageReqBuilder().
		MessageId(messageID).
		Body(larkim.NewUpdateMessageReqBodyBuilder().
			MsgType("text").
			Content(string(content)).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("调用飞书更新消息接口: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("飞书更新消息接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func sentMessageFromCreateResp(resp *larkim.CreateMessageResp, chatID string, chatType string) SentMessage {
	sent := SentMessage{ChatID: chatID, ChatType: chatType}
	if resp == nil || resp.Data == nil {
		return sent
	}
	sent.MessageID = value(resp.Data.MessageId)
	sent.ChatID = firstNonEmpty(value(resp.Data.ChatId), sent.ChatID)
	sent.ThreadID = value(resp.Data.ThreadId)
	sent.RootID = value(resp.Data.RootId)
	sent.ParentID = value(resp.Data.ParentId)
	return sent
}

func sentMessageFromReplyResp(resp *larkim.ReplyMessageResp, msg Message) SentMessage {
	sent := SentMessage{ChatID: msg.ChatID, ChatType: msg.ChatType}
	if resp == nil || resp.Data == nil {
		return sent
	}
	sent.MessageID = value(resp.Data.MessageId)
	sent.ChatID = firstNonEmpty(value(resp.Data.ChatId), sent.ChatID)
	sent.ThreadID = firstNonEmpty(value(resp.Data.ThreadId), msg.ThreadID)
	sent.RootID = value(resp.Data.RootId)
	sent.ParentID = value(resp.Data.ParentId)
	return sent
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// processingReactionEmojis 收到消息时随机选一个表情加在消息上
// 表情列表: https://open.larkoffice.com/document/server-docs/im-v1/message-reaction/emojis-introduce
var processingReactionEmojis = []string{
	"OK",         // OK
	"Get",        // 收到
	"WINK",       // Wink 眨眼
	"WITTY",      // 灵光一闪
	"DIZZY",      // 头晕
	"MeMeMe",     // 举手, 我我我
	"THINKING",   // 思考
	"Typing",     // 打字
	"OnIt",       // 在看了
	"OneSecond",  // 再等一下
	"GoGoGo",     // 开始干
	"SaluteFace", // 敬礼
}

type larkReactionClient struct {
	client *lark.Client
}

type larkMessageClient struct {
	client *lark.Client
}

func (c larkMessageClient) GetMessage(ctx context.Context, messageID string, workspace string) (*Message, error) {
	if c.client == nil {
		return nil, fmt.Errorf("飞书客户端未初始化")
	}
	if strings.TrimSpace(messageID) == "" {
		return nil, fmt.Errorf("飞书 message_id 为空")
	}
	req := larkim.NewGetMessageReqBuilder().
		MessageId(messageID).
		UserIdType(larkim.GetMessageContentV1UserIDTypeOpenId).
		Build()
	resp, err := c.client.Im.V1.Message.Get(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("调用飞书获取消息接口: %w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("飞书获取消息接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || len(resp.Data.Items) == 0 || resp.Data.Items[0] == nil {
		return nil, fmt.Errorf("飞书获取消息接口未返回消息")
	}
	msg := messageFromLarkMessage(resp.Data.Items[0])
	if msg == nil {
		return nil, nil
	}
	msg.Workspace = workspace
	msg.Images = hydrateMessageImages(ctx, c, msg.MessageID, workspace, msg.Images)
	setMessagePrimaryImage(msg)
	return msg, nil
}

func messageFromLarkMessage(item *larkim.Message) *Message {
	if item == nil {
		return nil
	}
	msg := &Message{
		MessageID: value(item.MessageId),
		MsgType:   value(item.MsgType),
	}
	if item.Sender != nil {
		msg.SenderID = value(item.Sender.Id)
		msg.SenderType = value(item.Sender.SenderType)
	}
	if item.Body != nil {
		content := value(item.Body.Content)
		msg.Images = parseMessageImages(content)
		setMessagePrimaryImage(msg)
		if strings.EqualFold(msg.MsgType, "image") {
			msg.Text = ""
		} else if strings.EqualFold(msg.MsgType, "post") {
			msg.Text = parsePostContent(content)
		} else {
			msg.Text = parseMessageTextContent(content)
		}
	}
	for _, mention := range item.Mentions {
		if mention == nil {
			continue
		}
		msg.Mentions = append(msg.Mentions, Mention{
			Key:  value(mention.Key),
			ID:   value(mention.Id),
			Name: value(mention.Name),
			Type: value(mention.IdType),
		})
	}
	msg.Text = replaceMentionKeys(msg.Text, msg.Mentions)
	if msg.MessageID == "" && msg.MsgType == "" && msg.Text == "" && msg.ImageKey == "" && len(msg.Images) == 0 {
		return nil
	}
	return msg
}

func (c larkMessageClient) DownloadImage(ctx context.Context, messageID string, imageKey string, workspace string) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("飞书客户端未初始化")
	}
	messageID = strings.TrimSpace(messageID)
	imageKey = strings.TrimSpace(imageKey)
	workspace = strings.TrimSpace(workspace)
	if messageID == "" {
		return "", fmt.Errorf("飞书 message_id 为空")
	}
	if imageKey == "" {
		return "", fmt.Errorf("飞书 image_key 为空")
	}
	if workspace == "" {
		return "", fmt.Errorf("workspace 为空，无法保存飞书图片")
	}
	path := messageImageCachePath(workspace, imageKey)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("检查飞书图片缓存: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("创建飞书图片缓存目录: %w", err)
	}
	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(imageKey).
		Type("image").
		Build()
	resp, err := c.client.Im.V1.MessageResource.Get(ctx, req)
	if err != nil {
		return "", fmt.Errorf("调用飞书获取图片资源接口: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("飞书获取图片资源接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if err := resp.WriteFile(path); err != nil {
		return "", fmt.Errorf("写入飞书图片缓存: %w", err)
	}
	return path, nil
}

func (c larkMessageClient) UploadImage(ctx context.Context, path string) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("飞书客户端未初始化")
	}
	path, err := normalizeReplyImagePath(path)
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("打开图片文件: %w", err)
	}
	defer file.Close()
	body := larkim.NewCreateImageReqBodyBuilder().
		ImageType(larkim.CreateImageImageTypeMessage).
		Image(file).
		Build()
	resp, err := c.client.Im.V1.Image.Create(ctx, larkim.NewCreateImageReqBuilder().Body(body).Build())
	if err != nil {
		return "", fmt.Errorf("调用飞书上传图片接口: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("飞书上传图片接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || strings.TrimSpace(value(resp.Data.ImageKey)) == "" {
		return "", fmt.Errorf("飞书上传图片接口未返回 image_key")
	}
	return value(resp.Data.ImageKey), nil
}

func hydrateMessageImages(ctx context.Context, client messageClient, messageID string, workspace string, images []MessageImage) []MessageImage {
	if client == nil || len(images) == 0 {
		return images
	}
	hydrated := make([]MessageImage, 0, len(images))
	for _, image := range images {
		if strings.TrimSpace(image.ImageKey) != "" && strings.TrimSpace(image.LocalPath) == "" {
			path, err := client.DownloadImage(ctx, messageID, image.ImageKey, workspace)
			if err != nil {
				slog.WarnContext(ctx, "下载飞书图片失败", "错误", err)
				slog.DebugContext(ctx, "下载飞书图片失败详情", "message_id", messageID, "image_key", image.ImageKey, "错误", err)
			} else {
				image.LocalPath = path
			}
		}
		hydrated = append(hydrated, image)
	}
	return hydrated
}

func setMessagePrimaryImage(msg *Message) {
	if msg == nil || len(msg.Images) == 0 {
		return
	}
	msg.ImageKey = msg.Images[0].ImageKey
	msg.LocalPath = msg.Images[0].LocalPath
}

func setReplyPrimaryImage(reply *ReplyContext) {
	if reply == nil || len(reply.Images) == 0 {
		return
	}
	reply.ImageKey = reply.Images[0].ImageKey
	reply.LocalPath = reply.Images[0].LocalPath
}

func messageImageCachePath(workspace string, imageKey string) string {
	return filepath.Join(workspace, "cache", safeImageCacheName(imageKey)+".png")
}

func safeImageCacheName(imageKey string) string {
	imageKey = strings.TrimSpace(imageKey)
	if imageKey == "" {
		return "image"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_")
	return replacer.Replace(imageKey)
}

func normalizeReplyImagePath(path string) (string, error) {
	path = trimReplyImagePath(path)
	if path == "" {
		return "", fmt.Errorf("图片路径为空")
	}
	if strings.Contains(path, "://") && !strings.HasPrefix(strings.ToLower(path), "file://") {
		return "", fmt.Errorf("只支持本地图片路径: %s", path)
	}
	if strings.HasPrefix(strings.ToLower(path), "file://") {
		path = strings.TrimPrefix(path, "file://")
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("查找用户主目录: %w", err)
		}
		path = filepath.Join(home, path[2:])
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("图片路径必须是绝对路径: %s", path)
	}
	path = filepath.Clean(path)
	if !hasSupportedImageExtension(path) {
		return "", fmt.Errorf("不支持的图片类型: %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("检查图片文件: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("图片路径是目录: %s", path)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("图片文件为空: %s", path)
	}
	if info.Size() > 10*1024*1024 {
		return "", fmt.Errorf("图片文件超过 10MB: %s", path)
	}
	return path, nil
}

func trimReplyImagePath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.Trim(path, "\"'")
	return strings.TrimSpace(path)
}

func hasSupportedImageExtension(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".tif", ".tiff", ".bmp", ".ico":
		return true
	default:
		return false
	}
}

func (c larkReactionClient) AddReaction(ctx context.Context, messageID string) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("飞书客户端未初始化")
	}
	if messageID == "" {
		return "", fmt.Errorf("飞书 message_id 为空")
	}
	emojiType := randomProcessingReactionEmoji()
	req := larkim.NewCreateMessageReactionReqBuilder().
		MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(emojiType).Build()).
			Build()).
		Build()
	resp, err := c.client.Im.V1.MessageReaction.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("调用飞书添加 reaction 接口: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("飞书添加 reaction 接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.ReactionId == nil || strings.TrimSpace(*resp.Data.ReactionId) == "" {
		return "", fmt.Errorf("飞书添加 reaction 接口未返回 reaction_id")
	}
	return strings.TrimSpace(*resp.Data.ReactionId), nil
}

func randomProcessingReactionEmoji() string {
	if len(processingReactionEmojis) == 0 {
		return "OnIt"
	}
	return processingReactionEmojis[rand.Intn(len(processingReactionEmojis))]
}

func (c larkReactionClient) DeleteReaction(ctx context.Context, messageID string, reactionID string) error {
	if c.client == nil {
		return fmt.Errorf("飞书客户端未初始化")
	}
	if messageID == "" {
		return fmt.Errorf("飞书 message_id 为空")
	}
	if reactionID == "" {
		return fmt.Errorf("飞书 reaction_id 为空")
	}
	req := larkim.NewDeleteMessageReactionReqBuilder().
		MessageId(messageID).
		ReactionId(reactionID).
		Build()
	resp, err := c.client.Im.V1.MessageReaction.Delete(ctx, req)
	if err != nil {
		return fmt.Errorf("调用飞书删除 reaction 接口: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("飞书删除 reaction 接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (a *Adapter) addProcessingReaction(ctx context.Context, msg Message) string {
	if a.reaction == nil || strings.TrimSpace(msg.MessageID) == "" {
		return ""
	}
	reactionID, err := a.reaction.AddReaction(ctx, msg.MessageID)
	if err != nil {
		slog.WarnContext(ctx, "添加飞书处理中 reaction 失败", "错误", err)
		return ""
	}
	slog.InfoContext(ctx, "已添加飞书处理中 reaction", "reaction_id", reactionID)
	return reactionID
}

func (a *Adapter) StartProcessingReaction(ctx context.Context, msg Message) func() {
	reactionID := a.addProcessingReaction(ctx, msg)
	if strings.TrimSpace(reactionID) == "" {
		return func() {}
	}
	return func() {
		a.deleteProcessingReaction(ctx, msg, reactionID)
	}
}

func (a *Adapter) deleteProcessingReaction(ctx context.Context, msg Message, reactionID string) {
	if a.reaction == nil || strings.TrimSpace(reactionID) == "" {
		return
	}
	if err := a.reaction.DeleteReaction(ctx, msg.MessageID, reactionID); err != nil {
		slog.WarnContext(ctx, "删除飞书处理中 reaction 失败", "reaction_id", reactionID, "错误", err)
		return
	}
	slog.InfoContext(ctx, "已删除飞书处理中 reaction", "reaction_id", reactionID)
}
