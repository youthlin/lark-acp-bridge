package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

type sessionForkFixture struct {
	workspace string
	workDir   string
	store     *SessionStore
	runtime   *fakeRuntime
	outbound  *fakeSentMessageClient
	service   *Service
	source    Session
}

func newSessionForkFixture(t *testing.T, sourceKey SessionKey) sessionForkFixture {
	t.Helper()
	workspace := t.TempDir()
	workDir := t.TempDir()
	cfg := config.Default()
	cfg.Bots = []config.BotConfig{{
		ID:           "bot-a",
		Workspace:    workspace,
		OwnerOpenIDs: []string{testOwnerOpenID},
		Trace:        config.TraceConfig{Enabled: true, RetentionDays: 7},
	}}
	store := NewSessionStore(workspaceLocalPath(workspace, "sessions.json"))
	source := Session{
		Key:            normalizeSessionKey(sourceKey),
		Title:          "原会话",
		ManualTitle:    true,
		AgentName:      "traex",
		ACPSessionID:   "acp-source",
		Cwd:            workDir,
		Workspace:      workspace,
		AutoCompact:    true,
		AutoCompactPct: 75,
	}
	if err := store.Upsert(source); err != nil {
		t.Fatalf("Upsert(source) error = %v", err)
	}
	runtime := &fakeRuntime{
		newSessionID: "acp-fork",
		promptReply:  "分支上下文已接续",
		promptUpdates: []acp.PromptUpdate{{Update: acp.SessionUpdate{
			SessionUpdate: "agent_message_chunk",
			Content:       &acp.ContentBlock{Type: "text", Text: "分支上下文已接续"},
		}}},
		permissionRequest: &acp.PermissionRequest{Options: []acp.PermissionOption{
			{OptionID: "allow", Kind: "allow_once"},
			{OptionID: "reject", Kind: "reject_once"},
		}},
	}
	outbound := newFakeSentMessageClient("om_fork_display")
	service := NewService(cfg, store)
	service.setRuntime(runtime)
	service.setOutbound("bot-a", outbound)
	appendForkCompletedTurn(t, service.traceStoreForSession(source), source, "om_done", "帮我实现 session fork", "已完成方案")
	return sessionForkFixture{workspace: workspace, workDir: workDir, store: store, runtime: runtime, outbound: outbound, service: service, source: source}
}

func appendForkCompletedTurn(t *testing.T, store *traceStore, session Session, messageID, userText, reply string) {
	t.Helper()
	for _, record := range []traceRecord{
		{Type: "user", MessageID: messageID, Content: userText},
		{Type: "assistant", MessageID: messageID, IsFinal: true, Content: reply},
		{Type: "turn_result", MessageID: messageID, StopReason: "end_turn"},
	} {
		if err := store.Append(session, record); err != nil {
			t.Fatalf("Append(%s) error = %v", record.Type, err)
		}
	}
}

func runtimeNewCallsSnapshot(runtime *fakeRuntime) []fakeNewCall {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return append([]fakeNewCall(nil), runtime.newCalls...)
}

