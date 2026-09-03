package bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestFeishuMessageReactionEmojiTypesDoNotOverlapProcessingReactions(t *testing.T) {
	processing := map[string]struct{}{
		"OK":         {},
		"Get":        {},
		"WINK":       {},
		"WITTY":      {},
		"DIZZY":      {},
		"MeMeMe":     {},
		"THINKING":   {},
		"Typing":     {},
		"OnIt":       {},
		"OneSecond":  {},
		"GoGoGo":     {},
		"SaluteFace": {},
	}
	for _, emoji := range feishuMessageReactionEmojiTypes {
		if _, ok := processing[emoji]; ok {
			t.Fatalf("feishuMessageReactionEmojiTypes contains processing reaction %q", emoji)
		}
	}
}

func TestHandleFeishuGroupChatStartsReactionOnlyWhenMessageIsProcessed(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "ACP 回复"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var started, cleaned int
	client := newFakeSentMessageClient("")
	client.reactionStarter = func(context.Context, feishu.Message) func() {
		started++
		return func() {
			cleaned++
		}
	}
	svc.setOutbound("bot-a", client)
	ctx := context.Background()

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_group_ignored",
		ChatID:    "oc_group",
		ChatType:  "group",
		Text:      "没有 at",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group no mention) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want silent ignore", reply)
	}
	if started != 0 || cleaned != 0 {
		t.Fatalf("reaction lifecycle = started %d cleaned %d, want none for ignored message", started, cleaned)
	}

	reply, err = handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_group_processed",
		ChatID:    "oc_group",
		ChatType:  "group",
		Text:      "@智能助手 你好",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group mention) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want ACP reply", reply)
	}
	if started != 1 || cleaned != 1 {
		t.Fatalf("reaction lifecycle = started %d cleaned %d, want one processed message", started, cleaned)
	}

	key := ChatKey{BotID: "bot-a", ChatID: "oc_group"}
	if err := store.UpsertChat(ChatConfig{Key: key, MentionOptional: true, AtMode: atModeAuto}); err != nil {
		t.Fatalf("UpsertChat(auto) error = %v", err)
	}
	rt.mu.Lock()
	rt.promptResults = []acp.PromptResult{{Text: "SILENT"}}
	rt.mu.Unlock()
	reply, err = handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_auto_plain",
		ChatID:    "oc_group",
		ChatType:  "group",
		Text:      "auto 判断但不需要表情",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group auto no mention) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want SILENT suppressed", reply)
	}
	if started != 1 || cleaned != 1 {
		t.Fatalf("reaction lifecycle = started %d cleaned %d, want no processing reaction for plain auto", started, cleaned)
	}

	if err := store.UpsertChat(ChatConfig{Key: key, MentionOptional: true, AtMode: atModeAutoReaction}); err != nil {
		t.Fatalf("UpsertChat(auto-reaction) error = %v", err)
	}
	rt.mu.Lock()
	rt.promptResults = []acp.PromptResult{{Text: "SILENT"}}
	rt.mu.Unlock()
	reply, err = handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_auto_reaction",
		ChatID:    "oc_group",
		ChatType:  "group",
		Text:      "auto 判断并显示表情",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group auto-reaction no mention) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want SILENT suppressed", reply)
	}
	if started != 2 || cleaned != 2 {
		t.Fatalf("reaction lifecycle = started %d cleaned %d, want one processing reaction for auto-reaction", started, cleaned)
	}
}

func TestHandleFeishuGroupChatAtAutoUsesFinalTextAfterLastToolInDelayedStreamCard(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	updates := []acp.PromptUpdate{
		{
			SessionID: "acp-session-1",
			Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "我会查成员。"},
			},
		},
		{
			SessionID: "acp-session-1",
			Update: acp.SessionUpdate{
				SessionUpdate: "tool_call",
				Title:         "List chat members",
			},
		},
		{
			SessionID: "acp-session-1",
			Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "接口是只读的。"},
			},
		},
		{
			SessionID: "acp-session-1",
			Update: acp.SessionUpdate{
				SessionUpdate: "tool_call",
				Title:         "Fetch all pages",
			},
		},
		{
			SessionID: "acp-session-1",
			Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "能查。当前群有 5 个成员。"},
			},
		},
	}
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptResults: []acp.PromptResult{
			{Text: "RESPOND"},
			{Text: "我会查成员。\n接口是只读的。\n能查。当前群有 5 个成员。"},
		},
		promptUpdatesByCall: [][]acp.PromptUpdate{
			nil,
			updates,
		},
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	key := ChatKey{BotID: "bot-a", ChatID: "oc_group"}
	if err := store.UpsertChat(ChatConfig{Key: key, MentionOptional: true, AtMode: atModeAuto}); err != nil {
		t.Fatalf("UpsertChat() error = %v", err)
	}
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_auto_final",
		ChatID:    key.ChatID,
		ChatType:  "group",
		SenderID:  "ou_a",
		Text:      "你能查群成员有几人/几个bot吗",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group no mention auto) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because delayed auto card was sent", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one delayed auto stream card", cards)
	}
	card := cards[0]
	if got := card.finalTextUpdatesSnapshot(); len(got) == 0 || got[len(got)-1] != "能查。当前群有 5 个成员。" {
		t.Fatalf("finalTextUpdates = %+v, want only final text after last tool on card", got)
	}
	processUpdates := card.processUpdatesSnapshot()
	if len(processUpdates) == 0 {
		t.Fatalf("processUpdates = %+v, want delayed process updates on card", processUpdates)
	}
	lastProcess := processUpdates[len(processUpdates)-1]
	for _, want := range []string{"💬 我会查成员。", "⏳ List chat members", "💬 接口是只读的。", "⏳ Fetch all pages"} {
		if !strings.Contains(lastProcess, want) {
			t.Fatalf("last process update = %q, want %q", lastProcess, want)
		}
	}
	if strings.Contains(lastProcess, "能查。当前群有 5 个成员") {
		t.Fatalf("last process update = %q, should not contain final reply", lastProcess)
	}
}

