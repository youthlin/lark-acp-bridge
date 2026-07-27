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
	anchor := loopAnchor{
		message: feishu.Message{MessageID: "om_loop_start"},
		request: loopRequest{Prompt: "持续推进", Interval: time.Second},
		card: &fakeLoopStatusCard{
			client:                client,
			message:               feishu.SentMessage{MessageID: "om_loop_start"},
			failOnCanceledContext: true,
		},
	}

	if !svc.updateLoopAnchor(parent, anchor, loopProgressFinished, 0, "agent 返回 DONE") {
		t.Fatal("updateLoopAnchor() = false, want update with detached context")
	}
	finishes := client.finishesSnapshot()
	if len(finishes) != 1 || !strings.Contains(finishes[0], "状态：已完成") || !strings.Contains(finishes[0], "结束原因：agent 返回 DONE") {
		t.Fatalf("finishes = %#v, want completed finish update", finishes)
	}
}

func TestHandleLoopCancelAllowsOwnerAndCancelsRunningLoop(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}
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
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"}
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
	ctx := withFakeSentMessageClient(context.Background(), client)
	ctx = feishu.WithStreamCardStarter(ctx, func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{failOnCanceledContext: true}
		cardsMu.Lock()
		cards = append(cards, card)
		cardsMu.Unlock()
		return card, nil
	})

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_user",
		ChatID:    "oc_chat",
		ChatType:  "group",
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
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
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
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
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
