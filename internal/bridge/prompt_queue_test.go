package bridge

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

type queueIntermediateReplies struct {
	mu     sync.Mutex
	values []string
}

func (r *queueIntermediateReplies) append(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values = append(r.values, text)
}

func (r *queueIntermediateReplies) equal(want []string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return reflect.DeepEqual(r.values, want)
}

func (r *queueIntermediateReplies) contains(want string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, value := range r.values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

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

	var intermediate queueIntermediateReplies
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate.append(text)
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
		return rt.promptCallCount() == 3 && intermediate.equal([]string{"queued one done", "queued two done"})
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

func TestHandleQueueCommandRefreshesWorkspacePromptedBeforeDrain(t *testing.T) {
	workspace := t.TempDir()
	markWorkspaceBootstrapped(t, workspace)
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	session := testReadySession(t, store)
	session.Workspace = workspace
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session workspace) error = %v", err)
	}
	rt := &fakeRuntime{
		promptResults: []acp.PromptResult{
			{Text: "running done"},
			{Text: "queued done"},
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

	var intermediate queueIntermediateReplies
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate.append(text)
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
			Text:      "首轮",
			Workspace: workspace,
		})
		if err != nil {
			promptDone <- "ERR:" + err.Error()
			return
		}
		promptDone <- reply
	}()
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })
	firstCall := rt.promptCallsSnapshot()[0]
	if !strings.Contains(firstCall.Text, "## Workspace Context") || !strings.Contains(firstCall.Text, "## Workspace Memory Policy") {
		t.Fatalf("first prompt = %q, want workspace context and memory policy", firstCall.Text)
	}

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_queue",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "/queue 排队消息",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/queue) error = %v", err)
	}
	if !strings.Contains(reply, "第 1 条") {
		t.Fatalf("queue reply = %q, want acknowledgement", reply)
	}

	close(rt.blockPrompt)
	if reply := <-promptDone; reply != "running done" {
		t.Fatalf("running prompt reply = %q, want running done", reply)
	}
	waitForCondition(t, time.Second, func() bool {
		return rt.promptCallCount() == 2 && intermediate.equal([]string{"queued done"})
	})
	calls := rt.promptCallsSnapshot()
	if len(calls) != 2 {
		t.Fatalf("prompt calls = %+v, want running plus queued prompt", calls)
	}
	if !strings.Contains(calls[1].Text, "排队消息") {
		t.Fatalf("queued prompt = %q, want queued user text", calls[1].Text)
	}
	if strings.Contains(calls[1].Text, "## Workspace Context") || strings.Contains(calls[1].Text, "## Workspace Memory Policy") {
		t.Fatalf("queued prompt = %q, should not repeat workspace prompt", calls[1].Text)
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

	var intermediate queueIntermediateReplies
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate.append(text)
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
		return rt.promptCallCount() == 1 && intermediate.equal([]string{"queued done"})
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

	var intermediate queueIntermediateReplies
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate.append(text)
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
		return rt.promptCallCount() == 1 && intermediate.equal([]string{"late queued done"})
	})
}

func TestEnqueuePromptSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "chat-a"}
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "chat-b"})
	cases := []struct {
		name           string
		setup          func(svc *Service)
		item           queuedPrompt
		wantIndex      int
		wantQueue      *promptQueue
		wantOtherQueue *promptQueue
	}{
		{
			name:      "非规范化 key 下新建队列并写入序号",
			setup:     func(svc *Service) { svc.promptQueues = nil },
			item:      queuedPrompt{text: "queued-new", session: Session{Key: key}},
			wantIndex: 1,
			wantQueue: &promptQueue{
				items: []queuedPrompt{
					{text: "queued-new", session: Session{Key: normalizedKey}, replyIndex: 1},
				},
				nextSeq: 1,
			},
		},
		{
			name: "已有队列继续递增序号并追加到队尾",
			setup: func(svc *Service) {
				svc.promptQueues[normalizedKey] = &promptQueue{
					items: []queuedPrompt{
						{text: "queued-1", session: Session{Key: normalizedKey}, replyIndex: 1},
					},
					nextSeq: 1,
				}
			},
			item:      queuedPrompt{text: "queued-2", session: Session{Key: key}},
			wantIndex: 2,
			wantQueue: &promptQueue{
				items: []queuedPrompt{
					{text: "queued-1", session: Session{Key: normalizedKey}, replyIndex: 1},
					{text: "queued-2", session: Session{Key: normalizedKey}, replyIndex: 2},
				},
				nextSeq: 2,
			},
		},
		{
			name: "其他 session 队列不影响当前 session 序号",
			setup: func(svc *Service) {
				svc.promptQueues[otherKey] = &promptQueue{
					items:   []queuedPrompt{{text: "other", session: Session{Key: otherKey}, replyIndex: 3}},
					nextSeq: 3,
				}
			},
			item:      queuedPrompt{text: "queued-new", session: Session{Key: key}},
			wantIndex: 1,
			wantQueue: &promptQueue{
				items: []queuedPrompt{
					{text: "queued-new", session: Session{Key: normalizedKey}, replyIndex: 1},
				},
				nextSeq: 1,
			},
			wantOtherQueue: &promptQueue{
				items:   []queuedPrompt{{text: "other", session: Session{Key: otherKey}, replyIndex: 3}},
				nextSeq: 3,
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			tt.setup(svc)

			got := svc.enqueuePrompt(tt.item)
			if got != tt.wantIndex {
				t.Fatalf("enqueuePrompt() = %d, want %d", got, tt.wantIndex)
			}
			if queue := svc.promptQueues[normalizedKey]; !reflect.DeepEqual(queue, tt.wantQueue) {
				t.Fatalf("promptQueues[%+v] = %+v, want %+v", normalizedKey, queue, tt.wantQueue)
			}
			if queue := svc.promptQueues[otherKey]; !reflect.DeepEqual(queue, tt.wantOtherQueue) {
				t.Fatalf("promptQueues[%+v] = %+v, want %+v", otherKey, queue, tt.wantOtherQueue)
			}
		})
	}
}

func TestBeginPromptQueueDrainSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "chat-a"}
	normalizedKey := normalizeSessionKey(key)
	cases := []struct {
		name         string
		setup        func(svc *Service)
		wantBegin    bool
		wantDraining bool
	}{
		{
			name: "非规范化 key 也能开始 drain",
			setup: func(svc *Service) {
				svc.promptQueues[normalizedKey] = &promptQueue{items: []queuedPrompt{{text: "queued"}}}
			},
			wantBegin:    true,
			wantDraining: true,
		},
		{
			name:         "空队列不开始 drain",
			setup:        func(svc *Service) { svc.promptQueues[normalizedKey] = &promptQueue{} },
			wantBegin:    false,
			wantDraining: false,
		},
		{
			name: "已在 drain 的队列不重复开始",
			setup: func(svc *Service) {
				svc.promptQueues[normalizedKey] = &promptQueue{draining: true, items: []queuedPrompt{{text: "queued"}}}
			},
			wantBegin:    false,
			wantDraining: true,
		},
		{
			name: "当前 session 有运行任务时不开始 drain",
			setup: func(svc *Service) {
				svc.promptQueues[normalizedKey] = &promptQueue{items: []queuedPrompt{{text: "queued"}}}
				svc.tasks[normalizedKey] = &runningTask{kind: taskKindUser}
			},
			wantBegin:    false,
			wantDraining: false,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			tt.setup(svc)

			got := svc.beginPromptQueueDrain(key)
			if got != tt.wantBegin {
				t.Fatalf("beginPromptQueueDrain() = %v, want %v", got, tt.wantBegin)
			}
			queue := svc.promptQueues[normalizedKey]
			if queue == nil {
				t.Fatal("prompt queue missing after beginPromptQueueDrain")
			}
			if queue.draining != tt.wantDraining {
				t.Fatalf("queue.draining = %v, want %v", queue.draining, tt.wantDraining)
			}
		})
	}
}

func TestTakeQueuedPromptSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "chat-a"}
	normalizedKey := normalizeSessionKey(key)
	cases := []struct {
		name      string
		setup     func(svc *Service)
		wantOK    bool
		wantText  string
		wantItems []queuedPrompt
	}{
		{
			name: "非规范化 key 按 FIFO 取出首个队列项",
			setup: func(svc *Service) {
				svc.promptQueues[normalizedKey] = &promptQueue{items: []queuedPrompt{
					{text: "queued-1"},
					{text: "queued-2"},
				}}
			},
			wantOK:    true,
			wantText:  "queued-1",
			wantItems: []queuedPrompt{{text: "queued-2"}},
		},
		{
			name: "空队列返回 false",
			setup: func(svc *Service) {
				svc.promptQueues[normalizedKey] = &promptQueue{}
			},
			wantOK:    false,
			wantItems: nil,
		},
		{
			name:      "不存在的队列返回 false",
			setup:     func(svc *Service) {},
			wantOK:    false,
			wantItems: nil,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			tt.setup(svc)

			got, ok := svc.takeQueuedPrompt(key)
			if ok != tt.wantOK {
				t.Fatalf("takeQueuedPrompt() ok = %v, want %v", ok, tt.wantOK)
			}
			if got.text != tt.wantText {
				t.Fatalf("takeQueuedPrompt() text = %q, want %q", got.text, tt.wantText)
			}
			var remaining []queuedPrompt
			if queue := svc.promptQueues[normalizedKey]; queue != nil {
				remaining = queue.items
			}
			if !reflect.DeepEqual(remaining, tt.wantItems) {
				t.Fatalf("remaining items = %+v, want %+v", remaining, tt.wantItems)
			}
		})
	}
}

func TestPrependQueuedPromptSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "chat-a"}
	normalizedKey := normalizeSessionKey(key)
	cases := []struct {
		name      string
		setup     func(svc *Service)
		item      queuedPrompt
		wantItems []queuedPrompt
	}{
		{
			name: "非规范化 key 下新建队列并规范化 item session key",
			setup: func(svc *Service) {
				svc.promptQueues = nil
			},
			item: queuedPrompt{text: "queued-new", session: Session{Key: key}},
			wantItems: []queuedPrompt{
				{text: "queued-new", session: Session{Key: normalizedKey}},
			},
		},
		{
			name: "已有队列时放回队首",
			setup: func(svc *Service) {
				svc.promptQueues[normalizedKey] = &promptQueue{items: []queuedPrompt{
					{text: "queued-1", session: Session{Key: normalizedKey}},
					{text: "queued-2", session: Session{Key: normalizedKey}},
				}}
			},
			item: queuedPrompt{text: "retry", session: Session{Key: key}},
			wantItems: []queuedPrompt{
				{text: "retry", session: Session{Key: normalizedKey}},
				{text: "queued-1", session: Session{Key: normalizedKey}},
				{text: "queued-2", session: Session{Key: normalizedKey}},
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			tt.setup(svc)

			svc.prependQueuedPrompt(key, tt.item)

			queue := svc.promptQueues[normalizedKey]
			if queue == nil {
				t.Fatal("prompt queue missing after prependQueuedPrompt")
			}
			if !reflect.DeepEqual(queue.items, tt.wantItems) {
				t.Fatalf("queue.items = %+v, want %+v", queue.items, tt.wantItems)
			}
		})
	}
}

func TestFinishPromptQueueDrainSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "chat-a"}
	normalizedKey := normalizeSessionKey(key)
	cases := []struct {
		name      string
		setup     func(svc *Service)
		wantQueue *promptQueue
	}{
		{
			name:      "不存在的队列直接返回",
			setup:     func(svc *Service) {},
			wantQueue: nil,
		},
		{
			name: "空队列完成 drain 后删除",
			setup: func(svc *Service) {
				svc.promptQueues[normalizedKey] = &promptQueue{draining: true}
			},
			wantQueue: nil,
		},
		{
			name: "当前 session busy 时保留队列但不重启 drain",
			setup: func(svc *Service) {
				svc.promptQueues[normalizedKey] = &promptQueue{
					draining: true,
					items:    []queuedPrompt{{text: "queued"}},
				}
				svc.tasks[normalizedKey] = &runningTask{kind: taskKindUser}
			},
			wantQueue: &promptQueue{items: []queuedPrompt{{text: "queued"}}},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			tt.setup(svc)

			svc.finishPromptQueueDrain(context.Background(), key)

			got := svc.promptQueues[normalizedKey]
			if !reflect.DeepEqual(got, tt.wantQueue) {
				t.Fatalf("promptQueues[%+v] = %+v, want %+v", normalizedKey, got, tt.wantQueue)
			}
		})
	}
}

func TestCanRestartPromptQueueDrainSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "chat-a"}
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "chat-b"})
	cases := []struct {
		name  string
		setup func(svc *Service)
		want  bool
	}{
		{
			name:  "当前 session 空闲时可以重启 drain",
			setup: func(svc *Service) {},
			want:  true,
		},
		{
			name: "当前 session 有运行任务时不可重启 drain",
			setup: func(svc *Service) {
				svc.tasks[normalizedKey] = &runningTask{kind: taskKindUser}
			},
			want: false,
		},
		{
			name: "其他 session 有运行任务不影响当前 session 重启 drain",
			setup: func(svc *Service) {
				svc.tasks[otherKey] = &runningTask{kind: taskKindUser}
			},
			want: true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			tt.setup(svc)

			svc.taskMu.Lock()
			got := svc.canRestartPromptQueueDrainLocked(key)
			svc.taskMu.Unlock()
			if got != tt.want {
				t.Fatalf("canRestartPromptQueueDrainLocked() = %v, want %v", got, tt.want)
			}
		})
	}
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

	var intermediate queueIntermediateReplies
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate.append(text)
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
		return rt.promptCallCount() == 2 && intermediate.equal([]string{"queued refreshed"})
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

func TestHandleQueueCommandRecordsACPErrorForQueuedPromptFailure(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	session := testReadySession(t, store)
	session.Workspace = workspace
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{
		promptErrors: []error{errors.New("queue failed Authorization: Bearer queue-token")},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	client := newFakeSentMessageClient("")
	var intermediate queueIntermediateReplies
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate.append(text)
		return nil
	}
	ctx := withFakeSentMessageClient(context.Background(), svc, session.Key.BotID, client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     session.Key.BotID,
		MessageID: "om_queue",
		ChatID:    session.Key.ChatID,
		ChatType:  "topic_group",
		ThreadID:  session.Key.SubID,
		Mentions:  testBotMentions(),
		Text:      "/queue 失败的队列任务",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/queue) error = %v", err)
	}
	if !strings.Contains(reply, "第 1 条") {
		t.Fatalf("reply = %q, want queue acknowledgement", reply)
	}
	waitForCondition(t, time.Second, func() bool {
		return rt.promptCallCount() == 1 && intermediate.contains("队列任务执行失败")
	})

	status, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    session.Key.BotID,
		ChatID:   session.Key.ChatID,
		ChatType: "topic_group",
		ThreadID: session.Key.SubID,
		Mentions: testBotMentions(),
		Text:     "/status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/status) error = %v", err)
	}
	for _, want := range []string{
		"ACP错误：",
		"queue prompt：queue failed",
		"Authorization:[已隐藏] [已隐藏] [已隐藏]",
	} {
		if !strings.Contains(status, want) {
			t.Fatalf("status = %q, want %q", status, want)
		}
	}
	if strings.Contains(status, "queue-token") {
		t.Fatalf("status = %q, should not include queue token", status)
	}
}

