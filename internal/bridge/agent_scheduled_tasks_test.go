package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestCreateAgentScheduledTaskPersistsBoundSessionLimits(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(workspaceLocalPath(workspace, "sessions.json"))
	cfg := config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
		Bots: []config.BotConfig{{
			ID: "bot-a", Workspace: workspace, OwnerOpenIDs: []string{"ou_owner"},
		}},
	}
	svc := NewService(cfg, store).WithConfigPath(t.TempDir() + "/config.json")
	defer svc.stopScheduledTasks()
	key := imSessionKey("bot-a", "oc_chat", "")
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: workspace, Workspace: workspace}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	msg := feishu.Message{BotID: "bot-a", ChatID: "oc_chat", ChatType: "group", MessageID: "om_create", SenderID: "ou_owner"}
	capability, prompt := svc.issueAgentTaskCapability(msg, session)
	if capability == "" || !strings.Contains(prompt, "task create --stdin") {
		t.Fatalf("capability=%q prompt=%q, want agent task tool", capability, prompt)
	}
	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	result, err := svc.CreateAgentScheduledTask(context.Background(), AgentScheduledTaskCreateRequest{
		Capability: capability,
		Schedule:   "@every 5m",
		Prompt:     "检查服务状态",
		MaxRuns:    12,
		ExpiresAt:  expiresAt.Format(time.RFC3339),
		RunTimeout: "4m",
	})
	if err != nil {
		t.Fatalf("CreateAgentScheduledTask() error = %v", err)
	}
	task := result.Task
	if task.SessionMode != scheduleSessionModeBound || task.BoundSessionKey != normalizeSessionKey(key) || task.BoundACPSessionID != session.ACPSessionID {
		t.Fatalf("task binding = %+v, want current ACP session", task)
	}
	if task.MaxRuns != 12 || task.RunCount != 0 || task.RunTimeoutSec != 240 || task.ExpiresAt == nil || !task.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("task limits = %+v, want max=12 expires=%s timeout=240", task, expiresAt)
	}
	stored, ok := svc.scheduledTaskStoreForBotID("bot-a").Get(task.ID)
	if !ok || stored.BoundACPSessionID != session.ACPSessionID {
		t.Fatalf("stored task = %+v ok=%v", stored, ok)
	}
}

func TestCreateAgentScheduledTaskRequiresBoundedLifetime(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(workspaceLocalPath(workspace, "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace, OwnerOpenIDs: []string{"ou_owner"}}},
	}, store).WithConfigPath(t.TempDir() + "/config.json")
	key := imSessionKey("bot-a", "oc_chat", "")
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: workspace, Workspace: workspace}
	if err := store.Upsert(session); err != nil {
		t.Fatal(err)
	}
	capability, _ := svc.issueAgentTaskCapability(feishu.Message{BotID: "bot-a", ChatID: "oc_chat", SenderID: "ou_owner"}, session)
	_, err := svc.CreateAgentScheduledTask(context.Background(), AgentScheduledTaskCreateRequest{
		Capability: capability,
		Schedule:   "@every 5m",
		Prompt:     "检查服务状态",
	})
	if err == nil || !strings.Contains(err.Error(), "至少提供一个") {
		t.Fatalf("error = %v, want bounded lifetime rejection", err)
	}
}

func TestCreateAgentScheduledTaskRejectsAtSpec(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(workspaceLocalPath(workspace, "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace, OwnerOpenIDs: []string{"ou_owner"}}},
	}, store).WithConfigPath(t.TempDir() + "/config.json")
	key := imSessionKey("bot-a", "oc_chat", "")
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: workspace, Workspace: workspace}
	if err := store.Upsert(session); err != nil {
		t.Fatal(err)
	}
	capability, _ := svc.issueAgentTaskCapability(feishu.Message{BotID: "bot-a", ChatID: "oc_chat", SenderID: "ou_owner"}, session)
	_, err := svc.CreateAgentScheduledTask(context.Background(), AgentScheduledTaskCreateRequest{
		Capability: capability,
		Schedule:   "@at " + time.Now().Add(time.Hour).Format(time.RFC3339),
		Prompt:     "检查服务状态",
		MaxRuns:    1,
	})
	if err == nil || !strings.Contains(err.Error(), "不支持 @at") {
		t.Fatalf("error = %v, want @at rejection", err)
	}
}

