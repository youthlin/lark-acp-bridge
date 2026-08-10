package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestWikiTraceShowsFullProcess(t *testing.T) {
	cfg := config.Config{Bots: []config.BotConfig{{
		ID:        "bot-a",
		Workspace: t.TempDir(),
		WikiTrace: config.WikiTraceConfig{Enabled: true, ChatID: "oc_trace"},
	}}}
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err := store.UpsertChat(ChatConfig{
		Key:          ChatKey{BotID: "bot-a", ChatID: "oc_trace"},
		ShowThoughts: true,
	}); err != nil {
		t.Fatalf("UpsertChat(trace show config) error = %v", err)
	}
	svc := NewService(cfg, store)
	var target feishu.Message
	var initialMeta feishu.StreamCardMeta
	card := &fakeStreamCard{}
	svc.scheduleStreams["bot-a"] = func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		target = msg
		initialMeta = feishu.StreamCardMetaFromContext(ctx)
		return card, nil
	}
	session := Session{
		Key:          normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_source", SubID: "omt_source"}),
		Title:        "来源会话标题",
		ACPSessionID: "acp-wiki",
		Cwd:          t.TempDir(),
	}
	observer := svc.wikiTraceObserver(session)
	observer.start(context.Background())
	observer.onUpdate(acp.PromptUpdate{Update: acp.SessionUpdate{
		SessionUpdate: "agent_message_chunk",
		Content:       &acp.ContentBlock{Type: "text", Text: "正在整理知识。"},
	}})
	observer.onUpdate(acp.PromptUpdate{Update: acp.SessionUpdate{
		SessionUpdate: "tool_call",
		ToolCallID:    "tool-0",
		Title:         "读取 workspace",
		Status:        "in_progress",
	}})
	observer.onUpdate(acp.PromptUpdate{Update: acp.SessionUpdate{
		SessionUpdate: "agent_message_chunk",
		Content:       &acp.ContentBlock{Type: "text", Text: "整理完成。"},
	}})
	observer.onUpdate(acp.PromptUpdate{Update: acp.SessionUpdate{
		SessionUpdate: "thought_chunk",
		Content:       &acp.ContentBlock{Type: "text", Text: "分析是否需要沉淀。"},
	}})
	observer.onUpdate(acp.PromptUpdate{Update: acp.SessionUpdate{
		SessionUpdate: "plan",
		Message:       "检查知识索引",
	}})
	observer.onUpdate(acp.PromptUpdate{Update: acp.SessionUpdate{
		SessionUpdate: "tool_call",
		ToolCallID:    "tool-1",
		Title:         "读取 knowledge/index.md",
		Status:        "in_progress",
	}})
	result := acp.PromptResult{
		Text:       "changed: yes\nfiles:\n- knowledge/index.md\nsummary: 修复索引\nreason: 保持一致性",
		StopReason: "end_turn",
	}
	observer.complete(context.Background(), result, nil)

	if target.ChatID != "oc_trace" || target.MessageID != "" || target.ThreadID != "" {
		t.Fatalf("stream target = %+v, want new root card in trace chat", target)
	}
	if initialMeta.Title != wikiTraceCardRunning || !initialMeta.HideHeaderIcon || !strings.Contains(initialMeta.Metadata, "**来源会话：**来源会话标题") || !strings.Contains(initialMeta.Metadata, "**来源聊天：**oc_source") || !strings.Contains(initialMeta.Metadata, "**来源话题：**omt_source") {
		t.Fatalf("initial meta = %+v, want source metadata", initialMeta)
	}
	process := strings.Join(card.processUpdatesSnapshot(), "\n")
	for _, want := range []string{"分析是否需要沉淀", "检查知识索引", "读取 knowledge/index.md"} {
		if !strings.Contains(process, want) {
			t.Fatalf("process updates = %q, want %q", process, want)
		}
	}
	if got := strings.Join(card.textUpdatesSnapshot(), "\n"); !strings.Contains(got, "正在整理知识") {
		t.Fatalf("text updates = %q, want agent text", got)
	}
	final := card.finalTextUpdatesSnapshot()
	if len(final) != 1 || final[0] != "整理完成。" {
		t.Fatalf("final text updates = %+v, want final agent text after boundary", final)
	}
	if !card.isClosed() {
		t.Fatal("wiki trace card was not closed")
	}
}

