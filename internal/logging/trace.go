package logging

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

const TraceIDAttr = "trace_id"

func TraceID(ctx context.Context) string {
	return CtxAttrString(ctx, TraceIDAttr)
}

func CtxAttrString(ctx context.Context, key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	attrs := CtxAttrs(ctx)
	for i := len(attrs) - 1; i >= 0; i-- {
		if attrs[i].Key != key {
			continue
		}
		if attrs[i].Value.Kind() == slog.KindString {
			return strings.TrimSpace(attrs[i].Value.String())
		}
		return strings.TrimSpace(attrs[i].Value.String())
	}
	return ""
}

func CtxHasAttr(ctx context.Context, key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	for _, attr := range CtxAttrs(ctx) {
		if attr.Key == key {
			return true
		}
	}
	return false
}

func CtxAddMissingAttr(ctx context.Context, attrs ...slog.Attr) context.Context {
	missing := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		if strings.TrimSpace(attr.Key) == "" || CtxHasAttr(ctx, attr.Key) {
			continue
		}
		missing = append(missing, attr)
	}
	return CtxAddAttr(ctx, missing...)
}

func EnsureTraceID(ctx context.Context, parts ...string) (context.Context, string) {
	if traceID := TraceID(ctx); traceID != "" {
		return ctx, traceID
	}
	traceID := TraceIDFromParts(parts...)
	return CtxAddMissingAttr(ctx, slog.String(TraceIDAttr, traceID)), traceID
}

func TraceIDFromParts(parts ...string) string {
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			clean = append(clean, part)
		}
	}
	if len(clean) == 0 {
		return randomTraceID()
	}
	sum := sha256.Sum256([]byte(strings.Join(clean, "\x00")))
	return "tr_" + hex.EncodeToString(sum[:8])
}

func randomTraceID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return "tr_" + hex.EncodeToString(buf[:])
	}
	return "tr_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}
