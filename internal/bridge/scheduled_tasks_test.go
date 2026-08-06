package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestScheduledTaskStoreUpsertPersistsAndLoadsTasks(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "scheduled_tasks.json")
	store := NewScheduledTaskStore(storePath)

	second, err := store.Upsert(ScheduledTask{
		ID:                   " task-b ",
		BotID:                " bot-a ",
		Enabled:              true,
		Spec:                 " 0 9 * * * ",
		Timezone:             " Asia/Shanghai ",
		AgentName:            " traex ",
		Cwd:                  " /repo ",
		Prompt:               " run daily report ",
		CreatorOpenID:        " ou_creator ",
		CreatedFromChatID:    " oc_chat ",
		CreatedFromThreadID:  " omt_thread ",
		CreatedFromMessageID: " om_message ",
		ResultSink: ScheduledTaskResultSink{
			Type:      " im ",
			ChatID:    " oc_chat ",
			ThreadID:  " omt_thread ",
			MessageID: " om_message ",
		},
	})
	if err != nil {
		t.Fatalf("Upsert(second) error = %v", err)
	}
	if second.ID != "task-b" || second.BotID != "bot-a" || second.Spec != "0 9 * * *" || second.AgentName != "traex" || second.Cwd != "/repo" || second.Prompt != "run daily report" {
		t.Fatalf("normalized task = %+v, want trimmed fields", second)
	}
	if second.OverlapPolicy != scheduleOverlapSkipIfRunning {
		t.Fatalf("OverlapPolicy = %q, want default skip_if_running", second.OverlapPolicy)
	}
	if second.CreatedAt.IsZero() || second.UpdatedAt.IsZero() {
		t.Fatalf("timestamps = %s/%s, want populated", second.CreatedAt, second.UpdatedAt)
	}

	first, err := store.Upsert(ScheduledTask{
		ID:            "task-a",
		BotID:         "bot-a",
		Enabled:       true,
		Spec:          "*/10 * * * *",
		Timezone:      "UTC",
		AgentName:     "claude",
		Cwd:           "/repo",
		Prompt:        "run quick check",
		OverlapPolicy: "skip_if_running",
	})
	if err != nil {
		t.Fatalf("Upsert(first) error = %v", err)
	}
	if first.ID != "task-a" {
		t.Fatalf("first ID = %q, want task-a", first.ID)
	}

	tasks := store.List()
	if len(tasks) != 2 || tasks[0].ID != "task-a" || tasks[1].ID != "task-b" {
		t.Fatalf("List() = %+v, want tasks sorted by id", tasks)
	}

	data, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("ReadFile(scheduled_tasks.json) error = %v", err)
	}
	var file scheduledTaskFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("Unmarshal(scheduled_tasks.json) error = %v", err)
	}
	if file.Version != 1 {
		t.Fatalf("file version = %d, want 1", file.Version)
	}
	if len(file.Tasks) != 2 || file.Tasks[0].ID != "task-a" || file.Tasks[1].ID != "task-b" {
		t.Fatalf("file tasks = %+v, want stable id order", file.Tasks)
	}

	reloaded := NewScheduledTaskStore(storePath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	loaded, ok := reloaded.Get("task-b")
	if !ok {
		t.Fatal("Get(task-b) ok = false, want true")
	}
	if loaded.ResultSink.Type != "im" || loaded.ResultSink.ChatID != "oc_chat" || loaded.CreatedFromMessageID != "om_message" {
		t.Fatalf("loaded task = %+v, want persisted source and sink fields", loaded)
	}
}

func TestScheduledTaskStoreLoadsLegacyPathAndWritesLocalPath(t *testing.T) {
	workspace := t.TempDir()
	legacyPath := filepath.Join(workspace, "scheduled_tasks.json")
	localPath := filepath.Join(workspace, ".local", "scheduled_tasks.json")
	legacy := NewScheduledTaskStore(legacyPath)
	if _, err := legacy.Upsert(ScheduledTask{
		ID:        "task-a",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "@every 1h",
		AgentName: "traex",
		Cwd:       "/repo",
		Prompt:    "run task",
	}); err != nil {
		t.Fatalf("Upsert(legacy) error = %v", err)
	}

	store := NewScheduledTaskStoreWithFallback(localPath, legacyPath)
	if err := store.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if task, ok := store.Get("task-a"); !ok || task.Prompt != "run task" {
		t.Fatalf("legacy task = %+v ok=%v, want loaded", task, ok)
	}
	if _, err := store.Upsert(ScheduledTask{
		ID:        "task-b",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "@every 2h",
		AgentName: "traex",
		Cwd:       "/repo",
		Prompt:    "run second task",
	}); err != nil {
		t.Fatalf("Upsert(local) error = %v", err)
	}
	if _, err := os.Stat(localPath); err != nil {
		t.Fatalf("local scheduled tasks file err = %v, want created", err)
	}
}

