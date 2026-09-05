package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

const testBotOpenID = "ou_bot"

const testOwnerOpenID = "ou_owner"

func testBotMention(name string) feishu.Mention {
	return feishu.Mention{ID: testBotOpenID, Name: name, Type: "bot"}
}

func testBotMentions() []feishu.Mention {
	return []feishu.Mention{testBotMention("智能助手")}
}

func newTestService(cfg config.Config, store *SessionStore) *Service {
	if len(cfg.Bots) > 0 && len(cfg.Bots[0].OwnerOpenIDs) == 0 {
		cfg.Bots[0].OwnerOpenIDs = []string{testOwnerOpenID}
	}
	return NewService(cfg, store)
}

func mustConfigAgent(t testing.TB, cfg config.Config, name string) config.AgentConfig {
	t.Helper()
	agent, ok := cfg.Agent(name)
	if !ok {
		t.Fatalf("missing config agent %q in %#v", name, cfg.AgentList)
	}
	return agent
}

func handleFeishuMessage(t *testing.T, svc *Service, ctx context.Context, msg feishu.Message) (string, error) {
	t.Helper()
	if strings.EqualFold(msg.ChatType, "topic_group") && strings.TrimSpace(msg.GroupMessageType) == "" {
		msg.GroupMessageType = "thread"
	}
	if strings.TrimSpace(msg.BotOpenID) == "" {
		msg.BotOpenID = testBotOpenID
	}
	if strings.TrimSpace(msg.SenderID) == "" && strings.HasPrefix(strings.TrimSpace(stripMentionNames(msg.Text, msg.Mentions)), "/") {
		ensureTestOwner(t, svc, msg.BotID)
		if owners := svc.ownerOpenIDs(msg.BotID); len(owners) > 0 {
			msg.SenderID = strings.TrimSpace(owners[0])
		} else {
			msg.SenderID = testOwnerOpenID
		}
	}
	if strings.TrimSpace(msg.Workspace) == "" {
		msg.Workspace = filepath.Join(t.TempDir(), "workspace")
		if err := os.MkdirAll(msg.Workspace, 0o755); err != nil {
			t.Fatalf("MkdirAll(workspace) error = %v", err)
		}
		botID := msg.BotID
		if strings.TrimSpace(botID) == "" {
			botID = "bot-a"
		}
		for _, file := range workspaceFiles(botID) {
			path := filepath.Join(msg.Workspace, file.name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(file.name), err)
			}
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatalf("WriteFile(%s) error = %v", file.name, err)
			}
		}
		markWorkspaceBootstrapped(t, msg.Workspace)
	}
	return svc.HandleFeishuMessage(ctx, msg)
}

type fakeSentMessageClient struct {
	mu                      sync.Mutex
	nextID                  string
	replySender             func(context.Context, feishu.Message, string) error
	streamStarter           func(context.Context, feishu.Message, feishu.StreamCardOptions) (feishu.StreamCard, error)
	reactionStarter         func(context.Context, feishu.Message) func()
	modelSelectionSender    func(context.Context, feishu.Message, feishu.ModelSelectionCard) error
	modeSelectionSender     func(context.Context, feishu.Message, feishu.ModeSelectionCard) error
	sessionSelectionSender  func(context.Context, feishu.Message, feishu.SessionSelectionCard) error
	overviewSender          func(context.Context, feishu.Message, feishu.OverviewCard) error
	configDetailSender      func(context.Context, feishu.Message, feishu.ConfigDetailCard) error
	driveCommentReplySender func(context.Context, feishu.DriveComment, string) error
	traceChatCreator        func(context.Context, feishu.CreateDriveCommentTraceChatRequest) (feishu.CreatedChat, error)
	chatCreator             func(context.Context, feishu.CreateChatRequest) (feishu.CreatedChat, error)
	chatMemberAdder         func(context.Context, feishu.AddChatMembersRequest) (feishu.AddChatMembersResult, error)
	shareChatSender         func(context.Context, feishu.Message, string) (feishu.SentMessage, error)
	traceBotNameProvider    func(context.Context) (string, error)
	sent                    []string
	msgs                    []feishu.Message
	updates                 []string
	updateIDs               []string
	finishes                []string
	finishIDs               []string
	loopRequests            []feishu.LoopStatusCardRequest
	textUpdates             []string
	textUpdateIDs           []string
	sharedChatIDs           []string
}

