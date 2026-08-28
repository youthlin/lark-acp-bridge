package bridge

import (
	"context"
	"errors"
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
		"最终只返回一条简短的 /loop 命令",
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

func TestLoopStatusHelpersIgnoreStaleStarted(t *testing.T) {
	svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
	oldStarted := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	newStarted := oldStarted.Add(time.Minute)
	svc.markLoopStarted(key, oldStarted, loopRequest{Prompt: "旧 loop", MaxRounds: 5, Interval: time.Second})
	svc.markLoopStarted(key, newStarted, loopRequest{Prompt: "新 loop", MaxRounds: 3, Interval: 2 * time.Second})
	if !svc.addLoopPendingMessage(key, "新补充") {
		t.Fatal("addLoopPendingMessage() = false, want running loop")
	}

	svc.markLoopRound(key, oldStarted, 7)
	if got := svc.takeLoopPendingAdd(key, oldStarted); got != "" {
		t.Fatalf("takeLoopPendingAdd(old) = %q, want empty", got)
	}
	svc.markLoopFinished(key, oldStarted, "旧 loop 结束", nil)

	svc.taskMu.Lock()
	status := svc.loopStatuses[key]
	svc.taskMu.Unlock()
	if !status.running || !status.started.Equal(newStarted) || status.round != 0 || status.reason != "" {
		t.Fatalf("loop status after stale updates = %+v, want new loop still running", status)
	}
	if status.pendingAdd != "新补充" {
		t.Fatalf("pendingAdd = %q, want preserved new supplement", status.pendingAdd)
	}

	svc.markLoopRound(key, newStarted, 1)
	if got := svc.takeLoopPendingAdd(key, newStarted); got != "新补充" {
		t.Fatalf("takeLoopPendingAdd(new) = %q, want new supplement", got)
	}
	svc.markLoopFinished(key, newStarted, "新 loop 完成", nil)

	svc.taskMu.Lock()
	status = svc.loopStatuses[key]
	svc.taskMu.Unlock()
	if status.running || status.round != 1 || status.reason != "新 loop 完成" || status.lastError != "" {
		t.Fatalf("loop status after current finish = %+v, want finished new loop", status)
	}
}

func TestStartLoopStatusSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "oc_chat", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "oc_other", ""))
	started := time.Date(2026, 8, 1, 10, 30, 0, 0, time.UTC)
	cases := []struct {
		name      string
		existing  loopRunStatus
		request   loopRequest
		wantRound int
	}{
		{
			name:     "新 loop 状态按规范化 key 写入",
			request:  loopRequest{Prompt: "持续推进", MaxRounds: 3, MaxDuration: 5 * time.Minute, Interval: 2 * time.Second},
			existing: loopRunStatus{},
		},
		{
			name: "新 loop 开始会覆盖同 session 旧状态",
			existing: loopRunStatus{
				running:    false,
				started:    started.Add(-time.Minute),
				round:      4,
				pendingAdd: "旧补充",
				reason:     "旧 loop 完成",
				lastError:  "旧错误",
				prompt:     "旧 loop",
			},
			request: loopRequest{Prompt: "新 loop", MaxRounds: 1, Interval: time.Second},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
			svc.taskMu.Lock()
			if !tt.existing.started.IsZero() || tt.existing.prompt != "" {
				svc.loopStatuses[normalizedKey] = tt.existing
			}
			svc.loopStatuses[otherKey] = loopRunStatus{
				running: true,
				started: started.Add(time.Minute),
				round:   8,
				prompt:  "其他 loop",
			}
			svc.taskMu.Unlock()

			svc.startLoopStatus(key, started, tt.request)

			svc.taskMu.Lock()
			status := svc.loopStatuses[normalizedKey]
			otherStatus := svc.loopStatuses[otherKey]
			_, rawExists := svc.loopStatuses[key]
			svc.taskMu.Unlock()
			if rawExists && key != normalizedKey {
				t.Fatalf("loopStatuses contains raw key %+v, want only normalized key", key)
			}
			if !status.running || !status.started.Equal(started) || status.round != 0 || status.prompt != tt.request.Prompt {
				t.Fatalf("status = %+v, want new running loop for prompt %q", status, tt.request.Prompt)
			}
			if status.pendingAdd != "" || status.reason != "" || status.lastError != "" {
				t.Fatalf("status = %+v, want stale terminal fields cleared", status)
			}
			if status.maxRounds != tt.request.MaxRounds || status.maxDuration != tt.request.MaxDuration || status.interval != tt.request.Interval {
				t.Fatalf("status = %+v, want request limits copied from %+v", status, tt.request)
			}
			if otherStatus.round != 8 || otherStatus.prompt != "其他 loop" {
				t.Fatalf("other status = %+v, want unchanged other session", otherStatus)
			}
		})
	}
}

func TestAppendLoopPendingMessageSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "oc_chat", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "oc_other", ""))
	started := time.Date(2026, 8, 1, 10, 45, 0, 0, time.UTC)
	cases := []struct {
		name        string
		existing    *loopRunStatus
		text        string
		wantAdded   bool
		wantPending string
	}{
		{
			name:        "运行中 loop 写入裁剪后的补充消息",
			existing:    &loopRunStatus{running: true, started: started, prompt: "目标 loop"},
			text:        "  补充 A  ",
			wantAdded:   true,
			wantPending: "补充 A",
		},
		{
			name:        "运行中 loop 追加补充消息用空行分隔",
			existing:    &loopRunStatus{running: true, started: started, pendingAdd: "已有补充", prompt: "目标 loop"},
			text:        "补充 B",
			wantAdded:   true,
			wantPending: "已有补充\n\n补充 B",
		},
		{
			name:        "已结束 loop 不接受补充消息",
			existing:    &loopRunStatus{running: false, started: started, reason: "已完成", prompt: "目标 loop"},
			text:        "补充 C",
			wantPending: "",
		},
		{
			name:        "缺少 loop 状态不接受补充消息",
			text:        "补充 D",
			wantPending: "",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
			svc.taskMu.Lock()
			if tt.existing != nil {
				svc.loopStatuses[normalizedKey] = *tt.existing
			}
			svc.loopStatuses[otherKey] = loopRunStatus{
				running:    true,
				started:    started.Add(time.Minute),
				pendingAdd: "其他补充",
				prompt:     "其他 loop",
			}
			svc.taskMu.Unlock()

			added := svc.appendLoopPendingMessage(key, tt.text)

			svc.taskMu.Lock()
			status := svc.loopStatuses[normalizedKey]
			otherStatus := svc.loopStatuses[otherKey]
			svc.taskMu.Unlock()
			if added != tt.wantAdded {
				t.Fatalf("appendLoopPendingMessage() = %v, want %v", added, tt.wantAdded)
			}
			if status.pendingAdd != tt.wantPending {
				t.Fatalf("pendingAdd = %q, want %q", status.pendingAdd, tt.wantPending)
			}
			if otherStatus.pendingAdd != "其他补充" || otherStatus.prompt != "其他 loop" {
				t.Fatalf("other status = %+v, want unchanged other session", otherStatus)
			}
		})
	}
}

func TestConsumeLoopPendingMessageSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "oc_chat", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "oc_other", ""))
	started := time.Date(2026, 8, 1, 10, 50, 0, 0, time.UTC)
	otherStarted := started.Add(time.Minute)
	cases := []struct {
		name          string
		existing      *loopRunStatus
		consumeStart  time.Time
		wantPending   string
		wantRemaining string
	}{
		{
			name:          "当前代际消费补充消息并清空",
			existing:      &loopRunStatus{running: true, started: started, pendingAdd: "补充 A", prompt: "目标 loop"},
			consumeStart:  started,
			wantPending:   "补充 A",
			wantRemaining: "",
		},
		{
			name:          "旧代际不消费当前补充消息",
			existing:      &loopRunStatus{running: true, started: started, pendingAdd: "补充 B", prompt: "目标 loop"},
			consumeStart:  started.Add(-time.Minute),
			wantPending:   "",
			wantRemaining: "补充 B",
		},
		{
			name:          "空补充消息返回空且保持空",
			existing:      &loopRunStatus{running: true, started: started, prompt: "目标 loop"},
			consumeStart:  started,
			wantPending:   "",
			wantRemaining: "",
		},
		{
			name:          "缺少 loop 状态返回空",
			consumeStart:  started,
			wantPending:   "",
			wantRemaining: "",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
			svc.taskMu.Lock()
			if tt.existing != nil {
				svc.loopStatuses[normalizedKey] = *tt.existing
			}
			svc.loopStatuses[otherKey] = loopRunStatus{
				running:    true,
				started:    otherStarted,
				pendingAdd: "其他补充",
				prompt:     "其他 loop",
			}
			svc.taskMu.Unlock()

			pending := svc.consumeLoopPendingMessage(key, tt.consumeStart)

			svc.taskMu.Lock()
			status := svc.loopStatuses[normalizedKey]
			otherStatus := svc.loopStatuses[otherKey]
			svc.taskMu.Unlock()
			if pending != tt.wantPending {
				t.Fatalf("consumeLoopPendingMessage() = %q, want %q", pending, tt.wantPending)
			}
			if status.pendingAdd != tt.wantRemaining {
				t.Fatalf("remaining pendingAdd = %q, want %q", status.pendingAdd, tt.wantRemaining)
			}
			if otherStatus.pendingAdd != "其他补充" || otherStatus.prompt != "其他 loop" {
				t.Fatalf("other status = %+v, want unchanged other session", otherStatus)
			}
		})
	}
}

func TestUpdateLoopRoundStatusSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "oc_chat", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "oc_other", ""))
	started := time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC)
	otherStarted := started.Add(time.Minute)
	cases := []struct {
		name          string
		updateKey     SessionKey
		updateStarted time.Time
		round         int
		wantUpdated   bool
		wantRound     int
		wantRunning   bool
		wantOther     int
	}{
		{
			name:          "当前代际更新轮次并标记 running",
			updateKey:     key,
			updateStarted: started,
			round:         3,
			wantUpdated:   true,
			wantRound:     3,
			wantRunning:   true,
			wantOther:     8,
		},
		{
			name:          "旧代际不覆盖当前 loop 轮次",
			updateKey:     key,
			updateStarted: started.Add(-time.Minute),
			round:         9,
			wantUpdated:   false,
			wantRound:     1,
			wantRunning:   false,
			wantOther:     8,
		},
		{
			name:          "其他 session 更新不影响目标 session",
			updateKey:     otherKey,
			updateStarted: otherStarted,
			round:         4,
			wantUpdated:   true,
			wantRound:     1,
			wantRunning:   false,
			wantOther:     4,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
			svc.taskMu.Lock()
			svc.loopStatuses[normalizedKey] = loopRunStatus{
				started: started,
				running: false,
				round:   1,
				prompt:  "目标 loop",
			}
			svc.loopStatuses[otherKey] = loopRunStatus{
				started: otherStarted,
				running: true,
				round:   8,
				prompt:  "其他 loop",
			}
			svc.taskMu.Unlock()

			updated := svc.updateLoopRoundStatus(tt.updateKey, tt.updateStarted, tt.round)

			svc.taskMu.Lock()
			status := svc.loopStatuses[normalizedKey]
			otherStatus := svc.loopStatuses[otherKey]
			svc.taskMu.Unlock()
			if updated != tt.wantUpdated {
				t.Fatalf("updateLoopRoundStatus() = %v, want %v", updated, tt.wantUpdated)
			}
			if status.round != tt.wantRound || status.running != tt.wantRunning || status.prompt != "目标 loop" {
				t.Fatalf("status = %+v, want round=%d running=%v", status, tt.wantRound, tt.wantRunning)
			}
			if otherStatus.round != tt.wantOther || otherStatus.prompt != "其他 loop" {
				t.Fatalf("other status = %+v, want round=%d unchanged prompt", otherStatus, tt.wantOther)
			}
		})
	}
}

func TestFinishLoopStatusSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "oc_chat", "")
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(imSessionKey("bot-a", "oc_other", ""))
	started := time.Date(2026, 8, 1, 11, 30, 0, 0, time.UTC)
	otherStarted := started.Add(time.Minute)
	loopErr := errors.New("agent 调用失败")
	cases := []struct {
		name          string
		existing      loopRunStatus
		finishStarted time.Time
		reason        string
		err           error
		wantUpdated   bool
		wantRunning   bool
		wantReason    string
		wantError     string
	}{
		{
			name: "当前代际成功完成并清理错误",
			existing: loopRunStatus{
				running:   true,
				started:   started,
				lastError: "旧错误",
				prompt:    "目标 loop",
			},
			finishStarted: started,
			reason:        "agent 返回 DONE",
			wantUpdated:   true,
			wantReason:    "agent 返回 DONE",
		},
		{
			name: "当前代际普通错误记录 lastError",
			existing: loopRunStatus{
				running: true,
				started: started,
				prompt:  "目标 loop",
			},
			finishStarted: started,
			reason:        "执行失败",
			err:           loopErr,
			wantUpdated:   true,
			wantReason:    "执行失败",
			wantError:     loopErr.Error(),
		},
		{
			name: "旧代际 finish 不覆盖当前状态",
			existing: loopRunStatus{
				running: true,
				started: started,
				round:   2,
				prompt:  "目标 loop",
			},
			finishStarted: started.Add(-time.Minute),
			reason:        "旧 loop 完成",
			wantRunning:   true,
			wantReason:    "",
		},
		{
			name: "手动取消后的 context canceled 不覆盖既有原因",
			existing: loopRunStatus{
				running:   false,
				started:   started,
				reason:    "已手动停止",
				lastError: "",
				prompt:    "目标 loop",
			},
			finishStarted: started,
			reason:        "已取消",
			err:           context.Canceled,
			wantReason:    "已手动停止",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
			svc.taskMu.Lock()
			svc.loopStatuses[normalizedKey] = tt.existing
			svc.loopStatuses[otherKey] = loopRunStatus{
				running: true,
				started: otherStarted,
				round:   8,
				prompt:  "其他 loop",
			}
			svc.taskMu.Unlock()

			updated := svc.finishLoopStatus(key, tt.finishStarted, tt.reason, tt.err)

			svc.taskMu.Lock()
			status := svc.loopStatuses[normalizedKey]
			otherStatus := svc.loopStatuses[otherKey]
			svc.taskMu.Unlock()
			if updated != tt.wantUpdated {
				t.Fatalf("finishLoopStatus() = %v, want %v", updated, tt.wantUpdated)
			}
			if status.running != tt.wantRunning || status.reason != tt.wantReason || status.lastError != tt.wantError {
				t.Fatalf("status = %+v, want running=%v reason=%q lastError=%q", status, tt.wantRunning, tt.wantReason, tt.wantError)
			}
			if tt.wantUpdated && status.ended.IsZero() {
				t.Fatalf("status.ended is zero after updated finish: %+v", status)
			}
			if !tt.wantUpdated && !status.ended.IsZero() {
				t.Fatalf("status.ended = %v, want unchanged zero time", status.ended)
			}
			if otherStatus.round != 8 || otherStatus.prompt != "其他 loop" {
				t.Fatalf("other status = %+v, want unchanged other session", otherStatus)
			}
		})
	}
}

func TestLoopStatusSnapshotNormalizesSessionKey(t *testing.T) {
	svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
	key := imSessionKey("bot-a", "oc_chat", "")
	started := time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC)
	svc.markLoopStarted(key, started, loopRequest{Prompt: "持续推进", MaxRounds: 2, Interval: time.Second})

	status, ok := svc.loopStatusSnapshot(key)
	if !ok {
		t.Fatal("loopStatusSnapshot() ok=false, want normalized lookup to find status")
	}
	if !status.started.Equal(started) || status.prompt != "持续推进" || status.maxRounds != 2 {
		t.Fatalf("loop status = %+v, want status written by markLoopStarted", status)
	}
}

