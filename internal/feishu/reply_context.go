package feishu

import (
	"context"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

type intermediateReplySender func(context.Context, Message, string) error

type intermediateReplySenderKey struct{}

// StreamCard 表示一张可流式更新的飞书卡片。
type StreamCard interface {
	UpdateProcess(context.Context, string) error
	UpdateText(context.Context, string) error
	Close(context.Context) error
}

type streamCardStarter func(context.Context, Message) (StreamCard, error)

type streamCardStarterKey struct{}

type permissionRequester func(context.Context, Message, acp.PermissionRequest) (acp.PermissionOutcome, error)

type permissionRequesterKey struct{}

type ModelOption struct {
	Value string
	Name  string
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

type modelSelectionCardSender func(context.Context, Message, ModelSelectionCard) error

type modelSelectionCardSenderKey struct{}

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

func WithStreamCardStarter(ctx context.Context, starter func(context.Context, Message) (StreamCard, error)) context.Context {
	if starter == nil {
		return ctx
	}
	return context.WithValue(ctx, streamCardStarterKey{}, streamCardStarter(starter))
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