func newFakeSentMessageClient(nextID string) *fakeSentMessageClient {
	return &fakeSentMessageClient{nextID: nextID}
}

func (f *fakeSentMessageClient) Outbound() {}

func (f *fakeSentMessageClient) SendText(ctx context.Context, msg feishu.Message, text string) error {
	if f != nil && f.replySender != nil {
		return f.replySender(ctx, msg, text)
	}
	_, err := f.SendTextMessage(ctx, msg, text)
	return err
}

func (f *fakeSentMessageClient) SendTextMessage(ctx context.Context, msg feishu.Message, text string) (feishu.SentMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := strings.TrimSpace(f.nextID)
	if id == "" {
		id = "om_loop_start"
	}
	f.sent = append(f.sent, text)
	f.msgs = append(f.msgs, msg)
	return feishu.SentMessage{
		MessageID: id,
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		ThreadID:  msg.ThreadID,
		RootID:    id,
	}, nil
}

func (f *fakeSentMessageClient) UpdateText(ctx context.Context, messageID string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.textUpdateIDs = append(f.textUpdateIDs, messageID)
	f.textUpdates = append(f.textUpdates, text)
	return nil
}

func (f *fakeSentMessageClient) SendShareChatMessage(ctx context.Context, msg feishu.Message, chatID string) (feishu.SentMessage, error) {
	if f != nil && f.shareChatSender != nil {
		return f.shareChatSender(ctx, msg, chatID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sharedChatIDs = append(f.sharedChatIDs, chatID)
	f.msgs = append(f.msgs, msg)
	return feishu.SentMessage{MessageID: "om_share_chat", ChatID: msg.ChatID, ChatType: msg.ChatType, ThreadID: msg.ThreadID}, nil
}

func (f *fakeSentMessageClient) SendLoopStatusCard(ctx context.Context, msg feishu.Message, request feishu.LoopStatusCardRequest) (feishu.LoopStatusCard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := strings.TrimSpace(f.nextID)
	if id == "" {
		id = "om_loop_start"
	}
	sent := feishu.SentMessage{
		MessageID: id,
		ChatID:    msg.ChatID,
		ChatType:  msg.ChatType,
		ThreadID:  msg.ThreadID,
		RootID:    id,
	}
	f.sent = append(f.sent, request.Text)
	f.msgs = append(f.msgs, msg)
	f.loopRequests = append(f.loopRequests, request)
	return &fakeLoopStatusCard{client: f, message: sent}, nil
}

func (f *fakeSentMessageClient) StartStreamCard(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
	if f == nil || f.streamStarter == nil {
		return nil, nil
	}
	return f.streamStarter(ctx, msg, options)
}

type fakeStreamCardCollector struct {
	mu    sync.Mutex
	cards []*fakeStreamCard
}

func (c *fakeStreamCardCollector) add(card *fakeStreamCard) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cards = append(c.cards, card)
}

func (c *fakeStreamCardCollector) snapshot() []*fakeStreamCard {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*fakeStreamCard(nil), c.cards...)
}

func (f *fakeSentMessageClient) StartProcessingReaction(ctx context.Context, msg feishu.Message) func() {
	if f == nil || f.reactionStarter == nil {
		return func() {}
	}
	return f.reactionStarter(ctx, msg)
}

func (f *fakeSentMessageClient) SendModelSelectionCard(ctx context.Context, msg feishu.Message, card feishu.ModelSelectionCard) error {
	if f == nil || f.modelSelectionSender == nil {
		return nil
	}
	return f.modelSelectionSender(ctx, msg, card)
}

func (f *fakeSentMessageClient) SendModeSelectionCard(ctx context.Context, msg feishu.Message, card feishu.ModeSelectionCard) error {
	if f == nil || f.modeSelectionSender == nil {
		return nil
	}
	return f.modeSelectionSender(ctx, msg, card)
}

func (f *fakeSentMessageClient) SendSessionSelectionCard(ctx context.Context, msg feishu.Message, card feishu.SessionSelectionCard) error {
	if f == nil || f.sessionSelectionSender == nil {
		return nil
	}
	return f.sessionSelectionSender(ctx, msg, card)
}

func (f *fakeSentMessageClient) SendOverviewCard(ctx context.Context, msg feishu.Message, card feishu.OverviewCard) error {
	if f == nil || f.overviewSender == nil {
		return nil
	}
	return f.overviewSender(ctx, msg, card)
}