func TestScheduledTaskStoreRejectsIncompleteTask(t *testing.T) {
	store := NewScheduledTaskStore(filepath.Join(t.TempDir(), "scheduled_tasks.json"))
	_, err := store.Upsert(ScheduledTask{
		ID:        "task-a",
		BotID:     "bot-a",
		Spec:      "0 9 * * *",
		AgentName: "traex",
		Cwd:       "/repo",
	})
	if err == nil {
		t.Fatal("Upsert() error = nil, want incomplete task rejection")
	}
	if len(store.List()) != 0 {
		t.Fatalf("List() = %+v, want no task after rejected upsert", store.List())
	}
}

func TestScheduledTaskStoreLoadMissingFileClearsState(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "scheduled_tasks.json")
	store := NewScheduledTaskStore(storePath)
	if _, err := store.Upsert(ScheduledTask{
		ID:            "task-a",
		BotID:         "bot-a",
		Enabled:       true,
		Spec:          "0 9 * * *",
		Timezone:      "UTC",
		AgentName:     "traex",
		Cwd:           "/repo",
		Prompt:        "run daily report",
		OverlapPolicy: "skip_if_running",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if err := os.Remove(storePath); err != nil {
		t.Fatalf("Remove(scheduled_tasks.json) error = %v", err)
	}
	if err := store.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(store.List()) != 0 {
		t.Fatalf("List() = %+v, want empty after missing file load", store.List())
	}
}

func TestScheduledTaskStoreWriteFailureRestoresMemoryState(t *testing.T) {
	dir := t.TempDir()
	store := NewScheduledTaskStore(filepath.Join(dir, "scheduled_tasks.json"))
	original, err := store.Upsert(ScheduledTask{
		ID:        "task-a",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "0 9 * * *",
		Timezone:  "UTC",
		AgentName: "traex",
		Cwd:       "/repo",
		Prompt:    "run daily report",
	})
	if err != nil {
		t.Fatalf("Upsert(original) error = %v", err)
	}
	blockingPath := filepath.Join(dir, "blocking")
	if err := os.WriteFile(blockingPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile(blocking) error = %v", err)
	}
	store.path = filepath.Join(blockingPath, "scheduled_tasks.json")

	_, err = store.Upsert(ScheduledTask{
		ID:        "task-b",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "*/10 * * * *",
		Timezone:  "UTC",
		AgentName: "traex",
		Cwd:       "/repo",
		Prompt:    "run quick check",
	})
	if err == nil {
		t.Fatal("Upsert(task-b) error = nil, want write failure")
	}
	if _, ok := store.Get("task-b"); ok {
		t.Fatal("task-b exists after failed write, want rollback")
	}
	got, ok := store.Get("task-a")
	if !ok {
		t.Fatal("task-a missing after failed write")
	}
	if got.ID != original.ID || got.CreatedAt != original.CreatedAt {
		t.Fatalf("task-a = %+v, want original %+v", got, original)
	}
}

func TestScheduledTaskTriggerRequestBuildsExplicitNonIMRequest(t *testing.T) {
	triggeredAt := time.Date(2026, 7, 30, 9, 8, 7, 0, time.FixedZone("CST", 8*60*60))
	sink := noopTriggerSink{}
	task := ScheduledTask{
		ID:                   " daily ",
		BotID:                " bot-a ",
		Enabled:              true,
		Spec:                 " 0 9 * * * ",
		Timezone:             " Asia/Shanghai ",
		AgentName:            " traex ",
		Cwd:                  " /repo ",
		Prompt:               " generate report ",
		CreatorOpenID:        " ou_creator ",
		CreatedFromChatID:    " oc_chat ",
		CreatedFromThreadID:  " omt_thread ",
		CreatedFromMessageID: " om_message ",
		ResultSink: ScheduledTaskResultSink{
			Type:      " im ",
			ChatID:    " oc_chat ",
			ThreadID:  " omt_thread ",
			MessageID: " om_message ",
		},
	}

	req, err := scheduledTaskTriggerRequest(task, " run-1 ", triggeredAt, " /workspace ", sink)
	if err != nil {
		t.Fatalf("scheduledTaskTriggerRequest() error = %v", err)
	}
	wantKey := SessionKey{BotID: "bot-a", Source: sessionSourceSchedule, MainID: "task:daily", SubID: "run:run-1"}
	if req.Key != wantKey {
		t.Fatalf("Key = %+v, want %+v", req.Key, wantKey)
	}
	if req.BotID != "bot-a" || req.Workspace != "/workspace" || req.AgentName != "traex" || req.Cwd != "/repo" || req.Title != "daily" {
		t.Fatalf("request = %+v, want explicit normalized task fields", req)
	}
	if req.EnableWikiReflection {
		t.Fatal("EnableWikiReflection = true, want schedule trigger disabled by default")
	}
	if req.Sink == nil {
		t.Fatal("Sink = nil, want provided sink")
	}
	if req.Metadata["source"] != sessionSourceSchedule || req.Metadata["task_id"] != "daily" || req.Metadata["run_id"] != "run-1" {
		t.Fatalf("Metadata = %+v, want schedule run metadata", req.Metadata)
	}
	if req.Metadata["triggered_at"] != "2026-07-30T09:08:07+08:00" {
		t.Fatalf("triggered_at = %q, want RFC3339 timestamp", req.Metadata["triggered_at"])
	}
	for _, want := range []string{"## Schedule Metadata", "```json", `"task_id": "daily"`, `"run_id": "run-1"`, `"schedule_spec": "0 9 * * *"`, "## User Message", "generate report"} {
		if !strings.Contains(req.Prompt, want) {
			t.Fatalf("Prompt = %q, want %q", req.Prompt, want)
		}
	}
}

func TestScheduledTaskTriggerRequestRejectsMissingRunContext(t *testing.T) {
	task := ScheduledTask{
		ID:        "daily",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "0 9 * * *",
		AgentName: "traex",
		Cwd:       "/repo",
		Prompt:    "generate report",
	}
	if _, err := scheduledTaskTriggerRequest(task, "", time.Now(), "/workspace", nil); err == nil {
		t.Fatal("scheduledTaskTriggerRequest(empty run id) error = nil, want rejection")
	}
	if _, err := scheduledTaskTriggerRequest(task, "run-1", time.Now(), "", nil); err == nil {
		t.Fatal("scheduledTaskTriggerRequest(empty workspace) error = nil, want rejection")
	}
}

func TestNewServiceCreatesScheduledTaskStoresForBotWorkspaces(t *testing.T) {
	workspaceA := filepath.Join(t.TempDir(), "bot-a")
	workspaceB := filepath.Join(t.TempDir(), "bot-b")
	cfg := config.Default()
	cfg.Bots = []config.BotConfig{
		{ID: "bot-a", AppID: "cli_a", AppSecret: config.FileSecret("bot-a.appsecret"), Workspace: workspaceA},
		{ID: "bot-b", AppID: "cli_b", AppSecret: config.FileSecret("bot-b.appsecret"), Workspace: workspaceB},
		{ID: "bot-c", AppID: "cli_c", AppSecret: config.FileSecret("bot-c.appsecret")},
	}

	svc := NewService(cfg, nil)
	storeA := svc.scheduledTaskStoreForBotID(" bot-a ")
	if storeA == nil {
		t.Fatal("schedule store for bot-a = nil, want workspace store")
	}
	if storeA.path != filepath.Join(workspaceA, ".local", "scheduled_tasks.json") {
		t.Fatalf("bot-a schedule store path = %q, want workspace .local scheduled_tasks.json", storeA.path)
	}
	storeB := svc.scheduledTaskStoreForBotID("bot-b")
	if storeB == nil || storeB.path != filepath.Join(workspaceB, ".local", "scheduled_tasks.json") {
		t.Fatalf("bot-b schedule store = %+v, want workspace .local scheduled_tasks.json", storeB)
	}
	if got := svc.scheduledTaskStoreForBotID("bot-c"); got != nil {
		t.Fatalf("schedule store for bot-c = %+v, want nil without workspace", got)
	}
	if got := svc.scheduledTaskStoreForBotID("missing"); got != nil {
		t.Fatalf("schedule store for missing bot = %+v, want nil without default schedule store", got)
	}
}

func TestParseScheduleSpecSupportsEveryAndCron(t *testing.T) {
	every, err := parseScheduleSpec("@every 10m", "")
	if err != nil {
		t.Fatalf("parseScheduleSpec(@every) error = %v", err)
	}
	next, ok := every.Next(time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC))
	if !ok || next != time.Date(2026, 7, 30, 9, 10, 0, 0, time.UTC) {
		t.Fatalf("@every next = %s ok=%v, want +10m", next, ok)
	}

	cron, err := parseScheduleSpec("*/15 9-10 * * 1-5", "Asia/Shanghai")
	if err != nil {
		t.Fatalf("parseScheduleSpec(cron) error = %v", err)
	}
	after := time.Date(2026, 7, 30, 9, 8, 0, 0, time.FixedZone("CST", 8*60*60))
	next, ok = cron.Next(after)
	if !ok {
		t.Fatal("cron Next() ok = false, want true")
	}
	if next.Format(time.RFC3339) != "2026-07-30T09:15:00+08:00" {
		t.Fatalf("cron next = %s, want 2026-07-30T09:15:00+08:00", next.Format(time.RFC3339))
	}

	cron, err = parseScheduleSpec("0 0 1 * 1", "UTC")
	if err != nil {
		t.Fatalf("parseScheduleSpec(cron day-or-weekday) error = %v", err)
	}
	next, ok = cron.Next(time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("cron day-or-weekday Next() ok = false, want true")
	}
	if next != time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("cron day-or-weekday next = %s, want next Monday before next first day", next)
	}

	if _, err := parseScheduleSpec("bad spec", ""); err == nil {
		t.Fatal("parseScheduleSpec(bad spec) error = nil, want validation error")
	}
}

