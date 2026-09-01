package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestWikiStateStorePersistsAtomically(t *testing.T) {
	workspace := t.TempDir()
	store := newWikiStateStore(workspace)
	wantTime := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	if err := store.update(func(state *wikiState) {
		state.Sources["acp-source"] = wikiSourceState{AgentName: "traex", CommittedSeq: 9, LastSuccessAt: wantTime}
	}); err != nil {
		t.Fatalf("update() error = %v", err)
	}

	reloaded := newWikiStateStore(workspace)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got := reloaded.snapshot().Sources["acp-source"]
	if got.CommittedSeq != 9 || !got.LastSuccessAt.Equal(wantTime) {
		t.Fatalf("source = %+v, want committed seq and timestamp", got)
	}
	info, err := os.Stat(filepath.Join(workspace, ".local", "wiki", "state.json"))
	if err != nil {
		t.Fatalf("Stat(state.json) error = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}
}

func TestReadWikiTraceRangeStopsAtLastCompleteTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	records := []traceRecord{
		{Seq: 1, Type: "user", MessageID: "m1", Content: "one"},
		{Seq: 2, Type: "assistant", MessageID: "m1", IsFinal: true, Content: "done"},
		{Seq: 3, Type: "turn_result", MessageID: "m1"},
		{Seq: 4, Type: "user", MessageID: "m2", Content: "half"},
	}
	var data []byte
	for _, record := range records {
		line, _ := json.Marshal(record)
		data = append(data, append(line, '\n')...)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	rangeInfo, err := readWikiTraceRange(path, 0, 4)
	if err != nil {
		t.Fatalf("readWikiTraceRange() error = %v", err)
	}
	if rangeInfo.ToSeq != 3 {
		t.Fatalf("to seq = %d, want 3", rangeInfo.ToSeq)
	}
}

func TestTraceSequenceContinuesAcrossStoreRestart(t *testing.T) {
	workspace := t.TempDir()
	session := Session{Key: normalizeSessionKey(imSessionKey("bot-a", "oc", "")), ACPSessionID: "acp-source"}
	first := newTraceStore(workspace, config.TraceConfig{Enabled: true})
	seq1, err := first.AppendSeq(session, traceRecord{Type: "user"})
	if err != nil {
		t.Fatal(err)
	}
	second := newTraceStore(workspace, config.TraceConfig{Enabled: true})
	seq2, err := second.AppendSeq(session, traceRecord{Type: "turn_result"})
	if err != nil {
		t.Fatal(err)
	}
	if seq1 != 1 || seq2 != 2 {
		t.Fatalf("seq = %d, %d; want 1, 2", seq1, seq2)
	}
}

func TestReadWikiTraceRangeReportsLostCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	line, _ := json.Marshal(traceRecord{Seq: 5, Type: "turn_result", MessageID: "m2"})
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readWikiTraceRange(path, 3, 5); err == nil || !strings.Contains(err.Error(), "committed_seq=3") {
		t.Fatalf("error = %v, want cursor lost error", err)
	}
}

func TestWikiStateStoreBacksUpCorruptFile(t *testing.T) {
	workspace := t.TempDir()
	store := newWikiStateStore(workspace)
	if err := os.MkdirAll(filepath.Dir(store.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	matches, err := filepath.Glob(store.path + ".corrupt-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("corrupt backups = %v, err = %v", matches, err)
	}
}

func TestWikiCoordinatorUsesCompanionAndCommitsFrozenCursor(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Enabled: true, RetentionDays: 7}
	cfg.Bots[0].WikiTrace = config.WikiTraceConfig{Enabled: true, ChatID: "oc_trace"}
	store := NewSessionStore(filepath.Join(workspace, ".local", "sessions.json"))
	svc := newTestService(cfg, store)
	runtime := &fakeRuntime{newSessionID: "acp-wiki-companion", promptReply: "NoReply"}
	svc.setRuntime(runtime)
	var traceInitialProcess string
	card := &fakeStreamCard{message: feishu.SentMessage{MessageID: "om_wiki_trace", ChatID: "oc_trace"}}
	svc.scheduleStreams["bot-a"] = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		traceInitialProcess = options.InitialProcess
		return card, nil
	}
	session := Session{
		Key:             normalizeSessionKey(imSessionKey("bot-a", "oc-chat", "")),
		AgentName:       "traex",
		ACPSessionID:    "acp-source",
		Workspace:       workspace,
		WikiIntervalSec: 60,
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(source session) error = %v", err)
	}
	recorder := svc.newTraceRecorderForPrompt(session, feishu.Message{MessageID: "om-1"}, "请记住方案", true)
	terminalSeq := recorder.Complete(acp.PromptResult{Text: "好的"}, nil)
	coordinator := svc.wikiCoordinator("bot-a")
	coordinator.cancelSource(session.ACPSessionID, false)
	coordinator.enqueue(session.ACPSessionID)
	waitForCondition(t, time.Second, func() bool {
		return coordinator.state.snapshot().Sources[session.ACPSessionID].CommittedSeq == terminalSeq
	})

	if runtime.wikiRuntimeCallCount() != 1 {
		t.Fatalf("wiki runtime calls = %d, want 1", runtime.wikiRuntimeCallCount())
	}
	call := runtime.wikiRuntimeCalls[0]
	if call.Runtime.Scope != runtimeScopeWikiCompanion || call.Session.ACPSessionID != "acp-wiki-companion" {
		t.Fatalf("companion call = %+v", call)
	}
	if !strings.Contains(call.Text, "seq: (0, 3]") || !strings.Contains(call.Text, "acp-source.jsonl") {
		t.Fatalf("companion prompt = %q", call.Text)
	}
	if !strings.Contains(traceInitialProcess, "sid: acp-wiki-companion") || strings.Contains(traceInitialProcess, "sid: acp-source") {
		t.Fatalf("trace initial process = %q, want companion sid and not source sid", traceInitialProcess)
	}
	if !strings.Contains(traceInitialProcess, "msg: wiki\\_acp-source\\_0\\_3") {
		t.Fatalf("trace initial process = %q, want source trace message id", traceInitialProcess)
	}
	got, binding, ok := store.SessionForMessage("bot-a", "oc_trace", "om_wiki_trace")
	if !ok {
		t.Fatalf("SessionForMessage(wiki trace card) ok=false binding=%+v", binding)
	}
	if got.Key != session.Key || got.ACPSessionID != session.ACPSessionID {
		t.Fatalf("bound session = %+v, want source session %+v", got, session)
	}
	if runtime.promptCallCount() != 0 {
		t.Fatalf("ordinary prompt calls = %d, want no original-session wiki prompt", runtime.promptCallCount())
	}
}

func TestWikiCoordinatorTraceUpdatesSIDAfterCompanionRecreate(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Enabled: true, RetentionDays: 7}
	cfg.Bots[0].WikiTrace = config.WikiTraceConfig{Enabled: true, ChatID: "oc_trace"}
	store := NewSessionStore(filepath.Join(workspace, ".local", "sessions.json"))
	svc := newTestService(cfg, store)
	runtime := &fakeRuntime{
		newSessionID: "acp-wiki-new",
		promptReply:  "NoReply",
		promptErrors: []error{errACPSessionUnavailable},
	}
	svc.setRuntime(runtime)
	var traceInitialProcess string
	card := &fakeStreamCard{message: feishu.SentMessage{MessageID: "om_wiki_trace", ChatID: "oc_trace"}}
	svc.scheduleStreams["bot-a"] = func(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
		traceInitialProcess = options.InitialProcess
		return card, nil
	}
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc-chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-source",
		Workspace:    workspace,
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(source session) error = %v", err)
	}
	recorder := svc.newTraceRecorderForPrompt(session, feishu.Message{MessageID: "om-1"}, "请记住方案", true)
	recorder.Complete(acp.PromptResult{Text: "好的"}, nil)
	coordinator := svc.wikiCoordinator("bot-a")
	t.Cleanup(coordinator.stop)
	if err := coordinator.state.update(func(state *wikiState) {
		state.Companions["traex"] = wikiCompanionState{AgentName: "traex", ACPSessionID: "acp-wiki-old"}
	}); err != nil {
		t.Fatalf("save old companion state error = %v", err)
	}
	coordinator.cancelSource(session.ACPSessionID, false)

	if err := coordinator.runSource(context.Background(), session.ACPSessionID); err != nil {
		t.Fatalf("runSource() error = %v", err)
	}

	if !strings.Contains(traceInitialProcess, "sid: acp-wiki-old") {
		t.Fatalf("trace initial process = %q, want old companion sid before recreate", traceInitialProcess)
	}
	process := strings.Join(card.processUpdatesSnapshot(), "\n")
	if !strings.Contains(process, "sid: acp-wiki-new") || strings.Contains(process, "sid: acp-source") {
		t.Fatalf("process updates = %q, want recreated companion sid and not source sid", process)
	}
	calls := runtime.wikiRuntimeCallsSnapshot()
	if len(calls) != 2 || calls[0].Session.ACPSessionID != "acp-wiki-old" || calls[1].Session.ACPSessionID != "acp-wiki-new" {
		t.Fatalf("wiki runtime calls = %+v, want old companion then recreated companion", calls)
	}
}