func TestSessionForkCreatesPrivateChatAndKeepsSourceSession(t *testing.T) {
	fixture := newSessionForkFixture(t, imSessionKey("bot-a", "oc_source", ""))
	var createRequest feishu.CreateChatRequest
	var addRequest feishu.AddChatMembersRequest
	fixture.outbound.chatCreator = func(_ context.Context, request feishu.CreateChatRequest) (feishu.CreatedChat, error) {
		createRequest = request
		return feishu.CreatedChat{ChatID: "oc_fork", Name: request.Name, ChatType: "private", GroupMessageType: "chat"}, nil
	}
	fixture.outbound.chatMemberAdder = func(_ context.Context, request feishu.AddChatMembersRequest) (feishu.AddChatMembersResult, error) {
		addRequest = request
		return feishu.AddChatMembersResult{}, nil
	}
	var bootstrapMessage feishu.Message
	fixture.outbound.streamStarter = func(_ context.Context, msg feishu.Message, _ feishu.StreamCardOptions) (feishu.StreamCard, error) {
		bootstrapMessage = msg
		return &fakeStreamCard{message: feishu.SentMessage{MessageID: "om_bootstrap", ChatID: msg.ChatID}}, nil
	}

	command := feishu.Message{
		BotID:     "bot-a",
		BotOpenID: testBotOpenID,
		Workspace: fixture.workspace,
		ChatID:    "oc_source",
		ChatType:  "p2p",
		MessageID: "om_fork_command",
		SenderID:  testOwnerOpenID,
		Mentions:  []feishu.Mention{{ID: "ou_alice", Name: "Alice", Type: "user"}},
	}
	if reply := fixture.service.handleSessionForkCommand(context.Background(), "/session fork 新方向 @Alice", command); reply != "" {
		t.Fatalf("handleSessionForkCommand() = %q, want outbound-only reply", reply)
	}

	if createRequest.Name != "新方向" || createRequest.Mode != newChatModeGroup || createRequest.ChatType != "private" ||
		createRequest.OwnerOpenID != testOwnerOpenID || !createRequest.SetBotManager || !reflect.DeepEqual(createRequest.UserOpenIDs, []string{testOwnerOpenID}) {
		t.Fatalf("CreateChat request = %+v, want sender-owned private ordinary chat", createRequest)
	}
	if addRequest.ChatID != "oc_fork" || !reflect.DeepEqual(addRequest.UserOpenIDs, []string{"ou_alice"}) {
		t.Fatalf("AddChatMembers request = %+v, want explicit mention only", addRequest)
	}
	targetKey := imSessionKey("bot-a", "oc_fork", "")
	target, ok := fixture.store.Get(targetKey)
	if !ok {
		t.Fatal("target session not found")
	}
	if target.ACPSessionID != "acp-fork" || target.ForkOrigin == nil || target.ForkOrigin.SourceACPSessionID != fixture.source.ACPSessionID ||
		!target.AutoCompact || target.AutoCompactPct != fixture.source.AutoCompactPct {
		t.Fatalf("target session = %+v, want fork lineage and inherited auto compact", target)
	}
	currentSource, ok := fixture.store.Get(fixture.source.Key)
	if !ok || currentSource.ACPSessionID != fixture.source.ACPSessionID || currentSource.ForkOrigin != nil {
		t.Fatalf("source session = %+v, %v; want unchanged source", currentSource, ok)
	}
	if fixture.runtime.cancelCallCount() != 0 {
		t.Fatalf("cancel calls = %d, want source session untouched", fixture.runtime.cancelCallCount())
	}
	newCalls := runtimeNewCallsSnapshot(fixture.runtime)
	if len(newCalls) != 1 || newCalls[0].Key != targetKey || newCalls[0].Cwd != fixture.workDir {
		t.Fatalf("new session calls = %+v, want target-only session", newCalls)
	}
	promptCalls := fixture.runtime.promptCallsSnapshot()
	if len(promptCalls) != 1 || promptCalls[0].Session.Key != targetKey || !promptCalls[0].HasPermissionHandler ||
		!strings.Contains(promptCalls[0].Text, "本轮不要继续执行未完成任务") || !strings.Contains(promptCalls[0].Text, "source cutoff seq: 3") {
		t.Fatalf("bootstrap calls = %+v, want guarded target bootstrap", promptCalls)
	}
	if fixture.runtime.permissionOutcome.Outcome != "selected" || fixture.runtime.permissionOutcome.OptionID != "reject" {
		t.Fatalf("permission outcome = %+v, want automatic reject", fixture.runtime.permissionOutcome)
	}
	if bootstrapMessage.ChatID != "oc_fork" || bootstrapMessage.MessageID != "" || bootstrapMessage.ForceReplyInThread {
		t.Fatalf("bootstrap message = %+v, want top-level card in new chat", bootstrapMessage)
	}
	sent := fixture.outbound.sentSnapshot()
	if len(sent) != 2 || !strings.Contains(sent[0], "正在接续上下文") || sent[1] != "已创建新群聊并分叉当前会话。" {
		t.Fatalf("sent texts = %#v, want target display and source notice", sent)
	}
	messages := fixture.outbound.messagesSnapshot()
	if len(messages) != 3 || messages[1].MessageID != command.MessageID || !messages[1].ForceReplyInThread ||
		messages[2].MessageID != command.MessageID || !messages[2].ForceReplyInThread {
		t.Fatalf("outbound messages = %+v, want notice and share-chat as command thread replies", messages)
	}
	fixture.outbound.mu.Lock()
	sharedChatIDs := append([]string(nil), fixture.outbound.sharedChatIDs...)
	fixture.outbound.mu.Unlock()
	if !reflect.DeepEqual(sharedChatIDs, []string{"oc_fork"}) {
		t.Fatalf("shared chat IDs = %v, want target chat card", sharedChatIDs)
	}
	updates := fixture.outbound.textUpdatesSnapshot()
	if len(updates) != 1 || !strings.Contains(updates[0], "上下文已接续") {
		t.Fatalf("display updates = %#v, want ready status", updates)
	}
	operation, ok := fixture.service.forkStoreForWorkspace(fixture.workspace).GetByCommand(command.MessageID)
	if !ok || operation.State != forkStateReady || operation.TargetSession != "acp-fork" || !operation.OriginalNoticeSent || !operation.OriginalShareChatSent {
		t.Fatalf("operation = %+v, %v; want ready persisted operation", operation, ok)
	}
}

