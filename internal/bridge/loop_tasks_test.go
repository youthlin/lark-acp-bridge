package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestUpdateLoopAnchorIgnoresCanceledParentContext(t *testing.T) {
	svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
	client := newFakeSentMessageClient("om_loop_start")
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now().Add(-2 * time.Minute)
	anchor := loopAnchor{
		message: feishu.Message{MessageID: "om_loop_start"},
		request: loopRequest{Prompt: "持续推进", Interval: time.Second},
		card: &fakeLoopStatusCard{
			client:                client,
			message:               feishu.SentMessage{MessageID: "om_loop_start"},
			failOnCanceledContext: true,
		},
		started: started,
	}

	if !svc.updateLoopAnchor(parent, anchor, loopProgressFinished, 0, "agent 返回 DONE") {
		t.Fatal("updateLoopAnchor() = false, want update with detached context")
	}
	finishes := client.finishesSnapshot()
	if len(finishes) != 1 || !strings.Contains(finishes[0], "状态：已完成") || !strings.Contains(finishes[0], "结束原因：agent 返回 DONE") || !strings.Contains(finishes[0], "已运行：") {
		t.Fatalf("finishes = %#v, want completed finish update", finishes)
	}
}

func TestLoopAnchorTextIncludesElapsedDuration(t *testing.T) {
	started := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	now := started.Add(2*time.Minute + 3*time.Second)
	text := loopAnchorText(loopRequest{Prompt: "持续推进", Interval: time.Second}, loopProgressRunning, 2, "", started, now)

	for _, want := range []string{
		"状态：第 2 轮运行中",
		"已运行：2m3s",
		"更新时间：2026-07-29T00:02:03Z",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("loopAnchorText() = %q, want %q", text, want)
		}
	}
}

func TestHandleLoopHowCommandReturnsRecommendedCommand(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workDir := t.TempDir()
	cfg := config.Default()
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "/loop -t 0 -n 0 -i 30s 持续修复 todo.md 中的优化项"}
	svc := newTestService(cfg, store)
	svc.setRuntime(rt)
	msg := feishu.Message{
		BotID:     "bot-a",
		Workspace: t.TempDir(),
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		Text:      "/loop how 持续修复 todo.md 中的优化项",
	}

	reply := svc.handleLoopCommand(context.Background(), msg.Text, msg)
	for _, want := range []string{
		"/loop -t 0 -n 0 -i 30s 持续修复 todo.md 中的优化项",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
	if rt.promptCallCount() != 1 {
		t.Fatalf("prompt calls = %d, want /loop how to ask model once", rt.promptCallCount())
	}
	rt.mu.Lock()
	prompt := rt.promptCalls[0].Text
	rt.mu.Unlock()
	for _, want := range []string{
		"持续修复 todo.md 中的优化项",
		"## /loop 命令格式",
		"不要默认生成无限循环",
		"最终只返回一条 /loop 命令",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
	if len(svc.loopStatuses) != 0 {
		t.Fatalf("loopStatuses = %+v, want no started loop", svc.loopStatuses)
	}
}

func TestLoopAddCommandAppendsSupplementToNextRoundOnce(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptResults: []acp.PromptResult{{Text: "继续"}, {Text: "DONE"}},
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat"})
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := newFakeSentMessageClient("om_loop_start")
	ctx := withFakeSentMessageClient(context.Background(), svc, "bot-a", client)
	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop -n 2 -i 1ms 持续推进",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after sending loop start message", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	addReply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop add 补充上下文：优先检查 todo.md",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop add) error = %v", err)
	}
	if addReply != "已追加到下一轮 loop prompt，下一轮执行完成后自动清空。" {
		t.Fatalf("add reply = %q, want success confirmation", addReply)
	}
	if got := rt.cancelCallCount(); got != 0 {
		t.Fatalf("cancel calls = %d, want /loop add not to cancel running loop", got)
	}

	close(rt.blockPrompt)
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 2 })
	rt.mu.Lock()
	calls := append([]fakePromptCall(nil), rt.promptCalls...)
	rt.mu.Unlock()
	if strings.Contains(calls[0].Text, "## 本轮补充消息") || strings.Contains(calls[0].Text, "补充上下文：优先检查 todo.md") {
		t.Fatalf("first loop prompt = %q, want no supplement in current round", calls[0].Text)
	}
	for _, want := range []string{
		"round: 2",
		"## 本轮补充消息",
		"补充上下文：优先检查 todo.md",
	} {
		if !strings.Contains(calls[1].Text, want) {
			t.Fatalf("second loop prompt = %q, want %q", calls[1].Text, want)
		}
	}

	svc.taskMu.Lock()
	status := svc.loopStatuses[key]
	svc.taskMu.Unlock()
	if status.pendingAdd != "" {
		t.Fatalf("loop status pendingAdd = %q, want consumed after next round prompt", status.pendingAdd)
	}
}

