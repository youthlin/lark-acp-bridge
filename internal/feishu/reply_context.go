package feishu

import "context"

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