func TestStartLoadsEnabledScheduledTasksAndTriggersRuns(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	cfg := config.Config{
		Bots: []config.BotConfig{{
			ID:        "bot-a",
			Workspace: workspace,
		}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}
	storePath := filepath.Join(workspace, "scheduled_tasks.json")
	store := NewScheduledTaskStore(storePath)
	if _, err := store.Upsert(ScheduledTask{
		ID:        "enabled",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "@every 20ms",
		AgentName: "traex",
		Cwd:       cwd,
		Prompt:    "run enabled task",
	}); err != nil {
		t.Fatalf("Upsert(enabled) error = %v", err)
	}
	if _, err := store.Upsert(ScheduledTask{
		ID:        "disabled",
		BotID:     "bot-a",
		Enabled:   false,
		Spec:      "@every 20ms",
		AgentName: "traex",
		Cwd:       cwd,
		Prompt:    "run disabled task",
	}); err != nil {
		t.Fatalf("Upsert(disabled) error = %v", err)
	}

	svc := NewService(cfg, nil)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-schedule"},
		promptReply:    "done",
	}
	svc.setRuntime(rt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if got := svc.scheduledTaskJobCount(); got != 1 {
		t.Fatalf("scheduledTaskJobCount() = %d, want only enabled task registered", got)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() >= 1 })
	cancel()

	rt.mu.Lock()
	promptCalls := append([]fakePromptCall(nil), rt.promptCalls...)
	newCalls := append([]fakeNewCall(nil), rt.newCalls...)
	rt.mu.Unlock()
	if len(promptCalls) == 0 || !strings.Contains(promptCalls[0].Text, "run enabled task") {
		t.Fatalf("promptCalls = %+v, want enabled scheduled task prompt", promptCalls)
	}
	if strings.Contains(promptCalls[0].Text, "run disabled task") {
		t.Fatalf("promptCalls[0].Text = %q, disabled task should not run", promptCalls[0].Text)
	}
	if len(newCalls) == 0 || newCalls[0].Key.Source != sessionSourceSchedule || newCalls[0].Key.MainID != "task:enabled" || newCalls[0].Workspace != workspace {
		t.Fatalf("newCalls = %+v, want schedule session for enabled task in workspace", newCalls)
	}
}