func TestHandleLoopCancelAllowsOwnerAndCancelsRunningLoop(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "omt_thread"))
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

func TestCancelLoopTaskSessionWorkBoundaries(t *testing.T) {
	agent := config.AgentConfig{Command: "traex"}
	cases := []struct {
		name              string
		targetKind        taskKind
		otherKind         taskKind
		wantCanceled      bool
		wantTargetRunning bool
		wantOtherRunning  bool
	}{
		{
			name:              "不会取消同会话非 loop 任务",
			targetKind:        taskKindUser,
			otherKind:         taskKindLoop,
			wantCanceled:      false,
			wantTargetRunning: true,
			wantOtherRunning:  true,
		},
		{
			name:              "只取消目标会话 loop",
			targetKind:        taskKindLoop,
			otherKind:         taskKindLoop,
			wantCanceled:      true,
			wantTargetRunning: false,
			wantOtherRunning:  true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rt := &fakeRuntime{}
			svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
			svc.setRuntime(rt)
			targetKey := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "omt_target"))
			otherKey := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "omt_other"))
			targetSession := Session{Key: targetKey, AgentName: "traex", ACPSessionID: "acp-target"}
			otherSession := Session{Key: otherKey, AgentName: "traex", ACPSessionID: "acp-other"}
			targetCtx, targetFinish := svc.startTask(context.Background(), targetSession, agent, tt.targetKind)
			defer targetFinish()
			otherCtx, otherFinish := svc.startTask(context.Background(), otherSession, agent, tt.otherKind)
			defer otherFinish()
			svc.taskMu.Lock()
			svc.loopStatuses[targetKey] = loopRunStatus{running: true, started: time.Now()}
			svc.loopStatuses[otherKey] = loopRunStatus{running: true, started: time.Now()}
			svc.taskMu.Unlock()

			if got := svc.cancelLoopTask(context.Background(), targetKey, "已手动停止"); got != tt.wantCanceled {
				t.Fatalf("cancelLoopTask() = %v, want %v", got, tt.wantCanceled)
			}

			if tt.wantCanceled {
				select {
				case <-targetCtx.Done():
				default:
					t.Fatal("target loop context was not canceled")
				}
				waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() == 1 })
			} else {
				select {
				case <-targetCtx.Done():
					t.Fatal("target non-loop context was canceled")
				default:
				}
				if got := rt.cancelCallCount(); got != 0 {
					t.Fatalf("cancel calls = %d, want none for non-loop target", got)
				}
			}
			select {
			case <-otherCtx.Done():
				t.Fatal("other session context was canceled")
			default:
			}

			svc.taskMu.Lock()
			targetTask := svc.tasks[targetKey]
			otherTask := svc.tasks[otherKey]
			targetStatus := svc.loopStatuses[targetKey]
			otherStatus := svc.loopStatuses[otherKey]
			svc.taskMu.Unlock()
			if (targetTask != nil) != tt.wantTargetRunning {
				t.Fatalf("target task = %+v, want running=%v", targetTask, tt.wantTargetRunning)
			}
			if (otherTask != nil) != tt.wantOtherRunning {
				t.Fatalf("other task = %+v, want running=%v", otherTask, tt.wantOtherRunning)
			}
			if targetStatus.running != tt.wantTargetRunning {
				t.Fatalf("target status = %+v, want running=%v", targetStatus, tt.wantTargetRunning)
			}
			if !otherStatus.running {
				t.Fatalf("other status = %+v, want still running", otherStatus)
			}
			if tt.wantCanceled && targetStatus.reason != "已手动停止" {
				t.Fatalf("target status reason = %q, want 已手动停止", targetStatus.reason)
			}
		})
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
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "omt_thread"))
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
	client.streamStarter = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
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
			if strings.Contains(update, "🚫") {
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
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
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
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_chat", ""))
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
