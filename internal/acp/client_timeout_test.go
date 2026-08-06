package acp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestDefaultRPCTimeoutAppliesToKnownMethods(t *testing.T) {
	for _, method := range []string{
		"initialize",
		"authenticate",
		"session/new",
		"session/load",
		"session/resume",
		"session/close",
		"session/delete",
		"session/list",
		"session/set_config_option",
		"session/set_mode",
	} {
		if got := defaultRPCTimeout(method); got <= 0 {
			t.Errorf("defaultRPCTimeout(%q) = %s, want > 0", method, got)
		}
	}
}

func TestDefaultRPCTimeoutSkipsPromptAndUnknown(t *testing.T) {
	for _, method := range []string{"session/prompt", "session/update", "unknown"} {
		if got := defaultRPCTimeout(method); got != 0 {
			t.Errorf("defaultRPCTimeout(%q) = %s, want 0", method, got)
		}
	}
}

// TestCallRespectsExistingDeadline 验证当调用方已设置 deadline 时，
// call 不会被方法级默认超时放大：很短的 ctx 会按自身时间超时返回。
func TestCallRespectsExistingDeadline(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	// drain server：读掉请求和可能的 cancel 通知，避免 client 写 stdin 阻塞。
	go func() {
		dec := json.NewDecoder(server.reader)
		for {
			var msg Message
			if err := dec.Decode(&msg); err != nil {
				return
			}
			// 故意不回复 set_mode，让请求等到 ctx 超时。
			_ = msg
		}
	}()

	shortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.call(shortCtx, "session/set_mode", map[string]any{
		"sessionId": "session-1",
		"modeId":    "x",
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call error = %v, want DeadlineExceeded", err)
	}
	// 默认 op 超时为 30s；若默认超时覆盖了短 ctx，这里会接近 30s。
	if elapsed > time.Second {
		t.Fatalf("call took %s, existing short deadline was not honored", elapsed)
	}
}

// TestCallDefaultTimeoutAppliesWithoutDeadline 验证无 deadline 的 ctx 会套用方法级默认超时：
// 当 server 不回复时，请求在默认超时（测试里临时改小）后返回 DeadlineExceeded，而非永久挂起。
func TestCallDefaultTimeoutAppliesWithoutDeadline(t *testing.T) {
	orig := defaultSessionOpTimeout
	defaultSessionOpTimeout = 50 * time.Millisecond
	defer func() { defaultSessionOpTimeout = orig }()

	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	go func() {
		dec := json.NewDecoder(server.reader)
		for {
			var msg Message
			if err := dec.Decode(&msg); err != nil {
				return
			}
			_ = msg
		}
	}()

	start := time.Now()
	_, err := client.call(context.Background(), "session/set_mode", map[string]any{
		"sessionId": "session-1",
		"modeId":    "x",
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call error = %v, want DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("call took %s, default timeout not applied", elapsed)
	}
}