func TestRunBoundScheduledTaskUsesOriginalACPSession(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(workspaceLocalPath(workspace, "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace}},
	}, store)
	rt := &fakeRuntime{promptReply: "检查完成"}
	svc.setRuntime(rt)
	key := imSessionKey("bot-a", "oc_chat", "")
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: workspace, Workspace: workspace}
	if err := store.Upsert(session); err != nil {
		t.Fatal(err)
	}
	task := ScheduledTask{
		ID: "task-bound", BotID: "bot-a", Enabled: true, Spec: "@every 5m", AgentName: "traex", Cwd: workspace,
		Prompt: "检查服务状态", SessionMode: scheduleSessionModeBound, BoundSessionKey: key, BoundACPSessionID: session.ACPSessionID,
		CreatorOpenID: "ou_owner", CreatedFromChatID: "oc_chat", CreatedFromChatType: "group", CreatedFromMessageID: "om_create",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := svc.runBoundScheduledTask(ctx, task, "run-1", time.Now())
	if err != nil {
		t.Fatalf("runBoundScheduledTask() error = %v", err)
	}
	if result.ACPSessionID != session.ACPSessionID || result.Text != "检查完成" {
		t.Fatalf("result = %+v, want original session reply", result)
	}
	if len(rt.promptCalls) != 1 || rt.promptCalls[0].Session.ACPSessionID != session.ACPSessionID {
		t.Fatalf("promptCalls = %+v, want original ACP session", rt.promptCalls)
	}
	if !strings.Contains(rt.promptCalls[0].Text, "## Schedule Metadata") || !strings.Contains(rt.promptCalls[0].Text, "检查服务状态") {
		t.Fatalf("prompt = %q, want schedule metadata and task prompt", rt.promptCalls[0].Text)
	}
}

func TestBoundScheduledTaskDrainsAfterLoopOrWikiTaskFinishes(t *testing.T) {
	for _, kind := range []taskKind{taskKindLoop, taskKindWiki} {
		t.Run(string(kind), func(t *testing.T) {
			workspace := t.TempDir()
			store := NewSessionStore(workspaceLocalPath(workspace, "sessions.json"))
			svc := NewService(config.Config{
				AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
				Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace}},
			}, store)
			rt := &fakeRuntime{promptReply: "检查完成"}
			svc.setRuntime(rt)
			key := imSessionKey("bot-a", "oc_chat", "")
			session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: workspace, Workspace: workspace}
			if err := store.Upsert(session); err != nil {
				t.Fatal(err)
			}
			_, finishBlockingTask := svc.startTask(context.Background(), session, config.AgentConfig{Command: "traex"}, kind)
			task := ScheduledTask{
				ID: "task-bound", BotID: "bot-a", Enabled: true, Spec: "@every 5m", AgentName: "traex", Cwd: workspace,
				Prompt: "检查服务状态", SessionMode: scheduleSessionModeBound, BoundSessionKey: key, BoundACPSessionID: session.ACPSessionID,
				CreatorOpenID: "ou_owner", CreatedFromChatID: "oc_chat", CreatedFromChatType: "group", CreatedFromMessageID: "om_create",
			}
			resultCh := make(chan error, 1)
			go func() {
				_, err := svc.runBoundScheduledTask(context.Background(), task, "run-1", time.Now())
				resultCh <- err
			}()
			waitForCondition(t, time.Second, func() bool {
				svc.taskMu.Lock()
				defer svc.taskMu.Unlock()
				queue := svc.promptQueues[normalizeSessionKey(key)]
				return queue != nil && len(queue.items) == 1 && !queue.draining
			})
			if rt.promptCallCount() != 0 {
				t.Fatalf("prompt calls = %d, want bound run queued behind %s task", rt.promptCallCount(), kind)
			}
			finishBlockingTask()
			select {
			case err := <-resultCh:
				if err != nil {
					t.Fatalf("runBoundScheduledTask() error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatalf("bound run remained queued after %s task finished", kind)
			}
			if rt.promptCallCount() != 1 {
				t.Fatalf("prompt calls = %d, want queued bound run consumed", rt.promptCallCount())
			}
		})
	}
}

