package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestWikiTimerRunsSilentReflection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptReply: "changed: yes\nfiles:\n- knowledge/core.md\nsummary: 更新知识入口\nreason: 用户要求长期保留"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "omt_thread"})
	session := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-1",
		Cwd:             t.TempDir(),
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		WikiIntervalSec: 1,
	}
	svc.scheduleWikiAfterUserPrompt(session, mustConfigAgent(t, config.Default(), "traex"))

	waitForCondition(t, 2*time.Second, func() bool { return rt.promptCallCount() == 1 })
	if got := rt.promptCalls[0].Text; !strings.Contains(got, "请对刚才的对话进行反思") || !strings.Contains(got, "NoReply") {
		t.Fatalf("wiki prompt = %q, want reflection prompt", got)
	}
	svc.taskMu.Lock()
	status := svc.wikiStatuses[key]
	_, hasTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if hasTimer {
		t.Fatal("wiki timer should not reschedule itself after reflection")
	}
	if !status.lastSuccess || status.running {
		t.Fatalf("wiki status = %+v, want completed success", status)
	}
	if !strings.Contains(status.lastSummary, "knowledge/core.md") {
		t.Fatalf("wiki summary = %q, want changed files", status.lastSummary)
	}
}

func TestWikiReflectionPromptRequestsAuditSummary(t *testing.T) {
	prompt := wikiReflectionPrompt("/workspace")
	for _, want := range []string{
		"/workspace/skills/wiki/SKILL.md",
		"changed: yes",
		"files:",
		"summary:",
		"reason:",
		"NoReply",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("wiki reflection prompt = %q, want %q", prompt, want)
		}
	}
}

func TestWikiLintRunsPromptRecordsSummaryAndKeepsTimer(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptReply: "changed: yes\nfiles:\n- knowledge/index.md\nsummary: 修复索引\nreason: lint 检查",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "正在检查索引。"},
				},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	client := newFakeSentMessageClient("")
	var cardsMu sync.Mutex
	var cards []*fakeStreamCard
	cardsSnapshot := func() []*fakeStreamCard {
		cardsMu.Lock()
		defer cardsMu.Unlock()
		return append([]*fakeStreamCard(nil), cards...)
	}
	client.streamStarter = func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cardsMu.Lock()
		cards = append(cards, card)
		cardsMu.Unlock()
		return card, nil
	}
	svc.setOutbound("bot-a", client)
	key := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat"})
	session := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-1",
		Cwd:             t.TempDir(),
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
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
		t.Fatal("wiki timer should be scheduled before lint")
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/wiki lint",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki lint) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty direct reply when outbound can send acknowledgement", reply)
	}
	waitForCondition(t, time.Second, func() bool {
		if rt.promptCallCount() != 1 {
			return false
		}
		client.mu.Lock()
		sentCount := len(client.sent)
		client.mu.Unlock()
		gotCards := cardsSnapshot()
		return sentCount == 1 && len(gotCards) == 1 && gotCards[0].isClosed()
	})
	client.mu.Lock()
	sent := append([]string(nil), client.sent...)
	client.mu.Unlock()
	if !strings.Contains(sent[0], "wiki lint 已开始") {
		t.Fatalf("sent[0] = %q, want lint start acknowledgement", sent[0])
	}
	if len(sent) != 1 {
		t.Fatalf("sent = %q, want only lint start acknowledgement when stream card is used", sent)
	}
	gotCards := cardsSnapshot()
	if len(gotCards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", gotCards)
	}
	if got := gotCards[0].textUpdatesSnapshot(); len(got) != 1 || !strings.Contains(got[0], "正在检查索引") {
		t.Fatalf("stream text updates = %+v, want lint progress", got)
	}
	if got := gotCards[0].finalTextUpdatesSnapshot(); len(got) != 1 || !strings.Contains(got[0], "knowledge/index.md") {
		t.Fatalf("stream final text = %+v, want lint summary", got)
	}
	if got := rt.promptCallCount(); got != 1 {
		t.Fatalf("prompt calls = %d, want one lint prompt", got)
	}
	rt.mu.Lock()
	call := rt.promptCalls[0]
	rt.mu.Unlock()
	for _, want := range []string{"请检查并修复", "/workspace/knowledge/lint.md", "changed: yes/no"} {
		if !strings.Contains(call.Text, want) {
			t.Fatalf("lint prompt = %q, want %q", call.Text, want)
		}
	}
	if !call.HasUpdateHandler || !call.HasPermissionHandler {
		t.Fatalf("prompt call = %+v, want stream update and permission handlers", call)
	}
	svc.taskMu.Lock()
	afterGeneration := svc.wikiGenerations[key]
	_, afterTimer := svc.wikiTimers[key]
	status := svc.wikiStatuses[key]
	svc.taskMu.Unlock()
	if !afterTimer {
		t.Fatal("/wiki lint should keep pending wiki timer")
	}
	if afterGeneration != beforeGeneration {
		t.Fatalf("wiki generation = %d, want unchanged %d", afterGeneration, beforeGeneration)
	}
	if !status.lastSuccess || !strings.Contains(status.lastSummary, "knowledge/index.md") {
		t.Fatalf("wiki status = %+v, want lint summary recorded", status)
	}
}

