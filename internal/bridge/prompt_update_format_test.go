package bridge

import (
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

func TestFormatPromptUpdatePrefixesProcessMessageOnly(t *testing.T) {
	tests := []struct {
		name   string
		update acp.PromptUpdate
		want   string
	}{
		{
			name: "process message",
			update: acp.PromptUpdate{Update: acp.SessionUpdate{
				SessionUpdate: "status",
				Message:       "准备处理",
			}},
			want: "💬 准备处理",
		},
		{
			name: "agent message",
			update: acp.PromptUpdate{Update: acp.SessionUpdate{
				SessionUpdate: "agent_message",
				Message:       "先说明一下",
			}},
			want: "💬 先说明一下",
		},
		{
			name: "thought message",
			update: acp.PromptUpdate{Update: acp.SessionUpdate{
				SessionUpdate: "reasoning",
				Message:       "分析用户需求",
			}},
			want: "🧠 分析用户需求",
		},
		{
			name: "agent chunk stays final text candidate",
			update: acp.PromptUpdate{Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "正文"},
			}},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatPromptUpdate(tt.update); got != tt.want {
				t.Fatalf("formatPromptUpdate() = %q, want %q", got, tt.want)
			}
		})
	}
}