func TestHandleFeishuGroupChatAtAutoSuppressesExplicitEmptyFinalText(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	updates := []acp.PromptUpdate{
		{
			SessionID: "acp-session-1",
			Update: acp.SessionUpdate{
				SessionUpdate: "tool_call",
				Title:         "Check context",
			},
		},
	}
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptResults: []acp.PromptResult{
			{Text: "RESPOND"},
			{Text: "raw result should not be sent"},
		},
		promptUpdatesByCall: [][]acp.PromptUpdate{
			nil,
			updates,
		},
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	key := ChatKey{BotID: "bot-a", ChatID: "oc_group"}
	if err := store.UpsertChat(ChatConfig{Key: key, MentionOptional: true, AtMode: atModeAuto}); err != nil {
		t.Fatalf("UpsertChat() error = %v", err)
	}
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_auto_empty",
		ChatID:    key.ChatID,
		ChatType:  "group",
		SenderID:  "ou_a",
		Text:      "普通群消息",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(group no mention auto) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want no fallback text for explicit empty final text", reply)
	}
	if len(cards) != 0 {
		t.Fatalf("cards = %+v, want no delayed auto card without final text", cards)
	}
	if got := client.sent; len(got) != 0 {
		t.Fatalf("sent = %+v, want no default fallback message", got)
	}
}

func TestHandleFeishuMessageForwardsPromptProgress(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "收到。现在开始。\n工具处理完成。",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "收到。现在开始。\n"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "function_call",
					Name:          "exec_command",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "reasoning",
					Message:       "The\nuser\nwants\nan\nEnglish\nparagraph.",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "工具处理完成。"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "初始化",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	card := cards[0]
	if got := card.textUpdatesSnapshot(); len(got) != 2 || got[0] != "收到。现在开始。" || got[1] != "工具处理完成。" {
		t.Fatalf("textUpdates = %+v, want pre-tool text kept until final candidate replaces it", got)
	}
	if got := card.processUpdatesSnapshot(); len(got) != 2 ||
		got[0] != "sid: acp-session-1\nmsg: om\\_msg\n\n💬 收到。现在开始。" ||
		got[1] != "sid: acp-session-1\nmsg: om\\_msg\n\n💬 收到。现在开始。\n⏳ exec\\_command" {
		t.Fatalf("processUpdates = %+v, want immediate tool update without default thought display", got)
	}
	if !card.isClosed() {
		t.Fatalf("stream card should be closed")
	}
}

func TestHandleFeishuMessageFinalStreamCardUsesMarkdownImageRenderContext(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "结果如下：\n\n![截图](result.png)",
		promptUpdates: []acp.PromptUpdate{
			{SessionID: "acp-session-1", Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "结果如下：\n\n![截图](result.png)"},
			}},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "发图",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because card was used", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].finalTextUpdatesSnapshot(); len(got) != 1 || got[0] != "结果如下：\n\n![截图](result.png)" {
		t.Fatalf("final text updates = %+v, want markdown image final text", got)
	}
	if got := cards[0].finalRenderContextsSnapshot(); len(got) != 1 || got[0].BaseDir != workDir {
		t.Fatalf("final render contexts = %+v, want session cwd", got)
	}
	if !cards[0].isClosed() {
		t.Fatalf("stream card should be closed")
	}
}

