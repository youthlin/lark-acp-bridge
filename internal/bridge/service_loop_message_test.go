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

func TestLoopCommandRunsUntilDoneAndUpdatesStartCard(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptResults: []acp.PromptResult{{Text: "继续"}, {Text: "DONE"}}}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	client := newFakeSentMessageClient("om_loop_start")
	var intermediate []string
	ctx := withFakeSentMessageClient(context.Background(), svc, "bot-a", client)
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate = append(intermediate, text)
		return nil
	}

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop -n 5 -i 1ms 持续推进这个目标",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after sending loop start message", reply)
	}
	sent := client.sentSnapshot()
	if len(sent) != 1 || !strings.Contains(sent[0], "已启动 loop") || !strings.Contains(sent[0], "最大轮次：5") {
		t.Fatalf("sent = %#v, want loop start confirmation", sent)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 2 })
	waitForCondition(t, time.Second, func() bool {
		updates := client.updatesSnapshot()
		return len(updates) > 0 && strings.Contains(updates[len(updates)-1], "状态：已完成") && strings.Contains(updates[len(updates)-1], "结束原因：agent 返回 DONE")
	})
	if len(intermediate) != 0 {
		t.Fatalf("intermediate = %#v, want no separate finish reply", intermediate)
	}
	updates := client.updatesSnapshot()
	if !containsStringWithAll(updates, "状态：第 1 轮运行中", "更新时间：") {
		t.Fatalf("updates = %#v, want round 1 running update", updates)
	}
	if !containsStringWithAll(updates, "状态：第 2 轮已完成", "更新时间：") {
		t.Fatalf("updates = %#v, want round 2 completed update", updates)
	}
	for _, id := range client.updateIDsSnapshot() {
		if id != "om_loop_start" {
			t.Fatalf("update message id = %q, want loop start card message", id)
		}
	}
	if textUpdates := client.textUpdatesSnapshot(); len(textUpdates) != 0 {
		t.Fatalf("text updates = %#v, want loop status card updates only", textUpdates)
	}
	rt.mu.Lock()
	calls := append([]fakePromptCall(nil), rt.promptCalls...)
	rt.mu.Unlock()
	if !strings.Contains(calls[0].Text, "## Loop Metadata") || !strings.Contains(calls[0].Text, "round: 1") || !strings.Contains(calls[0].Text, "## Loop Stop Rules") {
		t.Fatalf("first loop prompt = %q, want loop metadata and stop rules", calls[0].Text)
	}
	if !strings.Contains(calls[1].Text, "round: 2") {
		t.Fatalf("second loop prompt = %q, want round 2 metadata", calls[1].Text)
	}
	svc.taskMu.Lock()
	status := svc.loopStatuses[key]
	svc.taskMu.Unlock()
	if status.running || status.round != 2 || status.reason != "agent 返回 DONE" {
		t.Fatalf("loop status = %+v, want completed at round 2 by DONE", status)
	}
}

func TestLoopCommandStopsWhenDoneComesFromStreamChunk(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptResult: acp.PromptResult{
			Usage: acp.TokenUsage{InputTokens: 1200},
		},
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "usage_update",
					Used:          53000,
					Size:          200000,
				},
			},
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "DONE"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	client := newFakeSentMessageClient("om_loop_start")
	var intermediate []string
	var streamMsgs []feishu.Message
	var cards []*fakeStreamCard
	ctx := withFakeSentMessageClient(context.Background(), svc, "bot-a", client)
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate = append(intermediate, text)
		return nil
	}
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		streamMsgs = append(streamMsgs, msg)
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop -n 3 -i 1ms 等待流式 DONE",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after sending loop start message", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })
	waitForCondition(t, time.Second, func() bool {
		updates := client.updatesSnapshot()
		return len(updates) > 0 && strings.Contains(updates[len(updates)-1], "结束原因：agent 返回 DONE")
	})
	if len(intermediate) != 0 {
		t.Fatalf("intermediate = %#v, want no separate finish reply", intermediate)
	}
	if len(streamMsgs) != 1 || streamMsgs[0].MessageID != "om_loop_start" || !streamMsgs[0].ForceReplyInThread {
		t.Fatalf("stream messages = %+v, want thread reply to loop start message", streamMsgs)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	statusUpdates := cards[0].statusUpdatesSnapshot()
	if !containsStringWithAll(statusUpdates, "L1 ⏳ ", " | 53K/200K") {
		t.Fatalf("status updates = %+v, want L1 running status", statusUpdates)
	}
	if !containsStringWithAll(statusUpdates, "L1 ✅ ", " | 1.2K | 53K/200K") {
		t.Fatalf("status updates = %+v, want L1 completed status", statusUpdates)
	}
	if textUpdates := client.textUpdatesSnapshot(); len(textUpdates) != 0 {
		t.Fatalf("text updates = %#v, want loop status card updates only", textUpdates)
	}
	svc.taskMu.Lock()
	status := svc.loopStatuses[key]
	svc.taskMu.Unlock()
	if status.running || status.round != 1 || status.reason != "agent 返回 DONE" {
		t.Fatalf("loop status = %+v, want completed at round 1 by streamed DONE", status)
	}
}