func TestScheduleRunStatusTransitions(t *testing.T) {
	svc := NewService(config.Config{}, nil)
	task := ScheduledTask{
		ID:        "daily",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "0 9 * * *",
		AgentName: "traex",
		Cwd:       "/repo",
		Prompt:    "generate report",
	}
	key := scheduledTaskRunKey(task, "run-1")
	started := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)

	pending := svc.markScheduleRunPending(task, "run-1", key, started)
	if pending.State != scheduleRunPending || !pending.StartedAt.Equal(started) || pending.SessionKey != key {
		t.Fatalf("pending status = %+v, want pending with key and start time", pending)
	}
	stored, ok := svc.scheduleRunStatus("daily", "run-1")
	if !ok || stored.State != scheduleRunPending {
		t.Fatalf("stored pending status = %+v ok=%v, want pending", stored, ok)
	}

	running := svc.markScheduleRunRunning(task, "run-1", key, started.Add(time.Minute), " prompt ")
	if running.State != scheduleRunRunning || running.TriggerText != "prompt" {
		t.Fatalf("running status = %+v, want running with prompt", running)
	}

	completed := svc.markScheduleRunFinished("daily", "run-1", started.Add(2*time.Minute), nil)
	if completed.State != scheduleRunCompleted || !completed.EndedAt.Equal(started.Add(2*time.Minute)) || completed.LastError != "" {
		t.Fatalf("completed status = %+v, want completed without error", completed)
	}

	failedErr := errors.New("boom")
	failed := svc.markScheduleRunFinished("daily", "run-2", started.Add(3*time.Minute), failedErr)
	if failed.State != scheduleRunFailed || failed.LastError != "boom" {
		t.Fatalf("failed status = %+v, want failed with error", failed)
	}

	cancelled := svc.markScheduleRunFinished("daily", "run-3", started.Add(4*time.Minute), context.Canceled)
	if cancelled.State != scheduleRunCancelled || cancelled.LastError != "" {
		t.Fatalf("cancelled status = %+v, want cancelled without last error", cancelled)
	}

	skipped := svc.markScheduleRunSkipped(task, "run-4", scheduledTaskRunKey(task, "run-4"), started.Add(5*time.Minute), "still running")
	if skipped.State != scheduleRunSkipped || skipped.SkipReason != "still running" || skipped.EndedAt.IsZero() || skipped.SkippedAt.IsZero() {
		t.Fatalf("skipped status = %+v, want skipped with reason and timestamps", skipped)
	}
}

