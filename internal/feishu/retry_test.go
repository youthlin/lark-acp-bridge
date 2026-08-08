package feishu

import (
	"context"
	"errors"
	"io"
	"net/http"
	"syscall"
	"testing"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
)

func TestRetryFeishuAPIRetriesTransientError(t *testing.T) {
	var attempts int
	var sleeps []time.Duration
	got, err := retryFeishuAPI(context.Background(), feishuRetryOptions{
		MaxAttempts: 3,
		BaseDelay:   10 * time.Millisecond,
		Sleep: func(ctx context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
	}, func(context.Context) (string, error) {
		attempts++
		if attempts == 1 {
			return "", io.ErrUnexpectedEOF
		}
		return "ok", nil
	}, nil)
	if err != nil {
		t.Fatalf("retryFeishuAPI() error = %v", err)
	}
	if got != "ok" || attempts != 2 {
		t.Fatalf("result = %q attempts = %d, want ok after retry", got, attempts)
	}
	if len(sleeps) != 1 || sleeps[0] != 10*time.Millisecond {
		t.Fatalf("sleeps = %v, want one base delay", sleeps)
	}
}

func TestRetryFeishuAPIRetriesRetryableStatus(t *testing.T) {
	var attempts int
	got, err := retryFeishuAPI(context.Background(), feishuRetryOptions{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		Sleep:       func(context.Context, time.Duration) error { return nil },
	}, func(context.Context) (*larkcore.ApiResp, error) {
		attempts++
		status := http.StatusOK
		if attempts == 1 {
			status = http.StatusTooManyRequests
		}
		return &larkcore.ApiResp{StatusCode: status}, nil
	}, shouldRetryFeishuAPIResp)
	if err != nil {
		t.Fatalf("retryFeishuAPI() error = %v", err)
	}
	if got.StatusCode != http.StatusOK || attempts != 2 {
		t.Fatalf("status = %d attempts = %d, want retry after 429", got.StatusCode, attempts)
	}
}

func TestRetryFeishuAPIStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var attempts int
	_, err := retryFeishuAPI(ctx, feishuRetryOptions{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		Sleep: func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		},
	}, func(context.Context) (string, error) {
		attempts++
		return "", syscall.ECONNRESET
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want no retry after cancelled sleep", attempts)
	}
}

func TestRetryFeishuAPIDoesNotRetryBusinessError(t *testing.T) {
	var attempts int
	_, err := retryFeishuAPI(context.Background(), feishuRetryOptions{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		Sleep:       func(context.Context, time.Duration) error { return nil },
	}, func(context.Context) (string, error) {
		attempts++
		return "", errors.New("飞书获取消息接口返回错误: code=99991663 msg=invalid param")
	}, nil)
	if err == nil {
		t.Fatal("retryFeishuAPI() error = nil, want business error")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want no retry for business error", attempts)
	}
}

func TestShouldRetryFeishuAPIResp(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		if !shouldRetryFeishuAPIResp(&larkcore.ApiResp{StatusCode: status}) {
			t.Fatalf("status %d should be retryable", status)
		}
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		if shouldRetryFeishuAPIResp(&larkcore.ApiResp{StatusCode: status}) {
			t.Fatalf("status %d should not be retryable", status)
		}
	}
}