func TestHandleFeishuMessageFinalStreamCardRendersMarkdownImageAfterTool(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "结果如下：\n\n![截图](result.png)",
		promptUpdates: []acp.PromptUpdate{
			{SessionID: "acp-session-1", Update: acp.SessionUpdate{
				SessionUpdate: "tool_call",
				Title:         "生成图片",
			}},
			{SessionID: "acp-session-1", Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "结果如下：\n\n![截图](result.png)"},
			}},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "生成图片",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because card was used", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].finalTextUpdatesSnapshot(); len(got) != 1 || got[0] != "结果如下：\n\n![截图](result.png)" {
		t.Fatalf("final text updates = %+v, want markdown image final text after tool", got)
	}
	if got := cards[0].finalRenderContextsSnapshot(); len(got) != 1 || got[0].BaseDir != workDir {
		t.Fatalf("final render contexts = %+v, want session cwd", got)
	}
	if !cards[0].isClosed() {
		t.Fatalf("stream card should be closed")
	}
}

func TestHandleFeishuMessageKeepsOnlyAgentTextAfterLastToolAsFinal(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "先检查。\n中间说明。\n最终结论。",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "先检查。"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call",
					Title:         "Read config",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "中间说明。"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call",
					Title:         "Run tests",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "最终结论。"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	if err := store.UpsertChat(ChatConfig{
		Key:          chatKeyFromMessage(feishu.Message{BotID: "bot-a", ChatID: "oc_private"}),
		ShowThoughts: true,
	}); err != nil {
		t.Fatalf("UpsertChat(chat) error = %v", err)
	}
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "run",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	textUpdates := cards[0].textUpdatesSnapshot()
	if len(textUpdates) == 0 || textUpdates[len(textUpdates)-1] != "最终结论。" {
		t.Fatalf("textUpdates = %+v, want only text after last tool as final candidate", textUpdates)
	}
	wantTextUpdates := []string{"先检查。", "中间说明。", "最终结论。"}
	if !reflect.DeepEqual(textUpdates, wantTextUpdates) {
		t.Fatalf("textUpdates = %+v, want intermediate candidates replaced without clearing as %+v", textUpdates, wantTextUpdates)
	}
	processUpdates := cards[0].processUpdatesSnapshot()
	if len(processUpdates) == 0 {
		t.Fatalf("processUpdates = %+v, want process updates", processUpdates)
	}
	lastProcess := processUpdates[len(processUpdates)-1]
	for _, want := range []string{"💬 先检查。", "⏳ Read config", "💬 中间说明。", "⏳ Run tests"} {
		if !strings.Contains(lastProcess, want) {
			t.Fatalf("last process update = %q, want %q", lastProcess, want)
		}
	}
	if strings.Contains(lastProcess, "最终结论") {
		t.Fatalf("last process update = %q, should not include final agent text", lastProcess)
	}
	if got := cards[0].finalTextUpdatesSnapshot(); len(got) != 1 || got[0] != "最终结论。" {
		t.Fatalf("final text updates = %+v, want only text after last tool", got)
	}
}

func TestHandleFeishuMessageKeepsTextBeforeSingleFinalBoundaryAsFallback(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "先检查。",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "先检查。"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call",
					Title:         "Run tests",
				},
			},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "run",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].finalTextUpdatesSnapshot(); len(got) != 1 || got[0] != "先检查。" {
		t.Fatalf("final text updates = %+v, want text before single final boundary", got)
	}
	processUpdates := cards[0].processUpdatesSnapshot()
	if len(processUpdates) == 0 {
		t.Fatalf("processUpdates = %+v, want text before boundary preserved in process", processUpdates)
	}
	lastProcess := processUpdates[len(processUpdates)-1]
	for _, want := range []string{"💬 先检查。", "⏳ Run tests"} {
		if !strings.Contains(lastProcess, want) {
			t.Fatalf("last process update = %q, want %q", lastProcess, want)
		}
	}
}

func TestHandleFeishuMessageDoesNotFallbackToRawResultAfterFinalBoundary(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "raw result should not be final",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call",
					Title:         "Run tests",
				},
			},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "run",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].finalTextUpdatesSnapshot(); len(got) != 0 {
		t.Fatalf("final text updates = %+v, want no raw result fallback after final boundary", got)
	}
}

func TestHandleFeishuMessageKeepsTextBetweenFinalBoundariesAsFallback(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "文字1\n文字2",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "文字1"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "plan",
					PlanEntries: []acp.PlanEntry{
						{Content: "第一步", Status: "completed"},
					},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "文字2"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call",
					Title:         "Run tests",
				},
			},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "run",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].finalTextUpdatesSnapshot(); len(got) != 1 || got[0] != "文字2" {
		t.Fatalf("final text updates = %+v, want text between final boundaries", got)
	}
	processUpdates := cards[0].processUpdatesSnapshot()
	if len(processUpdates) == 0 {
		t.Fatalf("processUpdates = %+v, want text before first boundary preserved in process", processUpdates)
	}
	lastProcess := processUpdates[len(processUpdates)-1]
	if !strings.Contains(lastProcess, "💬 文字1") {
		t.Fatalf("last process update = %q, want text before first boundary in process", lastProcess)
	}
	if !strings.Contains(lastProcess, "💬 文字2") {
		t.Fatalf("last process update = %q, want text before second boundary in process", lastProcess)
	}
}