func TestScheduleRunHistoryPrunesOldFinishedRunsAndKeepsRunning(t *testing.T) {
	svc := NewService(config.Config{}, nil)
	task := ScheduledTask{
		ID:        "daily",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "0 9 * * *",
		AgentName: "traex",
		Cwd:       "/repo",
		Prompt:    "generate report",
	}
	started := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	svc.markScheduleRunRunning(task, "still-running", scheduledTaskRunKey(task, "still-running"), started, "running")
	for i := 0; i < scheduleRunHistoryLimit+5; i++ {
		runID := "run-" + strconv.Itoa(i)
		svc.markScheduleRunSkipped(task, runID, scheduledTaskRunKey(task, runID), started.Add(time.Duration(i+1)*time.Minute), "done")
	}

	svc.taskMu.Lock()
	defer svc.taskMu.Unlock()
	if len(svc.scheduleRuns) != scheduleRunHistoryLimit+1 {
		t.Fatalf("scheduleRuns len = %d, want %d finished history + running", len(svc.scheduleRuns), scheduleRunHistoryLimit+1)
	}
	if _, ok := svc.scheduleRuns[scheduleRunStatusID("daily", "still-running")]; !ok {
		t.Fatal("running status pruned, want preserved")
	}
	if _, ok := svc.scheduleRuns[scheduleRunStatusID("daily", "run-0")]; ok {
		t.Fatal("oldest finished status still exists, want pruned")
	}
	if _, ok := svc.scheduleRuns[scheduleRunStatusID("daily", "run-104")]; !ok {
		t.Fatal("newest finished status missing, want retained")
	}
}

type noNextScheduleSpec struct{}

func (noNextScheduleSpec) Next(time.Time) (time.Time, bool) {
	return time.Time{}, false
}

func TestRunScheduledTaskJobRemovesDeadJobAndRecordsFailure(t *testing.T) {
	svc := NewService(config.Config{}, nil)
	task := ScheduledTask{
		ID:        "impossible",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "0 0 31 2 *",
		AgentName: "traex",
		Cwd:       "/repo",
		Prompt:    "generate report",
	}
	job := &scheduledTaskJob{task: task, schedule: noNextScheduleSpec{}}
	svc.taskMu.Lock()
	svc.scheduleJobs[scheduledTaskJobID(task)] = job
	svc.taskMu.Unlock()

	svc.runScheduledTaskJob(context.Background(), job)

	if got := svc.scheduledTaskJobCount(); got != 0 {
		t.Fatalf("scheduledTaskJobCount() = %d, want dead job removed", got)
	}
	last, ok := svc.lastScheduleRunStatus(task.ID)
	if !ok {
		t.Fatal("lastScheduleRunStatus ok = false, want recorded failure")
	}
	if last.State != scheduleRunFailed || !strings.Contains(last.LastError, "无法计算下次触发时间") {
		t.Fatalf("last status = %+v, want failed next-time error", last)
	}
}

func TestRunScheduledTaskOnceExecutesTriggerAndRecordsCompleted(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-run-1"},
		promptReply:    "schedule done",
	}
	svc.setRuntime(rt)
	task := ScheduledTask{
		ID:        "daily",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "0 9 * * *",
		Timezone:  "Asia/Shanghai",
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "generate report",
	}
	workspace := t.TempDir()
	triggeredAt := time.Date(2026, 7, 30, 9, 0, 0, 0, time.FixedZone("CST", 8*60*60))

	run, err := svc.runScheduledTaskOnce(context.Background(), task, "run-1", triggeredAt, workspace, nil)
	if err != nil {
		t.Fatalf("runScheduledTaskOnce() error = %v", err)
	}
	wantKey := SessionKey{BotID: "bot-a", Source: sessionSourceSchedule, MainID: "task:daily", SubID: "run:run-1"}
	if run.TriggerResult.Session.Key != wantKey || run.TriggerResult.Text != "schedule done" {
		t.Fatalf("trigger result = %+v, want schedule session and reply", run.TriggerResult)
	}
	if run.Status.State != scheduleRunCompleted || run.Status.LastError != "" {
		t.Fatalf("run status = %+v, want completed", run.Status)
	}
	if len(rt.newCalls) != 1 || rt.newCalls[0].Key != wantKey || rt.newCalls[0].Workspace != workspace {
		t.Fatalf("newCalls = %+v, want one schedule ACP session", rt.newCalls)
	}
	if len(rt.promptCalls) != 1 || !strings.Contains(rt.promptCalls[0].Text, "## Schedule Metadata") || !strings.Contains(rt.promptCalls[0].Text, "generate report") {
		t.Fatalf("promptCalls = %+v, want schedule metadata prompt", rt.promptCalls)
	}
	if _, ok := store.Get(wantKey); !ok {
		t.Fatal("schedule run session not persisted")
	}
}

func TestRunScheduledTaskOnceUsesDistinctRunSessionKeys(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{
		newSessionIDs: []string{"acp-run-1", "acp-run-2"},
		promptReply:   "done",
	}
	svc.setRuntime(rt)
	task := ScheduledTask{
		ID:        "daily",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "0 9 * * *",
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "generate report",
	}
	workspace := t.TempDir()

	if _, err := svc.runScheduledTaskOnce(context.Background(), task, "run-1", time.Now(), workspace, nil); err != nil {
		t.Fatalf("runScheduledTaskOnce(run-1) error = %v", err)
	}
	if _, err := svc.runScheduledTaskOnce(context.Background(), task, "run-2", time.Now(), workspace, nil); err != nil {
		t.Fatalf("runScheduledTaskOnce(run-2) error = %v", err)
	}
	firstKey := SessionKey{BotID: "bot-a", Source: sessionSourceSchedule, MainID: "task:daily", SubID: "run:run-1"}
	secondKey := SessionKey{BotID: "bot-a", Source: sessionSourceSchedule, MainID: "task:daily", SubID: "run:run-2"}
	if len(rt.newCalls) != 2 || rt.newCalls[0].Key != firstKey || rt.newCalls[1].Key != secondKey {
		t.Fatalf("newCalls = %+v, want distinct run sub ids", rt.newCalls)
	}
	if _, ok := store.Get(firstKey); !ok {
		t.Fatal("run-1 session missing")
	}
	if _, ok := store.Get(secondKey); !ok {
		t.Fatal("run-2 session missing")
	}
}