func TestWikiCoordinatorFirstTurnBuildsBaselineAfterExistingTrace(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Enabled: true, RetentionDays: 7}
	svc := newTestService(cfg, NewSessionStore(filepath.Join(workspace, ".local", "sessions.json")))
	session := Session{Key: normalizeSessionKey(imSessionKey("bot-a", "oc-chat", "")), AgentName: "traex", ACPSessionID: "acp-source", Workspace: workspace}
	trace := svc.traceStoreForSession(session)
	for _, record := range []traceRecord{{Type: "user"}, {Type: "assistant", IsFinal: true}, {Type: "turn_result"}} {
		if err := trace.Append(session, record); err != nil {
			t.Fatal(err)
		}
	}
	recorder := svc.newTraceRecorderForPrompt(session, feishu.Message{MessageID: "om-new"}, "new", true)
	recorder.Complete(acp.PromptResult{Text: "done"}, nil)
	source := svc.wikiCoordinator("bot-a").state.snapshot().Sources[session.ACPSessionID]
	if source.CommittedSeq != 3 || source.LastCompleteSeq != 6 {
		t.Fatalf("source = %+v, want baseline 3 and complete 6", source)
	}
	svc.wikiCoordinator("bot-a").stop()
}

func TestWikiCoordinatorSkipsTurnsWhileWikiDisabled(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Enabled: true, RetentionDays: 7}
	store := NewSessionStore(filepath.Join(workspace, ".local", "sessions.json"))
	svc := newTestService(cfg, store)
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc-chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-source",
		Workspace:    workspace,
	}
	if _, err := store.UpdateChat(ChatConfig{Key: ChatKey{BotID: "bot-a", ChatID: "oc-chat"}}, func(chat *ChatConfig) {
		chat.WikiDisabled = true
	}); err != nil {
		t.Fatal(err)
	}

	recorder := svc.newTraceRecorderForPrompt(session, feishu.Message{MessageID: "om-disabled"}, "disabled turn", true)
	terminalSeq := recorder.Complete(acp.PromptResult{Text: "done"}, nil)
	coordinator := svc.wikiCoordinator("bot-a")
	source := coordinator.state.snapshot().Sources[session.ACPSessionID]
	if source.LastCompleteSeq != terminalSeq || source.CommittedSeq != terminalSeq || !source.DueAt.IsZero() {
		t.Fatalf("source = %+v, want disabled turn committed without due timer", source)
	}
	if snapshot := coordinator.snapshotForSession(session.ACPSessionID); snapshot.Waiting || snapshot.Queued || snapshot.Running {
		t.Fatalf("snapshot = %+v, want no scheduled wiki while disabled", snapshot)
	}
	coordinator.stop()
}