func (f *fakeSentMessageClient) SendConfigDetailCard(ctx context.Context, msg feishu.Message, card feishu.ConfigDetailCard) error {
	if f == nil || f.configDetailSender == nil {
		return nil
	}
	return f.configDetailSender(ctx, msg, card)
}

func (f *fakeSentMessageClient) ReplyDriveComment(ctx context.Context, comment feishu.DriveComment, text string) error {
	if f == nil || f.driveCommentReplySender == nil {
		return nil
	}
	return f.driveCommentReplySender(ctx, comment, text)
}

func (f *fakeSentMessageClient) CreateDriveCommentTraceChat(ctx context.Context, req feishu.CreateDriveCommentTraceChatRequest) (feishu.CreatedChat, error) {
	if f == nil || f.traceChatCreator == nil {
		return feishu.CreatedChat{}, nil
	}
	return f.traceChatCreator(ctx, req)
}

func (f *fakeSentMessageClient) CreateChat(ctx context.Context, req feishu.CreateChatRequest) (feishu.CreatedChat, error) {
	if f == nil || f.chatCreator == nil {
		return feishu.CreatedChat{}, nil
	}
	return f.chatCreator(ctx, req)
}

func (f *fakeSentMessageClient) AddChatMembers(ctx context.Context, req feishu.AddChatMembersRequest) (feishu.AddChatMembersResult, error) {
	if f == nil || f.chatMemberAdder == nil {
		return feishu.AddChatMembersResult{}, nil
	}
	return f.chatMemberAdder(ctx, req)
}

func (f *fakeSentMessageClient) DriveCommentTraceBotName(ctx context.Context) (string, error) {
	if f == nil || f.traceBotNameProvider == nil {
		return "", nil
	}
	return f.traceBotNameProvider(ctx)
}

type fakeLoopStatusCard struct {
	client                *fakeSentMessageClient
	message               feishu.SentMessage
	failOnCanceledContext bool
}

func (f *fakeLoopStatusCard) Message() feishu.SentMessage {
	if f == nil {
		return feishu.SentMessage{}
	}
	return f.message
}

func (f *fakeLoopStatusCard) Update(ctx context.Context, text string) error {
	if f == nil || f.client == nil {
		return nil
	}
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	f.client.updateIDs = append(f.client.updateIDs, f.message.MessageID)
	f.client.updates = append(f.client.updates, text)
	return nil
}

func (f *fakeLoopStatusCard) Finish(ctx context.Context, text string) error {
	if f == nil || f.client == nil {
		return nil
	}
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.client.mu.Lock()
	defer f.client.mu.Unlock()
	f.client.updateIDs = append(f.client.updateIDs, f.message.MessageID)
	f.client.updates = append(f.client.updates, text)
	f.client.finishIDs = append(f.client.finishIDs, f.message.MessageID)
	f.client.finishes = append(f.client.finishes, text)
	return nil
}

func (f *fakeSentMessageClient) sentSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func (f *fakeSentMessageClient) updatesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.updates...)
}

func (f *fakeSentMessageClient) updateIDsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.updateIDs...)
}

func (f *fakeSentMessageClient) finishesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.finishes...)
}

func (f *fakeSentMessageClient) messagesSnapshot() []feishu.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]feishu.Message(nil), f.msgs...)
}

func (f *fakeSentMessageClient) textUpdatesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.textUpdates...)
}

func withFakeSentMessageClient(ctx context.Context, svc *Service, botID string, client *fakeSentMessageClient) context.Context {
	if svc != nil {
		svc.setOutbound(botID, client)
	}
	return ctx
}

func ensureTestOwner(t *testing.T, svc *Service, botID string) {
	t.Helper()
	if svc == nil || len(svc.ownerOpenIDs(botID)) > 0 {
		return
	}
	if len(svc.cfg.Bots) == 0 {
		return
	}
	botID = strings.TrimSpace(botID)
	for i := range svc.cfg.Bots {
		if strings.TrimSpace(svc.cfg.Bots[i].ID) == botID {
			svc.cfg.Bots[i].OwnerOpenIDs = []string{testOwnerOpenID}
			return
		}
	}
	if len(svc.cfg.Bots) == 1 {
		svc.cfg.Bots[0].OwnerOpenIDs = []string{testOwnerOpenID}
	}
}