func TestHandleFeishuMessageUpdatesStreamCardStatusBar(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptResult: acp.PromptResult{
			Text:       "完成。",
			StopReason: "end_turn",
			Raw:        json.RawMessage(`{"stopReason":"end_turn","usage":{"inputTokens":511800,"outputTokens":27600,"cachedReadTokens":496446},"_meta":{"trace":"abc"}}`),
			Usage: acp.TokenUsage{
				InputTokens:      511800,
				OutputTokens:     27600,
				CachedReadTokens: 496446,
			},
			Meta: acp.PromptResultMeta{
				TraeTokenUsage: &acp.TraeTokenUsage{
					TurnDisplay: acp.TokenUsage{
						InputTokens:  987,
						OutputTokens: 654,
					},
					ContextWindow: acp.ContextWindowUsage{
						Used: 69200,
						Size: 258400,
					},
				},
			},
		},
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "usage_update",
					Used:          69200,
					Size:          258400,
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	if err := store.UpsertChat(ChatConfig{
		Key:          chatKeyFromMessage(feishu.Message{BotID: "bot-a", ChatID: "oc_private"}),
		ShowThoughts: true,
	}); err != nil {
		t.Fatalf("UpsertChat(chat) error = %v", err)
	}
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "run",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want streamed reply", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	got := cards[0].statusUpdatesSnapshot()
	if len(got) != 3 {
		t.Fatalf("statusUpdates = %+v, want three updates", got)
	}
	statusWantParts := [][]string{
		{"⏳ ", " | 69K/258K"},
		{"⏳ ", " | 511.8K(97%), 27.6K | 69K/258K"},
		{"✅ ", " | 511.8K(97%), 27.6K | 69K/258K"},
	}
	for i, parts := range statusWantParts {
		for _, part := range parts {
			if !strings.Contains(got[i], part) {
				t.Fatalf("statusUpdates[%d] = %q, want part %q", i, got[i], part)
			}
		}
	}
	usageDetails := cards[0].usageDetailsSnapshot()
	if len(usageDetails) != 1 {
		t.Fatalf("usageDetails = %+v, want one usage detail update", usageDetails)
	}
	for _, want := range []string{"```json", `"stopReason": "end_turn"`, `"cachedReadTokens": 496446`, `"_meta": {`, `"trace": "abc"`} {
		if !strings.Contains(usageDetails[0], want) {
			t.Fatalf("usage detail = %q, want %q", usageDetails[0], want)
		}
	}
}

func TestHandleFeishuMessageRecordsTokenUsageAndReports(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{
			SessionID: "acp-session-1",
			ConfigOptions: []acp.SessionConfigOption{
				{ID: "model", Name: "Model", Category: "model", Type: "select", CurrentValue: "gpt-5.5"},
			},
		},
		promptResult: acp.PromptResult{
			Text:       "完成。",
			StopReason: "end_turn",
			Usage: acp.TokenUsage{
				InputTokens:      1200,
				OutputTokens:     345,
				CachedReadTokens: 100,
			},
		},
	}
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "run",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "完成。" {
		t.Fatalf("reply = %q, want 完成。", reply)
	}

	usagePath := filepath.Join(workspace, ".local", "token_usage.json")
	data, err := os.ReadFile(usagePath)
	if err != nil {
		t.Fatalf("ReadFile(token_usage.json) error = %v", err)
	}
	var file tokenUsageFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("Unmarshal(token_usage.json) error = %v", err)
	}
	if len(file.Records) != 1 {
		t.Fatalf("records = %+v, want one record", file.Records)
	}
	record := file.Records[0]
	if record.AgentName != "traex" || record.Model != "gpt-5.5" || record.Usage.TotalTokens != 1545 || record.Usage.CachedReadTokens != 100 {
		t.Fatalf("record = %+v, want traex/gpt-5.5 usage", record)
	}

	report, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_usage",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "/usage day",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/usage) error = %v", err)
	}
	for _, want := range []string{"Token 用量报告（今日）", "总计：1 次，1.5K tokens", "traex / gpt-5.5", "缓存读 100"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report = %q, want %q", report, want)
		}
	}
}