func TestLoopCommandStopsWhenFinalCardTextIsDoneAfterProcessMessages(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
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
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "DONE"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
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
	var cards []*fakeStreamCard
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	}

	if reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop -n 3 -i 1ms 等待流式 DONE",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	} else if reply != "" {
		t.Fatalf("reply = %q, want empty after sending loop start message", reply)
	}

	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })
	waitForCondition(t, time.Second, func() bool {
		updates := client.updatesSnapshot()
		return len(updates) > 0 && strings.Contains(updates[len(updates)-1], "结束原因：agent 返回 DONE")
	})
	svc.taskMu.Lock()
	status := svc.loopStatuses[key]
	svc.taskMu.Unlock()
	if status.running || status.round != 1 || status.reason != "agent 返回 DONE" {
		t.Fatalf("loop status = %+v, want completed at round 1 by final card DONE", status)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	textUpdates := cards[0].textUpdatesSnapshot()
	if len(textUpdates) == 0 || textUpdates[len(textUpdates)-1] != "DONE" {
		t.Fatalf("textUpdates = %+v, want final card text DONE", textUpdates)
	}
}

func TestLoopRoundCardsReplyToStartMessageInThread(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "DONE"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	client := newFakeSentMessageClient("om_loop_start")
	var streamMsgsMu sync.Mutex
	var streamMsgs []feishu.Message
	streamMsgsSnapshot := func() []feishu.Message {
		streamMsgsMu.Lock()
		defer streamMsgsMu.Unlock()
		return append([]feishu.Message(nil), streamMsgs...)
	}
	ctx := withFakeSentMessageClient(context.Background(), svc, "bot-a", client)
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		streamMsgsMu.Lock()
		streamMsgs = append(streamMsgs, msg)
		streamMsgsMu.Unlock()
		return &fakeStreamCard{}, nil
	}

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "group",
		Text:     "@智能助手 /loop -n 3 -i 1ms 等待流式 DONE",
		Mentions: []feishu.Mention{testBotMention("智能助手")},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after sending loop start message", reply)
	}
	startMsgs := client.messagesSnapshot()
	if len(startMsgs) != 1 {
		t.Fatalf("start messages = %+v, want one loop start card message", startMsgs)
	}
	if startMsgs[0].ForceReplyInThread {
		t.Fatalf("start message = %+v, want loop start card to reply normally to user message", startMsgs[0])
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })
	streamMsgList := streamMsgsSnapshot()
	if len(streamMsgList) != 1 {
		t.Fatalf("stream messages = %+v, want one card", streamMsgList)
	}
	if streamMsgList[0].MessageID != "om_loop_start" || !streamMsgList[0].ForceReplyInThread {
		t.Fatalf("stream message = %+v, want card reply to loop start message in thread", streamMsgList[0])
	}
}

func TestLoopCommandStopsAtMaxRounds(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptReply: "继续"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	if err := store.Upsert(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	client := newFakeSentMessageClient("om_loop_start")
	var intermediate []string
	ctx := withFakeSentMessageClient(context.Background(), svc, "bot-a", client)
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate = append(intermediate, text)
		return nil
	}

	if _, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop -n 2 -i 1ms 做两轮",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 2 })
	waitForCondition(t, time.Second, func() bool {
		updates := client.updatesSnapshot()
		return len(updates) > 0 && strings.Contains(updates[len(updates)-1], "结束原因：已达到最大轮次")
	})
	if len(intermediate) != 0 {
		t.Fatalf("intermediate = %#v, want no separate finish reply", intermediate)
	}
	if textUpdates := client.textUpdatesSnapshot(); len(textUpdates) != 0 {
		t.Fatalf("text updates = %#v, want loop status card updates only", textUpdates)
	}
	svc.taskMu.Lock()
	status := svc.loopStatuses[key]
	svc.taskMu.Unlock()
	if status.running || status.round != 2 || status.reason != "已达到最大轮次" {
		t.Fatalf("loop status = %+v, want completed by max rounds", status)
	}
}