func markWorkspaceBootstrapped(t *testing.T, workspace string) {
	t.Helper()
	if _, err := ensureWorkspace(workspace, "bot-a"); err != nil {
		t.Fatalf("ensureWorkspace(%s) error = %v", workspace, err)
	}
	if err := os.Remove(filepath.Join(workspace, workspaceBootstrapFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove(%s) error = %v", workspaceBootstrapFile, err)
	}
}

func slogAttrsMap(attrs []slog.Attr) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		out[attr.Key] = attr.Value.String()
	}
	return out
}

func testReadySession(t *testing.T, store *SessionStore) Session {
	t.Helper()
	workspace := filepath.Join(t.TempDir(), "workspace")
	markWorkspaceBootstrapped(t, workspace)
	session := Session{
		Key:          imSessionKey("bot-a", "oc_chat", "omt_thread"),
		Title:        "test session",
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
	}
	if store != nil {
		if err := store.Upsert(session); err != nil {
			t.Fatalf("Upsert(session) error = %v", err)
		}
	}
	return session
}

func waitForCondition(t *testing.T, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ok() {
		return
	}
	t.Fatalf("condition not met within %s", timeout)
}

func containsStringWithAll(values []string, parts ...string) bool {
	for _, value := range values {
		matched := true
		for _, part := range parts {
			if !strings.Contains(value, part) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func hasSubstring(values []string, part string) bool {
	for _, value := range values {
		if strings.Contains(value, part) {
			return true
		}
	}
	return false
}

func hasStatusWithoutSubstring(values []string, part string) bool {
	for _, value := range values {
		if strings.Contains(value, "⏳") && !strings.Contains(value, part) {
			return true
		}
	}
	return false
}

func assertReadyPromptContainsUserTextAndMemoryPolicy(t *testing.T, prompt, userText string) {
	t.Helper()
	for _, want := range []string{"Workspace Memory Policy", "本地文件工具", "MEMORY.md", "knowledge/core.md", "knowledge/index.md", "skills/core.md", "## User Message", userText} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
	for _, notWant := range []string{"fs/read_text_file", "fs/write_text_file"} {
		if strings.Contains(prompt, notWant) {
			t.Fatalf("prompt = %q, should not mention %q", prompt, notWant)
		}
	}
}

func assertPromptContainsUserTextOnly(t *testing.T, prompt, userText string) {
	t.Helper()
	for _, want := range []string{"## User Message", userText} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
	for _, notWant := range []string{"Workspace Context", "Workspace Memory Policy"} {
		if strings.Contains(prompt, notWant) {
			t.Fatalf("prompt = %q, should not contain %q", prompt, notWant)
		}
	}
}

func assertPromptContainsMessageMetadata(t *testing.T, prompt string, want map[string]string) {
	t.Helper()
	assertPromptContainsSectionMetadata(t, prompt, "## Message Metadata", want)
	messageIdx := strings.Index(prompt, "## Message Metadata")
	userIdx := strings.Index(prompt, "## User Message")
	if messageIdx < 0 || userIdx < 0 || messageIdx > userIdx {
		t.Fatalf("prompt = %q, want message metadata before user message", prompt)
	}
}

func assertPromptContainsSectionMetadata(t *testing.T, prompt string, section string, want map[string]string) {
	t.Helper()
	metadata := promptSectionJSON(t, prompt, section)
	for key, value := range want {
		if got := metadata[key]; got != value {
			t.Fatalf("%s metadata[%q] = %q, want %q; prompt = %q", section, key, got, value, prompt)
		}
	}
}

func promptSectionJSON(t *testing.T, prompt string, section string) map[string]string {
	t.Helper()
	idx := strings.Index(prompt, section)
	if idx < 0 {
		t.Fatalf("prompt = %q, want section %q", prompt, section)
	}
	rest := prompt[idx+len(section):]
	start := strings.Index(rest, "```json")
	if start < 0 {
		t.Fatalf("section %q in prompt = %q, want json fence", section, prompt)
	}
	rest = rest[start+len("```json"):]
	end := strings.Index(rest, "```")
	if end < 0 {
		t.Fatalf("section %q in prompt = %q, want closing fence", section, prompt)
	}
	var metadata map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest[:end])), &metadata); err != nil {
		t.Fatalf("Unmarshal(%s metadata) error = %v", section, err)
	}
	return metadata
}