func TestHandleFeishuMessageCanHideStreamCardStatusBar(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	if err := store.UpsertChat(ChatConfig{
		Key:           chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)}),
		HideStatusBar: true,
	}); err != nil {
		t.Fatalf("UpsertChat(chat) error = %v", err)
	}
	rt := &fakeRuntime{
		promptResult: acp.PromptResult{
			Text:       "完成。",
			StopReason: "end_turn",
			Usage: acp.TokenUsage{
				InputTokens:  1200,
				OutputTokens: 345,
			},
		},
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: session.ACPSessionID,
				Update: acp.SessionUpdate{
					SessionUpdate: "usage_update",
					Used:          53000,
					Size:          200000,
				},
			},
			{
				SessionID: session.ACPSessionID,
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	var statusBarEnabled *bool
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		enabled := options.StatusBarEnabled
		statusBarEnabled = &enabled
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound(session.Key.BotID, client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     session.Key.BotID,
		MessageID: "om_msg",
		ChatID:    sessionKeyMainID(session.Key),
		ThreadID:  session.Key.SubID,
		ChatType:  "topic_group",
		Text:      "run",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want streamed reply", reply)
	}
	if statusBarEnabled == nil || *statusBarEnabled {
		t.Fatalf("statusBarEnabled = %v, want false", statusBarEnabled)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].statusUpdatesSnapshot(); len(got) != 0 {
		t.Fatalf("statusUpdates = %+v, want none when status bar is hidden", got)
	}
}

func TestHandleFeishuMessageCanHideStreamCardUsageDetail(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	if err := store.UpsertChat(ChatConfig{
		Key:             chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)}),
		HideUsageDetail: true,
	}); err != nil {
		t.Fatalf("UpsertChat(chat) error = %v", err)
	}
	rt := &fakeRuntime{
		promptResult: acp.PromptResult{
			Text:       "完成。",
			StopReason: "end_turn",
			Raw:        json.RawMessage(`{"stopReason":"end_turn","usage":{"inputTokens":1200,"outputTokens":345},"_meta":{"trace":"abc"}}`),
			Usage: acp.TokenUsage{
				InputTokens:  1200,
				OutputTokens: 345,
			},
		},
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: session.ACPSessionID,
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound(session.Key.BotID, client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     session.Key.BotID,
		MessageID: "om_msg",
		ChatID:    sessionKeyMainID(session.Key),
		ThreadID:  session.Key.SubID,
		ChatType:  "topic_group",
		Text:      "run",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want streamed reply", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].usageDetailsSnapshot(); len(got) != 0 {
		t.Fatalf("usageDetails = %+v, want none when usage detail is hidden", got)
	}
}