func TestRunScheduledTaskOnceUsesPersistedTaskConfigNotIMChatConfig(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err := store.UpsertChat(ChatConfig{
		Key:       ChatKey{BotID: "bot-a", ChatID: "oc_chat"},
		AgentName: "claude",
	}); err != nil {
		t.Fatalf("UpsertChat() error = %v", err)
	}
	taskCwd := t.TempDir()
	chatDefaultCwd := t.TempDir()
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{
			{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}},
			{Name: "claude", AgentConfig: config.AgentConfig{Command: "claude", DefaultCwd: chatDefaultCwd}},
		},
	}, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-run-1"},
		promptReply:    "done",
	}
	svc.setRuntime(rt)
	task := ScheduledTask{
		ID:                   "daily",
		BotID:                "bot-a",
		Enabled:              true,
		Spec:                 "0 9 * * *",
		AgentName:            "traex",
		Cwd:                  taskCwd,
		Prompt:               "generate report",
		CreatedFromChatID:    "oc_chat",
		CreatedFromMessageID: "om_schedule",
		ResultSink: ScheduledTaskResultSink{
			Type:   "im",
			ChatID: "oc_chat",
		},
	}
	workspace := t.TempDir()

	if _, err := svc.runScheduledTaskOnce(context.Background(), task, "run-1", time.Now(), workspace, nil); err != nil {
		t.Fatalf("runScheduledTaskOnce() error = %v", err)
	}
	if len(rt.newCalls) != 1 {
		t.Fatalf("newCalls = %+v, want one schedule ACP session", rt.newCalls)
	}
	if rt.newCalls[0].AgentName != "traex" || rt.newCalls[0].Cwd != taskCwd || rt.newCalls[0].Workspace != workspace {
		t.Fatalf("newCalls = %+v, want persisted task agent/cwd/workspace", rt.newCalls)
	}
	if rt.newCalls[0].AgentName == "claude" || rt.newCalls[0].Cwd == chatDefaultCwd {
		t.Fatalf("newCalls = %+v, schedule run used IM chat config", rt.newCalls)
	}
}

func TestRunScheduledTaskOnceSkipsWhenPreviousRunIsRunning(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{newSessionInfo: acp.SessionInfo{SessionID: "acp-run-2"}, promptReply: "done"}
	svc.setRuntime(rt)
	task := ScheduledTask{
		ID:            "daily",
		BotID:         "bot-a",
		Enabled:       true,
		Spec:          "0 9 * * *",
		AgentName:     "traex",
		Cwd:           t.TempDir(),
		Prompt:        "generate report",
		OverlapPolicy: scheduleOverlapSkipIfRunning,
	}
	svc.markScheduleRunRunning(task, "run-1", scheduledTaskRunKey(task, "run-1"), time.Now(), "generate report")

	run, err := svc.runScheduledTaskOnce(context.Background(), task, "run-2", time.Now(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("runScheduledTaskOnce() error = %v", err)
	}
	if !run.TriggerResult.Skipped || run.Status.State != scheduleRunSkipped || !strings.Contains(run.Status.SkipReason, "run-1") {
		t.Fatalf("run = %+v, want skipped because run-1 is running", run)
	}
	if len(rt.newCalls) != 0 || len(rt.promptCalls) != 0 {
		t.Fatalf("runtime calls = new %+v prompt %+v, want none for skipped run", rt.newCalls, rt.promptCalls)
	}
}

func TestStopScheduledTaskCancelsActiveRun(t *testing.T) {
	svc := NewService(config.Config{}, nil)
	rt := &fakeRuntime{}
	svc.setRuntime(rt)
	task := ScheduledTask{
		ID:        "daily",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "0 9 * * *",
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "generate report",
	}
	runID := "run-1"
	key := scheduledTaskRunKey(task, runID)
	runCtx, cancel := context.WithCancel(context.Background())
	jobCtx, jobCancel := context.WithCancel(context.Background())
	job := &scheduledTaskJob{task: task, cancel: jobCancel}
	job.addActiveRun(runID, key, cancel)
	svc.taskMu.Lock()
	svc.scheduleJobs[scheduledTaskJobID(task)] = job
	svc.tasks[key] = &runningTask{
		kind:    taskKindUser,
		runtime: currentRuntimeKey(key),
		cancel:  cancel,
		session: Session{Key: key, ACPSessionID: "acp-run-1"},
		agent:   config.AgentConfig{Command: "traex"},
	}
	svc.taskMu.Unlock()

	svc.stopScheduledTask(task)

	if got := svc.scheduledTaskJobCount(); got != 0 {
		t.Fatalf("scheduledTaskJobCount() = %d, want stopped job", got)
	}
	select {
	case <-jobCtx.Done():
	default:
		t.Fatal("job context still active after stopScheduledTask")
	}
	select {
	case <-runCtx.Done():
	default:
		t.Fatal("active run context still active after stopScheduledTask")
	}
	svc.taskMu.Lock()
	remaining := svc.tasks[key]
	svc.taskMu.Unlock()
	if remaining != nil {
		t.Fatalf("remaining running task = %+v, want cancelled and removed", remaining)
	}
	waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() == 1 })
	rt.mu.Lock()
	cancelCalls := append([]fakeCancelCall(nil), rt.cancelCalls...)
	rt.mu.Unlock()
	if len(cancelCalls) != 1 || cancelCalls[0].Session.Key != key {
		t.Fatalf("cancelCalls = %+v, want active schedule run canceled", cancelCalls)
	}
}

