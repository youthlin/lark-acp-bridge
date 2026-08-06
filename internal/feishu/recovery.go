package feishu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"

	callback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

// ErrEventHandlerPanicked 表示飞书事件回调发生 panic 并已恢复。
var ErrEventHandlerPanicked = errors.New("feishu event handler panicked")

type eventPanicError struct {
	name  string
	value any
	stack []byte
}

func (e *eventPanicError) Error() string {
	return fmt.Sprintf("%s: %s: %v", ErrEventHandlerPanicked.Error(), e.name, e.value)
}

func (e *eventPanicError) Unwrap() error {
	return ErrEventHandlerPanicked
}

// recoverEventHandler 恢复飞书事件处理入口的 panic，避免单个事件处理异常
// 击穿 SDK dispatcher goroutine 或整个进程。
func recoverEventHandler(ctx context.Context, name string, errp *error) {
	if r := recover(); r != nil {
		stack := debug.Stack()
		err := &eventPanicError{name: name, value: r, stack: stack}
		slog.ErrorContext(ctx, "飞书事件处理发生 panic，已恢复",
			"handler", name,
			"panic", r,
			"stack", string(stack),
		)
		if errp != nil {
			*errp = err
		}
	}
}

// recoverCardEventHandler 恢复卡片交互回调 panic，并返回错误 toast 响应。
func recoverCardEventHandler(ctx context.Context, name string, response **callback.CardActionTriggerResponse, errp *error) {
	if r := recover(); r != nil {
		stack := debug.Stack()
		err := &eventPanicError{name: name, value: r, stack: stack}
		slog.ErrorContext(ctx, "飞书卡片交互处理发生 panic，已恢复",
			"handler", name,
			"panic", r,
			"stack", string(stack),
		)
		if response != nil && *response == nil {
			*response = permissionCardToast("error", "处理卡片操作失败")
		}
		if errp != nil {
			*errp = err
		}
	}
}
