package feishu

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/url"
	"strings"
	"syscall"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

const (
	defaultFeishuRetryMaxAttempts = 3
	defaultFeishuRetryBaseDelay   = 200 * time.Millisecond
)

type feishuRetryOptions struct {
	MaxAttempts int
	BaseDelay   time.Duration
	Sleep       func(context.Context, time.Duration) error
}

func defaultFeishuRetryOptions() feishuRetryOptions {
	return feishuRetryOptions{
		MaxAttempts: defaultFeishuRetryMaxAttempts,
		BaseDelay:   defaultFeishuRetryBaseDelay,
		Sleep:       sleepWithContext,
	}
}

func retryFeishuAPI[T any](ctx context.Context, opts feishuRetryOptions, call func(context.Context) (T, error), shouldRetryResp func(T) bool) (T, error) {
	opts = normalizeFeishuRetryOptions(opts)
	var last T
	for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		resp, err := call(ctx)
		last = resp
		if err == nil {
			if shouldRetryResp == nil || !shouldRetryResp(resp) || attempt == opts.MaxAttempts {
				return resp, nil
			}
		} else if !shouldRetryFeishuError(err) || attempt == opts.MaxAttempts {
			return resp, err
		}
		delay := feishuRetryDelay(opts.BaseDelay, attempt)
		if err := opts.Sleep(ctx, delay); err != nil {
			return last, err
		}
	}
	return last, nil
}

func normalizeFeishuRetryOptions(opts feishuRetryOptions) feishuRetryOptions {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = defaultFeishuRetryMaxAttempts
	}
	if opts.BaseDelay <= 0 {
		opts.BaseDelay = defaultFeishuRetryBaseDelay
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepWithContext
	}
	return opts
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func feishuRetryDelay(base time.Duration, attempt int) time.Duration {
	if attempt <= 1 {
		return base
	}
	multiplier := math.Pow(2, float64(attempt-1))
	return time.Duration(float64(base) * multiplier)
}

func shouldRetryFeishuError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if urlErr.Timeout() {
			return true
		}
		err = urlErr.Err
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	text := strings.ToLower(err.Error())
	for _, token := range []string{
		"server time out",
		"server timeout",
		"client timeout",
		"dial tcp",
		"connection reset by peer",
		"connection refused",
		"broken pipe",
	} {
		if strings.Contains(text, token) {
			return true
		}
	}
	return false
}

func shouldRetryFeishuAPIResp(resp *larkcore.ApiResp) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode == 429 || resp.StatusCode >= 500
}