type fakeRuntime struct {
	mu                     sync.Mutex
	newSessionID           string
	newSessionIDs          []string
	newSessionInfo         acp.SessionInfo
	newSessionError        error
	noDefaultState         bool
	afterNewSession        func(key SessionKey, sessionID string)
	promptReply            string
	promptErrors           []error
	promptUpdates          []acp.PromptUpdate
	promptUpdatesByCall    [][]acp.PromptUpdate
	afterUpdates           func()
	permissionRequest      *acp.PermissionRequest
	permissionOutcome      acp.PermissionOutcome
	blockPrompt            chan struct{}
	blockPromptAt          int
	blockPromptScope       string
	blockAfterPromptCancel bool
	promptResult           acp.PromptResult
	promptResults          []acp.PromptResult
	promptPanic            bool
	configOptions          []acp.SessionConfigOption
	configCalls            []fakeConfigCall
	modeCalls              []fakeModeCall
	newCalls               []fakeNewCall
	promptCalls            []fakePromptCall
	wikiRuntimeCalls       []fakePromptCall
	atAutoRuntimeCalls     []fakePromptCall
	cancelCalls            []fakeCancelCall
	callSeq                int
	blockCancel            chan struct{}
	closedRuntimeKeys      []runtimeKey
	closedKeys             []SessionKey
	shutdownCancelCount    int
	updateHandlers         map[SessionKey][]acp.UpdateHandler
	blockWikiRuntime       chan struct{}
	activeSessionIDs       map[SessionKey]string
	abortedSessionIDs      []string
	transitionBefore       func()
	transitionMu           sync.Mutex
	transitionCloseErr     error
}

type fakeNewCall struct {
	Runtime   runtimeKey
	Key       SessionKey
	AgentName string
	Cwd       string
	Workspace string
}

type fakePromptCall struct {
	Runtime              runtimeKey
	Session              Session
	Text                 string
	Attrs                map[string]string
	HasUpdateHandler     bool
	HasPermissionHandler bool
	Seq                  int
}

type fakeCancelCall struct {
	Runtime runtimeKey
	Session Session
	Attrs   map[string]string
	Seq     int
}

type fakeConfigCall struct {
	Session  Session
	ConfigID string
	Value    any
}

type fakeModeCall struct {
	Session Session
	ModeID  string
}