func TestHandleLoopCancelAllowsOwnerAndCancelsRunningLoop(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "omt_thread"})
	session := Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	taskCtx, finish := svc.startTask(context.Background(), session, config.AgentConfig{}, taskKindLoop)
	defer finish()
	svc.taskMu.Lock()
	svc.loopStatuses[key] = loopRunStatus{running: true, started: time.Now()}
	svc.taskMu.Unlock()

	display, err := svc.HandleLoopCancel(context.Background(), feishu.LoopCancel{
		BotID:        "bot-a",
		ChatID:       "oc_chat",
		ThreadID:     "omt_thread",
		ACPSessionID: "acp-session-1",
		OperatorID:   testOwnerOpenID,
	})
	if err != nil {
		t.Fatalf("HandleLoopCancel() error = %v", err)
	}
	if !strings.Contains(display, "loop 已结束") || !strings.Contains(display, "结束原因：已通过卡片取消") {
		t.Fatalf("display = %q, want finished cancel text", display)
	}
	select {
	case <-taskCtx.Done():
	default:
		t.Fatal("loop task context was not cancelled")
	}
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() >= 1 })
	svc.taskMu.Lock()
	status := svc.loopStatuses[key]
	_, stillRunning := svc.tasks[key]
	svc.taskMu.Unlock()
	if stillRunning || status.running || status.reason != "已通过卡片取消" {
		t.Fatalf("task stillRunning=%v status=%+v, want cancelled loop", stillRunning, status)
	}
}

func TestHandleLoopCancelUpdatesRunningRoundCardWithDetachedContext(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "正在执行\n"},
				},
			},
		},
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "omt_thread"})
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	client := newFakeSentMessageClient("om_loop_start")
	var cardsMu sync.Mutex
	var cards []*fakeStreamCard
	cardsSnapshot := func() []*fakeStreamCard {
		cardsMu.Lock()
		defer cardsMu.Unlock()
		return append([]*fakeStreamCard(nil), cards...)
	}
	ctx := withFakeSentMessageClient(context.Background(), svc, "bot-a", client)
	client.streamStarter = func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{failOnCanceledContext: true}
		cardsMu.Lock()
		cards = append(cards, card)
		cardsMu.Unlock()
		return card, nil
	}

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_user",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Text:      "@智能助手 /loop -n 0 -i 1ms 长循环",
		Mentions:  []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after sending loop start message", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 && len(cardsSnapshot()) == 1 })
	cardList := cardsSnapshot()
	if len(cardList) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cardList)
	}
	card := cardList[0]

	display, err := svc.HandleLoopCancel(context.Background(), feishu.LoopCancel{
		BotID:        "bot-a",
		ChatID:       "oc_chat",
		ThreadID:     "omt_thread",
		ACPSessionID: "acp-session-1",
		OperatorID:   testOwnerOpenID,
	})
	if err != nil {
		t.Fatalf("HandleLoopCancel() error = %v", err)
	}
	if !strings.Contains(display, "结束原因：已通过卡片取消") {
		t.Fatalf("display = %q, want card cancel reason", display)
	}
	waitForCondition(t, time.Second, func() bool {
		statusCancelled := false
		for _, update := range card.statusUpdatesSnapshot() {
			if strings.Contains(update, "已取消") {
				statusCancelled = true
				break
			}
		}
		return statusCancelled && card.isClosed()
	})

	processCancelled := false
	for _, update := range card.processUpdatesSnapshot() {
		if strings.Contains(update, "已取消") {
			processCancelled = true
			break
		}
	}
	if !processCancelled {
		t.Fatalf("process updates = %+v, want cancellation marker", card.processUpdatesSnapshot())
	}
}

func TestHandleLoopCancelRejectsNonOwner(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	key := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat"})
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	_, err := svc.HandleLoopCancel(context.Background(), feishu.LoopCancel{
		BotID:        "bot-a",
		ChatID:       "oc_chat",
		ACPSessionID: "acp-session-1",
		OperatorID:   "ou_other",
	})
	if err == nil || !strings.Contains(err.Error(), "只有 bot owner 可以取消 loop") {
		t.Fatalf("HandleLoopCancel(non-owner) error = %v, want owner-only error", err)
	}
}

func TestHandleLoopCancelRejectsExpiredCard(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	key := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat"})
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-current",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	_, err := svc.HandleLoopCancel(context.Background(), feishu.LoopCancel{
		BotID:        "bot-a",
		ChatID:       "oc_chat",
		ACPSessionID: "acp-session-old",
		OperatorID:   testOwnerOpenID,
	})
	if err == nil || !strings.Contains(err.Error(), "该 loop 卡片已过期") {
		t.Fatalf("HandleLoopCancel(expired) error = %v, want expired-card error", err)
	}
}