func TestSessionForkFallsBackToChatIDWhenShareChatFails(t *testing.T) {
	fixture := newSessionForkFixture(t, imSessionKey("bot-a", "oc_source", ""))
	fixture.outbound.chatCreator = func(_ context.Context, request feishu.CreateChatRequest) (feishu.CreatedChat, error) {
		return feishu.CreatedChat{ChatID: "oc_fork", Name: request.Name, ChatType: "private", GroupMessageType: "chat"}, nil
	}
	fixture.outbound.shareChatSender = func(context.Context, feishu.Message, string) (feishu.SentMessage, error) {
		return feishu.SentMessage{}, errors.New("share_chat failed")
	}
	command := feishu.Message{
		BotID: "bot-a", BotOpenID: testBotOpenID, Workspace: fixture.workspace, ChatID: "oc_source",
		ChatType: "p2p", MessageID: "om_fork_command", SenderID: testOwnerOpenID,
	}
	if reply := fixture.service.handleSessionForkCommand(context.Background(), "/session fork", command); reply != "" {
		t.Fatalf("handleSessionForkCommand() = %q, want outbound-only reply", reply)
	}
	sent := fixture.outbound.sentSnapshot()
	if len(sent) == 0 || sent[len(sent)-1] != "群名片发送失败，新群 chat_id：oc_fork" {
		t.Fatalf("sent texts = %#v, want chat_id fallback", sent)
	}
}

func TestSessionForkCreatesNewTopicInTopicGroup(t *testing.T) {
	fixture := newSessionForkFixture(t, imSessionKey("bot-a", "oc_topic", "omt_source"))
	fixture.outbound.chatCreator = func(_ context.Context, request feishu.CreateChatRequest) (feishu.CreatedChat, error) {
		t.Fatalf("CreateChat should not run for topic-group fork: %+v", request)
		return feishu.CreatedChat{}, nil
	}
	var bootstrapMessage feishu.Message
	fixture.outbound.streamStarter = func(_ context.Context, msg feishu.Message, _ feishu.StreamCardOptions) (feishu.StreamCard, error) {
		bootstrapMessage = msg
		return &fakeStreamCard{message: feishu.SentMessage{MessageID: "om_bootstrap", ChatID: msg.ChatID, ThreadID: msg.ThreadID}}, nil
	}
	command := feishu.Message{
		BotID:            "bot-a",
		BotOpenID:        testBotOpenID,
		Workspace:        fixture.workspace,
		ChatID:           "oc_topic",
		ChatType:         "topic_group",
		ChatMode:         "topic",
		GroupMessageType: "thread",
		MessageID:        "om_fork_command",
		ThreadID:         "omt_source",
		RootID:           "om_source_root",
		SenderID:         testOwnerOpenID,
	}
	if reply := fixture.service.handleSessionForkCommand(context.Background(), "/session fork 新话题", command); reply != "" {
		t.Fatalf("handleSessionForkCommand() = %q, want outbound-only reply", reply)
	}

	targetKey := imSessionKey("bot-a", "oc_topic", "om_fork_display")
	if target, ok := fixture.store.Get(targetKey); !ok || target.ACPSessionID != "acp-fork" {
		t.Fatalf("target session = %+v, %v; want new topic session", target, ok)
	}
	if bootstrapMessage.MessageID != "om_fork_display" || bootstrapMessage.ThreadID != "om_fork_display" || !bootstrapMessage.ForceReplyInThread {
		t.Fatalf("bootstrap message = %+v, want reply under new topic root", bootstrapMessage)
	}
	fixture.outbound.mu.Lock()
	sharedCount := len(fixture.outbound.sharedChatIDs)
	fixture.outbound.mu.Unlock()
	if sharedCount != 0 {
		t.Fatalf("share-chat count = %d, want none for topic fork", sharedCount)
	}
	sent := fixture.outbound.sentSnapshot()
	if len(sent) != 2 || sent[1] != "已创建分支话题「新话题」。" {
		t.Fatalf("sent texts = %#v, want topic display and source notice", sent)
	}
}

func TestSessionForkBusyRejectsAndForceUsesLastCompletedTurn(t *testing.T) {
	fixture := newSessionForkFixture(t, imSessionKey("bot-a", "oc_source", ""))
	fixture.outbound.chatCreator = func(_ context.Context, request feishu.CreateChatRequest) (feishu.CreatedChat, error) {
		return feishu.CreatedChat{ChatID: "oc_forced", Name: request.Name, ChatType: "private", GroupMessageType: "chat"}, nil
	}
	fixture.service.taskMu.Lock()
	fixture.service.tasks[fixture.source.Key] = &runningTask{kind: taskKindUser}
	fixture.service.taskMu.Unlock()
	trace := fixture.service.traceStoreForSession(fixture.source)
	if err := trace.Append(fixture.source, traceRecord{Type: "user", MessageID: "om_running", Content: "这条消息还在运行"}); err != nil {
		t.Fatal(err)
	}
	command := feishu.Message{BotID: "bot-a", Workspace: fixture.workspace, ChatID: "oc_source", ChatType: "p2p", SenderID: testOwnerOpenID}

	command.MessageID = "om_normal_fork"
	if reply := fixture.service.handleSessionForkCommand(context.Background(), "/session fork", command); reply != "" {
		t.Fatalf("normal busy fork reply = %q, want outbound response", reply)
	}
	sent := fixture.outbound.sentSnapshot()
	if len(sent) != 1 || !strings.Contains(sent[0], "上一轮正常结束的消息是：“帮我实现 session fork”") || !strings.Contains(sent[0], "/session fork --force") {
		t.Fatalf("busy response = %#v, want last completed turn and force hint", sent)
	}
	if len(runtimeNewCallsSnapshot(fixture.runtime)) != 0 {
		t.Fatal("normal busy fork created a session")
	}

	command.MessageID = "om_force_fork"
	if reply := fixture.service.handleSessionForkCommand(context.Background(), "/session fork --force 强制分支", command); reply != "" {
		t.Fatalf("force fork reply = %q, want outbound-only reply", reply)
	}
	operation, ok := fixture.service.forkStoreForWorkspace(fixture.workspace).GetByCommand(command.MessageID)
	if !ok || !operation.Source.Forced || operation.Source.SourceCutoffSeq != 3 || operation.Source.SourceMessageID != "om_done" {
		t.Fatalf("forced operation = %+v, %v; want cutoff at completed turn", operation, ok)
	}
	if fixture.runtime.cancelCallCount() != 0 {
		t.Fatalf("cancel calls = %d, want running source untouched", fixture.runtime.cancelCallCount())
	}
	fixture.service.taskMu.Lock()
	_, stillRunning := fixture.service.tasks[fixture.source.Key]
	fixture.service.taskMu.Unlock()
	if !stillRunning {
		t.Fatal("source running task was changed by force fork")
	}
}