func TestWikiCoordinatorEnableDropsPreviouslyUncommittedRange(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Enabled: true, RetentionDays: 7}
	svc := newTestService(cfg, NewSessionStore(filepath.Join(workspace, ".local", "sessions.json")))
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc-chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-source",
		Workspace:    workspace,
	}
	coordinator := svc.wikiCoordinator("bot-a")
	if err := coordinator.state.update(func(state *wikiState) {
		state.Sources[session.ACPSessionID] = wikiSourceState{
			SessionKey:      session.Key,
			AgentName:       session.AgentName,
			CommittedSeq:    3,
			LastCompleteSeq: 9,
			LastActivityAt:  time.Now(),
			DueAt:           time.Now().Add(-time.Minute),
		}
	}); err != nil {
		t.Fatal(err)
	}

	coordinator.enableSource(session)
	source := coordinator.state.snapshot().Sources[session.ACPSessionID]
	if source.CommittedSeq != 9 || source.LastCompleteSeq != 9 {
		t.Fatalf("source = %+v, want enable to advance committed seq to latest complete", source)
	}
	if snapshot := coordinator.snapshotForSession(session.ACPSessionID); snapshot.Waiting || snapshot.Queued || snapshot.Running {
		t.Fatalf("snapshot = %+v, want no catch-up scheduled on enable", snapshot)
	}
	coordinator.stop()
}

