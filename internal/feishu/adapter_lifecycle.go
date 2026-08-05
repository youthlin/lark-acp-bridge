package feishu

import (
	"context"
	"log/slog"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// Start 启动Bot监听
func (a *Adapter) Start(ctx context.Context) error {
	appSecret := a.cfg.AppSecret.RuntimeValue()
	if a.cfg.AppID == "" || appSecret == "" {
		slog.Warn("未配置飞书机器人凭证，消息监听未启动")
		return nil
	}
	if a.deduper != nil {
		if err := a.deduper.Load(); err != nil {
			slog.Warn("加载飞书消息去重状态失败", "bot", a.cfg.ID, "错误", err)
		}
	}

	clientOptions := []lark.ClientOptionFunc{lark.WithLogger(NewLogger(slog.LevelInfo, a.cfg.ID, "lark-sdk"))}
	a.client = lark.NewClient(a.cfg.AppID, appSecret, clientOptions...)
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
		appSecret,
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
	handler := dispatcher.NewEventDispatcher(a.cfg.AppID, a.cfg.AppSecret.RuntimeValue()).
		OnP2MessageReceiveV1(a.handleMessage).
		OnP2MessageReactionCreatedV1(a.handleReactionCreated).
		OnP2MessageReactionDeletedV1(a.handleReactionDeleted).
		OnP2NoticeCommentAddV1(a.handleDriveCommentAdd).
		OnP2CardActionTrigger(a.handleCardAction)
	handler.InitConfig(larkevent.WithLogger(NewLogger(slog.LevelInfo, a.cfg.ID, "lark-handler")))
	return handler
}

func (a *Adapter) Shutdown(ctx context.Context) error {
	if a.ws != nil {
		a.ws.Close()
	}
	return nil
}