func TestSessionForkRetryReusesCreatedTargetSession(t *testing.T) {
	fixture := newSessionForkFixture(t, imSessionKey("bot-a", "oc_source", ""))
	fixture.outbound.chatCreator = func(_ context.Context, request feishu.CreateChatRequest) (feishu.CreatedChat, error) {
		return feishu.CreatedChat{ChatID: "oc_retry", Name: request.Name, ChatType: "private", GroupMessageType: "chat"}, nil
	}
	fixture.runtime.promptUpdates = nil
	fixture.runtime.promptErrors = []error{errors.New("bootstrap failed")}
	command := feishu.Message{
		BotID: "bot-a", Workspace: fixture.workspace, ChatID: "oc_source", ChatType: "p2p",
		MessageID: "om_fork_command", SenderID: testOwnerOpenID,
	}
	if reply := fixture.service.handleSessionForkCommand(context.Background(), "/session fork 可重试", command); reply != "" {
		t.Fatalf("failed fork reply = %q, want outbound error response", reply)
	}
	operation, ok := fixture.service.forkStoreForWorkspace(fixture.workspace).GetByCommand(command.MessageID)
	if !ok || operation.State != forkStateFailed || operation.TargetSession != "acp-fork" {
		t.Fatalf("failed operation = %+v, %v; want retryable target session", operation, ok)
	}

	retry := feishu.Message{
		BotID: "bot-a", Workspace: fixture.workspace, ChatID: "oc_retry", ChatType: "group",
		MessageID: "om_retry_command", SenderID: testOwnerOpenID,
	}
	if reply := fixture.service.handleSessionForkCommand(context.Background(), "/session fork retry", retry); reply != "分支上下文已接续。" {
		t.Fatalf("retry reply = %q, want ready confirmation", reply)
	}
	operation, ok = fixture.service.forkStoreForWorkspace(fixture.workspace).Get(operation.ID)
	if !ok || operation.State != forkStateReady {
		t.Fatalf("retried operation = %+v, %v; want ready", operation, ok)
	}
	if len(runtimeNewCallsSnapshot(fixture.runtime)) != 1 || fixture.runtime.promptCallCount() != 2 {
		t.Fatalf("new calls = %d prompt calls = %d, want reuse existing target session", len(runtimeNewCallsSnapshot(fixture.runtime)), fixture.runtime.promptCallCount())
	}
}

func TestSessionForkRetryFromSourceCreatesMissingTarget(t *testing.T) {
	fixture := newSessionForkFixture(t, imSessionKey("bot-a", "oc_source", ""))
	createCalls := 0
	fixture.outbound.chatCreator = func(_ context.Context, request feishu.CreateChatRequest) (feishu.CreatedChat, error) {
		createCalls++
		if createCalls == 1 {
			return feishu.CreatedChat{}, errors.New("temporary create failure")
		}
		return feishu.CreatedChat{ChatID: "oc_retry_source", Name: request.Name, ChatType: "private", GroupMessageType: "chat"}, nil
	}
	command := feishu.Message{
		BotID: "bot-a", Workspace: fixture.workspace, ChatID: "oc_source", ChatType: "p2p",
		MessageID: "om_fork_command", SenderID: testOwnerOpenID,
		Mentions: []feishu.Mention{{ID: "ou_alice", Name: "Alice", Type: "user"}},
	}
	if reply := fixture.service.handleSessionForkCommand(context.Background(), "/session fork 可恢复 @Alice", command); reply != "" {
		t.Fatalf("failed fork reply = %q, want outbound error response", reply)
	}
	operation, ok := fixture.service.forkStoreForWorkspace(fixture.workspace).GetByCommand(command.MessageID)
	if !ok || operation.State != forkStateFailed || operation.TargetChatID != "" || !reflect.DeepEqual(operation.ExtraUserOpenIDs, []string{"ou_alice"}) {
		t.Fatalf("failed operation = %+v, %v; want source-retryable operation", operation, ok)
	}

	retry := command
	retry.MessageID = "om_retry_from_source"
	if reply := fixture.service.handleSessionForkCommand(context.Background(), "/session fork retry", retry); reply != "" {
		t.Fatalf("source retry reply = %q, want outbound-only reply", reply)
	}
	operation, ok = fixture.service.forkStoreForWorkspace(fixture.workspace).Get(operation.ID)
	if !ok || operation.State != forkStateReady || operation.TargetChatID != "oc_retry_source" || operation.TargetSession != "acp-fork" {
		t.Fatalf("retried operation = %+v, %v; want ready target", operation, ok)
	}
	if createCalls != 2 || len(runtimeNewCallsSnapshot(fixture.runtime)) != 1 || fixture.runtime.promptCallCount() != 1 {
		t.Fatalf("create calls = %d, new calls = %d, prompt calls = %d; want one resumed attempt", createCalls, len(runtimeNewCallsSnapshot(fixture.runtime)), fixture.runtime.promptCallCount())
	}
}