func TestWikiOffCommandCommitsPendingCoordinatorRange(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Enabled: true, RetentionDays: 7}
	store := NewSessionStore(filepath.Join(workspace, ".local", "sessions.json"))
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc-chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-source",
		Workspace:    workspace,
	}
	if err := store.Upsert(session); err != nil {
		t.Fatal(err)
	}
	svc := newTestService(cfg, store)
	coordinator := svc.wikiCoordinator("bot-a")
	if err := coordinator.state.update(func(state *wikiState) {
		state.Sources[session.ACPSessionID] = wikiSourceState{
			SessionKey:      session.Key,
			AgentName:       session.AgentName,
			CommittedSeq:    3,
			LastCompleteSeq: 9,
			LastActivityAt:  time.Now(),
			DueAt:           time.Now().Add(time.Hour),
		}
	}); err != nil {
		t.Fatal(err)
	}
	coordinator.scheduleTimer(session.ACPSessionID, time.Hour)

	got := svc.handleWikiCommand(context.Background(), "/wiki off", feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc-chat",
		MessageID: "om-off",
		Workspace: workspace,
	})
	if !strings.Contains(got, "已关闭") {
		t.Fatalf("handleWikiCommand() = %q", got)
	}
	source := coordinator.state.snapshot().Sources[session.ACPSessionID]
	if source.CommittedSeq != 9 || !source.DueAt.IsZero() {
		t.Fatalf("source = %+v, want committed seq advanced and due cleared", source)
	}
	if snapshot := coordinator.snapshotForSession(session.ACPSessionID); snapshot.Waiting || snapshot.Queued || snapshot.Running {
		t.Fatalf("snapshot = %+v, want no pending wiki work", snapshot)
	}
	chat, ok := store.GetChat(ChatKey{BotID: "bot-a", ChatID: "oc-chat"})
	if !ok || !chat.WikiDisabled {
		t.Fatalf("chat = %+v, ok = %v, want wiki disabled", chat, ok)
	}
	coordinator.stop()
}

func TestAutoCompactPromptDoesNotTriggerWikiCoordinator(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Enabled: true, RetentionDays: 7}
	svc := newTestService(cfg, NewSessionStore(filepath.Join(workspace, ".local", "sessions.json")))
	rt := &fakeRuntime{promptReply: "compact done"}
	svc.setRuntime(rt)
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc-chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-source",
		Workspace:    workspace,
	}

	run := svc.runUserPromptWithOptionsDetailed(context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc-chat",
		MessageID: "om-compact",
		Workspace: workspace,
	}, session, config.AgentConfig{}, "/compact", autoCompactTaskOptions())
	if run.err != nil {
		t.Fatalf("runUserPromptWithOptionsDetailed() error = %v", run.err)
	}
	path := filepath.Join(workspace, ".local", "traces", "acp-source.jsonl")
	if records := readTraceRecords(t, path); len(records) == 0 {
		t.Fatal("auto compact should still write trace records")
	}
	if _, ok := svc.wikiCoordinator("bot-a").state.snapshot().Sources[session.ACPSessionID]; ok {
		t.Fatal("auto compact should not create wiki source state")
	}
	svc.wikiCoordinator("bot-a").stop()
}

func TestPendingAtAutoDrainPromptDoesNotTriggerWikiCoordinator(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Enabled: true, RetentionDays: 7}
	svc := newTestService(cfg, NewSessionStore(filepath.Join(workspace, ".local", "sessions.json")))
	rt := &fakeRuntime{promptReply: "SILENT"}
	svc.setRuntime(rt)
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc-chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-source",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
	}

	result, err := svc.runAtAutoCompanionPrompt(context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc-chat",
		ChatType:  "group",
		MessageID: "om-auto",
		Workspace: workspace,
	}, session, config.AgentConfig{}, "请判断是否需要回复")
	if err != nil {
		t.Fatalf("runAtAutoCompanionPrompt() error = %v", err)
	}
	if result.Text != "SILENT" {
		t.Fatalf("result = %+v, want SILENT", result)
	}
	path := filepath.Join(workspace, ".local", "traces", "acp-session.jsonl")
	if records := readTraceRecords(t, path); len(records) == 0 {
		t.Fatal("at-auto companion prompt should still write trace records")
	}
	if _, ok := svc.wikiCoordinator("bot-a").state.snapshot().Sources[session.ACPSessionID]; ok {
		t.Fatal("at-auto companion prompt should not create wiki source state")
	}
	svc.wikiCoordinator("bot-a").stop()
}