func TestWikiLintReturnsImmediatelyWhenPromptIsStillRunning(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{
		promptReply: "changed: no\nsummary: ok",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-session-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "lint running"},
				},
			},
		},
		blockPrompt: make(chan struct{}),
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	client := newFakeSentMessageClient("")
	var cardsMu sync.Mutex
	var cards []*fakeStreamCard
	cardsSnapshot := func() []*fakeStreamCard {
		cardsMu.Lock()
		defer cardsMu.Unlock()
		return append([]*fakeStreamCard(nil), cards...)
	}
	client.streamStarter = func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cardsMu.Lock()
		cards = append(cards, card)
		cardsMu.Unlock()
		return card, nil
	}
	svc.setOutbound("bot-a", client)
	key := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat"})
	workspace := filepath.Join(t.TempDir(), "workspace")
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: t.TempDir(), Workspace: workspace}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		Workspace: workspace,
		Text:      "/wiki lint",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki lint) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want no direct reply while background lint runs", reply)
	}
	waitForCondition(t, time.Second, func() bool {
		client.mu.Lock()
		sentCount := len(client.sent)
		client.mu.Unlock()
		return rt.promptCallCount() == 1 && sentCount == 1 && len(cardsSnapshot()) == 1
	})
	client.mu.Lock()
	sent := append([]string(nil), client.sent...)
	client.mu.Unlock()
	if !strings.Contains(sent[0], "wiki lint 已开始") {
		t.Fatalf("sent = %q, want start acknowledgement", sent)
	}
	gotCards := cardsSnapshot()
	if len(gotCards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", gotCards)
	}
	if got := gotCards[0].textUpdatesSnapshot(); len(got) != 1 || !strings.Contains(got[0], "lint running") {
		t.Fatalf("stream text updates = %+v, want lint progress", got)
	}
	svc.taskMu.Lock()
	running := svc.tasks[key]
	svc.taskMu.Unlock()
	if running == nil || running.kind != taskKindWiki {
		t.Fatalf("running task = %+v, want background wiki lint task", running)
	}
	close(rt.blockPrompt)
	waitForCondition(t, time.Second, func() bool {
		gotCards := cardsSnapshot()
		return len(gotCards) == 1 && gotCards[0].isClosed()
	})
	client.mu.Lock()
	sent = append([]string(nil), client.sent...)
	client.mu.Unlock()
	if len(sent) != 1 {
		t.Fatalf("sent = %q, want no final text reply when stream card is used", sent)
	}
}

func TestWikiLintReportsBusyWithoutCancelingCurrentTask(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptReply: "changed: no\nsummary: ok"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat"})
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: t.TempDir()}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	_, finish := svc.startTask(context.Background(), session, mustConfigAgent(t, config.Default(), "traex"), taskKindUser)
	defer finish()

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/wiki lint",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki lint) error = %v", err)
	}
	if !strings.Contains(reply, "当前会话正在忙碌") {
		t.Fatalf("reply = %q, want busy message", reply)
	}
	if got := rt.promptCallCount(); got != 0 {
		t.Fatalf("prompt calls = %d, want no lint prompt", got)
	}
	if !svc.sessionHasRunningUserTask(key) {
		t.Fatal("running user task should not be canceled by /wiki lint")
	}
}

func TestWikiLintReportsBusyDuringBackgroundWikiTask(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptReply: "changed: no\nsummary: ok"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat"})
	workspace := filepath.Join(t.TempDir(), "workspace")
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: t.TempDir(), Workspace: workspace}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	_, finish, _ := svc.startWikiTask(context.Background(), session, mustConfigAgent(t, config.Default(), "traex"), wikiRuntimeKey(key, 1, session.ACPSessionID))
	defer finish()

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		Workspace: workspace,
		Text:      "/wiki lint",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki lint) error = %v", err)
	}
	if !strings.Contains(reply, "当前会话正在忙碌") {
		t.Fatalf("reply = %q, want busy message", reply)
	}
	if got := rt.promptCallCount(); got != 0 {
		t.Fatalf("prompt calls = %d, want no lint prompt", got)
	}
}

func TestWikiUpgradeUpdatesExistingWorkspaceWithoutACPSession(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	cfg := config.Default()
	cfg.Bots[0].Workspace = workspace
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	svc := newTestService(cfg, store)
	rt := &fakeRuntime{promptReply: "should not be called"}
	svc.setRuntime(rt)
	if _, err := ensureWorkspace(workspace, "default"); err != nil {
		t.Fatalf("ensureWorkspace() error = %v", err)
	}
	markWorkspaceBootstrapped(t, workspace)
	knowledgeAgents := filepath.Join(workspace, "knowledge", "AGENTS.md")
	if err := os.WriteFile(knowledgeAgents, []byte("# Existing Knowledge Rules\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(knowledge/AGENTS.md) error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "default",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		Workspace: workspace,
		Text:      "/wiki upgrade",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki upgrade) error = %v", err)
	}
	if !strings.Contains(reply, "wiki upgrade 完成") || !strings.Contains(reply, "knowledge/AGENTS.md") {
		t.Fatalf("reply = %q, want updated files", reply)
	}
	if got := rt.promptCallCount(); got != 0 {
		t.Fatalf("prompt calls = %d, want no ACP prompt", got)
	}
	data, err := os.ReadFile(knowledgeAgents)
	if err != nil {
		t.Fatalf("ReadFile(knowledge/AGENTS.md) error = %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "# Existing Knowledge Rules") || !strings.Contains(text, workspaceWikiPolicyMarker) {
		t.Fatalf("knowledge/AGENTS.md = %q, want existing content plus policy marker", text)
	}
	logData, err := os.ReadFile(filepath.Join(workspace, "knowledge", "log.md"))
	if err != nil {
		t.Fatalf("ReadFile(knowledge/log.md) error = %v", err)
	}
	if !strings.Contains(string(logData), "同步 bridge 当前知识库维护约束") {
		t.Fatalf("knowledge/log.md = %q, want upgrade log", logData)
	}
}

func TestWikiUpgradeReportsAlreadyCurrent(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	cfg := config.Default()
	cfg.Bots[0].Workspace = workspace
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	svc := newTestService(cfg, store)
	if _, err := ensureWorkspace(workspace, "default"); err != nil {
		t.Fatalf("ensureWorkspace() error = %v", err)
	}
	if _, err := upgradeWorkspaceWikiPolicy(workspace); err != nil {
		t.Fatalf("upgradeWorkspaceWikiPolicy() error = %v", err)
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "default",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		Workspace: workspace,
		Text:      "/wiki upgrade",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki upgrade) error = %v", err)
	}
	if !strings.Contains(reply, "已包含最新 wiki 维护规则") {
		t.Fatalf("reply = %q, want already current", reply)
	}
}