func TestSessionForkConcurrentRetryOnlyBootstrapsOnce(t *testing.T) {
	fixture := newSessionForkFixture(t, imSessionKey("bot-a", "oc_source", ""))
	fixture.outbound.chatCreator = func(_ context.Context, request feishu.CreateChatRequest) (feishu.CreatedChat, error) {
		return feishu.CreatedChat{ChatID: "oc_retry", Name: request.Name, ChatType: "private", GroupMessageType: "chat"}, nil
	}
	fixture.runtime.promptUpdates = nil
	fixture.runtime.promptErrors = []error{errors.New("bootstrap failed")}
	command := feishu.Message{
		BotID: "bot-a", Workspace: fixture.workspace, ChatID: "oc_source", ChatType: "p2p",
		MessageID: "om_fork_command", SenderID: testOwnerOpenID,
	}
	if reply := fixture.service.handleSessionForkCommand(context.Background(), "/session fork 可重试", command); reply != "" {
		t.Fatalf("failed fork reply = %q, want outbound error response", reply)
	}

	blockPrompt := make(chan struct{})
	fixture.runtime.mu.Lock()
	fixture.runtime.blockPrompt = blockPrompt
	fixture.runtime.blockPromptAt = 2
	fixture.runtime.mu.Unlock()
	retry := feishu.Message{
		BotID: "bot-a", Workspace: fixture.workspace, ChatID: "oc_retry", ChatType: "group",
		SenderID: testOwnerOpenID,
	}
	start := make(chan struct{})
	replies := make(chan string, 2)
	for i := 0; i < 2; i++ {
		retryMessage := retry
		retryMessage.MessageID = fmt.Sprintf("om_retry_%d", i)
		go func() {
			<-start
			replies <- fixture.service.handleSessionForkCommand(context.Background(), "/session fork retry", retryMessage)
		}()
	}
	close(start)
	waitForCondition(t, time.Second, func() bool {
		operation, ok := fixture.service.forkStoreForWorkspace(fixture.workspace).GetByCommand(command.MessageID)
		return ok && operation.State == forkStateBootstrapping && fixture.runtime.promptCallCount() == 2 && len(replies) == 1
	})
	close(blockPrompt)
	got := map[string]int{
		<-replies: 1,
	}
	got[<-replies]++
	if got["分支正在初始化，请稍候。"] != 1 || got["分支上下文已接续。"] != 1 {
		t.Fatalf("concurrent retry replies = %#v, want one initializing and one ready", got)
	}
	if calls := fixture.runtime.promptCallCount(); calls != 2 {
		t.Fatalf("prompt calls = %d, want initial failure plus one retry", calls)
	}
	fixture.runtime.mu.Lock()
	cancelCalls := len(fixture.runtime.cancelCalls)
	fixture.runtime.mu.Unlock()
	if cancelCalls != 0 {
		t.Fatalf("cancel calls = %d, want concurrent retry not to replace the winner", cancelCalls)
	}
}