func TestLoopRoundPromptDoesNotTriggerWikiCoordinator(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Enabled: true, RetentionDays: 7}
	svc := newTestService(cfg, NewSessionStore(filepath.Join(workspace, ".local", "sessions.json")))
	svc.setRuntime(&fakeRuntime{promptReply: "DONE"})
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc-chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-source",
		Workspace:    workspace,
	}

	run := svc.promptRuntimeWithProgressRawStatusPrefix(context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc-chat",
		MessageID: "om-loop",
		Workspace: workspace,
	}, session, config.AgentConfig{}, "loop round", "loop 1")
	if run.err != nil {
		t.Fatalf("promptRuntimeWithProgressRawStatusPrefix() error = %v", run.err)
	}
	if records := readTraceRecords(t, filepath.Join(workspace, ".local", "traces", "acp-source.jsonl")); len(records) == 0 {
		t.Fatal("loop round should still write trace records")
	}
	if _, ok := svc.wikiCoordinator("bot-a").state.snapshot().Sources[session.ACPSessionID]; ok {
		t.Fatal("loop round should not create wiki source state")
	}
	svc.wikiCoordinator("bot-a").stop()
}

func TestAtAutoDirectMessageTriggersWikiCoordinator(t *testing.T) {
	workspace := t.TempDir()
	markWorkspaceBootstrapped(t, workspace)
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Enabled: true, RetentionDays: 7}
	store := NewSessionStore(filepath.Join(workspace, ".local", "sessions.json"))
	session := Session{
		Key:               normalizeSessionKey(imSessionKey("bot-a", "oc-chat", "")),
		AgentName:         "traex",
		ACPSessionID:      "acp-source",
		Cwd:               t.TempDir(),
		Workspace:         workspace,
		WorkspacePrompted: true,
	}
	if err := store.Upsert(session); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertChat(ChatConfig{
		Key:             ChatKey{BotID: "bot-a", ChatID: "oc-chat"},
		MentionOptional: true,
		AtMode:          atModeAuto,
	}); err != nil {
		t.Fatal(err)
	}
	svc := newTestService(cfg, store)
	svc.setRuntime(&fakeRuntime{promptResults: []acp.PromptResult{{Text: "RESPOND"}, {Text: "需要处理"}}})
	t.Cleanup(func() { svc.wikiCoordinator("bot-a").stop() })

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc-chat",
		ChatType:  "group",
		MessageID: "om-direct-auto",
		Text:      "这个需要处理吗",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if reply != "需要处理" {
		t.Fatalf("reply = %q, want at-auto reply", reply)
	}
	source := svc.wikiCoordinator("bot-a").state.snapshot().Sources[session.ACPSessionID]
	if source.LastCompleteSeq == 0 || source.DueAt.IsZero() {
		t.Fatalf("source = %+v, want direct at-auto prompt scheduled for wiki", source)
	}
}

func TestAtAutoCompanionSilentDoesNotTriggerWikiCoordinator(t *testing.T) {
	workspace := t.TempDir()
	markWorkspaceBootstrapped(t, workspace)
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Enabled: true, RetentionDays: 7}
	store := NewSessionStore(filepath.Join(workspace, ".local", "sessions.json"))
	session := Session{
		Key:               normalizeSessionKey(imSessionKey("bot-a", "oc-chat", "")),
		AgentName:         "traex",
		ACPSessionID:      "acp-source",
		Cwd:               t.TempDir(),
		Workspace:         workspace,
		WorkspacePrompted: true,
	}
	if err := store.Upsert(session); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertChat(ChatConfig{
		Key:             ChatKey{BotID: "bot-a", ChatID: "oc-chat"},
		MentionOptional: true,
		AtMode:          atModeAuto,
	}); err != nil {
		t.Fatal(err)
	}
	svc := newTestService(cfg, store)
	svc.setRuntime(&fakeRuntime{promptReply: "SILENT"})
	t.Cleanup(func() { svc.wikiCoordinator("bot-a").stop() })

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc-chat",
		ChatType:  "group",
		MessageID: "om-direct-auto-silent",
		Text:      "路过闲聊",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want silent", reply)
	}
	if _, ok := svc.wikiCoordinator("bot-a").state.snapshot().Sources[session.ACPSessionID]; ok {
		t.Fatal("silent at-auto companion decision should not create source wiki state")
	}
}