func TestWikiUpgradeReportsBusyDuringWorkspaceTask(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	svc := newTestService(cfg, store)
	if _, err := ensureWorkspace(workspace, "bot-a"); err != nil {
		t.Fatalf("ensureWorkspace() error = %v", err)
	}
	markWorkspaceBootstrapped(t, workspace)
	otherKey := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_other"})
	otherSession := Session{Key: otherKey, AgentName: "traex", ACPSessionID: "acp-other", Cwd: t.TempDir(), Workspace: workspace}
	_, finish := svc.startTask(context.Background(), otherSession, mustConfigAgent(t, cfg, "traex"), taskKindUser)
	defer finish()

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		Workspace: workspace,
		Text:      "/wiki upgrade",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki upgrade) error = %v", err)
	}
	if !strings.Contains(reply, "当前会话正在忙碌") {
		t.Fatalf("reply = %q, want busy message", reply)
	}
	for _, file := range []string{
		filepath.Join("knowledge", "AGENTS.md"),
		filepath.Join("knowledge", "lint.md"),
		filepath.Join("skills", "wiki", "SKILL.md"),
	} {
		data, err := os.ReadFile(filepath.Join(workspace, file))
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", file, err)
		}
		if strings.Contains(string(data), workspaceWikiPolicyMarker) {
			t.Fatalf("%s contains policy marker despite busy upgrade:\n%s", file, data)
		}
	}
}

func TestWikiUpgradeReportsBusyDuringBackgroundWikiTask(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	svc := newTestService(cfg, store)
	if _, err := ensureWorkspace(workspace, "bot-a"); err != nil {
		t.Fatalf("ensureWorkspace() error = %v", err)
	}
	markWorkspaceBootstrapped(t, workspace)
	key := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat"})
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-wiki", Cwd: t.TempDir(), Workspace: workspace}
	_, finish, _ := svc.startWikiTask(context.Background(), session, mustConfigAgent(t, cfg, "traex"), wikiRuntimeKey(key, 1, session.ACPSessionID))
	defer finish()

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		Workspace: workspace,
		Text:      "/wiki upgrade",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki upgrade) error = %v", err)
	}
	if !strings.Contains(reply, "当前会话正在忙碌") {
		t.Fatalf("reply = %q, want busy message", reply)
	}
}

func TestWikiStatusIncludesLastSummary(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	key := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat"})
	if err := store.Upsert(Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	svc.taskMu.Lock()
	svc.wikiStatuses[key] = wikiRunStatus{
		lastStarted: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
		lastEnded:   time.Date(2026, 8, 5, 12, 0, 2, 0, time.UTC),
		lastSuccess: true,
		lastSummary: "changed: yes files: knowledge/core.md",
	}
	svc.taskMu.Unlock()

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
		Text:     "/wiki status",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/wiki status) error = %v", err)
	}
	if !strings.Contains(reply, "最近摘要：changed: yes files: knowledge/core.md") {
		t.Fatalf("reply = %q, want summary", reply)
	}
}