func TestSessionForkRetryMatchesTopicRootWhenThreadIDArrivesLater(t *testing.T) {
	fixture := newSessionForkFixture(t, imSessionKey("bot-a", "oc_topic", "omt_source"))
	fixture.runtime.promptUpdates = nil
	fixture.runtime.promptErrors = []error{errors.New("bootstrap failed")}
	command := feishu.Message{
		BotID: "bot-a", BotOpenID: testBotOpenID, Workspace: fixture.workspace, ChatID: "oc_topic", ChatType: "topic_group",
		ChatMode: "topic", GroupMessageType: "thread", MessageID: "om_fork_command", ThreadID: "omt_source",
		RootID: "om_source_root", SenderID: testOwnerOpenID,
	}
	if reply := fixture.service.handleSessionForkCommand(context.Background(), "/session fork 新话题", command); reply != "" {
		t.Fatalf("failed topic fork reply = %q, want outbound error response", reply)
	}

	targetMessage := feishu.Message{
		BotID: "bot-a", BotOpenID: testBotOpenID, Workspace: fixture.workspace, ChatID: "oc_topic", ChatType: "topic_group",
		ChatMode: "topic", GroupMessageType: "thread", MessageID: "om_user_reply", ThreadID: "omt_actual",
		RootID: "om_fork_display", ParentID: "om_fork_display", SenderID: testOwnerOpenID,
	}
	if notice := fixture.service.forkTargetGuard(targetMessage, "继续"); notice != "分支初始化失败，请使用 /session fork retry 重试。" {
		t.Fatalf("forkTargetGuard() = %q, want failed target notice", notice)
	}
	if reply := fixture.service.handleSessionForkCommand(context.Background(), "/session fork retry", targetMessage); reply != "分支上下文已接续。" {
		t.Fatalf("retry reply = %q, want ready confirmation", reply)
	}
	operation, ok := fixture.service.forkStoreForWorkspace(fixture.workspace).GetByCommand(command.MessageID)
	if !ok || operation.State != forkStateReady || operation.TargetKey.SubID != "om_fork_display" {
		t.Fatalf("operation after topic retry = %+v, %v; want root-keyed ready operation", operation, ok)
	}
}

func TestSessionForkTargetMessageLookupIsScopedByBot(t *testing.T) {
	store := newForkOperationStore(t.TempDir())
	operation := ForkOperation{
		ID: "fork_bot_a", State: forkStateFailed,
		Source:          SessionForkOrigin{ForkID: "fork_bot_a", ForkCommandMessageID: "om_command"},
		TargetKey:       imSessionKey("bot-a", "oc_topic", "om_root"),
		TargetChatID:    "oc_topic",
		TargetRootID:    "om_root",
		TargetMessageID: "om_root",
	}
	if err := store.Put(operation); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.GetByTargetMessageIDs("bot-b", "oc_topic", "om_root"); ok {
		t.Fatal("GetByTargetMessageIDs() matched an operation owned by another bot")
	}
	if got, ok := store.GetByTargetMessageIDs("bot-a", "oc_topic", "om_root"); !ok || got.ID != operation.ID {
		t.Fatalf("GetByTargetMessageIDs() = %+v, %v; want bot-scoped operation", got, ok)
	}
}