func TestACPCommandPromptDoesNotTriggerWikiCoordinator(t *testing.T) {
	workspace := t.TempDir()
	markWorkspaceBootstrapped(t, workspace)
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Enabled: true, RetentionDays: 7}
	store := NewSessionStore(filepath.Join(workspace, ".local", "sessions.json"))
	session := Session{
		Key:               normalizeSessionKey(imSessionKey("bot-a", "oc-chat", "")),
		AgentName:         "traex",
		ACPSessionID:      "acp-source",
		Cwd:               t.TempDir(),
		Workspace:         workspace,
		WorkspacePrompted: true,
		AvailableCommands: []acp.AvailableCommand{{Name: "review"}},
	}
	if err := store.Upsert(session); err != nil {
		t.Fatal(err)
	}
	svc := newTestService(cfg, store)
	svc.setRuntime(&fakeRuntime{promptReply: "review done"})
	t.Cleanup(func() { svc.wikiCoordinator("bot-a").stop() })

	reply := svc.forwardACPCommand(context.Background(), "/review 快速检查", feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc-chat",
		ChatType:  "group",
		MessageID: "om-command",
		Workspace: workspace,
	})
	if reply != "review done" {
		t.Fatalf("reply = %q, want review output", reply)
	}
	if records := readTraceRecords(t, filepath.Join(workspace, ".local", "traces", "acp-source.jsonl")); len(records) == 0 {
		t.Fatal("ACP command should still write trace records")
	}
	if _, ok := svc.wikiCoordinator("bot-a").state.snapshot().Sources[session.ACPSessionID]; ok {
		t.Fatal("ACP command prompt should not create wiki source state")
	}
}

func TestFailedPromptDoesNotTriggerWikiCoordinator(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Enabled: true, RetentionDays: 7}
	svc := newTestService(cfg, NewSessionStore(filepath.Join(workspace, ".local", "sessions.json")))
	svc.setRuntime(&fakeRuntime{promptErrors: []error{errors.New("boom")}})
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc-chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-source",
		Workspace:    workspace,
	}

	run := svc.runUserPromptWithOptionsDetailed(context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc-chat",
		MessageID: "om-error",
		Workspace: workspace,
	}, session, config.AgentConfig{}, "will fail", runningTaskOptions{silentPrompt: true, triggerWiki: true})
	if run.err == nil {
		t.Fatal("runUserPromptWithOptionsDetailed() error = nil, want failure")
	}
	if records := readTraceRecords(t, filepath.Join(workspace, ".local", "traces", "acp-source.jsonl")); len(records) == 0 {
		t.Fatal("failed prompt should still write trace records")
	}
	if _, ok := svc.wikiCoordinator("bot-a").state.snapshot().Sources[session.ACPSessionID]; ok {
		t.Fatal("failed prompt should not create wiki source state")
	}
	svc.wikiCoordinator("bot-a").stop()
}

func TestWikiCoordinatorFailureDoesNotAdvanceCursor(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	cfg.Bots[0].Trace = config.TraceConfig{Enabled: true, RetentionDays: 7}
	svc := newTestService(cfg, NewSessionStore(filepath.Join(workspace, ".local", "sessions.json")))
	svc.setRuntime(&fakeRuntime{newSessionID: "acp-wiki", promptErrors: []error{context.DeadlineExceeded}})
	session := Session{Key: normalizeSessionKey(imSessionKey("bot-a", "oc-chat", "")), AgentName: "traex", ACPSessionID: "acp-source", Workspace: workspace}
	recorder := svc.newTraceRecorderForPrompt(session, feishu.Message{MessageID: "om-1"}, "hello", true)
	recorder.Complete(acp.PromptResult{Text: "world"}, nil)
	coordinator := svc.wikiCoordinator("bot-a")
	coordinator.cancelSource(session.ACPSessionID, false)
	coordinator.enqueue(session.ACPSessionID)
	waitForCondition(t, time.Second, func() bool {
		return coordinator.state.snapshot().Sources[session.ACPSessionID].LastError != ""
	})
	if got := coordinator.state.snapshot().Sources[session.ACPSessionID].CommittedSeq; got != 0 {
		t.Fatalf("committed seq = %d, want 0 after failure", got)
	}
	coordinator.stop()
}