func TestWikiTraceUsesTraceChatShowConfig(t *testing.T) {
	tests := []struct {
		name        string
		chat        ChatConfig
		wantThought bool
	}{
		{
			name: "default hides thoughts",
			chat: ChatConfig{Key: ChatKey{BotID: "bot-a", ChatID: "oc_trace"}},
		},
		{
			name: "explicit hide thoughts",
			chat: ChatConfig{
				Key:          ChatKey{BotID: "bot-a", ChatID: "oc_trace"},
				ShowThoughts: false,
				HideThoughts: true,
			},
		},
		{
			name: "explicit show thoughts",
			chat: ChatConfig{
				Key:          ChatKey{BotID: "bot-a", ChatID: "oc_trace"},
				ShowThoughts: true,
			},
			wantThought: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{Bots: []config.BotConfig{{
				ID:        "bot-a",
				Workspace: t.TempDir(),
				WikiTrace: config.WikiTraceConfig{Enabled: true, ChatID: "oc_trace"},
			}}}
			store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
			if tt.chat.Key.Valid() {
				if err := store.UpsertChat(tt.chat); err != nil {
					t.Fatalf("UpsertChat(trace show config) error = %v", err)
				}
			}
			svc := NewService(cfg, store)
			card := &fakeStreamCard{}
			svc.scheduleStreams["bot-a"] = func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
				return card, nil
			}
			observer := svc.wikiTraceObserver(Session{
				Key:          normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_source"}),
				ACPSessionID: "acp-wiki",
			})
			if observer == nil {
				t.Fatal("wikiTraceObserver() = nil")
			}
			observer.start(context.Background())
			observer.onUpdate(acp.PromptUpdate{Update: acp.SessionUpdate{
				SessionUpdate: "thought_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "分析是否需要沉淀。"},
			}})
			observer.complete(context.Background(), acp.PromptResult{Text: "NoReply", StopReason: "end_turn"}, nil)
			process := strings.Join(card.processUpdatesSnapshot(), "\n")
			if tt.wantThought && !strings.Contains(process, "分析是否需要沉淀") {
				t.Fatalf("process updates = %q, want thought from trace chat show config", process)
			}
			if !tt.wantThought && strings.Contains(process, "分析是否需要沉淀") {
				t.Fatalf("process updates = %q, should hide thought by trace chat show config", process)
			}
		})
	}
}

func TestWikiTraceNoReplyShowsNoChangesSummary(t *testing.T) {
	cfg := config.Config{Bots: []config.BotConfig{{
		ID:        "bot-a",
		Workspace: t.TempDir(),
		WikiTrace: config.WikiTraceConfig{Enabled: true, ChatID: "oc_trace"},
	}}}
	svc := NewService(cfg, NewSessionStore(""))
	card := &fakeStreamCard{}
	svc.scheduleStreams["bot-a"] = func(context.Context, feishu.Message) (feishu.StreamCard, error) {
		return card, nil
	}
	observer := svc.wikiTraceObserver(Session{
		Key:          normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_source"}),
		ACPSessionID: "acp-wiki",
	})
	observer.start(context.Background())
	observer.complete(context.Background(), acp.PromptResult{Text: "NoReply", StopReason: "end_turn"}, nil)
	if got := card.finalTextUpdatesSnapshot(); len(got) != 1 || got[0] != "检查完成，无需沉淀。" {
		t.Fatalf("final text updates = %+v, want no changes summary", got)
	}
}
