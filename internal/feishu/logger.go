package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

var _ larkcore.Logger = (*logger)(nil)

type logger struct {
	*slog.Logger
	minLevel slog.Level
}

func NewLogger(minLevel slog.Level, botID, component string) *logger {
	return &logger{
		Logger:   slog.With("botID", botID, "comp", component),
		minLevel: minLevel,
	}
}

// Debug implements [larkcore.Logger].
func (l *logger) Debug(ctx context.Context, args ...any) {
	l.log(ctx, slog.LevelDebug, l.Logger.DebugContext, args...)
}

// Info implements [larkcore.Logger].
func (l *logger) Info(ctx context.Context, args ...any) {
	l.log(ctx, slog.LevelInfo, l.Logger.InfoContext, args...)

}

// Warn implements [larkcore.Logger].
func (l *logger) Warn(ctx context.Context, args ...any) {
	l.log(ctx, slog.LevelWarn, l.Logger.WarnContext, args...)

}

// Error implements [larkcore.Logger].
func (l *logger) Error(ctx context.Context, args ...any) {
	l.log(ctx, slog.LevelError, l.Logger.ErrorContext, args...)
}

func (l *logger) log(ctx context.Context, level slog.Level, fn func(ctx context.Context, msg string, args ...any), args ...any) {
	if level < l.minLevel {
		return
	}
	if l.Logger.Enabled(ctx, level) {
		format := make([]string, len(args))
		for i := range format {
			format[i] = "%v"
		}
		fn(ctx, fmt.Sprintf(strings.Join(format, " "), args...))
	}
}
