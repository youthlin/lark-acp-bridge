package logging

import (
	"context"
	"log/slog"
)

type ctxAttrsKey struct{}

// CtxAddAttr 往 ctx 上附加结构化日志字段。
func CtxAddAttr(ctx context.Context, attrs ...slog.Attr) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	all := append([]slog.Attr{}, CtxAttrs(ctx)...)
	all = append(all, attrs...)
	return context.WithValue(ctx, ctxAttrsKey{}, all)
}

// CtxAttrs 从 ctx 中取出通过 CtxAddAttr 附加的日志字段。
func CtxAttrs(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}
	attrs, _ := ctx.Value(ctxAttrsKey{}).([]slog.Attr)
	return attrs
}

// CtxHandler 会在 slog 记录写出前自动追加 ctx 上的日志字段。
type CtxHandler struct {
	inner slog.Handler
}

var _ slog.Handler = (*CtxHandler)(nil)

func NewCtxHandler(inner slog.Handler) *CtxHandler {
	return &CtxHandler{inner: inner}
}

func (h *CtxHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *CtxHandler) Handle(ctx context.Context, record slog.Record) error {
	if attrs := CtxAttrs(ctx); len(attrs) > 0 {
		record = record.Clone()
		record.AddAttrs(attrs...)
	}
	return h.inner.Handle(ctx, record)
}

func (h *CtxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &CtxHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *CtxHandler) WithGroup(name string) slog.Handler {
	return &CtxHandler{inner: h.inner.WithGroup(name)}
}