func (f *fakeRuntime) NewSession(ctx context.Context, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (acpSessionCandidate, error) {
	return f.NewSessionWithRuntimeKey(ctx, currentRuntimeKey(key), key, agentName, agent, cwd, workspace)
}

func (f *fakeRuntime) NewSessionWithRuntimeKey(ctx context.Context, runtime runtimeKey, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (acpSessionCandidate, error) {
	key = normalizeSessionKey(key)
	runtime = normalizeRuntimeKey(runtime)
	f.mu.Lock()
	f.newCalls = append(f.newCalls, fakeNewCall{Runtime: runtime, Key: key, AgentName: agentName, Cwd: cwd, Workspace: workspace})
	if f.newSessionError != nil {
		err := f.newSessionError
		f.mu.Unlock()
		return nil, err
	}
	info := f.newSessionInfo
	afterNewSession := f.afterNewSession
	if len(f.newSessionIDs) > 0 {
		info.SessionID = f.newSessionIDs[0]
		f.newSessionIDs = f.newSessionIDs[1:]
	} else if f.newSessionID != "" {
		info.SessionID = f.newSessionID
	}
	if info.SessionID == "" {
		info.SessionID = "acp-session"
	}
	if !f.noDefaultState && len(info.ConfigOptions) == 0 && info.Models == nil && info.Mode == nil {
		info.ConfigOptions = []acp.SessionConfigOption{
			{ID: "model", Name: "Model", Category: "model", Type: "select", CurrentValue: "gpt-5.5"},
		}
	}
	f.mu.Unlock()
	if afterNewSession != nil {
		afterNewSession(key, info.SessionID)
	}
	return &fakeSessionCandidate{runtime: f, key: key, info: info}, nil
}

type fakeSessionCandidate struct {
	runtime   *fakeRuntime
	key       SessionKey
	info      acp.SessionInfo
	committed bool
	aborted   bool
}

func (c *fakeSessionCandidate) Info() acp.SessionInfo {
	return c.info
}

func (c *fakeSessionCandidate) Commit(persist func() error) error {
	c.runtime.transitionMu.Lock()
	defer c.runtime.transitionMu.Unlock()
	if c.committed {
		return nil
	}
	if c.aborted {
		return fmt.Errorf("ACP session 候选已关闭")
	}
	if persist != nil {
		if err := persist(); err != nil {
			c.Abort()
			return err
		}
	}
	c.runtime.mu.Lock()
	if c.runtime.activeSessionIDs == nil {
		c.runtime.activeSessionIDs = make(map[SessionKey]string)
	}
	c.runtime.activeSessionIDs[c.key] = c.info.SessionID
	c.runtime.mu.Unlock()
	c.committed = true
	return nil
}

func (c *fakeSessionCandidate) Abort() {
	if c.committed || c.aborted {
		return
	}
	c.aborted = true
	c.runtime.mu.Lock()
	c.runtime.abortedSessionIDs = append(c.runtime.abortedSessionIDs, c.info.SessionID)
	c.runtime.mu.Unlock()
}

func (f *fakeRuntime) Prompt(ctx context.Context, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (acp.PromptResult, error) {
	session.Key = normalizeSessionKey(session.Key)
	return f.prompt(ctx, currentRuntimeKey(session.Key), session, text, opts, runtimeScopeCurrent)
}

func (f *fakeRuntime) PromptWithRuntimeKey(ctx context.Context, key runtimeKey, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (acp.PromptResult, error) {
	key = normalizeRuntimeKey(key)
	session.Key = normalizeSessionKey(session.Key)
	return f.prompt(ctx, key, session, text, opts, key.Scope)
}

func (f *fakeRuntime) prompt(ctx context.Context, key runtimeKey, session Session, text string, opts acp.PromptOptions, scope string) (acp.PromptResult, error) {
	key = normalizeRuntimeKey(key)
	session.Key = normalizeSessionKey(session.Key)
	f.mu.Lock()
	call := fakePromptCall{
		Runtime:              key,
		Session:              session,
		Text:                 text,
		Attrs:                slogAttrsMap(logging.CtxAttrs(ctx)),
		HasUpdateHandler:     opts.OnUpdate != nil,
		HasPermissionHandler: opts.OnPermissionRequest != nil,
		Seq:                  f.nextCallSeqLocked(),
	}
	switch scope {
	case runtimeScopeWiki, runtimeScopeWikiCompanion:
		f.wikiRuntimeCalls = append(f.wikiRuntimeCalls, call)
	case runtimeScopeAtAuto:
		f.atAutoRuntimeCalls = append(f.atAutoRuntimeCalls, call)
	default:
		f.promptCalls = append(f.promptCalls, call)
	}
	callNumber := len(f.promptCalls) + len(f.atAutoRuntimeCalls)
	updates := append([]acp.PromptUpdate(nil), f.promptUpdates...)
	if len(f.promptUpdatesByCall) > 0 {
		updates = append([]acp.PromptUpdate(nil), f.promptUpdatesByCall[0]...)
		f.promptUpdatesByCall = f.promptUpdatesByCall[1:]
	}
	afterUpdates := f.afterUpdates
	blockPrompt := f.blockPrompt
	blockThisPrompt := blockPrompt != nil &&
		(f.blockPromptAt == 0 || f.blockPromptAt == callNumber) &&
		(f.blockPromptScope == "" || f.blockPromptScope == scope)
	blockWikiRuntime := f.blockWikiRuntime
	result := f.promptResult
	if len(f.promptResults) > 0 {
		result = f.promptResults[0]
		f.promptResults = f.promptResults[1:]
	}
	if result.Text == "" {
		result.Text = f.promptReply
	}
	promptPanic := f.promptPanic
	var promptErr error
	if len(f.promptErrors) > 0 {
		promptErr = f.promptErrors[0]
		f.promptErrors = f.promptErrors[1:]
	}
	f.mu.Unlock()
	if promptPanic {
		panic("prompt panic")
	}
	if opts.OnUpdate != nil {
		for _, update := range updates {
			opts.OnUpdate(update)
		}
	}
	if afterUpdates != nil {
		afterUpdates()
	}
	if f.permissionRequest != nil && opts.OnPermissionRequest != nil {
		outcome, _ := opts.OnPermissionRequest(ctx, *f.permissionRequest)
		f.mu.Lock()
		f.permissionOutcome = outcome
		f.mu.Unlock()
	}
	if (scope == runtimeScopeWiki || scope == runtimeScopeWikiCompanion) && blockWikiRuntime != nil {
		select {
		case <-ctx.Done():
			return acp.PromptResult{}, ctx.Err()
		case <-blockWikiRuntime:
		}
	}
	if blockThisPrompt {
		select {
		case <-ctx.Done():
			if f.blockAfterPromptCancel {
				<-blockPrompt
			}
			return acp.PromptResult{}, ctx.Err()
		case <-blockPrompt:
		}
	}
	if promptErr != nil {
		return acp.PromptResult{}, promptErr
	}
	return result, nil
}

func (f *fakeRuntime) CancelSession(ctx context.Context, key runtimeKey, session Session, agent config.AgentConfig) error {
	key = normalizeRuntimeKey(key)
	session.Key = normalizeSessionKey(session.Key)
	f.mu.Lock()
	f.cancelCalls = append(f.cancelCalls, fakeCancelCall{Runtime: key, Session: session, Attrs: slogAttrsMap(logging.CtxAttrs(ctx)), Seq: f.nextCallSeqLocked()})
	blockCancel := f.blockCancel
	f.mu.Unlock()
	if blockCancel != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-blockCancel:
		}
	}
	return nil
}

func (f *fakeRuntime) nextCallSeqLocked() int {
	f.callSeq++
	return f.callSeq
}

func (f *fakeRuntime) SetConfigOption(ctx context.Context, session Session, agent config.AgentConfig, configID string, value any) ([]acp.SessionConfigOption, error) {
	session.Key = normalizeSessionKey(session.Key)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configCalls = append(f.configCalls, fakeConfigCall{Session: session, ConfigID: configID, Value: value})
	if len(f.configOptions) > 0 {
		return append([]acp.SessionConfigOption(nil), f.configOptions...), nil
	}
	options := append([]acp.SessionConfigOption(nil), session.ConfigOptions...)
	for i := range options {
		opt := options[i]
		if opt.ID == configID || strings.EqualFold(opt.ID, configID) || opt.Category == configID || strings.EqualFold(opt.Category, configID) {
			options[i].CurrentValue = value
			return options, nil
		}
	}
	return append(options, acp.SessionConfigOption{
		ID:           configID,
		Name:         configID,
		Category:     configID,
		Type:         "select",
		CurrentValue: value,
	}), nil
}

func (f *fakeRuntime) SetMode(ctx context.Context, session Session, agent config.AgentConfig, modeID string) error {
	session.Key = normalizeSessionKey(session.Key)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modeCalls = append(f.modeCalls, fakeModeCall{Session: session, ModeID: modeID})
	return nil
}

func (f *fakeRuntime) SubscribeUpdates(key SessionKey, handler acp.UpdateHandler) func() {
	if handler == nil {
		return func() {}
	}
	key = normalizeSessionKey(key)
	f.mu.Lock()
	if f.updateHandlers == nil {
		f.updateHandlers = make(map[SessionKey][]acp.UpdateHandler)
	}
	f.updateHandlers[key] = append(f.updateHandlers[key], handler)
	f.mu.Unlock()
	return func() {}
}

func (f *fakeRuntime) dispatchUpdate(key SessionKey, sessionID string, update acp.SessionUpdate) {
	key = normalizeSessionKey(key)
	f.mu.Lock()
	handlers := append([]acp.UpdateHandler(nil), f.updateHandlers[key]...)
	f.mu.Unlock()
	for _, handler := range handlers {
		handler(sessionID, update)
	}
}

func (f *fakeRuntime) CloseRuntimeKey(key runtimeKey) error {
	key = normalizeRuntimeKey(key)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedRuntimeKeys = append(f.closedRuntimeKeys, key)
	return nil
}

func (f *fakeRuntime) TransitionCurrentSession(key SessionKey, expectedSessionID string, transition func() (Session, bool, error)) (Session, bool, error) {
	key = normalizeSessionKey(key)
	f.transitionMu.Lock()
	defer f.transitionMu.Unlock()
	f.mu.Lock()
	before := f.transitionBefore
	f.mu.Unlock()
	if before != nil {
		before()
	}
	f.mu.Lock()
	activeSessionID := f.activeSessionIDs[key]
	f.mu.Unlock()
	if activeSessionID != "" && activeSessionID != expectedSessionID {
		return Session{}, false, nil
	}
	session, changed, err := transition()
	if err != nil || !changed {
		return session, changed, err
	}
	session.Key = normalizeSessionKey(session.Key)
	f.mu.Lock()
	f.closedKeys = append(f.closedKeys, key)
	if f.activeSessionIDs == nil {
		f.activeSessionIDs = make(map[SessionKey]string)
	}
	f.activeSessionIDs[key] = session.ACPSessionID
	closeErr := f.transitionCloseErr
	f.mu.Unlock()
	return session, true, closeErr
}

func (f *fakeRuntime) CloseSession(key SessionKey) error {
	key = normalizeSessionKey(key)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedKeys = append(f.closedKeys, key)
	return nil
}

func (f *fakeRuntime) Shutdown(ctx context.Context) error {
	f.mu.Lock()
	f.shutdownCancelCount = len(f.cancelCalls)
	f.mu.Unlock()
	return nil
}

func (f *fakeRuntime) promptCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.promptCalls)
}