func TestLoopCommandRetriesFailedAttemptWithoutConsumingRound(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptErrors:  []error{context.DeadlineExceeded},
		promptResults: []acp.PromptResult{{Text: "discarded"}, {Text: "继续"}, {Text: "继续"}},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	if err := store.Upsert(Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	client := newFakeSentMessageClient("om_loop_start")
	ctx := withFakeSentMessageClient(context.Background(), svc, "bot-a", client)

	if _, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID: "bot-a", ChatID: "oc_chat", ChatType: "p2p",
		Text: "/loop -n 2 -i 1ms 失败后继续",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 3 })
	waitForCondition(t, time.Second, func() bool {
		updates := client.updatesSnapshot()
		return len(updates) > 0 && strings.Contains(updates[len(updates)-1], "结束原因：已达到最大轮次")
	})

	rt.mu.Lock()
	calls := append([]fakePromptCall(nil), rt.promptCalls...)
	rt.mu.Unlock()
	if !strings.Contains(calls[0].Text, "round: 1") || !strings.Contains(calls[0].Text, "attempt: 1") {
		t.Fatalf("first prompt = %q, want round 1 attempt 1", calls[0].Text)
	}
	for _, want := range []string{"round: 1", "attempt: 2", "## Loop Recovery Hint", "上一轮异常终止"} {
		if !strings.Contains(calls[1].Text, want) {
			t.Fatalf("retry prompt = %q, want %q", calls[1].Text, want)
		}
	}
	if strings.Contains(calls[1].Text, context.DeadlineExceeded.Error()) {
		t.Fatalf("retry prompt = %q, should not include detailed error", calls[1].Text)
	}
	if !strings.Contains(calls[2].Text, "round: 2") || !strings.Contains(calls[2].Text, "attempt: 1") {
		t.Fatalf("third prompt = %q, want round 2 attempt 1", calls[2].Text)
	}
	if !containsStringWithAll(client.updatesSnapshot(), "状态：第 1 轮异常结束，准备重试", "连续失败 1/3") {
		t.Fatalf("updates = %#v, want retry state", client.updatesSnapshot())
	}
	status, ok := svc.loopStatusSnapshot(key)
	if !ok || status.running || status.round != 2 || status.failures != 0 || status.lastError != "" {
		t.Fatalf("loop status = %+v, want two successful logical rounds after one retry", status)
	}
}

func TestLoopCommandKeepsPendingAddAcrossFailedAttempt(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptErrors:  []error{nil, context.DeadlineExceeded},
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	if err := store.Upsert(Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	client := newFakeSentMessageClient("om_loop_start")
	ctx := withFakeSentMessageClient(context.Background(), svc, "bot-a", client)

	if _, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID: "bot-a", ChatID: "oc_chat", ChatType: "p2p",
		Text: "/loop -n 2 -i 1ms 补充消息重试",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })
	if _, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID: "bot-a", ChatID: "oc_chat", ChatType: "p2p",
		Text: "/loop add 重试时仍要携带",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/loop add) error = %v", err)
	}
	close(rt.blockPrompt)
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 3 })
	waitForCondition(t, time.Second, func() bool {
		status, ok := svc.loopStatusSnapshot(key)
		return ok && !status.running
	})

	calls := rt.promptCallsSnapshot()
	for i := 1; i <= 2; i++ {
		for _, want := range []string{"round: 2", "## 本轮补充消息", "重试时仍要携带"} {
			if !strings.Contains(calls[i].Text, want) {
				t.Fatalf("prompt call %d = %q, want %q", i+1, calls[i].Text, want)
			}
		}
	}
	if !strings.Contains(calls[2].Text, "attempt: 2") {
		t.Fatalf("retry prompt = %q, want second attempt", calls[2].Text)
	}
	status, _ := svc.loopStatusSnapshot(key)
	if status.pendingAdd != "" {
		t.Fatalf("pendingAdd = %q, want cleared after logical round succeeds", status.pendingAdd)
	}
}

func TestLoopRetryDoesNotWaitPastMaxDuration(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptErrors: []error{context.DeadlineExceeded}}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	if err := store.Upsert(Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	client := newFakeSentMessageClient("om_loop_start")
	ctx := withFakeSentMessageClient(context.Background(), svc, "bot-a", client)

	started := time.Now()
	if _, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID: "bot-a", ChatID: "oc_chat", ChatType: "p2p",
		Text: "/loop -t 50ms -i 24h 失败后不要超期等待",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		status, ok := svc.loopStatusSnapshot(key)
		return ok && !status.running && status.reason == "已达到最长运行时间"
	})
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("loop stopped after %s, want deadline checked before 24h retry interval", elapsed)
	}
	if got := rt.promptCallCount(); got != 1 {
		t.Fatalf("prompt calls = %d, want one failed attempt before max duration stop", got)
	}
}