func TestSessionForkRetryRecoversMissingTargetSessionCheckpoint(t *testing.T) {
	fixture := newSessionForkFixture(t, imSessionKey("bot-a", "oc_source", ""))
	targetKey := imSessionKey("bot-a", "oc_retry_gap", "")
	origin := SessionForkOrigin{
		ForkID:             "fork_gap",
		SourceKey:          fixture.source.Key,
		SourceACPSessionID: fixture.source.ACPSessionID,
		SourceMessageID:    "om_done",
		SourceCutoffSeq:    3,
	}
	target := Session{
		Key: targetKey, Title: "恢复分支", ManualTitle: true, AgentName: fixture.source.AgentName,
		ACPSessionID: "acp-existing-fork", Cwd: fixture.workDir, Workspace: fixture.workspace, ForkOrigin: &origin,
	}
	if err := fixture.store.InsertIfAbsent(target); err != nil {
		t.Fatal(err)
	}
	fixture.store.mu.Lock()
	delete(fixture.store.sessions, fixture.source.Key)
	fixture.store.history = nil
	fixture.store.mu.Unlock()
	bundlePath := filepath.Join(fixture.workspace, "bundle.jsonl")
	if err := os.WriteFile(bundlePath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	operation := ForkOperation{
		ID: "fork_gap", State: forkStateFailed, Source: origin, SourceTitle: fixture.source.Title, TargetTitle: target.Title,
		TargetKey: targetKey, TargetChatID: "oc_retry_gap", TargetMessageID: "om_display", BundlePath: bundlePath,
	}
	forkStore := fixture.service.forkStoreForWorkspace(fixture.workspace)
	if err := forkStore.Put(operation); err != nil {
		t.Fatal(err)
	}

	reply, err := fixture.service.HandleFeishuMessage(context.Background(), feishu.Message{
		BotID: "bot-a", BotOpenID: testBotOpenID, Workspace: fixture.workspace, ChatID: "oc_retry_gap", ChatType: "group",
		MessageID: "om_retry", SenderID: testOwnerOpenID, Text: "/session   fork   retry", Mentions: testBotMentions(),
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(retry) error = %v", err)
	}
	if reply != "分支上下文已接续。" {
		t.Fatalf("retrySessionFork() = %q, want ready confirmation", reply)
	}
	updated, ok := forkStore.Get(operation.ID)
	if !ok || updated.State != forkStateReady || updated.TargetSession != target.ACPSessionID {
		t.Fatalf("operation after retry = %+v, %v; want recovered target checkpoint", updated, ok)
	}
	if len(runtimeNewCallsSnapshot(fixture.runtime)) != 0 || fixture.runtime.promptCallCount() != 1 {
		t.Fatalf("new calls = %d prompt calls = %d, want bootstrap existing target", len(runtimeNewCallsSnapshot(fixture.runtime)), fixture.runtime.promptCallCount())
	}
}

func TestReadForkTraceSnapshotUsesLastCompleteTurnAndSanitizes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.jsonl")
	records := []traceRecord{
		{Seq: 1, Type: "user", MessageID: "m1", Content: "## User Message\n配置 API_KEY=sk-live-secret"},
		{Seq: 2, Type: "assistant", MessageID: "m1", Content: "中间过程"},
		{Seq: 3, Type: "tool", MessageID: "m1", Input: json.RawMessage(`{"token":"tool-secret"}`), Output: json.RawMessage(`{"result":"ok"}`), RawUpdate: json.RawMessage(`{"secret":"raw"}`)},
		{Seq: 4, Type: "assistant", MessageID: "m1", IsFinal: true, Content: "完成 Authorization: Bearer reply-secret"},
		{Seq: 5, Type: "turn_result", MessageID: "m1"},
		{Seq: 6, Type: "user", MessageID: "m2", Content: "尚未完成"},
		{Seq: 7, Type: "assistant", MessageID: "m2", IsFinal: true, Content: "半轮回复"},
		{Seq: 8, Type: "user", Source: "wiki", MessageID: "wiki_1", Content: "后台任务"},
		{Seq: 9, Type: "turn_result", Source: "wiki", MessageID: "wiki_1"},
	}
	var data []byte
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, append(line, '\n')...)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot, err := readForkTraceSnapshot(path, 9)
	if err != nil {
		t.Fatalf("readForkTraceSnapshot() error = %v", err)
	}
	if snapshot.SnapshotSeq != 9 || snapshot.CutoffSeq != 5 || snapshot.CutoffMessageID != "m1" || len(snapshot.Records) != 4 {
		t.Fatalf("snapshot = %+v, want first complete turn only", snapshot)
	}
	if snapshot.LastUserText != "配置 API_KEY=sk-live-secret" {
		t.Fatalf("last user text = %q, want stripped message envelope", snapshot.LastUserText)
	}
	encoded, err := json.Marshal(snapshot.Records)
	if err != nil {
		t.Fatal(err)
	}
	content := string(encoded)
	for _, secret := range []string{"sk-live-secret", "tool-secret", "reply-secret", "中间过程", "半轮回复", "后台任务", `\"raw\"`} {
		if strings.Contains(content, secret) {
			t.Fatalf("sanitized bundle contains %q: %s", secret, content)
		}
	}
	if !strings.Contains(content, secretInputPlaceholder) {
		t.Fatalf("sanitized bundle = %s, want hidden placeholders", content)
	}
}

func TestForkOperationStoreIsIdempotentAndRecoversInterrupted(t *testing.T) {
	workspace := t.TempDir()
	store := newForkOperationStore(workspace)
	first := ForkOperation{
		ID: "fork_first", State: forkStatePreparing,
		Source: SessionForkOrigin{ForkID: "fork_first", ForkCommandMessageID: "om_command"},
	}
	stored, inserted, err := store.PutIfCommandAbsent(first)
	if err != nil || !inserted || stored.ID != first.ID {
		t.Fatalf("PutIfCommandAbsent(first) = %+v, %v, %v", stored, inserted, err)
	}
	duplicate := first
	duplicate.ID = "fork_duplicate"
	stored, inserted, err = store.PutIfCommandAbsent(duplicate)
	if err != nil || inserted || stored.ID != first.ID {
		t.Fatalf("PutIfCommandAbsent(duplicate) = %+v, %v, %v; want original", stored, inserted, err)
	}

	reloaded := newForkOperationStore(workspace)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	recovered, ok := reloaded.Get(first.ID)
	if !ok || recovered.State != forkStateFailed || !strings.Contains(recovered.Error, "请在源位置执行 /session fork retry") {
		t.Fatalf("recovered operation = %+v, %v; want retryable failure", recovered, ok)
	}
	info, err := os.Stat(reloaded.indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("operation index mode = %o, want 600", info.Mode().Perm())
	}
}