func (f *fakeRuntime) promptCallsSnapshot() []fakePromptCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePromptCall(nil), f.promptCalls...)
}

func (f *fakeRuntime) wikiRuntimeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.wikiRuntimeCalls)
}

func (f *fakeRuntime) wikiRuntimeCallsSnapshot() []fakePromptCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePromptCall(nil), f.wikiRuntimeCalls...)
}

func (f *fakeRuntime) atAutoRuntimeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.atAutoRuntimeCalls)
}

func (f *fakeRuntime) atAutoRuntimeCallsSnapshot() []fakePromptCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePromptCall(nil), f.atAutoRuntimeCalls...)
}

func (f *fakeRuntime) cancelCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cancelCalls)
}

type fakeStreamCard struct {
	mu                    sync.Mutex
	message               feishu.SentMessage
	textUpdates           []string
	finalTextUpdates      []string
	finalRenderContexts   []feishu.OutboundRenderContext
	metaUpdates           []feishu.StreamCardMeta
	processUpdates        []string
	statusUpdates         []string
	usageDetails          []string
	closed                bool
	failOnCanceledContext bool
}

func (f *fakeStreamCard) Message() feishu.SentMessage {
	if f == nil {
		return feishu.SentMessage{}
	}
	return f.message
}

func (f *fakeStreamCard) UpdateText(ctx context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.textUpdates = append(f.textUpdates, text)
	return nil
}