func TestHandleFeishuMessageSkipsUsageDetailWithoutUsageInfo(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	rt := &fakeRuntime{
		promptResult: acp.PromptResult{
			Text:       "完成。",
			StopReason: "end_turn",
			Raw:        json.RawMessage(`{"stopReason":"end_turn"}`),
		},
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: session.ACPSessionID,
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound(session.Key.BotID, client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     session.Key.BotID,
		MessageID: "om_msg",
		ChatID:    sessionKeyMainID(session.Key),
		ThreadID:  session.Key.SubID,
		ChatType:  "topic_group",
		Text:      "run",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want streamed reply", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].usageDetailsSnapshot(); len(got) != 0 {
		t.Fatalf("usageDetails = %+v, want none without usage info", got)
	}
}

func TestHandleFeishuMessageMarksCancelledStopReasonInStreamCardStatus(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptResult: acp.PromptResult{
			Text:       "已取消。",
			StopReason: "cancelled",
			Usage: acp.TokenUsage{
				InputTokens: 1200,
			},
		},
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "已取消。"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	if err := store.UpsertChat(ChatConfig{
		Key:          chatKeyFromMessage(feishu.Message{BotID: "bot-a", ChatID: "oc_private"}),
		ShowThoughts: true,
	}); err != nil {
		t.Fatalf("UpsertChat(chat) error = %v", err)
	}
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "run",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want streamed reply", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	got := cards[0].statusUpdatesSnapshot()
	last := got[len(got)-1]
	if !strings.Contains(last, "🚫") || !strings.Contains(last, "1.2K") {
		t.Fatalf("statusUpdates = %+v, want final cancelled status with duration and token usage", got)
	}
}

func TestHandleFeishuMessageStreamsThoughtChunksAsOneProcessBlock(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "你好。",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_thought_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "**Restating the request**\n\nThe"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_thought_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: " user"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_thought_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: " said"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "你好。"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	if err := store.UpsertChat(ChatConfig{
		Key:          chatKeyFromMessage(feishu.Message{BotID: "bot-a", ChatID: "oc_private"}),
		ShowThoughts: true,
	}); err != nil {
		t.Fatalf("UpsertChat(chat) error = %v", err)
	}
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "hello",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	got := cards[0].processUpdatesSnapshot()
	if len(got) == 0 || len(got) > 2 {
		t.Fatalf("processUpdates = %+v, want debounced thought block updates", got)
	}
	if got[len(got)-1] != "sid: acp-session-1\nmsg: om\\_msg\n\n🧠 **Restating the request**\n\nThe user said" {
		t.Fatalf("last process update = %q, want folded thought chunk stream", got[len(got)-1])
	}
	if strings.Contains(got[len(got)-1], "The\nuser\nsaid") {
		t.Fatalf("last process update = %q, should not render one word per line", got[len(got)-1])
	}
}

func TestHandleFeishuMessageStreamsPlanUpdatesAsProcessBlock(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "完成。",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "plan",
					PlanEntries: []acp.PlanEntry{
						{Content: "读取现有实现", Status: "completed"},
						{Meta: map[string]any{"activeForm": "补过程消息展示"}, Status: "in_progress"},
					},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "hello",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	got := cards[0].processUpdatesSnapshot()
	if len(got) == 0 {
		t.Fatalf("processUpdates = %+v, want plan process update", got)
	}
	want := "sid: acp-session-1\nmsg: om\\_msg\n\n📌 计划\n• ✅ 读取现有实现\n• 🔄 补过程消息展示"
	if got[len(got)-1] != want {
		t.Fatalf("last process update = %q, want %q", got[len(got)-1], want)
	}
}

func TestHandleFeishuMessageUsesFinalTextAfterPlanBoundary(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "先说明。\n最终结论。",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "先说明。"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "plan",
					PlanEntries: []acp.PlanEntry{
						{Content: "确认实现", Status: "completed"},
					},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "最终结论。"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "hello",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].finalTextUpdatesSnapshot(); len(got) != 1 || got[0] != "最终结论。" {
		t.Fatalf("final text updates = %+v, want only text after plan boundary", got)
	}
	gotProcess := cards[0].processUpdatesSnapshot()
	if len(gotProcess) == 0 {
		t.Fatalf("processUpdates = %+v, want plan and previous text in process", gotProcess)
	}
	lastProcess := gotProcess[len(gotProcess)-1]
	for _, want := range []string{"💬 先说明。", "📌 计划", "确认实现"} {
		if !strings.Contains(lastProcess, want) {
			t.Fatalf("last process update = %q, want %q", lastProcess, want)
		}
	}
	if strings.Contains(lastProcess, "最终结论") {
		t.Fatalf("last process update = %q, should not include final text after plan boundary", lastProcess)
	}
}

func TestHandleFeishuMessageSeparatesPlanAndFollowingProcessRows(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "完成。",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "plan",
					PlanEntries: []acp.PlanEntry{
						{Content: "确认依赖和实体定义", Status: "completed"},
						{Content: "梳理仓库 Mongo 约定", Status: "in_progress"},
					},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call",
					Status:        "completed",
					Title:         "go test ./...",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "status",
					Message:       "继续读取实体定义",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "hello",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	got := cards[0].processUpdatesSnapshot()
	if len(got) == 0 {
		t.Fatalf("processUpdates = %+v, want process updates", got)
	}
	want := strings.Join([]string{
		"sid: acp-session-1",
		"msg: om\\_msg",
		"",
		"📌 计划",
		"• ✅ 确认依赖和实体定义",
		"• 🔄 梳理仓库 Mongo 约定",
		"✅ go test ./...",
		"💬 继续读取实体定义",
	}, "\n")
	if got[len(got)-1] != want {
		t.Fatalf("last process update = %q, want %q", got[len(got)-1], want)
	}
}

func TestHandleFeishuMessageStreamsGenericChunksAsOneProcessBlock(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "完成。",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call_output_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "line"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call_output_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: " one"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call_output_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "\nline two"},
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "hello",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	got := cards[0].processUpdatesSnapshot()
	if len(got) != 1 {
		t.Fatalf("processUpdates = %+v, want generic chunk stream to update once within throttle window", got)
	}
	if got[0] != "sid: acp-session-1\nmsg: om\\_msg\n\nline one line two" {
		t.Fatalf("process update = %q, want final accumulated generic chunk stream", got[0])
	}
}

func TestHandleFeishuMessageFormatsToolTitleAndStatus(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "完成。",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call",
					Status:        "in_progress",
					Title:         "Read AGENTS.md",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "tool_call_update",
					Status:        "completed",
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
		Text:      "读取一下AGENTS.md文件当前的内容",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty final reply because progress already streamed", reply)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	got := cards[0].processUpdatesSnapshot()
	if len(got) != 2 {
		t.Fatalf("processUpdates = %+v, want tool start and completion updates", got)
	}
	if got[0] != "sid: acp-session-1\nmsg: om\\_msg\n\n⏳ Read AGENTS.md" {
		t.Fatalf("first process update = %q, want tool title", got[0])
	}
	if got[1] != "sid: acp-session-1\nmsg: om\\_msg\n\n✅ Read AGENTS.md" {
		t.Fatalf("second process update = %q, want completed status replacing tool row", got[1])
	}
}