func TestWikiStatusSnapshotSessionWorkBoundaries(t *testing.T) {
	agent := config.AgentConfig{Command: "traex"}
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	normalizedKey := normalizeSessionKey(key)
	started := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name               string
		setup              func(t *testing.T, svc *Service)
		wantTimerSet       bool
		wantForegroundKind taskKind
		wantBackgroundTask bool
		wantLastSuccess    bool
		wantLastError      string
	}{
		{
			name: "读取等待触发的 wiki timer",
			setup: func(t *testing.T, svc *Service) {
				timer := time.NewTimer(time.Hour)
				t.Cleanup(func() { timer.Stop() })
				svc.taskMu.Lock()
				svc.wikiTimers[normalizedKey] = &pendingWikiRun{timer: timer, session: Session{Key: normalizedKey}}
				svc.taskMu.Unlock()
			},
			wantTimerSet: true,
		},
		{
			name: "读取前台 wiki task",
			setup: func(t *testing.T, svc *Service) {
				_, finish := svc.startTask(context.Background(), Session{Key: normalizedKey, AgentName: "traex"}, agent, taskKindWiki)
				t.Cleanup(finish)
			},
			wantForegroundKind: taskKindWiki,
		},
		{
			name: "读取后台 wiki runtime",
			setup: func(t *testing.T, svc *Service) {
				_, finish, _ := svc.startWikiTask(context.Background(), Session{Key: normalizedKey, AgentName: "traex", ACPSessionID: "acp-wiki"}, agent, wikiRuntimeKey(normalizedKey, 1, "acp-wiki"))
				t.Cleanup(finish)
			},
			wantBackgroundTask: true,
		},
		{
			name: "读取最近一次 wiki 状态",
			setup: func(t *testing.T, svc *Service) {
				svc.taskMu.Lock()
				svc.wikiStatuses[normalizedKey] = wikiRunStatus{
					lastStarted: started,
					lastEnded:   started.Add(time.Second),
					lastSuccess: false,
					lastError:   "失败原因",
				}
				svc.taskMu.Unlock()
			},
			wantLastSuccess: false,
			wantLastError:   "失败原因",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
			tt.setup(t, svc)

			snapshot := svc.wikiStatusSnapshot(key)
			if snapshot.timerSet != tt.wantTimerSet {
				t.Fatalf("timerSet = %v, want %v", snapshot.timerSet, tt.wantTimerSet)
			}
			if tt.wantForegroundKind != "" {
				if snapshot.foregroundTask == nil || snapshot.foregroundTask.kind != tt.wantForegroundKind {
					t.Fatalf("foregroundTask = %+v, want kind %s", snapshot.foregroundTask, tt.wantForegroundKind)
				}
			} else if snapshot.foregroundTask != nil {
				t.Fatalf("foregroundTask = %+v, want nil", snapshot.foregroundTask)
			}
			if snapshot.backgroundTask != tt.wantBackgroundTask {
				t.Fatalf("backgroundTask = %v, want %v", snapshot.backgroundTask, tt.wantBackgroundTask)
			}
			if tt.wantLastError != "" {
				if snapshot.status.lastSuccess != tt.wantLastSuccess || snapshot.status.lastError != tt.wantLastError || !snapshot.status.lastStarted.Equal(started) {
					t.Fatalf("status = %+v, want last wiki status", snapshot.status)
				}
			}
		})
	}
}

func TestWikiTimerSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_other"})
	newServiceWithTimers := func(t *testing.T) (*Service, *time.Timer, *time.Timer) {
		t.Helper()
		svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
		timer := time.NewTimer(time.Hour)
		otherTimer := time.NewTimer(time.Hour)
		t.Cleanup(func() {
			timer.Stop()
			otherTimer.Stop()
		})
		svc.taskMu.Lock()
		svc.wikiGenerations[normalizedKey] = 10
		svc.wikiGenerations[otherKey] = 20
		svc.wikiTimers[normalizedKey] = &pendingWikiRun{
			timer:      timer,
			generation: 10,
			session:    Session{Key: normalizedKey, AgentName: "traex", ACPSessionID: "acp-a"},
			agent:      config.AgentConfig{Command: "traex"},
		}
		svc.wikiTimers[otherKey] = &pendingWikiRun{
			timer:      otherTimer,
			generation: 20,
			session:    Session{Key: otherKey, AgentName: "traex", ACPSessionID: "acp-b"},
			agent:      config.AgentConfig{Command: "traex"},
		}
		svc.taskMu.Unlock()
		return svc, timer, otherTimer
	}
	cases := []struct {
		name         string
		run          func(t *testing.T, svc *Service)
		wantHasKey   bool
		wantOther    bool
		wantKeyGen   int64
		wantOtherGen int64
	}{
		{
			name: "hasWikiTimer 规范化 key 后只读取不修改",
			run: func(t *testing.T, svc *Service) {
				if !svc.hasWikiTimer(key) {
					t.Fatal("hasWikiTimer = false, want true")
				}
			},
			wantHasKey:   true,
			wantOther:    true,
			wantKeyGen:   10,
			wantOtherGen: 20,
		},
		{
			name: "cancelWikiTimer 规范化 key 后移除并推进代际",
			run: func(t *testing.T, svc *Service) {
				svc.cancelWikiTimer(key)
			},
			wantHasKey:   false,
			wantOther:    true,
			wantKeyGen:   11,
			wantOtherGen: 20,
		},
		{
			name: "takePendingWiki 规范化 key 后取出并推进代际",
			run: func(t *testing.T, svc *Service) {
				pending, ok := svc.takePendingWiki(key)
				if !ok {
					t.Fatal("takePendingWiki ok = false, want true")
				}
				if pending.session.Key != normalizedKey {
					t.Fatalf("pending session key = %+v, want %+v", pending.session.Key, normalizedKey)
				}
			},
			wantHasKey:   false,
			wantOther:    true,
			wantKeyGen:   11,
			wantOtherGen: 20,
		},
		{
			name: "takePendingWiki 缺失 timer 时仍推进代际失效旧回调",
			run: func(t *testing.T, svc *Service) {
				svc.cancelWikiTimer(key)
				if _, ok := svc.takePendingWiki(key); ok {
					t.Fatal("takePendingWiki ok = true, want false")
				}
			},
			wantHasKey:   false,
			wantOther:    true,
			wantKeyGen:   12,
			wantOtherGen: 20,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _ := newServiceWithTimers(t)

			tt.run(t, svc)

			svc.taskMu.Lock()
			_, hasKey := svc.wikiTimers[normalizedKey]
			_, hasOther := svc.wikiTimers[otherKey]
			keyGen := svc.wikiGenerations[normalizedKey]
			otherGen := svc.wikiGenerations[otherKey]
			svc.taskMu.Unlock()
			if hasKey != tt.wantHasKey {
				t.Fatalf("wikiTimers key exists = %v, want %v", hasKey, tt.wantHasKey)
			}
			if hasOther != tt.wantOther {
				t.Fatalf("wikiTimers other exists = %v, want %v", hasOther, tt.wantOther)
			}
			if keyGen != tt.wantKeyGen {
				t.Fatalf("wiki generation = %d, want %d", keyGen, tt.wantKeyGen)
			}
			if otherGen != tt.wantOtherGen {
				t.Fatalf("other wiki generation = %d, want %d", otherGen, tt.wantOtherGen)
			}
		})
	}
}

func TestScheduleWikiTimerSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	normalizedKey := normalizeSessionKey(key)
	agent := config.AgentConfig{Command: "traex"}
	oldTimer := time.NewTimer(time.Hour)
	oldTimer.Stop()
	cases := []struct {
		name          string
		setup         func(t *testing.T, svc *Service)
		run           func(t *testing.T, svc *Service)
		wantGen       int64
		wantSessionID string
		wantScheduled bool
	}{
		{
			name: "scheduleWikiTimer 规范化 key 并替换旧 timer",
			setup: func(t *testing.T, svc *Service) {
				svc.taskMu.Lock()
				svc.wikiGenerations[normalizedKey] = 3
				svc.wikiTimers[normalizedKey] = &pendingWikiRun{
					timer:      oldTimer,
					generation: 3,
					session:    Session{Key: normalizedKey, AgentName: "traex", ACPSessionID: "acp-old"},
					agent:      agent,
					scheduled:  time.Date(2020, 8, 1, 11, 0, 0, 0, time.UTC),
				}
				svc.taskMu.Unlock()
			},
			run: func(t *testing.T, svc *Service) {
				svc.scheduleWikiTimer(key, time.Hour, pendingWikiRun{
					session:   Session{Key: key, AgentName: "traex", ACPSessionID: "acp-new"},
					agent:     agent,
					scheduled: time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC),
				})
			},
			wantGen:       4,
			wantSessionID: "acp-new",
			wantScheduled: true,
		},
		{
			name: "restorePendingWiki 复用 timer 登记边界",
			setup: func(t *testing.T, svc *Service) {
				svc.taskMu.Lock()
				svc.wikiGenerations[normalizedKey] = 8
				svc.taskMu.Unlock()
			},
			run: func(t *testing.T, svc *Service) {
				svc.restorePendingWiki(pendingWikiRun{
					session:   Session{Key: key, AgentName: "traex", ACPSessionID: "acp-restored", WikiIntervalSec: 60},
					agent:     agent,
					scheduled: time.Now().Add(-time.Minute),
				})
			},
			wantGen:       9,
			wantSessionID: "acp-restored",
			wantScheduled: true,
		},
		{
			name: "restorePendingWiki 缺少 acp session 时不登记 timer",
			setup: func(t *testing.T, svc *Service) {
				svc.taskMu.Lock()
				svc.wikiGenerations[normalizedKey] = 12
				svc.taskMu.Unlock()
			},
			run: func(t *testing.T, svc *Service) {
				svc.restorePendingWiki(pendingWikiRun{
					session:   Session{Key: key, AgentName: "traex"},
					agent:     agent,
					scheduled: time.Now(),
				})
			},
			wantGen:       12,
			wantSessionID: "",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
			tt.setup(t, svc)

			tt.run(t, svc)

			svc.taskMu.Lock()
			pending := svc.wikiTimers[normalizedKey]
			gen := svc.wikiGenerations[normalizedKey]
			svc.taskMu.Unlock()
			if gen != tt.wantGen {
				t.Fatalf("wiki generation = %d, want %d", gen, tt.wantGen)
			}
			if tt.wantSessionID == "" {
				if pending != nil {
					t.Fatalf("pending wiki = %+v, want nil", pending)
				}
				return
			}
			if pending == nil {
				t.Fatal("pending wiki = nil, want scheduled timer")
			}
			t.Cleanup(func() {
				if pending.timer != nil {
					pending.timer.Stop()
				}
			})
			if pending.session.Key != normalizedKey {
				t.Fatalf("pending session key = %+v, want %+v", pending.session.Key, normalizedKey)
			}
			if pending.session.ACPSessionID != tt.wantSessionID {
				t.Fatalf("pending acp session = %q, want %q", pending.session.ACPSessionID, tt.wantSessionID)
			}
			if pending.generation != tt.wantGen {
				t.Fatalf("pending generation = %d, want %d", pending.generation, tt.wantGen)
			}
			if tt.wantScheduled && pending.scheduled.IsZero() {
				t.Fatal("pending scheduled is zero, want original schedule time")
			}
		})
	}
}

func TestBeginWikiTimerRunSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_other"})
	cases := []struct {
		name         string
		generation   int64
		busy         bool
		wantState    wikiTimerRunState
		wantHasKey   bool
		wantOther    bool
		wantKeyGen   int64
		wantOtherGen int64
	}{
		{
			name:         "旧代际回调不清理当前 timer",
			generation:   9,
			wantState:    wikiTimerRunStale,
			wantHasKey:   true,
			wantOther:    true,
			wantKeyGen:   10,
			wantOtherGen: 20,
		},
		{
			name:         "当前代际空闲时取走 timer 并允许执行",
			generation:   10,
			wantState:    wikiTimerRunReady,
			wantHasKey:   false,
			wantOther:    true,
			wantKeyGen:   10,
			wantOtherGen: 20,
		},
		{
			name:         "当前代际忙碌时取走 timer 并推进代际等待重排",
			generation:   10,
			busy:         true,
			wantState:    wikiTimerRunBusy,
			wantHasKey:   false,
			wantOther:    true,
			wantKeyGen:   11,
			wantOtherGen: 20,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
			timer := time.NewTimer(time.Hour)
			otherTimer := time.NewTimer(time.Hour)
			t.Cleanup(func() {
				timer.Stop()
				otherTimer.Stop()
			})
			svc.taskMu.Lock()
			svc.wikiGenerations[normalizedKey] = 10
			svc.wikiGenerations[otherKey] = 20
			svc.wikiTimers[normalizedKey] = &pendingWikiRun{
				timer:      timer,
				generation: 10,
				session:    Session{Key: normalizedKey, AgentName: "traex", ACPSessionID: "acp-a"},
				agent:      config.AgentConfig{Command: "traex"},
			}
			svc.wikiTimers[otherKey] = &pendingWikiRun{
				timer:      otherTimer,
				generation: 20,
				session:    Session{Key: otherKey, AgentName: "traex", ACPSessionID: "acp-b"},
				agent:      config.AgentConfig{Command: "traex"},
			}
			if tt.busy {
				svc.tasks[normalizedKey] = &runningTask{kind: taskKindUser}
			}
			svc.taskMu.Unlock()

			state := svc.beginWikiTimerRun(key, tt.generation, "")

			svc.taskMu.Lock()
			_, hasKey := svc.wikiTimers[normalizedKey]
			_, hasOther := svc.wikiTimers[otherKey]
			keyGen := svc.wikiGenerations[normalizedKey]
			otherGen := svc.wikiGenerations[otherKey]
			svc.taskMu.Unlock()
			if state != tt.wantState {
				t.Fatalf("state = %v, want %v", state, tt.wantState)
			}
			if hasKey != tt.wantHasKey {
				t.Fatalf("wiki timer exists = %v, want %v", hasKey, tt.wantHasKey)
			}
			if hasOther != tt.wantOther {
				t.Fatalf("other wiki timer exists = %v, want %v", hasOther, tt.wantOther)
			}
			if keyGen != tt.wantKeyGen {
				t.Fatalf("wiki generation = %d, want %d", keyGen, tt.wantKeyGen)
			}
			if otherGen != tt.wantOtherGen {
				t.Fatalf("other wiki generation = %d, want %d", otherGen, tt.wantOtherGen)
			}
		})
	}
}

func TestWikiStatusMarkersSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_other"})
	oldStarted := time.Date(2020, 8, 1, 11, 0, 0, 0, time.UTC)
	oldEnded := oldStarted.Add(time.Minute)
	newServiceWithStatuses := func(t *testing.T) *Service {
		t.Helper()
		svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
		svc.taskMu.Lock()
		svc.wikiStatuses[normalizedKey] = wikiRunStatus{
			running:     false,
			lastStarted: oldStarted,
			lastEnded:   oldEnded,
			lastSuccess: true,
			lastError:   "旧错误",
		}
		svc.wikiStatuses[otherKey] = wikiRunStatus{
			running:     true,
			lastStarted: oldStarted.Add(-time.Hour),
			lastEnded:   oldEnded.Add(-time.Hour),
			lastSuccess: false,
			lastError:   "其他 session 错误",
		}
		svc.taskMu.Unlock()
		return svc
	}
	cases := []struct {
		name            string
		run             func(svc *Service)
		wantRunning     bool
		wantSuccess     bool
		wantError       string
		wantEndedZero   bool
		wantStartedMove bool
	}{
		{
			name: "markWikiStarted 规范化 key 后进入 running 并清理上次结束状态",
			run: func(svc *Service) {
				svc.markWikiStarted(key)
			},
			wantRunning:     true,
			wantSuccess:     false,
			wantError:       "",
			wantEndedZero:   true,
			wantStartedMove: true,
		},
		{
			name: "markWikiFinished nil error 视为成功",
			run: func(svc *Service) {
				svc.markWikiFinished(key, Session{ACPSessionID: "acp-a"}, acp.PromptResult{Text: "changed: yes\nsummary: ok"}, nil)
			},
			wantRunning:   false,
			wantSuccess:   true,
			wantError:     "",
			wantEndedZero: false,
		},
		{
			name: "markWikiFinished context canceled 视为成功",
			run: func(svc *Service) {
				svc.markWikiFinished(key, Session{ACPSessionID: "acp-a"}, acp.PromptResult{}, context.Canceled)
			},
			wantRunning:   false,
			wantSuccess:   true,
			wantError:     "",
			wantEndedZero: false,
		},
		{
			name: "markWikiFinished 普通错误记录失败原因",
			run: func(svc *Service) {
				svc.markWikiFinished(key, Session{ACPSessionID: "acp-a"}, acp.PromptResult{}, errors.New("wiki failed"))
			},
			wantRunning:   false,
			wantSuccess:   false,
			wantError:     "wiki failed",
			wantEndedZero: false,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newServiceWithStatuses(t)

			tt.run(svc)

			svc.taskMu.Lock()
			status := svc.wikiStatuses[normalizedKey]
			otherStatus := svc.wikiStatuses[otherKey]
			svc.taskMu.Unlock()
			if status.running != tt.wantRunning {
				t.Fatalf("running = %v, want %v", status.running, tt.wantRunning)
			}
			if status.lastSuccess != tt.wantSuccess {
				t.Fatalf("lastSuccess = %v, want %v", status.lastSuccess, tt.wantSuccess)
			}
			if status.lastError != tt.wantError {
				t.Fatalf("lastError = %q, want %q", status.lastError, tt.wantError)
			}
			if status.lastEnded.IsZero() != tt.wantEndedZero {
				t.Fatalf("lastEnded zero = %v, want %v", status.lastEnded.IsZero(), tt.wantEndedZero)
			}
			if tt.wantStartedMove && !status.lastStarted.After(oldStarted) {
				t.Fatalf("lastStarted = %s, want after %s", status.lastStarted, oldStarted)
			}
			if !tt.wantStartedMove && !status.lastStarted.Equal(oldStarted) {
				t.Fatalf("lastStarted = %s, want unchanged %s", status.lastStarted, oldStarted)
			}
			if otherStatus.lastError != "其他 session 错误" || !otherStatus.running {
				t.Fatalf("other status = %+v, want unchanged", otherStatus)
			}
		})
	}
}

func TestWikiTaskLifecycleSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	normalizedKey := normalizeSessionKey(key)
	runtime := wikiRuntimeKey(normalizedKey, 1, "acp-a")
	otherRuntime := wikiRuntimeKey(normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_other"}), 1, "acp-b")
	cases := []struct {
		name           string
		replaceTask    bool
		finishRuntime  runtimeKey
		wantFinished   bool
		wantTargetTask string
		wantOtherTask  bool
	}{
		{
			name:           "匹配 runtime 和 task 时删除后台 wiki task",
			finishRuntime:  runtime,
			wantFinished:   true,
			wantTargetTask: "",
			wantOtherTask:  true,
		},
		{
			name:           "同 runtime 重复登记后台 wiki 被拒绝且旧 task 仍可 finish",
			replaceTask:    true,
			finishRuntime:  runtime,
			wantFinished:   true,
			wantTargetTask: "",
			wantOtherTask:  true,
		},
		{
			name:           "其他 runtime finish 不影响目标 task",
			finishRuntime:  otherRuntime,
			wantFinished:   false,
			wantTargetTask: "acp-a",
			wantOtherTask:  true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
			task := &runningTask{
				kind:    taskKindWiki,
				runtime: runtime,
				session: Session{Key: normalizedKey, AgentName: "traex", ACPSessionID: "acp-a"},
				agent:   config.AgentConfig{Command: "traex"},
			}
			otherTask := &runningTask{
				kind:    taskKindWiki,
				runtime: otherRuntime,
				session: Session{Key: otherRuntime.SessionKey, AgentName: "traex", ACPSessionID: "acp-b"},
				agent:   config.AgentConfig{Command: "traex"},
			}
			if !svc.beginWikiTask(runtime, task) {
				t.Fatal("beginWikiTask(runtime) = false, want true")
			}
			if !svc.beginWikiTask(otherRuntime, otherTask) {
				t.Fatal("beginWikiTask(otherRuntime) = false, want true")
			}
			if tt.replaceTask {
				newTask := &runningTask{
					kind:    taskKindWiki,
					runtime: runtime,
					session: Session{Key: normalizedKey, AgentName: "traex", ACPSessionID: "acp-new"},
					agent:   config.AgentConfig{Command: "traex"},
				}
				if svc.beginWikiTask(runtime, newTask) {
					t.Fatal("beginWikiTask(duplicate runtime) = true, want false")
				}
			}

			finished := svc.finishWikiTask(tt.finishRuntime, task)

			svc.taskMu.Lock()
			targetTask := svc.wikiTasks[runtime]
			otherRemaining := svc.wikiTasks[otherRuntime]
			svc.taskMu.Unlock()
			if finished != tt.wantFinished {
				t.Fatalf("finishWikiTask() = %v, want %v", finished, tt.wantFinished)
			}
			if tt.wantTargetTask == "" {
				if targetTask != nil {
					t.Fatalf("target task = %+v, want nil", targetTask)
				}
			} else if targetTask == nil || targetTask.session.ACPSessionID != tt.wantTargetTask {
				t.Fatalf("target task = %+v, want acp session %q", targetTask, tt.wantTargetTask)
			}
			if (otherRemaining != nil) != tt.wantOtherTask {
				t.Fatalf("other task = %+v, want exists=%v", otherRemaining, tt.wantOtherTask)
			}
		})
	}
}