func TestForkOperationStoreClaimRetryIsAtomic(t *testing.T) {
	workspace := t.TempDir()
	store := newForkOperationStore(workspace)
	operation := ForkOperation{
		ID: "fork_retry", State: forkStateFailed,
		Source: SessionForkOrigin{ForkID: "fork_retry", ForkCommandMessageID: "om_command"},
		Error:  "bootstrap failed",
	}
	if err := store.Put(operation); err != nil {
		t.Fatal(err)
	}
	operation, _ = store.Get(operation.ID)

	const attempts = 16
	start := make(chan struct{})
	type result struct {
		operation ForkOperation
		acquired  bool
		err       error
	}
	results := make(chan result, attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			<-start
			claimed, acquired, err := store.ClaimRetry(operation.ID, operation.Revision)
			results <- result{operation: claimed, acquired: acquired, err: err}
		}()
	}
	close(start)
	acquired := 0
	for i := 0; i < attempts; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("ClaimRetry() error = %v", result.err)
		}
		if result.operation.State != forkStateBootstrapping {
			t.Fatalf("ClaimRetry() operation = %+v, want bootstrapping", result.operation)
		}
		if result.acquired {
			acquired++
		}
	}
	if acquired != 1 {
		t.Fatalf("successful claims = %d, want 1", acquired)
	}
	reloaded := newForkOperationStore(workspace)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	stored, ok := reloaded.Get(operation.ID)
	if !ok || stored.State != forkStateBootstrapping || stored.Error != "" {
		t.Fatalf("persisted operation = %+v, %v; want claimed bootstrap state", stored, ok)
	}
	failedAgain := stored
	failedAgain.State = forkStateFailed
	failedAgain.Error = "second failure"
	if err := store.Put(failedAgain); err != nil {
		t.Fatal(err)
	}
	if current, acquired, err := store.ClaimRetry(operation.ID, operation.Revision); err != nil || acquired || current.State != forkStateFailed {
		t.Fatalf("stale ClaimRetry() = %+v, %v, %v; want changed failure without acquisition", current, acquired, err)
	}
}

func TestForkOperationStoreClaimRetryWriteFailureRollsBack(t *testing.T) {
	workspace := t.TempDir()
	store := newForkOperationStore(workspace)
	operation := ForkOperation{
		ID: "fork_retry", State: forkStateFailed,
		Source: SessionForkOrigin{ForkID: "fork_retry", ForkCommandMessageID: "om_command"},
		Error:  "bootstrap failed",
	}
	if err := store.Put(operation); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(workspace, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.indexPath = filepath.Join(blocker, "index.json")
	storedBeforeClaim, ok := store.Get(operation.ID)
	if !ok {
		t.Fatal("operation missing before claim")
	}
	if _, acquired, err := store.ClaimRetry(operation.ID, storedBeforeClaim.Revision); err == nil || acquired {
		t.Fatalf("ClaimRetry() acquired = %v, error = %v; want write failure", acquired, err)
	}
	stored, ok := store.Get(operation.ID)
	if !ok || stored.State != forkStateFailed || stored.Error != operation.Error {
		t.Fatalf("operation after failed claim = %+v, %v; want original failure state", stored, ok)
	}
}

func TestForkUserSummaryRedactsCollapsesAndTruncatesRunes(t *testing.T) {
	text := "  请   使用 API_KEY=sk-secret 继续处理 " + strings.Repeat("中", 80)
	summary := forkUserSummary(text)
	if strings.Contains(summary, "sk-secret") || strings.Contains(summary, "  ") {
		t.Fatalf("summary = %q, want redacted collapsed text", summary)
	}
	if len([]rune(summary)) != 60 {
		t.Fatalf("summary runes = %d, want 60; summary = %q", len([]rune(summary)), summary)
	}
}

func TestForkOperationStorePrunesExpiredArtifacts(t *testing.T) {
	workspace := t.TempDir()
	store := newForkOperationStore(workspace)
	operation := ForkOperation{
		ID: "fork_expired", State: forkStateFailed, UpdatedAt: time.Now().Add(-forkFailedRetention - time.Hour),
		Source: SessionForkOrigin{ForkID: "fork_expired", ForkCommandMessageID: "om_expired"},
	}
	store.operations[operation.ID] = operation
	artifactDir := filepath.Join(store.dir, operation.ID)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	removeForkArtifactDirs(store.pruneLocked(time.Now()))
	if _, ok := store.operations[operation.ID]; ok {
		t.Fatal("expired operation was not pruned")
	}
	if _, err := os.Stat(artifactDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact stat error = %v, want not exist", err)
	}
}

func TestForkOperationStoreWriteFailureKeepsPrunedArtifactsAndMemory(t *testing.T) {
	workspace := t.TempDir()
	store := newForkOperationStore(workspace)
	expired := ForkOperation{
		ID: "fork_expired", State: forkStateFailed, UpdatedAt: time.Now().Add(-forkFailedRetention - time.Hour),
		Source: SessionForkOrigin{ForkID: "fork_expired", ForkCommandMessageID: "om_expired"},
	}
	store.operations[expired.ID] = expired
	artifactDir := filepath.Join(store.dir, expired.ID)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(workspace, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.indexPath = filepath.Join(blocker, "index.json")
	current := ForkOperation{
		ID: "fork_current", State: forkStatePreparing,
		Source: SessionForkOrigin{ForkID: "fork_current", ForkCommandMessageID: "om_current"},
	}
	if err := store.Put(current); err == nil {
		t.Fatal("Put() error = nil, want write failure")
	}
	if _, ok := store.operations[expired.ID]; !ok {
		t.Fatal("expired operation was not restored after write failure")
	}
	if _, ok := store.operations[current.ID]; ok {
		t.Fatal("new operation remained in memory after write failure")
	}
	if _, err := os.Stat(artifactDir); err != nil {
		t.Fatalf("artifact removed before index commit: %v", err)
	}
}