func TestHandleFeishuMessageShowOptionsFilterProcessUpdates(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*ChatConfig)
		want     []string
		unwanted []string
	}{
		{
			name: "hide step",
			mutate: func(chat *ChatConfig) {
				chat.HideStepMessages = true
				chat.ShowThoughts = true
			},
			want:     []string{"📌 计划", "Plan item", "⏳ Run tests", "🧠 Thinking"},
			unwanted: []string{"💬 准备处理", "step chunk"},
		},
		{
			name: "hide plan",
			mutate: func(chat *ChatConfig) {
				chat.HidePlans = true
				chat.ShowThoughts = true
			},
			want:     []string{"💬 准备处理", "⏳ Run tests", "🧠 Thinking", "💬 step chunk"},
			unwanted: []string{"📌 计划", "Plan item"},
		},
		{
			name: "hide thought",
			mutate: func(chat *ChatConfig) {
				chat.HideThoughts = true
			},
			want:     []string{"💬 准备处理", "📌 计划", "Plan item", "⏳ Run tests", "💬 step chunk"},
			unwanted: []string{"🧠 Thinking"},
		},
		{
			name: "hide tool",
			mutate: func(chat *ChatConfig) {
				chat.HideTools = true
				chat.ShowThoughts = true
			},
			want:     []string{"💬 准备处理", "📌 计划", "Plan item", "🧠 Thinking", "💬 step chunk"},
			unwanted: []string{"Run tests", "tool output"},
		},
		{
			name: "default hide thought",
			mutate: func(chat *ChatConfig) {
			},
			want:     []string{"💬 准备处理", "📌 计划", "Plan item", "⏳ Run tests", "💬 step chunk"},
			unwanted: []string{"🧠 Thinking"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
			session := testReadySession(t, store)
			chat := ChatConfig{Key: chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)})}
			tt.mutate(&chat)
			if err := store.UpsertChat(chat); err != nil {
				t.Fatalf("UpsertChat(chat) error = %v", err)
			}
			rt := &fakeRuntime{
				promptReply: "完成。",
				promptUpdates: []acp.PromptUpdate{
					{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
						SessionUpdate: "status",
						Message:       "准备处理",
					}},
					{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
						SessionUpdate: "reasoning",
						Message:       "Thinking",
					}},
					{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
						SessionUpdate: "plan",
						PlanEntries: []acp.PlanEntry{
							{Content: "Plan item", Status: "in_progress"},
						},
					}},
					{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
						SessionUpdate: "tool_call",
						Title:         "Run tests",
					}},
					{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
						SessionUpdate: "tool_call_output_chunk",
						Content:       &acp.ContentBlock{Type: "text", Text: "tool output"},
					}},
					{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
						SessionUpdate: "progress_chunk",
						Content:       &acp.ContentBlock{Type: "text", Text: "step chunk"},
					}},
					{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
						SessionUpdate: "agent_message_chunk",
						Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
					}},
				},
			}
			svc := newTestService(config.Default(), store)
			svc.setRuntime(rt)
			var cards []*fakeStreamCard
			ctx := context.Background()
			client := newFakeSentMessageClient("")
			client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
				card := &fakeStreamCard{}
				cards = append(cards, card)
				return card, nil
			}
			svc.setOutbound(session.Key.BotID, client)

			reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
				BotID:     session.Key.BotID,
				MessageID: "om_msg",
				ChatID:    sessionKeyMainID(session.Key),
				ThreadID:  session.Key.SubID,
				ChatType:  "topic_group",
				Text:      "run",
				Mentions:  []feishu.Mention{testBotMention("智能助手")},
			})
			if err != nil {
				t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
			}
			if reply != "" {
				t.Fatalf("reply = %q, want streamed reply", reply)
			}
			if len(cards) != 1 {
				t.Fatalf("cards = %+v, want one stream card", cards)
			}
			process := strings.Join(cards[0].processUpdatesSnapshot(), "\n")
			for _, want := range tt.want {
				if !strings.Contains(process, want) {
					t.Fatalf("process updates = %q, want %q", process, want)
				}
			}
			for _, unwanted := range tt.unwanted {
				if strings.Contains(process, unwanted) {
					t.Fatalf("process updates = %q, should not contain %q", process, unwanted)
				}
			}
		})
	}
}

