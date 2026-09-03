package bridge

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestHandleFeishuMessageAutoCompactCancelsPendingWikiTimer(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	session.AvailableCommands = []acp.AvailableCommand{{Name: "compact"}}
	session.AutoCompact = true
	session.AutoCompactPct = 80
	session.WikiIntervalSec = 60
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{
		promptResults: []acp.PromptResult{
			{
				Text: "done",
				Meta: acp.PromptResultMeta{TraeTokenUsage: &acp.TraeTokenUsage{
					ContextWindow: acp.ContextWindowUsage{Used: 160000, Size: 200000},
				}},
			},
			{Text: "compacted"},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	_, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   sessionKeyMainID(session.Key),
		ThreadID: session.Key.SubID,
		ChatType: "topic_group",
		Mentions: testBotMentions(),
		Text:     "普通消息",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 2 })
	svc.taskMu.Lock()
	_, hasTimer := svc.wikiTimers[normalizeSessionKey(session.Key)]
	svc.taskMu.Unlock()
	if hasTimer {
		t.Fatal("pending wiki timer should be cancelled by automatic compact user task")
	}
}

func TestHandleFeishuMessageIMPromptKeepsReplyStreamAndWikiBehavior(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		newSessionID: "acp-session-1",
		promptReply:  "处理完成。",
		promptUpdates: []acp.PromptUpdate{
			{SessionID: "acp-session-1", Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "处理中。"},
			}},
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
		Text:      "结合引用回答",
		Workspace: workspace,
		Reply: &feishu.ReplyContext{
			MessageID:  "om_parent",
			SenderID:   "ou_parent",
			SenderType: "user",
			MsgType:    "text",
			Text:       "被回复的原文",
		},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want streamed reply", reply)
	}
	if len(rt.promptCalls) != 1 {
		t.Fatalf("promptCalls = %+v, want one IM prompt", rt.promptCalls)
	}
	prompt := rt.promptCalls[0].Text
	for _, want := range []string{"## Replied Message Context", "被回复的原文", "## User Message", "结合引用回答"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
	if len(cards) != 1 || !cards[0].isClosed() {
		t.Fatalf("cards = %+v closed=%v, want one closed stream card", cards, len(cards) == 1 && cards[0].isClosed())
	}
	if got := cards[0].textUpdatesSnapshot(); len(got) == 0 || got[0] != "处理中。" {
		t.Fatalf("textUpdates = %+v, want streamed text", got)
	}
	session, ok := store.Get(imSessionKey("bot-a", "oc_private", ""))
	if !ok {
		t.Fatal("IM session not stored")
	}
	coordinator := svc.wikiCoordinator("bot-a")
	t.Cleanup(coordinator.stop)
	if snapshot := coordinator.snapshotForSession(session.ACPSessionID); !snapshot.Waiting {
		t.Fatalf("wiki snapshot = %+v, want waiting companion job", snapshot)
	}
}

func TestHandleWikiCommandPersistsConfig(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	key := imSessionKey("bot-a", "oc_chat", "omt_thread")
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	msg := feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "/wiki INTERVAL 1s",
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki interval) error = %v", err)
	}
	if !strings.Contains(reply, "1s") {
		t.Fatalf("reply = %q, want interval confirmation", reply)
	}
	session, ok := store.Get(key)
	if !ok || session.WikiIntervalSec != 0 {
		t.Fatalf("session = %+v, want wiki interval not stored on session", session)
	}
	chat, ok := store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_chat"})
	if !ok || chat.WikiIntervalSec != 1 {
		t.Fatalf("chat config = %+v, %v; want wiki interval persisted", chat, ok)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_off",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "/wiki OFF",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki off) error = %v", err)
	}
	if !strings.Contains(reply, "已关闭") {
		t.Fatalf("reply = %q, want off confirmation", reply)
	}
	session, ok = store.Get(key)
	if !ok || session.WikiDisabled {
		t.Fatalf("session = %+v, want wiki disabled not stored on session", session)
	}
	chat, ok = store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_chat"})
	if !ok || !chat.WikiDisabled || chat.WikiIntervalSec != 1 {
		t.Fatalf("chat config = %+v, %v; want wiki disabled and interval persisted", chat, ok)
	}
}

func TestHandleWikiCommandRejectsSubSecondInterval(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	msg := feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_msg",
		ChatID:    "oc_chat",
		Text:      "/wiki interval 1ms",
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki interval 1ms) error = %v", err)
	}
	if !strings.Contains(reply, "不能小于 1s") {
		t.Fatalf("reply = %q, want sub-second rejection", reply)
	}
	if chat, ok := store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_chat"}); ok || chat.WikiIntervalSec != 0 {
		t.Fatalf("chat config = %+v, %v; want interval not persisted", chat, ok)
	}
}

func TestHandleWikiCommandSurvivesNewSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-new"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	msg := feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    msg.BotID,
		ChatID:   msg.ChatID,
		ChatType: msg.ChatType,
		Text:     "/wiki interval 1s",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki interval) error = %v", err)
	}
	if !strings.Contains(reply, "1s") {
		t.Fatalf("reply = %q, want wiki interval confirmation", reply)
	}

	reply, err = handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    msg.BotID,
		ChatID:   msg.ChatID,
		ChatType: msg.ChatType,
		Text:     "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "已为当前会话创建 ACP 会话") {
		t.Fatalf("reply = %q, want new session confirmation", reply)
	}
	session, ok := store.Get(imSessionKey(msg.BotID, msg.ChatID, ""))
	if !ok || session.WikiIntervalSec != 0 || session.WikiDisabled {
		t.Fatalf("session = %+v, %v; wiki options should not be stored on session", session, ok)
	}
	chat, ok := store.GetChat(ChatKey{BotID: msg.BotID, ChatID: msg.ChatID})
	if !ok || chat.WikiIntervalSec != 1 || chat.WikiDisabled {
		t.Fatalf("chat config = %+v, %v; want wiki interval to survive /new", chat, ok)
	}
	svc.scheduleWikiAfterUserPrompt(session, mustConfigAgent(t, config.Default(), "traex"))
	t.Cleanup(func() { svc.cancelWikiTimer(session.Key) })
	svc.taskMu.Lock()
	_, hasTimer := svc.wikiTimers[session.Key]
	svc.taskMu.Unlock()
	if !hasTimer {
		t.Fatal("wiki timer should use chat-level interval after /new")
	}
}

func TestNewSessionDoesNotControlWikiCompanion(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{
		newSessionID:     "acp-session-new",
		promptReply:      "NoReply",
		blockWikiRuntime: make(chan struct{}),
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	oldSession := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-old",
		Cwd:             t.TempDir(),
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		WikiIntervalSec: 60,
	}
	if err := store.Upsert(oldSession); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc.scheduleWikiAfterUserPrompt(oldSession, agent)
	t.Cleanup(func() {
		svc.cancelWikiTimer(key)
		close(rt.blockWikiRuntime)
	})

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "session：acp-session-new") {
		t.Fatalf("reply = %q, want new session reply without waiting for wiki runtime", reply)
	}
	if got := rt.wikiRuntimeCallCount(); got != 0 {
		t.Fatalf("wiki runtime calls = %d, want /new not to control companion", got)
	}
	svc.taskMu.Lock()
	_, hasTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if !hasTimer {
		t.Fatal("legacy timer fixture should remain untouched by /new")
	}
}

func TestNewSessionRuntimeFailureRestoresPendingWiki(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{
		newSessionError: errors.New("boom"),
		promptReply:     "NoReply",
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	oldSession := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-old",
		Cwd:             t.TempDir(),
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		WikiIntervalSec: 60,
	}
	if err := store.Upsert(oldSession); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc.scheduleWikiAfterUserPrompt(oldSession, agent)
	t.Cleanup(func() { svc.cancelWikiTimer(key) })

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new) error = %v", err)
	}
	if !strings.Contains(reply, "创建 ACP session 失败") {
		t.Fatalf("reply = %q, want new session error", reply)
	}
	svc.taskMu.Lock()
	_, hasTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if !hasTimer {
		t.Fatal("failed /new should restore pending wiki timer")
	}
	if got := rt.wikiRuntimeCallCount(); got != 0 {
		t.Fatalf("wiki runtime calls = %d, want none when /new fails", got)
	}
}

func TestNewSessionInvalidRequestKeepsPendingWiki(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = ""
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-new", promptReply: "NoReply"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	oldSession := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-old",
		Cwd:             t.TempDir(),
		WikiIntervalSec: 60,
	}
	svc.scheduleWikiAfterUserPrompt(oldSession, agent)
	t.Cleanup(func() { svc.cancelWikiTimer(key) })

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/new",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new invalid) error = %v", err)
	}
	if !strings.Contains(reply, "当前 agent traex 未配置 default_cwd") {
		t.Fatalf("reply = %q, want missing cwd warning", reply)
	}
	svc.taskMu.Lock()
	_, hasTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if !hasTimer {
		t.Fatal("invalid /new should keep pending wiki timer")
	}
	if got := rt.wikiRuntimeCallCount(); got != 0 {
		t.Fatalf("wiki runtime calls = %d, want none for invalid /new", got)
	}
}