func TestHandleQueueCommandRunningQueuedPromptIsInterruptedByNormalPrompt(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	session := testReadySession(t, store)
	session.Workspace = workspace
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session workspace) error = %v", err)
	}
	rt := &fakeRuntime{
		promptResults: []acp.PromptResult{
			{Text: "queued done"},
			{Text: "normal done"},
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

	var intermediate queueIntermediateReplies
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate.append(text)
		return nil
	}
	svc.setOutbound("bot-a", client)

	queueReply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_queue",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "/queue 先排队但会被打断",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/queue) error = %v", err)
	}
	if !strings.Contains(queueReply, "第 1 条") {
		t.Fatalf("queue reply = %q, want queue acknowledgement", queueReply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	normalReply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_normal",
		ChatID:    "oc_chat",
		ChatType:  "topic_group",
		ThreadID:  "omt_thread",
		Mentions:  testBotMentions(),
		Text:      "改成执行普通消息",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(normal) error = %v", err)
	}
	if normalReply != "normal done" {
		t.Fatalf("normal reply = %q, want normal done", normalReply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() == 1 })
	rt.mu.Lock()
	calls := append([]fakePromptCall(nil), rt.promptCalls...)
	cancelCalls := append([]fakeCancelCall(nil), rt.cancelCalls...)
	rt.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("prompt calls = %+v, want queued prompt then normal prompt", calls)
	}
	if !strings.Contains(calls[0].Text, "先排队但会被打断") || !strings.Contains(calls[1].Text, "改成执行普通消息") {
		t.Fatalf("prompt calls = %+v, want queued prompt interrupted by normal prompt", calls)
	}
	if len(cancelCalls) != 1 || cancelCalls[0].Session.ACPSessionID != session.ACPSessionID {
		t.Fatalf("cancel calls = %+v, want queued prompt runtime canceled", cancelCalls)
	}
	if !intermediate.equal(nil) {
		t.Fatal("queued prompt reply should be suppressed after cancellation")
	}
}

func TestHandleQueueCommandUsesQueuedSessionAfterSessionResume(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "omt_thread"}
	oldSession := Session{
		Key:          key,
		Title:        "old session",
		AgentName:    "traex",
		ACPSessionID: "acp-session-old",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
	}
	currentSession := Session{
		Key:          key,
		Title:        "current session",
		AgentName:    "traex",
		ACPSessionID: "acp-session-current",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
	}
	if err := store.Upsert(oldSession); err != nil {
		t.Fatalf("Upsert(oldSession) error = %v", err)
	}
	if err := store.Upsert(currentSession); err != nil {
		t.Fatalf("Upsert(currentSession) error = %v", err)
	}
	rt := &fakeRuntime{
		promptResult:     acp.PromptResult{Text: "queued done"},
		activeSessionIDs: map[SessionKey]string{normalizeSessionKey(key): currentSession.ACPSessionID},
	}
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = currentSession.Cwd
	cfg.SetAgent("traex", agent)
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	var intermediate queueIntermediateReplies
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate.append(text)
		return nil
	}
	svc.setOutbound("bot-a", client)

	normalizedKey := normalizeSessionKey(key)
	svc.promptQueues[normalizedKey] = &promptQueue{
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
			session:  currentSession,
			agent:    agent,
			text:     "入队时绑定当前会话",
			userText: "入队时绑定当前会话",
		}},
	}

	resumed, errText := svc.resumeSessionByID(ctx, feishu.Message{
		BotID:            "bot-a",
		ChatID:           "oc_chat",
		ChatType:         "topic_group",
		GroupMessageType: "thread",
		ThreadID:         "omt_thread",
		Workspace:        workspace,
	}, oldSession.ACPSessionID, nil)
	if errText != "" {
		t.Fatalf("resumeSessionByID() error = %q", errText)
	}
	if resumed.ACPSessionID != oldSession.ACPSessionID {
		t.Fatalf("resumed session = %+v, want old session", resumed)
	}

	svc.finishPromptQueueDrain(ctx, normalizedKey)
	waitForCondition(t, time.Second, func() bool {
		return rt.promptCallCount() == 1 && intermediate.equal([]string{"queued done"})
	})
	rt.mu.Lock()
	calls := append([]fakePromptCall(nil), rt.promptCalls...)
	rt.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("prompt calls = %+v, want queued prompt", calls)
	}
	if calls[0].Session.ACPSessionID != currentSession.ACPSessionID {
		t.Fatalf("queued prompt session = %q, want queued snapshot %q", calls[0].Session.ACPSessionID, currentSession.ACPSessionID)
	}
	if current, ok := store.Get(key); !ok || current.ACPSessionID != oldSession.ACPSessionID {
		t.Fatalf("current session = %+v, %v; want restored old session", current, ok)
	}
}