func TestRunScheduledTaskOnceSendsResultToIMSink(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-run-1"},
		promptReply:    "schedule done",
	}
	svc.setRuntime(rt)
	task := ScheduledTask{
		ID:        "daily",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "0 9 * * *",
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "generate report",
		ResultSink: ScheduledTaskResultSink{
			Type:      "im",
			ChatID:    "oc_chat",
			ThreadID:  "omt_thread",
			MessageID: "om_source",
		},
	}
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	svc.setOutbound("bot-a", client)

	_, err := svc.runScheduledTaskOnce(ctx, task, "run-1", time.Now(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("runScheduledTaskOnce() error = %v", err)
	}
	sent := client.sentSnapshot()
	if len(sent) != 1 || sent[0] != "schedule done" {
		t.Fatalf("sent = %+v, want schedule result", sent)
	}
	sentMsgs := client.messagesSnapshot()
	if len(sentMsgs) != 1 || sentMsgs[0].BotID != "bot-a" || sentMsgs[0].ChatID != "oc_chat" || sentMsgs[0].ThreadID != "" || sentMsgs[0].MessageID != "" || sentMsgs[0].ForceReplyInThread {
		t.Fatalf("sent messages = %+v, want new root result message target", sentMsgs)
	}
}

func TestRunScheduledTaskOnceSendsResultToServiceIMSender(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-run-1"},
		promptReply:    "schedule done",
	}
	svc.setRuntime(rt)
	task := ScheduledTask{
		ID:        "daily",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "0 9 * * *",
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "generate report",
		ResultSink: ScheduledTaskResultSink{
			Type:      "im",
			ChatID:    "oc_chat",
			ThreadID:  "omt_thread",
			MessageID: "om_source",
		},
	}
	var sent []string
	var sentMsgs []feishu.Message
	ctx := context.Background()
	var sentRender []feishu.OutboundRenderContext
	svc.scheduleSenders["bot-a"] = func(ctx context.Context, msg feishu.Message, text string, render feishu.OutboundRenderContext) error {
		sent = append(sent, text)
		sentMsgs = append(sentMsgs, msg)
		sentRender = append(sentRender, render)
		return nil
	}

	_, err := svc.runScheduledTaskOnce(ctx, task, "run-1", time.Now(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("runScheduledTaskOnce() error = %v", err)
	}
	if len(sent) != 1 || sent[0] != "schedule done" {
		t.Fatalf("sent = %+v, want schedule result through service sender", sent)
	}
	if len(sentMsgs) != 1 || sentMsgs[0].BotID != "bot-a" || sentMsgs[0].ChatID != "oc_chat" || sentMsgs[0].ThreadID != "" || sentMsgs[0].MessageID != "" || sentMsgs[0].ForceReplyInThread {
		t.Fatalf("sent messages = %+v, want new root result message target", sentMsgs)
	}
	if len(sentRender) != 1 || sentRender[0].BaseDir != task.Cwd {
		t.Fatalf("sent render contexts = %+v, want task cwd", sentRender)
	}
}