func TestCancelWikiTasksSessionBoundaries(t *testing.T) {
	agent := config.AgentConfig{Command: "traex"}
	cases := []struct {
		name                 string
		cancelKey            SessionKey
		includeSecondWiki    bool
		wantCanceledRuntimes []runtimeKey
		wantRemainingRuntime runtimeKey
	}{
		{
			name: "取消同 session 后台 wiki",
			cancelKey: SessionKey{
				BotID:  "bot-a",
				Source: "im",
				ChatID: "chat-a",
				MainID: "chat-a",
			},
			wantCanceledRuntimes: []runtimeKey{
				{
					SessionKey: SessionKey{BotID: "bot-a", Source: "im", ChatID: "chat-a", MainID: "chat-a"},
					Scope:      runtimeScopeWiki,
					RunID:      "1:acp-a",
				},
			},
			wantRemainingRuntime: runtimeKey{
				SessionKey: SessionKey{BotID: "bot-a", Source: "im", ChatID: "chat-b", MainID: "chat-b"},
				Scope:      runtimeScopeWiki,
				RunID:      "1:acp-b",
			},
		},
		{
			name: "规范化 key 后取消同 session 后台 wiki",
			cancelKey: SessionKey{
				BotID:  "bot-a",
				ChatID: "chat-a",
			},
			wantCanceledRuntimes: []runtimeKey{
				{
					SessionKey: SessionKey{BotID: "bot-a", Source: "im", ChatID: "chat-a", MainID: "chat-a"},
					Scope:      runtimeScopeWiki,
					RunID:      "1:acp-a",
				},
			},
			wantRemainingRuntime: runtimeKey{
				SessionKey: SessionKey{BotID: "bot-a", Source: "im", ChatID: "chat-b", MainID: "chat-b"},
				Scope:      runtimeScopeWiki,
				RunID:      "1:acp-b",
			},
		},
		{
			name: "同 session 下第二个后台 wiki 不登记",
			cancelKey: SessionKey{
				BotID:  "bot-a",
				ChatID: "chat-a",
			},
			includeSecondWiki: true,
			wantCanceledRuntimes: []runtimeKey{
				{
					SessionKey: SessionKey{BotID: "bot-a", Source: "im", ChatID: "chat-a", MainID: "chat-a"},
					Scope:      runtimeScopeWiki,
					RunID:      "1:acp-a",
				},
			},
			wantRemainingRuntime: runtimeKey{
				SessionKey: SessionKey{BotID: "bot-a", Source: "im", ChatID: "chat-b", MainID: "chat-b"},
				Scope:      runtimeScopeWiki,
				RunID:      "1:acp-b",
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			rt := &fakeRuntime{}
			svc.setRuntime(rt)
			sessionA := Session{
				Key:          normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "chat-a"}),
				AgentName:    "traex",
				ACPSessionID: "acp-a",
			}
			sessionB := Session{
				Key:          normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "chat-b"}),
				AgentName:    "traex",
				ACPSessionID: "acp-b",
			}
			sessionA2 := Session{
				Key:          sessionA.Key,
				AgentName:    "traex",
				ACPSessionID: "acp-a2",
			}
			runtimeA := wikiRuntimeKey(sessionA.Key, 1, sessionA.ACPSessionID)
			runtimeA2 := wikiRuntimeKey(sessionA2.Key, 2, sessionA2.ACPSessionID)
			runtimeB := wikiRuntimeKey(sessionB.Key, 1, sessionB.ACPSessionID)
			ctxA, finishA, okA := svc.startWikiTask(context.Background(), sessionA, agent, runtimeA)
			if !okA {
				t.Fatal("startWikiTask(session A) = false, want true")
			}
			var ctxA2 context.Context
			var finishA2 func()
			var okA2 bool
			if tt.includeSecondWiki {
				ctxA2, finishA2, okA2 = svc.startWikiTask(context.Background(), sessionA2, agent, runtimeA2)
				if okA2 {
					t.Fatal("startWikiTask(second session A) = true, want false")
				}
			}
			ctxB, finishB, okB := svc.startWikiTask(context.Background(), sessionB, agent, runtimeB)
			if !okB {
				t.Fatal("startWikiTask(session B) = false, want true")
			}
			t.Cleanup(finishA)
			if finishA2 != nil {
				t.Cleanup(finishA2)
			}
			t.Cleanup(finishB)

			svc.cancelWikiTasks(context.Background(), tt.cancelKey)
			select {
			case <-ctxA.Done():
				if !slices.Contains(tt.wantCanceledRuntimes, runtimeA) {
					t.Fatal("session A wiki task was cancelled unexpectedly")
				}
			default:
				if slices.Contains(tt.wantCanceledRuntimes, runtimeA) {
					t.Fatal("session A wiki task was not cancelled")
				}
			}
			if ctxA2 != nil && okA2 {
				select {
				case <-ctxA2.Done():
					if !slices.Contains(tt.wantCanceledRuntimes, runtimeA2) {
						t.Fatal("session A second wiki task was cancelled unexpectedly")
					}
				default:
					if slices.Contains(tt.wantCanceledRuntimes, runtimeA2) {
						t.Fatal("session A second wiki task was not cancelled")
					}
				}
			}
			select {
			case <-ctxB.Done():
				if !slices.Contains(tt.wantCanceledRuntimes, runtimeB) {
					t.Fatal("session B wiki task was cancelled unexpectedly")
				}
			default:
				if slices.Contains(tt.wantCanceledRuntimes, runtimeB) {
					t.Fatal("session B wiki task was not cancelled")
				}
			}

			waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() == len(tt.wantCanceledRuntimes) })
			rt.mu.Lock()
			cancelCalls := append([]fakeCancelCall(nil), rt.cancelCalls...)
			rt.mu.Unlock()
			canceledRuntimeSet := make(map[runtimeKey]bool)
			for _, call := range cancelCalls {
				canceledRuntimeSet[call.Runtime] = true
			}
			for _, want := range tt.wantCanceledRuntimes {
				if !canceledRuntimeSet[want] {
					t.Fatalf("cancel runtimes = %+v, want include %+v", cancelCalls, want)
				}
			}
			svc.taskMu.Lock()
			hasCanceled := false
			for _, runtime := range tt.wantCanceledRuntimes {
				if _, ok := svc.wikiTasks[runtime]; ok {
					hasCanceled = true
					break
				}
			}
			_, hasRemaining := svc.wikiTasks[tt.wantRemainingRuntime]
			svc.taskMu.Unlock()
			if hasCanceled {
				t.Fatalf("wikiTasks still contains canceled runtime from %+v", tt.wantCanceledRuntimes)
			}
			if !hasRemaining {
				t.Fatalf("wikiTasks missing remaining runtime %+v", tt.wantRemainingRuntime)
			}
		})
	}
}
