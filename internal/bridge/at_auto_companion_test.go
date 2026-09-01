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

func TestAtAutoFlowQueuesMessagesAcrossDecisionAndMainReply(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
	key := normalizeSessionKey(imSessionKey("bot-a", "oc_group", ""))
	if !svc.beginAtAutoFlow(key) {
		t.Fatal("beginAtAutoFlow(first) = false, want true")
	}
	if svc.beginAtAutoFlow(key) {
		t.Fatal("beginAtAutoFlow(second) = true while first flow is active")
	}
	msg := feishu.Message{BotID: "bot-a", ChatID: "oc_group", ChatType: "group", SenderID: "ou_b", Text: "第二条消息"}
	if !svc.queueAtAutoMessageIfBusy(msg) {
		t.Fatal("queueAtAutoMessageIfBusy() = false during flow handoff window")
	}
	pending := svc.takePendingAtAutoForFlow(key, true)
	if len(pending) != 1 || pending[0].Text != "第二条消息" {
		t.Fatalf("pending = %+v, want queued second message", pending)
	}
	if svc.beginAtAutoFlow(key) {
		t.Fatal("flow ownership should remain claimed by the pending batch")
	}
	svc.finishAtAutoFlow(key)
	if !svc.beginAtAutoFlow(key) {
		t.Fatal("flow should be available after pending batch finishes")
	}
	svc.finishAtAutoFlow(key)
}

func TestAtAutoCompanionStateStoreIsSharedByWorkspace(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Disabled: true}
	svc := NewService(cfg, NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
	if svc.wikiCoordinator("bot-a") != nil {
		t.Fatal("wiki coordinator should be disabled with trace")
	}
	first := svc.atAutoCompanionStateStore("bot-a", workspace)
	second := svc.atAutoCompanionStateStore("bot-a", filepath.Join(workspace, "."))
	if first == nil || first != second {
		t.Fatalf("state stores = %p and %p, want one shared store", first, second)
	}
	if err := first.update(func(state *wikiState) {
		state.AtAutoCompanions["first"] = wikiCompanionState{AgentName: "traex", ACPSessionID: "acp-1"}
	}); err != nil {
		t.Fatal(err)
	}
	if err := second.update(func(state *wikiState) {
		state.AtAutoCompanions["second"] = wikiCompanionState{AgentName: "traex", ACPSessionID: "acp-2"}
	}); err != nil {
		t.Fatal(err)
	}
	loaded := newWikiStateStore(workspace)
	if err := loaded.Load(); err != nil {
		t.Fatal(err)
	}
	state := loaded.snapshot()
	if len(state.AtAutoCompanions) != 2 || !strings.HasPrefix(state.AtAutoCompanions["first"].ACPSessionID, "acp-") || !strings.HasPrefix(state.AtAutoCompanions["second"].ACPSessionID, "acp-") {
		t.Fatalf("persisted companions = %+v, want both updates", state.AtAutoCompanions)
	}
}

func TestIsAtAutoDecisionPositiveRequiresRespondSentinel(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		want  bool
	}{
		{name: "exact respond", reply: "RESPOND", want: true},
		{name: "case and whitespace", reply: "\n respond \t", want: true},
		{name: "notice before respond", reply: "Context compacted.\nRESPOND", want: true},
		{name: "empty", reply: "", want: false},
		{name: "silent", reply: "SILENT", want: false},
		{name: "no reply", reply: "NoReply", want: false},
		{name: "explanation without sentinel", reply: "这条消息需要回复", want: false},
		{name: "word suffix is not sentinel", reply: "RESPONDER", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAtAutoDecisionPositive(tt.reply); got != tt.want {
				t.Fatalf("isAtAutoDecisionPositive(%q) = %v, want %v", tt.reply, got, tt.want)
			}
		})
	}
}

func TestRunAtAutoCompanionPromptCreatesAndReusesCompanionSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	workspace := t.TempDir()
	workDir := t.TempDir()
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	agent := mustConfigAgent(t, cfg, "traex")
	agent.DefaultCwd = workDir
	cfg.SetAgent("traex", agent)
	rt := &fakeRuntime{
		newSessionIDs: []string{"at-auto-session-1"},
		promptResults: []acp.PromptResult{
			{Text: "SILENT"},
			{Text: "RESPOND"},
		},
	}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)
	t.Cleanup(func() { svc.wikiCoordinator("bot-a").stop() })

	source := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc_group", "")),
		AgentName:    "traex",
		ACPSessionID: "main-session",
		Cwd:          workDir,
		Workspace:    workspace,
	}
	msg := feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_group",
		ChatType:  "group",
		MessageID: "om_auto",
		Workspace: workspace,
	}

	if _, err := svc.runAtAutoCompanionPrompt(context.Background(), msg, source, agent, "判断一下"); err != nil {
		t.Fatalf("runAtAutoCompanionPrompt(first) error = %v", err)
	}
	if _, err := svc.runAtAutoCompanionPrompt(context.Background(), msg, source, agent, "再判断一下"); err != nil {
		t.Fatalf("runAtAutoCompanionPrompt(second) error = %v", err)
	}

	rt.mu.Lock()
	newCalls := append([]fakeNewCall(nil), rt.newCalls...)
	rt.mu.Unlock()
	if len(newCalls) != 1 {
		t.Fatalf("newCalls = %+v, want one companion session creation", newCalls)
	}
	if newCalls[0].Runtime.Scope != runtimeScopeAtAuto || newCalls[0].Key.Source != runtimeScopeAtAuto {
		t.Fatalf("new call = %+v, want at-auto runtime and session key", newCalls[0])
	}
	autoCalls := rt.atAutoRuntimeCallsSnapshot()
	if len(autoCalls) != 2 {
		t.Fatalf("atAutoRuntimeCalls = %+v, want two companion prompts", autoCalls)
	}
	for _, call := range autoCalls {
		if call.Runtime.Scope != runtimeScopeAtAuto || call.Session.Key.Source != runtimeScopeAtAuto {
			t.Fatalf("companion call = %+v, want at-auto runtime and session", call)
		}
		if call.Session.ACPSessionID != "at-auto-session-1" {
			t.Fatalf("companion session = %+v, want persisted at-auto session id", call.Session)
		}
		if call.Session.ACPSessionID == source.ACPSessionID {
			t.Fatalf("companion call reused source ACP session: %+v", call.Session)
		}
	}
	stateKey := atAutoCompanionStateID(source.Key, source.AgentName)
	state := svc.wikiCoordinator("bot-a").state.snapshot().AtAutoCompanions[stateKey]
	if state.AgentName != "traex" || state.ACPSessionID != "at-auto-session-1" || state.CreatedAt.IsZero() || state.UpdatedAt.IsZero() {
		t.Fatalf("stored at-auto companion state = %+v, want persisted companion", state)
	}
	tracePath := filepath.Join(workspace, ".local", "traces", "at-auto-session-1.jsonl")
	if records := readTraceRecords(t, tracePath); len(records) == 0 {
		t.Fatalf("missing at-auto companion trace records at %s", tracePath)
	}
	if got := rt.promptCallCount(); got != 0 {
		t.Fatalf("prompt calls = %d, want no main session prompt from companion helper", got)
	}
}