func TestBoundQueuedPromptRejectsChangedSessionAndDoesNotRecover(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(workspaceLocalPath(workspace, "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace}},
	}, store)
	rt := &fakeRuntime{promptErrors: []error{errACPSessionUnavailable}}
	svc.setRuntime(rt)
	key := imSessionKey("bot-a", "oc_chat", "")
	original := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-original", Cwd: workspace, Workspace: workspace}
	if err := store.Upsert(original); err != nil {
		t.Fatal(err)
	}
	item := queuedPrompt{
		msg:                  feishu.Message{BotID: "bot-a", ChatID: "oc_chat"},
		session:              original,
		agent:                config.AgentConfig{Command: "traex"},
		text:                 "scheduled prompt",
		textWrapped:          true,
		requiredACPSessionID: original.ACPSessionID,
	}
	if _, err := svc.promptQueuedItem(context.Background(), item); !errors.Is(err, errACPSessionUnavailable) {
		t.Fatalf("promptQueuedItem() error = %v, want session unavailable without recovery", err)
	}
	if len(rt.newCalls) != 0 {
		t.Fatalf("newCalls = %+v, bound prompt must not rebuild a session", rt.newCalls)
	}

	changed := original
	changed.ACPSessionID = "acp-current"
	if err := store.Upsert(changed); err != nil {
		t.Fatal(err)
	}
	before := rt.promptCallCount()
	if _, err := svc.promptQueuedItem(context.Background(), item); err == nil || !strings.Contains(err.Error(), "已不再是当前会话") {
		t.Fatalf("promptQueuedItem() error = %v, want changed session rejection", err)
	}
	if got := rt.promptCallCount(); got != before {
		t.Fatalf("prompt calls = %d, want no prompt after session changed", got)
	}
}

func TestBoundScheduledTaskReplyFailureMarksRunFailed(t *testing.T) {
	workspace := t.TempDir()
	store := NewSessionStore(workspaceLocalPath(workspace, "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace}},
	}, store)
	svc.setRuntime(&fakeRuntime{promptReply: "检查完成"})
	client := newFakeSentMessageClient("")
	client.replySender = func(context.Context, feishu.Message, string) error { return errors.New("send failed") }
	svc.setOutbound("bot-a", client)
	key := imSessionKey("bot-a", "oc_chat", "")
	session := Session{Key: key, AgentName: "traex", ACPSessionID: "acp-session-1", Cwd: workspace, Workspace: workspace}
	if err := store.Upsert(session); err != nil {
		t.Fatal(err)
	}
	task := ScheduledTask{
		ID: "task-bound", BotID: "bot-a", Enabled: true, Spec: "@every 5m", AgentName: "traex", Cwd: workspace,
		Prompt: "检查服务状态", SessionMode: scheduleSessionModeBound, BoundSessionKey: key, BoundACPSessionID: session.ACPSessionID,
		CreatorOpenID: "ou_owner", CreatedFromChatID: "oc_chat", CreatedFromChatType: "group", CreatedFromMessageID: "om_create",
	}
	run, err := svc.runScheduledTaskOnce(context.Background(), task, "run-1", time.Now(), workspace, nil)
	if err == nil || !strings.Contains(err.Error(), "send failed") {
		t.Fatalf("runScheduledTaskOnce() error = %v, want reply failure", err)
	}
	if run.Status.State != scheduleRunFailed {
		t.Fatalf("run status = %+v, want failed", run.Status)
	}
}

func TestScheduledTaskStopReasonUsesRunLimitAndDeadline(t *testing.T) {
	if got := scheduledTaskStopReason(ScheduledTask{MaxRuns: 2, RunCount: 2}, time.Now()); got != "已达到最大执行次数" {
		t.Fatalf("stop reason = %q", got)
	}
	past := time.Now().Add(-time.Second)
	if got := scheduledTaskStopReason(ScheduledTask{ExpiresAt: &past}, time.Now()); got != "已达到截止时间" {
		t.Fatalf("stop reason = %q", got)
	}
}

func TestBoundScheduledTaskCancellationDoesNotTargetSharedSession(t *testing.T) {
	task := ScheduledTask{
		ID:                "task-bound",
		BotID:             "bot-a",
		SessionMode:       scheduleSessionModeBound,
		BoundSessionKey:   imSessionKey("bot-a", "oc_chat", ""),
		BoundACPSessionID: "acp-session-1",
	}
	if got := scheduledTaskExecutionKey(task, "run-1"); got.Valid() {
		t.Fatalf("scheduledTaskExecutionKey() = %+v, want empty key so cancellation only uses the run context", got)
	}
}
