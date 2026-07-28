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
			name: "plan entries",
			update: acp.PromptUpdate{Update: acp.SessionUpdate{
				SessionUpdate: "plan",
				PlanEntries: []acp.PlanEntry{
					{Content: "读取现有实现", Status: "completed"},
					{Meta: map[string]any{"activeForm": "补过程消息展示"}, Status: "in_progress"},
				},
			}},
			want: "📌 计划\n- ✅ 读取现有实现\n- 🔄 补过程消息展示",
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

func TestPromptUpdateChunkRoutesPlanChunksToProcessPanel(t *testing.T) {
	chunk, ok := promptUpdateChunk(acp.PromptUpdate{Update: acp.SessionUpdate{
		SessionUpdate: "plan_chunk",
		Content:       &acp.ContentBlock{Type: "text", Text: "读取代码"},
	}})
	if !ok {
		t.Fatal("promptUpdateChunk() ok = false, want true")
	}
	if chunk.Target != promptChunkTargetPlan || chunk.Key != "plan_chunk" || chunk.Text != "读取代码" {
		t.Fatalf("chunk = %+v, want plan process chunk", chunk)
	}
	if isThoughtUpdateKind("plan") {
		t.Fatal("isThoughtUpdateKind(plan) = true, want false so /show thought does not hide plans")
	}
}

func TestIsPlanUpdateKindRequiresPlanToken(t *testing.T) {
	for _, kind := range []string{"plan", "plan_chunk", "agent_plan_chunk", "session.plan.delta"} {
		if !isPlanUpdateKind(kind) {
			t.Fatalf("isPlanUpdateKind(%q) = false, want true", kind)
		}
	}
	for _, kind := range []string{"explanation", "explanation_chunk", "planning", "plane"} {
		if isPlanUpdateKind(kind) {
			t.Fatalf("isPlanUpdateKind(%q) = true, want false", kind)
		}
	}
}
