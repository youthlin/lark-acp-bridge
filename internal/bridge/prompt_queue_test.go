package bridge

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestHandleQueueCommandQueuesWithoutCancelingRunningTaskAndDrainsFIFO(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	session := testReadySession(t, store)
	session.Workspace = workspace
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session workspace) error = %v", err)
	}
	rt := &fakeRuntime{
		promptResults: []acp.PromptResult{
			{Text: "running done"},
			{Text: "queued one done"},
			{Text: "queued two done"},
		},
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = session.Cwd
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	var intermediate []string
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate = append(intermediate, text)
		return nil
	}
	svc.setOutbound("bot-a", client)
	promptDone := make(chan string, 1)
	go func() {
		reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
			BotID:     "bot-a",
			MessageID: "om_running",
			ChatID:    "oc_chat",
			ChatType:  "topic_group",
			ThreadID:  "omt_thread",
			Mentions:  testBotMentions(),
			Text:      "正在运行",
			Workspace: workspace,
		})
		if err != nil {
			promptDone <- "ERR:" + err.Error()
			return
		}
		promptDone <- reply
	}()
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	firstAck, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_queue_1",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "/queue 第一条",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/queue first) error = %v", err)
	}
	secondAck, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_queue_2",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "/queue 第二条",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/queue second) error = %v", err)
	}
	if !strings.Contains(firstAck, "第 1 条") || !strings.Contains(secondAck, "第 2 条") {
		t.Fatalf("queue replies = %q / %q, want sequence acknowledgements", firstAck, secondAck)
	}
	if rt.cancelCallCount() != 0 {
		t.Fatalf("cancel calls = %d, want queued prompts not cancel running task", rt.cancelCallCount())
	}

	close(rt.blockPrompt)
	if reply := <-promptDone; reply != "running done" {
		t.Fatalf("running prompt reply = %q, want running done", reply)
	}
	waitForCondition(t, time.Second, func() bool {
		return rt.promptCallCount() == 3 && reflect.DeepEqual(intermediate, []string{"queued one done", "queued two done"})
	})
	rt.mu.Lock()
	calls := append([]fakePromptCall(nil), rt.promptCalls...)
	rt.mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("prompt calls = %+v, want running plus two queued prompts", calls)
	}
	if !strings.Contains(calls[1].Text, "第一条") || !strings.Contains(calls[2].Text, "第二条") {
		t.Fatalf("queued prompt calls = %q / %q, want FIFO user text", calls[1].Text, calls[2].Text)
	}
}

func TestHandleQueueCommandRunsImmediatelyWhenIdle(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	session := testReadySession(t, store)
	session.Workspace = workspace
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session workspace) error = %v", err)
	}
	rt := &fakeRuntime{promptResult: acp.PromptResult{Text: "queued done"}}
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = session.Cwd
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	var intermediate []string
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate = append(intermediate, text)
		return nil
	}
	svc.setOutbound("bot-a", client)
	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_queue",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "/queue 立即执行",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/queue) error = %v", err)
	}
	if !strings.Contains(reply, "第 1 条") {
		t.Fatalf("reply = %q, want queue acknowledgement", reply)
	}
	waitForCondition(t, time.Second, func() bool {
		return rt.promptCallCount() == 1 && reflect.DeepEqual(intermediate, []string{"queued done"})
	})
}

func TestFinishPromptQueueDrainRestartsWhenItemArrivesBeforeFinish(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	session := testReadySession(t, store)
	session.Workspace = workspace
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session workspace) error = %v", err)
	}
	rt := &fakeRuntime{promptResult: acp.PromptResult{Text: "late queued done"}}
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = session.Cwd
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	var intermediate []string
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate = append(intermediate, text)
		return nil
	}
	svc.setOutbound("bot-a", client)
	key := normalizeSessionKey(session.Key)
	svc.promptQueues[key] = &promptQueue{
		draining: true,
		items: []queuedPrompt{{
			msg: feishu.Message{
				BotID:     "bot-a",
				MessageID: "om_queue",
				ChatID:    "oc_chat",
				ChatType:  "topic_group",
				ThreadID:  "omt_thread",
				Workspace: workspace,
			},
			session:  session,
			agent:    agent,
			text:     "迟到的队列任务",
			userText: "迟到的队列任务",
		}},
	}

	svc.finishPromptQueueDrain(ctx, key)

	waitForCondition(t, time.Second, func() bool {
		return rt.promptCallCount() == 1 && reflect.DeepEqual(intermediate, []string{"late queued done"})
	})
}

func TestHandleQueueCommandRefreshesUnavailableACPSession(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	session := testReadySession(t, store)
	session.Workspace = workspace
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session workspace) error = %v", err)
	}
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-refreshed"},
		promptResults: []acp.PromptResult{
			{},
			{Text: "queued refreshed"},
		},
		promptErrors: []error{errACPSessionUnavailable},
	}
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = session.Cwd
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	var intermediate []string
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate = append(intermediate, text)
		return nil
	}
	svc.setOutbound("bot-a", client)
	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_queue",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "/queue 需要恢复",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/queue) error = %v", err)
	}
	if !strings.Contains(reply, "第 1 条") {
		t.Fatalf("reply = %q, want queue acknowledgement", reply)
	}
	waitForCondition(t, time.Second, func() bool {
		return rt.promptCallCount() == 2 && reflect.DeepEqual(intermediate, []string{"queued refreshed"})
	})
	rt.mu.Lock()
	newCalls := append([]fakeNewCall(nil), rt.newCalls...)
	promptCalls := append([]fakePromptCall(nil), rt.promptCalls...)
	rt.mu.Unlock()
	wantKey := normalizeSessionKey(session.Key)
	if len(newCalls) != 1 || newCalls[0].Key.BotID != wantKey.BotID || newCalls[0].Key.Source != wantKey.Source || newCalls[0].Key.MainID != wantKey.MainID || newCalls[0].Key.SubID != wantKey.SubID {
		t.Fatalf("newCalls = %+v, want one refresh for queued session", newCalls)
	}
	if len(promptCalls) != 2 || promptCalls[1].Session.ACPSessionID != "acp-refreshed" {
		t.Fatalf("promptCalls = %+v, want second prompt on refreshed session", promptCalls)
	}
}

func TestPromptQueuedItemReturnsBusyWithoutCancelingRunningTask(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	rt := &fakeRuntime{}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	taskCtx, finish := svc.startTask(context.Background(), session, config.AgentConfig{}, taskKindUser)
	defer finish()

	_, err := svc.promptQueuedItem(taskCtx, queuedPrompt{
		msg:     feishu.Message{BotID: session.Key.BotID},
		session: session,
		agent:   config.AgentConfig{},
		text:    "queued",
	})
	if !errors.Is(err, errSessionTaskBusy) {
		t.Fatalf("promptQueuedItem() error = %v, want errSessionTaskBusy", err)
	}
	if rt.cancelCallCount() != 0 {
		t.Fatalf("cancel calls = %d, want queued prompt not cancel running task", rt.cancelCallCount())
	}
	if rt.promptCallCount() != 0 {
		t.Fatalf("prompt calls = %d, want queued prompt not run while busy", rt.promptCallCount())
	}
}