func TestHandleFeishuMessageShowOptionsCanHideWholeProcessPanel(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	if err := store.UpsertChat(ChatConfig{
		Key:              chatKeyFromMessage(feishu.Message{BotID: session.Key.BotID, ChatID: sessionKeyMainID(session.Key)}),
		HideStepMessages: true,
		HidePlans:        true,
		HideThoughts:     true,
		HideTools:        true,
	}); err != nil {
		t.Fatalf("UpsertChat(chat) error = %v", err)
	}
	rt := &fakeRuntime{
		promptReply: "完成。",
		promptUpdates: []acp.PromptUpdate{
			{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
				SessionUpdate: "status",
				Message:       "准备处理",
			}},
			{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
				SessionUpdate: "reasoning",
				Message:       "Thinking",
			}},
			{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
				SessionUpdate: "tool_call",
				Title:         "Run tests",
			}},
			{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "完成。"},
			}},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	var processPanelEnabled *bool
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		enabled := options.ProcessPanelEnabled
		processPanelEnabled = &enabled
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound(session.Key.BotID, client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     session.Key.BotID,
		MessageID: "om_msg",
		ChatID:    sessionKeyMainID(session.Key),
		ThreadID:  session.Key.SubID,
		ChatType:  "topic_group",
		Text:      "run",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want streamed reply", reply)
	}
	if processPanelEnabled == nil || *processPanelEnabled {
		t.Fatalf("processPanelEnabled = %v, want false when all process classes are hidden", processPanelEnabled)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card for final text", cards)
	}
	if got := cards[0].processUpdatesSnapshot(); len(got) != 0 {
		t.Fatalf("processUpdates = %+v, want none", got)
	}
}

func TestHandleFeishuMessageRefreshesRunningStreamCardStatus(t *testing.T) {
	oldInterval := promptStatusRefreshInterval
	promptStatusRefreshInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		promptStatusRefreshInterval = oldInterval
	})

	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	rt := &fakeRuntime{
		promptReply: "完成。",
		promptUpdates: []acp.PromptUpdate{
			{SessionID: session.ACPSessionID, Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "处理中。"},
			}},
		},
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	var cards fakeStreamCardCollector
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards.add(card)
		return card, nil
	}
	svc.setOutbound(session.Key.BotID, client)

	done := make(chan struct {
		reply string
		err   error
	}, 1)
	go func() {
		reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
			BotID:     session.Key.BotID,
			MessageID: "om_msg",
			ChatID:    sessionKeyMainID(session.Key),
			ThreadID:  session.Key.SubID,
			ChatType:  "topic_group",
			Text:      "run",
			Mentions:  []feishu.Mention{testBotMention("智能助手")},
		})
		done <- struct {
			reply string
			err   error
		}{reply: reply, err: err}
	}()
	waitForCondition(t, time.Second, func() bool {
		snapshot := cards.snapshot()
		return len(snapshot) == 1 && len(snapshot[0].statusUpdatesSnapshot()) >= 2
	})

	close(rt.blockPrompt)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("HandleFeishuMessage(prompt) error = %v", got.err)
		}
		if got.reply != "" {
			t.Fatalf("reply = %q, want streamed reply", got.reply)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt did not finish")
	}
}

func TestPromptRuntimeWaitsForInFlightDebouncedCardFlush(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "Hello",
		afterUpdates: func() {
			time.Sleep(promptCardFlushDelay + 80*time.Millisecond)
		},
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "Hello"},
				},
			},
		},
	}
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	started := make(chan struct{})
	release := make(chan struct{})
	var cards []*fakeStreamCard
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		close(started)
		<-release
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}
	svc.setOutbound("bot-a", client)

	result := make(chan struct {
		reply string
		err   error
	}, 1)
	go func() {
		reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
			BotID:     "bot-a",
			MessageID: "om_msg",
			ChatID:    "oc_private",
			ChatType:  "p2p",
			Text:      "hello",
		})
		result <- struct {
			reply string
			err   error
		}{reply: reply, err: err}
	}()

	select {
	case <-started:
	case <-time.After(promptCardFlushDelay + time.Second):
		t.Fatal("stream card starter was not called")
	}
	select {
	case got := <-result:
		t.Fatalf("HandleFeishuMessage returned before in-flight card flush finished: reply=%q err=%v", got.reply, got.err)
	default:
	}
	close(release)

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("HandleFeishuMessage(prompt) error = %v", got.err)
		}
		if got.reply != "" {
			t.Fatalf("reply = %q, want empty final reply because card flush already started", got.reply)
		}
	case <-time.After(time.Second):
		t.Fatal("HandleFeishuMessage did not return after releasing stream card starter")
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].textUpdatesSnapshot(); len(got) != 1 || got[0] != "Hello" {
		t.Fatalf("textUpdates = %+v, want debounced card update", got)
	}
	if !cards[0].isClosed() {
		t.Fatalf("stream card should be closed")
	}
}
