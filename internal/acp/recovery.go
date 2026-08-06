package acp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
)

// ErrClientMessagePanicked 表示 ACP 消息处理 goroutine 发生 panic 并已恢复。
var ErrClientMessagePanicked = errors.New("ACP client message handler panicked")

type clientPanicError struct {
	value any
	stack []byte
}

func (e *clientPanicError) Error() string {
	return fmt.Sprintf("%s: %v", ErrClientMessagePanicked.Error(), e.value)
}

func (e *clientPanicError) Unwrap() error {
	return ErrClientMessagePanicked
}

// withRecover 在 ACP client 后台 goroutine 入口恢复 panic。
// name 用于标识 readLoop / agent request 等来源；发生 panic 时记录堆栈，
// 并按调用方传入的 onPanic 执行 fail pending / close client 等清理。
func withRecover(ctx context.Context, name string, onPanic func(error)) {
	if r := recover(); r != nil {
		stack := debug.Stack()
		err := &clientPanicError{value: r, stack: stack}
		slog.ErrorContext(ctx, "ACP client 后台处理发生 panic，已恢复",
			"handler", name,
			"panic", r,
			"stack", string(stack),
		)
		if onPanic != nil {
			onPanic(err)
		}
	}
}