func (f *fakeStreamCard) SetFinalText(ctx context.Context, text string, render feishu.OutboundRenderContext) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.finalTextUpdates = append(f.finalTextUpdates, text)
	f.finalRenderContexts = append(f.finalRenderContexts, render)
	return nil
}

func (f *fakeStreamCard) UpdateMeta(ctx context.Context, meta feishu.StreamCardMeta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.metaUpdates = append(f.metaUpdates, meta)
	return nil
}

func (f *fakeStreamCard) UpdateProcess(ctx context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.processUpdates = append(f.processUpdates, text)
	return nil
}

func (f *fakeStreamCard) UpdateStatus(ctx context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.statusUpdates = append(f.statusUpdates, text)
	return nil
}

func (f *fakeStreamCard) UpdateUsageDetail(ctx context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.usageDetails = append(f.usageDetails, text)
	return nil
}

func (f *fakeStreamCard) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnCanceledContext && ctx.Err() != nil {
		return ctx.Err()
	}
	f.closed = true
	return nil
}

func (f *fakeStreamCard) textUpdatesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.textUpdates...)
}

func (f *fakeStreamCard) finalTextUpdatesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.finalTextUpdates...)
}

func (f *fakeStreamCard) finalRenderContextsSnapshot() []feishu.OutboundRenderContext {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]feishu.OutboundRenderContext(nil), f.finalRenderContexts...)
}

func (f *fakeStreamCard) metaUpdatesSnapshot() []feishu.StreamCardMeta {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]feishu.StreamCardMeta(nil), f.metaUpdates...)
}

func (f *fakeStreamCard) processUpdatesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.processUpdates...)
}

func (f *fakeStreamCard) usageDetailsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.usageDetails...)
}

func (f *fakeStreamCard) statusUpdatesSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.statusUpdates...)
}

func (f *fakeStreamCard) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}