func TestWikiOffCancelsRunningWikiRuntimeReflection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = t.TempDir()
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{
		newSessionID:     "acp-session-new",
		promptReply:      "NoReply",
		blockWikiRuntime: make(chan struct{}),
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	oldSession := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-old",
		Cwd:             t.TempDir(),
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		WikiIntervalSec: 60,
	}
	if err := store.Upsert(oldSession); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc.scheduleWikiAfterUserPrompt(oldSession, agent)
	t.Cleanup(func() {
		svc.cancelSessionWork(context.Background(), key)
		close(rt.blockWikiRuntime)
	})
	pending, ok := svc.takePendingWiki(key)
	if !ok {
		t.Fatal("takePendingWiki() = false")
	}
	svc.runPendingWikiAsync(pending)

	waitForCondition(t, time.Second, func() bool { return rt.wikiRuntimeCallCount() == 1 })
	rt.mu.Lock()
	wikiKey := rt.wikiRuntimeCalls[0].Runtime
	rt.mu.Unlock()

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/wiki off",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki off) error = %v", err)
	}
	if !strings.Contains(reply, "已关闭当前聊天的自动知识沉淀") {
		t.Fatalf("reply = %q, want wiki disabled", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() == 1 })
	rt.mu.Lock()
	cancelCall := rt.cancelCalls[0]
	closedKeys := append([]runtimeKey(nil), rt.closedRuntimeKeys...)
	rt.mu.Unlock()
	if cancelCall.Runtime != wikiKey {
		t.Fatalf("cancel runtime = %+v, want %+v", cancelCall.Runtime, wikiKey)
	}
	if len(closedKeys) == 0 || closedKeys[0] != wikiKey {
		t.Fatalf("closed runtime keys = %+v, want first %+v", closedKeys, wikiKey)
	}
}

func TestWikiStatusDoesNotCancelScheduledReflection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "omt_thread"))
	session := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-1",
		Cwd:             t.TempDir(),
		WikiIntervalSec: 60,
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc.scheduleWikiAfterUserPrompt(session, mustConfigAgent(t, config.Default(), "traex"))
	t.Cleanup(func() { svc.cancelWikiTimer(key) })
	svc.taskMu.Lock()
	beforeGeneration := svc.wikiGenerations[key]
	_, beforeTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if !beforeTimer {
		t.Fatal("wiki timer should be scheduled before status command")
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_status",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "/wiki STATUS",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki status) error = %v", err)
	}
	if !strings.Contains(reply, "等待定时触发") {
		t.Fatalf("reply = %q, want scheduled timer status", reply)
	}
	svc.taskMu.Lock()
	afterGeneration := svc.wikiGenerations[key]
	_, afterTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if !afterTimer {
		t.Fatal("/wiki status should not cancel scheduled wiki timer")
	}
	if afterGeneration != beforeGeneration {
		t.Fatalf("wiki generation = %d, want unchanged %d", afterGeneration, beforeGeneration)
	}
}

func TestWikiIntervalReschedulesScheduledReflection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	svc.setRuntime(&fakeRuntime{promptReply: "NoReply"})
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "omt_thread"))
	session := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-1",
		Cwd:             t.TempDir(),
		WikiIntervalSec: 60,
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc.scheduleWikiAfterUserPrompt(session, mustConfigAgent(t, config.Default(), "traex"))
	t.Cleanup(func() { svc.cancelWikiTimer(key) })
	svc.taskMu.Lock()
	beforeGeneration := svc.wikiGenerations[key]
	_, beforeTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if !beforeTimer {
		t.Fatal("wiki timer should be scheduled before interval command")
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_interval",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "/wiki interval 1s",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki interval) error = %v", err)
	}
	if !strings.Contains(reply, "1s") {
		t.Fatalf("reply = %q, want interval confirmation", reply)
	}
	svc.taskMu.Lock()
	afterGeneration := svc.wikiGenerations[key]
	_, afterTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if !afterTimer {
		t.Fatal("/wiki interval should keep wiki timer scheduled")
	}
	if afterGeneration <= beforeGeneration {
		t.Fatalf("wiki generation = %d, want greater than %d after reschedule", afterGeneration, beforeGeneration)
	}
	session, ok := store.Get(key)
	if !ok || session.WikiIntervalSec != 60 {
		t.Fatalf("session = %+v, want legacy session wiki interval unchanged", session)
	}
	chat, ok := store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc_chat"})
	if !ok || chat.WikiIntervalSec != 1 {
		t.Fatalf("chat config = %+v, %v; want wiki interval persisted", chat, ok)
	}
}

func TestNewMessageCancelsRunningWikiReflection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptReply:   "ACP 回复",
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "omt_thread"))
	session := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-1",
		Cwd:             t.TempDir(),
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		WikiIntervalSec: 1,
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc.taskMu.Lock()
	svc.wikiGenerations[key] = 1
	svc.taskMu.Unlock()
	wikiDone := make(chan struct{})
	go func() {
		svc.runWikiTimer(context.Background(), key, 1, session, mustConfigAgent(t, config.Default(), "traex"))
		close(wikiDone)
	}()
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_user",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "先处理我的新问题",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(user) error = %v", err)
	}
	if reply != "ACP 回复" {
		t.Fatalf("reply = %q, want user prompt reply", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() >= 1 })
	select {
	case <-wikiDone:
	case <-time.After(time.Second):
		t.Fatal("wiki reflection was not cancelled")
	}
	if rt.promptCallCount() != 2 {
		t.Fatalf("prompt calls = %d, want wiki then user prompt", rt.promptCallCount())
	}
}