func TestLoopCommandStopsAfterThreeConsecutiveAttemptFailures(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	errBoom := context.DeadlineExceeded
	rt := &fakeRuntime{promptErrors: []error{errBoom, errBoom, errBoom}}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	if err := store.Upsert(Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	client := newFakeSentMessageClient("om_loop_start")
	ctx := withFakeSentMessageClient(context.Background(), svc, "bot-a", client)

	if _, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID: "bot-a", ChatID: "oc_chat", ChatType: "p2p",
		Text: "/loop -n 0 -i 1ms 连续失败",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 3 })
	waitForCondition(t, time.Second, func() bool {
		updates := client.updatesSnapshot()
		return len(updates) > 0 && strings.Contains(updates[len(updates)-1], "连续 3 次执行失败")
	})
	status, ok := svc.loopStatusSnapshot(key)
	if !ok || status.running || status.round != 1 || status.attempt != 3 || status.failures != 3 {
		t.Fatalf("loop status = %+v, want stopped after three failures", status)
	}
}

func TestLoopDoneRulesDoNotWrapFollowingNormalPrompt(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptResults: []acp.PromptResult{{Text: "DONE"}, {Text: "正常回答"}}}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	if err := store.Upsert(Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	client := newFakeSentMessageClient("om_loop_start")
	ctx := withFakeSentMessageClient(context.Background(), svc, "bot-a", client)

	if _, err := handleFeishuMessage(t, svc, ctx, feishu.Message{BotID: "bot-a", ChatID: "oc_chat", ChatType: "p2p", Text: "/loop -i 1ms 完成后退出"}); err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool {
		status, ok := svc.loopStatusSnapshot(key)
		return ok && !status.running && status.reason == "agent 返回 DONE"
	})
	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{BotID: "bot-a", ChatID: "oc_chat", ChatType: "p2p", Text: "现在完成了吗？"})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(normal) error = %v", err)
	}
	if reply != "正常回答" {
		t.Fatalf("reply = %q, want normal answer", reply)
	}
	rt.mu.Lock()
	calls := append([]fakePromptCall(nil), rt.promptCalls...)
	rt.mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("prompt calls = %d, want loop and normal prompt", len(calls))
	}
	if !strings.Contains(calls[0].Text, "后续不含该段的普通用户消息不得沿用本规则") {
		t.Fatalf("loop prompt = %q, want scoped DONE rule", calls[0].Text)
	}
	if strings.Contains(calls[1].Text, "## Loop Metadata") || strings.Contains(calls[1].Text, "## Loop Stop Rules") {
		t.Fatalf("normal prompt = %q, should not contain loop rules", calls[1].Text)
	}
}

func TestNewMessageCancelsRunningLoop(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptReply:   "ACP 回复",
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
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
		Text:     "/loop -n 0 -i 1ms 一直推进",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after sending loop start message", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	secondReply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "改成处理这个",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(user) error = %v", err)
	}
	if secondReply != "ACP 回复" {
		t.Fatalf("second reply = %q, want user prompt reply", secondReply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() >= 1 })
	close(rt.blockPrompt)
	if rt.promptCallCount() != 2 {
		t.Fatalf("prompt calls = %d, want loop prompt and replacement user prompt", rt.promptCallCount())
	}
	updates := client.updatesSnapshot()
	if !containsStringWithAll(updates, "状态：已完成", "结束原因：已被新消息打断") {
		t.Fatalf("updates = %#v, want cancelled loop start message update", updates)
	}
	if textUpdates := client.textUpdatesSnapshot(); len(textUpdates) != 0 {
		t.Fatalf("text updates = %#v, want loop status card updates only", textUpdates)
	}
	svc.taskMu.Lock()
	status := svc.loopStatuses[key]
	svc.taskMu.Unlock()
	if status.running || status.reason != "已被新消息打断" {
		t.Fatalf("loop status = %+v, want cancelled by new message", status)
	}
}

func TestLoopStopCancelsRunningLoopAndStatusReportsManualStop(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptReply:   "继续",
		blockPrompt:   make(chan struct{}),
		blockPromptAt: 1,
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
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

	if _, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop -n 0 -i 1ms 长循环",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/loop) error = %v", err)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop STOP",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop stop) error = %v", err)
	}
	if reply != "已停止当前会话的 loop。" {
		t.Fatalf("reply = %q, want stop confirmation", reply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() >= 1 })
	updates := client.updatesSnapshot()
	if !containsStringWithAll(updates, "状态：已完成", "结束原因：已手动停止") {
		t.Fatalf("updates = %#v, want manual stop update", updates)
	}
	if textUpdates := client.textUpdatesSnapshot(); len(textUpdates) != 0 {
		t.Fatalf("text updates = %#v, want loop status card updates only", textUpdates)
	}
	close(rt.blockPrompt)

	statusReply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/loop STATUS",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/loop status) error = %v", err)
	}
	if !strings.Contains(statusReply, "状态：已结束") || !strings.Contains(statusReply, "原因：已手动停止") {
		t.Fatalf("status reply = %q, want manual stop status", statusReply)
	}
}
