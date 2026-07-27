package feishu

import (
	"context"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

type intermediateReplySender func(context.Context, Message, string) error

type intermediateReplySenderKey struct{}

type sentMessageSender func(context.Context, Message, string) (SentMessage, error)

type sentMessageSenderKey struct{}

type messageUpdater func(context.Context, string, string) error

type messageUpdaterKey struct{}

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

type loopStatusCardSender func(context.Context, Message, LoopStatusCardRequest) (LoopStatusCard, error)

type loopStatusCardSenderKey struct{}

// StreamCard 表示一张可流式更新的飞书卡片。
type StreamCard interface {
	UpdateProcess(context.Context, string) error
	UpdateStatus(context.Context, string) error
	UpdateUsageDetail(context.Context, string) error
	UpdateText(context.Context, string) error
	Close(context.Context) error
}

type streamCardStarter func(context.Context, Message) (StreamCard, error)

type streamCardStarterKey struct{}

type streamCardProcessPanelKey struct{}

type streamCardStatusBarKey struct{}

type permissionRequester func(context.Context, Message, acp.PermissionRequest) (acp.PermissionOutcome, error)

type permissionRequesterKey struct{}

type processingReactionStarter func(context.Context, Message) func()

type processingReactionStarterKey struct{}

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
	BotID        string
	ChatID       string
	ThreadID     string
	ACPSessionID string
	RequesterID  string
	CurrentModel string
	Options      []ModelOption
}

type ModeSelectionCard struct {
	BotID        string
	ChatID       string
	ThreadID     string
	ACPSessionID string
	RequesterID  string
	CurrentMode  string
	Options      []ModeOption
}

type SessionSelectionCard struct {
	BotID               string
	ChatID              string
	ThreadID            string
	RequesterID         string
	CurrentACPSessionID string
	Options             []SessionOption
}

type modelSelectionCardSender func(context.Context, Message, ModelSelectionCard) error

type modelSelectionCardSenderKey struct{}

type modeSelectionCardSender func(context.Context, Message, ModeSelectionCard) error

type modeSelectionCardSenderKey struct{}

type sessionSelectionCardSender func(context.Context, Message, SessionSelectionCard) error

type sessionSelectionCardSenderKey struct{}

type configDetailCardSender func(context.Context, Message, ConfigDetailCard) error

type configDetailCardSenderKey struct{}

func WithIntermediateReplySender(ctx context.Context, sender func(context.Context, Message, string) error) context.Context {
	if sender == nil {
		return ctx
	}
	return context.WithValue(ctx, intermediateReplySenderKey{}, intermediateReplySender(sender))
}

func SendIntermediateReply(ctx context.Context, msg Message, text string) (bool, error) {
	sender, ok := ctx.Value(intermediateReplySenderKey{}).(intermediateReplySender)
	if !ok || sender == nil {
		return false, nil
	}
	return true, sender(ctx, msg, text)
}

func WithSentMessageSender(ctx context.Context, sender func(context.Context, Message, string) (SentMessage, error)) context.Context {
	if sender == nil {
		return ctx
	}
	return context.WithValue(ctx, sentMessageSenderKey{}, sentMessageSender(sender))
}

func SendMessage(ctx context.Context, msg Message, text string) (SentMessage, bool, error) {
	sender, ok := ctx.Value(sentMessageSenderKey{}).(sentMessageSender)
	if !ok || sender == nil {
		return SentMessage{}, false, nil
	}
	sent, err := sender(ctx, msg, text)
	return sent, true, err
}

func WithMessageUpdater(ctx context.Context, updater func(context.Context, string, string) error) context.Context {
	if updater == nil {
		return ctx
	}
	return context.WithValue(ctx, messageUpdaterKey{}, messageUpdater(updater))
}

func UpdateMessageText(ctx context.Context, messageID string, text string) (bool, error) {
	updater, ok := ctx.Value(messageUpdaterKey{}).(messageUpdater)
	if !ok || updater == nil {
		return false, nil
	}
	return true, updater(ctx, messageID, text)
}

func WithLoopStatusCardSender(ctx context.Context, sender func(context.Context, Message, LoopStatusCardRequest) (LoopStatusCard, error)) context.Context {
	if sender == nil {
		return ctx
	}
	return context.WithValue(ctx, loopStatusCardSenderKey{}, loopStatusCardSender(sender))
}

