package bridge

import (
	"encoding/json"
	"strings"
	"testing"

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
	status := promptStatusBar{state: promptStatusRunning}
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

	got := status.text()
	want := "执行中 | 511.8K(97%), 356 | 69K/258K"
	if got != want {
		t.Fatalf("status text = %q, want %q", got, want)
	}
}

func TestPromptStatusBarUsesMillionUnit(t *testing.T) {
	status := promptStatusBar{
		state:       promptStatusCompleted,
		input:       2_908_700,
		cachedInput: 2_763_265,
		output:      1_200_000,
		Context:     acp.ContextWindowUsage{Used: 2_908_700, Size: 4_096_000},
	}

	got := status.text()
	want := "已完成 | 2.9M(95%), 1.2M | 2M/4M"
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
			want: "已完成 | 1K",
		},
		{
			name: "input only without cache rate",
			bar:  promptStatusBar{state: promptStatusCompleted, input: 1000},
			want: "已完成 | 1K",
		},
		{
			name: "no usage",
			bar:  promptStatusBar{state: promptStatusCompleted},
			want: "已完成",
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
	if got, want := status.text(), "已取消 | 1.2K, 345"; got != want {
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