func TestRunScheduledTaskOnceBindsStreamCardMessageForRootReplyRouting(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-run-1"},
		promptReply:    "schedule done",
		promptUpdates: []acp.PromptUpdate{
			{
				SessionID: "acp-run-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: "schedule"},
				},
			},
			{
				SessionID: "acp-run-1",
				Update: acp.SessionUpdate{
					SessionUpdate: "agent_message_chunk",
					Content:       &acp.ContentBlock{Type: "text", Text: " done"},
				},
			},
		},
	}
	svc.setRuntime(rt)
	task := ScheduledTask{
		ID:        "daily",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "0 9 * * *",
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "generate report",
		ResultSink: ScheduledTaskResultSink{
			Type:      "im",
			ChatID:    "oc_chat",
			ThreadID:  "omt_thread",
			MessageID: "om_source",
		},
	}
	var streamTargets []feishu.Message
	var streamMetas []feishu.StreamCardMeta
	var streamCard *fakeStreamCard
	svc.scheduleStreams["bot-a"] = func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		streamTargets = append(streamTargets, msg)
		streamMetas = append(streamMetas, feishu.StreamCardMetaFromContext(ctx))
		streamCard = &fakeStreamCard{message: feishu.SentMessage{
			MessageID: "om_schedule_result",
			ChatID:    msg.ChatID,
			ChatType:  msg.ChatType,
			ThreadID:  msg.ThreadID,
			RootID:    "om_schedule_result",
		}}
		return streamCard, nil
	}
	chatSession := Session{
		Key:          imSessionKey("bot-a", "oc_chat", ""),
		AgentName:    "traex",
		ACPSessionID: "acp-chat",
		Cwd:          task.Cwd,
	}
	if err := store.Upsert(chatSession); err != nil {
		t.Fatalf("Upsert(chatSession) error = %v", err)
	}

	runResult, err := svc.runScheduledTaskOnce(context.Background(), task, "run-1", time.Now(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("runScheduledTaskOnce() error = %v", err)
	}
	if len(streamTargets) != 1 {
		t.Fatalf("streamTargets = %+v, want one stream card", streamTargets)
	}
	if streamTargets[0].ChatID != "oc_chat" || streamTargets[0].ThreadID != "" || streamTargets[0].MessageID != "" || streamTargets[0].ForceReplyInThread {
		t.Fatalf("stream target = %+v, want new root result card target", streamTargets[0])
	}
	if len(streamMetas) != 1 || streamMetas[0].Title != "定时任务执行中" || streamMetas[0].Subtitle != "task-id: daily" || streamMetas[0].Footer != "本消息的回复链将在本次执行会话中处理。" {
		t.Fatalf("stream metas = %+v, want schedule result card metadata", streamMetas)
	}
	if streamCard == nil {
		t.Fatal("stream card was not created")
	}
	metaUpdates := streamCard.metaUpdatesSnapshot()
	if len(metaUpdates) != 1 || metaUpdates[0].Title != "定时任务已完成" || metaUpdates[0].Subtitle != "task-id: daily" || metaUpdates[0].Footer != "本消息的回复链将在本次执行会话中处理。" {
		t.Fatalf("meta updates = %+v, want completed schedule result card metadata", metaUpdates)
	}
	runSession := runResult.TriggerResult.Session
	if runSession.Key.Source != sessionSourceSchedule || runSession.Key.MainID != "task:daily" || runSession.Key.SubID != "run:run-1" {
		t.Fatalf("run session key = %+v, want schedule run key", runSession.Key)
	}
	boundSession, binding, ok := store.SessionForMessage("bot-a", "oc_chat", "om_schedule_result")
	if !ok {
		t.Fatalf("SessionForMessage() ok=false binding=%+v", binding)
	}
	if boundSession.ACPSessionID != "acp-run-1" || boundSession.Key != runSession.Key {
		t.Fatalf("bound session = %+v, want run session %+v", boundSession, runSession)
	}
	replySession, ok := svc.findSession(feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		MessageID: "om_reply",
		RootID:    "om_schedule_result",
		ParentID:  "om_schedule_result",
	})
	if !ok {
		t.Fatal("findSession(root reply) ok=false")
	}
	if replySession.Key != runSession.Key || replySession.ACPSessionID != "acp-run-1" {
		t.Fatalf("reply session = %+v, want schedule run session %+v", replySession, runSession)
	}

	refreshedRunSession := runSession
	refreshedRunSession.ACPSessionID = "acp-run-2"
	if err := store.Upsert(refreshedRunSession); err != nil {
		t.Fatalf("Upsert(refreshedRunSession) error = %v", err)
	}
	replySession, ok = svc.findSession(feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		MessageID: "om_reply_after_refresh",
		RootID:    "om_schedule_result",
		ParentID:  "om_schedule_result",
	})
	if !ok {
		t.Fatal("findSession(root reply after ACP refresh) ok=false")
	}
	if replySession.Key != runSession.Key || replySession.ACPSessionID != "acp-run-2" {
		t.Fatalf("reply session after ACP refresh = %+v, want refreshed schedule run session %+v", replySession, refreshedRunSession)
	}
}

func TestRunScheduledTaskOnceSendsErrorToIMSink(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := NewService(config.Config{
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
	}, store)
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-run-1"},
		promptErrors:   []error{errors.New("boom")},
	}
	svc.setRuntime(rt)
	task := ScheduledTask{
		ID:        "daily",
		BotID:     "bot-a",
		Enabled:   true,
		Spec:      "0 9 * * *",
		AgentName: "traex",
		Cwd:       t.TempDir(),
		Prompt:    "generate report",
		ResultSink: ScheduledTaskResultSink{
			Type:   "im",
			ChatID: "oc_chat",
		},
	}
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	svc.setOutbound("bot-a", client)

	_, err := svc.runScheduledTaskOnce(ctx, task, "run-1", time.Now(), t.TempDir(), nil)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("runScheduledTaskOnce() error = %v, want boom", err)
	}
	sent := client.sentSnapshot()
	if len(sent) != 1 || !strings.Contains(sent[0], "定时任务执行失败") || !strings.Contains(sent[0], "boom") {
		t.Fatalf("sent = %+v, want failure text", sent)
	}
}