func SendLoopStatusCard(ctx context.Context, msg Message, request LoopStatusCardRequest) (LoopStatusCard, bool, error) {
	sender, ok := ctx.Value(loopStatusCardSenderKey{}).(loopStatusCardSender)
	if !ok || sender == nil {
		return nil, false, nil
	}
	card, err := sender(ctx, msg, request)
	return card, true, err
}

func WithStreamCardStarter(ctx context.Context, starter func(context.Context, Message) (StreamCard, error)) context.Context {
	if starter == nil {
		return ctx
	}
	return context.WithValue(ctx, streamCardStarterKey{}, streamCardStarter(starter))
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

func StartStreamCard(ctx context.Context, msg Message) (StreamCard, bool, error) {
	starter, ok := ctx.Value(streamCardStarterKey{}).(streamCardStarter)
	if !ok || starter == nil {
		return nil, false, nil
	}
	card, err := starter(ctx, msg)
	return card, true, err
}

func WithPermissionRequester(ctx context.Context, requester func(context.Context, Message, acp.PermissionRequest) (acp.PermissionOutcome, error)) context.Context {
	if requester == nil {
		return ctx
	}
	return context.WithValue(ctx, permissionRequesterKey{}, permissionRequester(requester))
}

func RequestPermission(ctx context.Context, msg Message, req acp.PermissionRequest) (acp.PermissionOutcome, bool, error) {
	requester, ok := ctx.Value(permissionRequesterKey{}).(permissionRequester)
	if !ok || requester == nil {
		return acp.PermissionOutcome{}, false, nil
	}
	outcome, err := requester(ctx, msg, req)
	return outcome, true, err
}

func WithProcessingReactionStarter(ctx context.Context, starter func(context.Context, Message) func()) context.Context {
	if starter == nil {
		return ctx
	}
	return context.WithValue(ctx, processingReactionStarterKey{}, processingReactionStarter(starter))
}

func StartProcessingReaction(ctx context.Context, msg Message) (func(), bool) {
	starter, ok := ctx.Value(processingReactionStarterKey{}).(processingReactionStarter)
	if !ok || starter == nil {
		return nil, false
	}
	cleanup := starter(ctx, msg)
	if cleanup == nil {
		cleanup = func() {}
	}
	return cleanup, true
}

func WithModelSelectionCardSender(ctx context.Context, sender func(context.Context, Message, ModelSelectionCard) error) context.Context {
	if sender == nil {
		return ctx
	}
	return context.WithValue(ctx, modelSelectionCardSenderKey{}, modelSelectionCardSender(sender))
}

func SendModelSelectionCard(ctx context.Context, msg Message, card ModelSelectionCard) (bool, error) {
	sender, ok := ctx.Value(modelSelectionCardSenderKey{}).(modelSelectionCardSender)
	if !ok || sender == nil {
		return false, nil
	}
	return true, sender(ctx, msg, card)
}

func WithModeSelectionCardSender(ctx context.Context, sender func(context.Context, Message, ModeSelectionCard) error) context.Context {
	if sender == nil {
		return ctx
	}
	return context.WithValue(ctx, modeSelectionCardSenderKey{}, modeSelectionCardSender(sender))
}

func SendModeSelectionCard(ctx context.Context, msg Message, card ModeSelectionCard) (bool, error) {
	sender, ok := ctx.Value(modeSelectionCardSenderKey{}).(modeSelectionCardSender)
	if !ok || sender == nil {
		return false, nil
	}
	return true, sender(ctx, msg, card)
}

func WithSessionSelectionCardSender(ctx context.Context, sender func(context.Context, Message, SessionSelectionCard) error) context.Context {
	if sender == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionSelectionCardSenderKey{}, sessionSelectionCardSender(sender))
}

func SendSessionSelectionCard(ctx context.Context, msg Message, card SessionSelectionCard) (bool, error) {
	sender, ok := ctx.Value(sessionSelectionCardSenderKey{}).(sessionSelectionCardSender)
	if !ok || sender == nil {
		return false, nil
	}
	return true, sender(ctx, msg, card)
}

func WithConfigDetailCardSender(ctx context.Context, sender func(context.Context, Message, ConfigDetailCard) error) context.Context {
	if sender == nil {
		return ctx
	}
	return context.WithValue(ctx, configDetailCardSenderKey{}, configDetailCardSender(sender))
}

func SendConfigDetailCard(ctx context.Context, msg Message, card ConfigDetailCard) (bool, error) {
	sender, ok := ctx.Value(configDetailCardSenderKey{}).(configDetailCardSender)
	if !ok || sender == nil {
		return false, nil
	}
	return true, sender(ctx, msg, card)
}
