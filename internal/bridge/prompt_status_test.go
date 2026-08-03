package bridge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

func TestPromptResultHasUsageDetail(t *testing.T) {
	tests := []struct {
		name   string
		result acp.PromptResult
		want   bool
	}{
		{
			name: "structured usage",
			result: acp.PromptResult{Usage: acp.TokenUsage{
				InputTokens: 1,
			}},
			want: true,
		},
		{
			name: "trae meta",
			result: acp.PromptResult{Meta: acp.PromptResultMeta{
				TraeTokenUsage: &acp.TraeTokenUsage{},
			}},
			want: true,
		},
		{
			name:   "raw usage",
			result: acp.PromptResult{Raw: json.RawMessage(`{"usage":{"inputTokens":1}}`)},
			want:   true,
		},
		{
			name:   "raw meta",
			result: acp.PromptResult{Raw: json.RawMessage(`{"_meta":{"trace":"abc"}}`)},
			want:   true,
		},
		{
			name:   "no usage",
			result: acp.PromptResult{Raw: json.RawMessage(`{"stopReason":"end_turn"}`)},
			want:   false,
		},
		{
			name:   "empty usage object",
			result: acp.PromptResult{Raw: json.RawMessage(`{"usage":{}}`)},
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := promptResultHasUsageDetail(tt.result); got != tt.want {
				t.Fatalf("promptResultHasUsageDetail() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPromptStatusBarUsesMetaOnlyAsFallback(t *testing.T) {
	startedAt := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	status := promptStatusBar{state: promptStatusRunning, startedAt: startedAt}
	status.Context = acp.ContextWindowUsage{Used: 69200, Size: 258400}
	status.applyPromptResult(acp.PromptResult{
		Usage: acp.TokenUsage{
			InputTokens:      511800,
			CachedReadTokens: 496446,
		},
		Meta: acp.PromptResultMeta{
			TraeTokenUsage: &acp.TraeTokenUsage{
				TurnDisplay: acp.TokenUsage{
					InputTokens:  987,
					OutputTokens: 356,
				},
				ContextWindow: acp.ContextWindowUsage{Used: 99000, Size: 300000},
			},
		},
	})

	got := status.textAt(startedAt.Add(95 * time.Second))
	want := "⏳ 1m35s | 511.8K(97%), 356 | 69K/258K(27%)"
	if got != want {
		t.Fatalf("status text = %q, want %q", got, want)
	}
}

func TestPromptStatusBarUsesMillionUnit(t *testing.T) {
	startedAt := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	status := promptStatusBar{
		state:       promptStatusCompleted,
		startedAt:   startedAt,
		endedAt:     startedAt.Add(2*time.Minute + 5*time.Second),
		input:       2_908_700,
		cachedInput: 2_763_265,
		output:      1_200_000,
		Context:     acp.ContextWindowUsage{Used: 2_908_700, Size: 4_096_000},
	}

	got := status.text()
	want := "✅ 2m5s | 2.9M(95%), 1.2M | 2M/4M(71%)"
	if got != want {
		t.Fatalf("status text = %q, want %q", got, want)
	}
}

func TestPromptStatusBarOmitsMissingTokenUsage(t *testing.T) {
	tests := []struct {
		name string
		bar  promptStatusBar
		want string
	}{
		{
			name: "output only",
			bar:  promptStatusBar{state: promptStatusCompleted, output: 1000},
			want: "✅ 0s | 1K",
		},
		{
			name: "input only without cache rate",
			bar:  promptStatusBar{state: promptStatusCompleted, input: 1000},
			want: "✅ 0s | 1K",
		},
		{
			name: "no usage",
			bar:  promptStatusBar{state: promptStatusCompleted},
			want: "✅ 0s",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.bar.text(); got != tt.want {
				t.Fatalf("status text = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatPromptElapsedDuration(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want string
	}{
		{name: "subsecond", in: 999 * time.Millisecond, want: "0s"},
		{name: "seconds", in: 5 * time.Second, want: "5s"},
		{name: "minutes seconds", in: 95 * time.Second, want: "1m35s"},
		{name: "whole minutes", in: 2 * time.Minute, want: "2m"},
		{name: "hours minutes seconds", in: 2*time.Hour + 3*time.Minute + 4*time.Second + 500*time.Millisecond, want: "2h3m4s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatPromptElapsedDuration(tt.in); got != tt.want {
				t.Fatalf("formatPromptElapsedDuration(%s) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPromptStatusBarCancelledStopReason(t *testing.T) {
	status := promptStatusBar{state: promptStatusRunning}
	status.applyPromptResult(acp.PromptResult{
		StopReason: "cancelled",
		Usage: acp.TokenUsage{
			InputTokens:  1200,
			OutputTokens: 345,
		},
	})
	status.state = promptStatusFromStopReason("cancelled")
	status.stopReason = "cancelled"
	if got, want := status.text(), "🚫 0s | 1.2K, 345"; got != want {
		t.Fatalf("status text = %q, want %q", got, want)
	}
}

func TestFormatPromptResultDetailEscapesCodeFence(t *testing.T) {
	got := formatPromptResultDetail(acp.PromptResult{
		Raw: json.RawMessage("{\"message\":\"contains ``` fence\"}"),
	})
	if !strings.HasPrefix(got, "````json\n") || !strings.HasSuffix(got, "\n````") {
		t.Fatalf("detail = %q, want four-backtick fence", got)
	}
	if !strings.Contains(got, "contains ``` fence") {
		t.Fatalf("detail = %q, want raw content", got)
	}
}
