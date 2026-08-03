package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestHandleScheduleCommandAddListStatusPauseResumeDelete(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	cfg := config.Config{
		Bots: []config.BotConfig{{
			ID:           "bot-a",
			Workspace:    workspace,
			OwnerOpenIDs: []string{testOwnerOpenID},
		}},
		AgentList: []config.NamedAgentConfig{
			{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}},
		},
	}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, "sessions.json")))
	msg := feishu.Message{
		BotID:            "bot-a",
		ChatID:           "oc_chat",
		ChatType:         "group",
		GroupMessageType: "thread",
		ThreadID:         "omt_thread",
		MessageID:        "om_schedule",
		SenderID:         testOwnerOpenID,
		Workspace:        workspace,
		Text:             "@智能助手 /schedule add @every 1h 生成日报",
		Mentions:         testBotMentions(),
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/schedule add) error = %v", err)
	}
	if !strings.Contains(reply, "已创建定时任务：task-") || !strings.Contains(reply, "触发：@every 1h") || !strings.Contains(reply, "cwd："+cwd) {
		t.Fatalf("add reply = %q, want created task summary", reply)
	}
	store := svc.scheduledTaskStoreForBotID("bot-a")
	tasks := store.List()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want one created task", tasks)
	}
	task := tasks[0]
	if !task.Enabled || task.Spec != "@every 1h" || task.AgentName != "traex" || task.Cwd != cwd || task.Prompt != "生成日报" {
		t.Fatalf("task = %+v, want persisted command fields", task)
	}
	if task.CreatorOpenID != testOwnerOpenID || task.CreatedFromChatID != "oc_chat" || task.CreatedFromThreadID != "omt_thread" || task.CreatedFromMessageID != "om_schedule" {
		t.Fatalf("task source = %+v, want IM origin", task)
	}
	if task.ResultSink.Type != "im" || task.ResultSink.ChatID != "oc_chat" || task.ResultSink.ThreadID != "" || task.ResultSink.MessageID != "" {
		t.Fatalf("task sink = %+v, want IM chat root result sink", task.ResultSink)
	}
	if got := svc.scheduledTaskJobCount(); got != 1 {
		t.Fatalf("scheduledTaskJobCount() = %d, want created task registered", got)
	}

	list := svc.handleScheduleCommand(context.Background(), "/schedule list", msg)
	if !strings.Contains(list, task.ID) || !strings.Contains(list, "[启用]") || !strings.Contains(list, "生成日报") {
		t.Fatalf("list reply = %q, want created task", list)
	}
	status := svc.handleScheduleCommand(context.Background(), "/schedule status "+task.ID, msg)
	if !strings.Contains(status, "定时任务："+task.ID) || !strings.Contains(status, "状态：启用") || !strings.Contains(status, "回传：IM oc_chat") || strings.Contains(status, "回传：IM oc_chat /") {
		t.Fatalf("status reply = %q, want task details", status)
	}

	pause := svc.handleScheduleCommand(context.Background(), "/schedule pause "+task.ID, msg)
	if pause != "已暂停定时任务："+task.ID {
		t.Fatalf("pause reply = %q", pause)
	}
	paused, _ := store.Get(task.ID)
	if paused.Enabled {
		t.Fatalf("paused task = %+v, want disabled", paused)
	}
	if got := svc.scheduledTaskJobCount(); got != 0 {
		t.Fatalf("scheduledTaskJobCount() = %d, want job stopped after pause", got)
	}

	resume := svc.handleScheduleCommand(context.Background(), "/schedule resume "+task.ID, msg)
	if resume != "已恢复定时任务："+task.ID {
		t.Fatalf("resume reply = %q", resume)
	}
	resumed, _ := store.Get(task.ID)
	if !resumed.Enabled {
		t.Fatalf("resumed task = %+v, want enabled", resumed)
	}
	if got := svc.scheduledTaskJobCount(); got != 1 {
		t.Fatalf("scheduledTaskJobCount() = %d, want job registered after resume", got)
	}

	deleted := svc.handleScheduleCommand(context.Background(), "/schedule delete "+task.ID, msg)
	if deleted != "已删除定时任务："+task.ID {
		t.Fatalf("delete reply = %q", deleted)
	}
	if _, ok := store.Get(task.ID); ok {
		t.Fatal("deleted task still exists")
	}
	if got := svc.scheduledTaskJobCount(); got != 0 {
		t.Fatalf("scheduledTaskJobCount() = %d, want job stopped after delete", got)
	}
}

func TestHandleScheduleCommandAddCronSpec(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	cfg := config.Config{
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace, OwnerOpenIDs: []string{testOwnerOpenID}}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
	}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, "sessions.json")))
	msg := feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		MessageID: "om_schedule",
		SenderID:  testOwnerOpenID,
		Workspace: workspace,
		Text:      "/schedule add 0 9 * * 1 生成周报",
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/schedule add cron) error = %v", err)
	}
	if !strings.Contains(reply, "触发：0 9 * * 1") {
		t.Fatalf("reply = %q, want cron spec", reply)
	}
	tasks := svc.scheduledTaskStoreForBotID("bot-a").List()
	if len(tasks) != 1 || tasks[0].Spec != "0 9 * * 1" || tasks[0].Prompt != "生成周报" {
		t.Fatalf("tasks = %+v, want cron task", tasks)
	}
}

func TestHandleScheduleEditCommandUpdatesEnabledTaskAndReregistersJob(t *testing.T) {
	workspace := t.TempDir()
	defaultCwd := t.TempDir()
	explicitCwd := t.TempDir()
	cfg := config.Config{
		Bots: []config.BotConfig{{
			ID:           "bot-a",
			Workspace:    workspace,
			OwnerOpenIDs: []string{testOwnerOpenID},
		}},
		AgentList: []config.NamedAgentConfig{
			{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: defaultCwd}},
			{Name: "claude", AgentConfig: config.AgentConfig{Command: "claude", DefaultCwd: t.TempDir()}},
		},
	}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, "sessions.json")))
	msg := feishu.Message{
		BotID:            "bot-a",
		ChatID:           "oc_chat",
		ChatType:         "group",
		GroupMessageType: "thread",
		ThreadID:         "omt_thread",
		MessageID:        "om_schedule",
		SenderID:         testOwnerOpenID,
		Workspace:        workspace,
		Text:             "@智能助手 /schedule add @every 1h 生成日报",
		Mentions:         testBotMentions(),
	}
	if _, err := handleFeishuMessage(t, svc, context.Background(), msg); err != nil {
		t.Fatalf("HandleFeishuMessage(/schedule add) error = %v", err)
	}
	store := svc.scheduledTaskStoreForBotID("bot-a")
	tasks := store.List()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want one created task", tasks)
	}
	task := tasks[0]
	createdAt := task.CreatedAt
	updatedAt := task.UpdatedAt
	if got := svc.scheduledTaskJobCount(); got != 1 {
		t.Fatalf("scheduledTaskJobCount() = %d, want created task registered", got)
	}

	reply := svc.handleScheduleCommand(context.Background(), "/schedule edit "+task.ID+" --cwd "+explicitCwd+" --agent claude 0 9 * * 1 生成周报", msg)
	for _, want := range []string{
		"已更新定时任务：" + task.ID,
		"状态：启用",
		"触发：0 9 * * 1",
		"agent：claude",
		"cwd：" + explicitCwd,
		"prompt：生成周报",
		"回传：IM oc_chat",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
	if strings.Contains(reply, "回传：IM oc_chat /") {
		t.Fatalf("reply = %q, want chat root result sink without thread", reply)
	}
	edited, ok := store.Get(task.ID)
	if !ok {
		t.Fatalf("Get(%s) ok = false", task.ID)
	}
	if !edited.Enabled || edited.Spec != "0 9 * * 1" || edited.AgentName != "claude" || edited.Cwd != explicitCwd || edited.Prompt != "生成周报" {
		t.Fatalf("edited task = %+v, want updated fields", edited)
	}
	if edited.CreatorOpenID != testOwnerOpenID || edited.CreatedFromChatID != "oc_chat" || edited.CreatedFromThreadID != "omt_thread" || edited.CreatedFromMessageID != "om_schedule" {
		t.Fatalf("edited task source = %+v, want preserved source", edited)
	}
	if edited.ResultSink != task.ResultSink {
		t.Fatalf("edited sink = %+v, want preserved %+v", edited.ResultSink, task.ResultSink)
	}
	if !edited.CreatedAt.Equal(createdAt) {
		t.Fatalf("CreatedAt = %s, want preserved %s", edited.CreatedAt, createdAt)
	}
	if !edited.UpdatedAt.After(updatedAt) && !edited.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("UpdatedAt = %s, want not before %s", edited.UpdatedAt, updatedAt)
	}
	if got := svc.scheduledTaskJobCount(); got != 1 {
		t.Fatalf("scheduledTaskJobCount() = %d, want edited enabled task registered", got)
	}
}

func TestHandleScheduleEditCommandKeepsDefaultsAndPausedState(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	cfg := config.Config{
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace, OwnerOpenIDs: []string{testOwnerOpenID}}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
	}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, "sessions.json")))
	msg := feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		MessageID: "om_schedule",
		SenderID:  testOwnerOpenID,
		Workspace: workspace,
		Text:      "/schedule add @every 1h 生成日报",
	}
	if _, err := handleFeishuMessage(t, svc, context.Background(), msg); err != nil {
		t.Fatalf("HandleFeishuMessage(/schedule add) error = %v", err)
	}
	store := svc.scheduledTaskStoreForBotID("bot-a")
	task := store.List()[0]
	if pause := svc.handleScheduleCommand(context.Background(), "/schedule pause "+task.ID, msg); pause != "已暂停定时任务："+task.ID {
		t.Fatalf("pause reply = %q", pause)
	}
	if got := svc.scheduledTaskJobCount(); got != 0 {
		t.Fatalf("scheduledTaskJobCount() = %d, want paused task stopped", got)
	}

	reply := svc.handleScheduleCommand(context.Background(), "/schedule edit "+task.ID+" @every 2h 生成双小时日报", msg)
	for _, want := range []string{
		"已更新定时任务：" + task.ID,
		"状态：暂停",
		"触发：@every 2h",
		"agent：traex",
		"cwd：" + cwd,
		"prompt：生成双小时日报",
	} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply = %q, want %q", reply, want)
		}
	}
	edited, ok := store.Get(task.ID)
	if !ok {
		t.Fatalf("Get(%s) ok = false", task.ID)
	}
	if edited.Enabled || edited.Spec != "@every 2h" || edited.AgentName != "traex" || edited.Cwd != cwd || edited.Prompt != "生成双小时日报" {
		t.Fatalf("edited task = %+v, want updated prompt/spec and preserved defaults", edited)
	}
	if got := svc.scheduledTaskJobCount(); got != 0 {
		t.Fatalf("scheduledTaskJobCount() = %d, want edited paused task not registered", got)
	}
}

func TestHandleScheduleEditCommandRejectsMissingTaskAndInvalidArgs(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	cfg := config.Config{
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace, OwnerOpenIDs: []string{testOwnerOpenID}}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
	}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, "sessions.json")))
	msg := feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		MessageID: "om_schedule",
		SenderID:  testOwnerOpenID,
		Workspace: workspace,
	}

	reply := svc.handleScheduleCommand(context.Background(), "/schedule edit missing @every 1h 生成日报", msg)
	if reply != "定时任务不存在：missing" {
		t.Fatalf("reply = %q, want missing task rejection", reply)
	}

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		MessageID: "om_schedule",
		SenderID:  testOwnerOpenID,
		Workspace: workspace,
		Text:      "/schedule add @every 1h 生成日报",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/schedule add) error = %v", err)
	}
	task := svc.scheduledTaskStoreForBotID("bot-a").List()[0]

	reply = svc.handleScheduleCommand(context.Background(), "/schedule edit "+task.ID+" --agent missing @every 1h 生成日报", msg)
	if reply != "未知 agent：missing" {
		t.Fatalf("reply = %q, want unknown agent rejection", reply)
	}

	reply = svc.handleScheduleCommand(context.Background(), "/schedule edit "+task.ID+" bad spec", msg)
	if !strings.Contains(reply, "cron spec 需要 5 段") {
		t.Fatalf("reply = %q, want invalid spec rejection", reply)
	}
}

func TestHandleScheduleRunCommandStartsImmediateRunAndSendsResult(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	cfg := config.Config{
		Bots: []config.BotConfig{{
			ID:           "bot-a",
			Workspace:    workspace,
			OwnerOpenIDs: []string{testOwnerOpenID},
		}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
	}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, "sessions.json")))
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-run-now"},
		promptReply:    "立即执行完成",
	}
	svc.setRuntime(rt)
	type sentResult struct {
		msg  feishu.Message
		text string
	}
	sent := make(chan sentResult, 1)
	svc.scheduleSenders["bot-a"] = func(ctx context.Context, msg feishu.Message, text string, render feishu.OutboundRenderContext) error {
		sent <- sentResult{msg: msg, text: text}
		return nil
	}
	msg := feishu.Message{
		BotID:            "bot-a",
		ChatID:           "oc_chat",
		ChatType:         "group",
		GroupMessageType: "thread",
		ThreadID:         "omt_thread",
		MessageID:        "om_schedule",
		SenderID:         testOwnerOpenID,
		Workspace:        workspace,
		Text:             "@智能助手 /schedule add @every 1h 生成日报",
		Mentions:         testBotMentions(),
	}
	addReply, err := handleFeishuMessage(t, svc, context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/schedule add) error = %v", err)
	}
	if !strings.Contains(addReply, "已创建定时任务：task-") {
		t.Fatalf("add reply = %q, want created task", addReply)
	}
	tasks := svc.scheduledTaskStoreForBotID("bot-a").List()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want one task", tasks)
	}

	runReply := svc.handleScheduleCommand(context.Background(), "/schedule run "+tasks[0].ID, msg)
	if !strings.Contains(runReply, "已开始立即执行定时任务："+tasks[0].ID) || !strings.Contains(runReply, "run："+tasks[0].ID+"-") {
		t.Fatalf("run reply = %q, want immediate run ack", runReply)
	}
	waitForCondition(t, time.Second, func() bool { return rt.promptCallCount() == 1 })

	var gotSent sentResult
	select {
	case gotSent = <-sent:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for immediate run result sink")
	}
	if gotSent.text != "立即执行完成" {
		t.Fatalf("sent text = %q, want immediate run result", gotSent.text)
	}
	if gotSent.msg.BotID != "bot-a" || gotSent.msg.ChatID != "oc_chat" || gotSent.msg.ThreadID != "" || gotSent.msg.MessageID != "" || gotSent.msg.ForceReplyInThread {
		t.Fatalf("sent message = %+v, want new root result message target", gotSent.msg)
	}
	rt.mu.Lock()
	promptCalls := append([]fakePromptCall(nil), rt.promptCalls...)
	newCalls := append([]fakeNewCall(nil), rt.newCalls...)
	rt.mu.Unlock()
	if len(promptCalls) != 1 || !strings.Contains(promptCalls[0].Text, "生成日报") || !strings.Contains(promptCalls[0].Text, `"source": "schedule"`) {
		t.Fatalf("promptCalls = %+v, want schedule prompt", promptCalls)
	}
	if len(newCalls) != 1 || newCalls[0].Key.Source != sessionSourceSchedule || newCalls[0].Key.MainID != "task:"+tasks[0].ID || newCalls[0].Workspace != workspace {
		t.Fatalf("newCalls = %+v, want immediate schedule session in bot workspace", newCalls)
	}
	last, ok := svc.lastScheduleRunStatus(tasks[0].ID)
	if !ok || last.State != scheduleRunCompleted {
		t.Fatalf("last status = %+v ok=%v, want completed immediate run", last, ok)
	}
}

func TestHandleScheduleRunCommandRejectsMissingTask(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Config{
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace, OwnerOpenIDs: []string{testOwnerOpenID}}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: t.TempDir()}}},
	}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, "sessions.json")))
	msg := feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		MessageID: "om_schedule",
		SenderID:  testOwnerOpenID,
		Workspace: workspace,
	}

	reply := svc.handleScheduleCommand(context.Background(), "/schedule run missing", msg)
	if reply != "定时任务不存在：missing" {
		t.Fatalf("reply = %q, want missing task rejection", reply)
	}
}

func TestHandleScheduleCommandAddSupportsExplicitCwdAndAgent(t *testing.T) {
	workspace := t.TempDir()
	defaultCwd := t.TempDir()
	explicitCwd := t.TempDir()
	cfg := config.Config{
		Bots: []config.BotConfig{{ID: "bot-a", Workspace: workspace, OwnerOpenIDs: []string{testOwnerOpenID}}},
		AgentList: []config.NamedAgentConfig{
			{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: defaultCwd}},
			{Name: "claude", AgentConfig: config.AgentConfig{Command: "claude", DefaultCwd: t.TempDir()}},
		},
	}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, "sessions.json")))
	msg := feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		MessageID: "om_schedule",
		SenderID:  testOwnerOpenID,
		Workspace: workspace,
		Text:      "/schedule add --cwd " + explicitCwd + " --agent claude @every 30m 生成指定项目日报",
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/schedule add explicit flags) error = %v", err)
	}
	if !strings.Contains(reply, "agent：claude") || !strings.Contains(reply, "cwd："+explicitCwd) {
		t.Fatalf("reply = %q, want explicit agent and cwd", reply)
	}
	tasks := svc.scheduledTaskStoreForBotID("bot-a").List()
	if len(tasks) != 1 || tasks[0].AgentName != "claude" || tasks[0].Cwd != explicitCwd || tasks[0].Prompt != "生成指定项目日报" {
		t.Fatalf("tasks = %+v, want explicit agent/cwd persisted", tasks)
	}
}

func TestHandleScheduleHowCommandReturnsRecommendedAddCommand(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	cfg := config.Config{
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace, OwnerOpenIDs: []string{testOwnerOpenID}}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
	}
	rt := &fakeRuntime{newSessionID: "acp-session-1", promptReply: "/schedule add 0 8 * * * 执行 scripts/report.sh"}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, "sessions.json")))
	svc.setRuntime(rt)
	msg := feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		MessageID: "om_schedule",
		SenderID:  testOwnerOpenID,
		Workspace: workspace,
		Text:      "/schedule how 每天上午 8 点执行 scripts/report.sh",
	}

	reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/schedule how) error = %v", err)
	}
	if reply != "/schedule add 0 8 * * * 执行 scripts/report.sh" {
		t.Fatalf("reply = %q, want recommended /schedule add command", reply)
	}
	if rt.promptCallCount() != 1 {
		t.Fatalf("prompt calls = %d, want /schedule how to ask model once", rt.promptCallCount())
	}
	rt.mu.Lock()
	prompt := rt.promptCalls[0].Text
	rt.mu.Unlock()
	for _, want := range []string{
		"每天上午 8 点执行 scripts/report.sh",
		"## /schedule add 命令格式",
		"每天上午 8 点应生成 0 8 * * *",
		"最终只返回一条 /schedule add 命令",
		"不要真正创建任务，只生成命令",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
	if tasks := svc.scheduledTaskStoreForBotID("bot-a").List(); len(tasks) != 0 {
		t.Fatalf("tasks = %+v, want /schedule how not to create task", tasks)
	}
}

func TestHandleScheduleCommandAddRejectsInvalidExplicitFlags(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	cfg := config.Config{
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace, OwnerOpenIDs: []string{testOwnerOpenID}}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
	}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, "sessions.json")))
	msg := feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		MessageID: "om_schedule",
		SenderID:  testOwnerOpenID,
		Workspace: workspace,
	}

	msg.Text = "/schedule add --agent missing @every 1h 生成日报"
	reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(invalid agent) error = %v", err)
	}
	if reply != "未知 agent：missing" {
		t.Fatalf("reply = %q, want unknown agent rejection", reply)
	}

	msg.Text = "/schedule add --cwd " + filepath.Join(t.TempDir(), "missing") + " @every 1h 生成日报"
	reply, err = handleFeishuMessage(t, svc, context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(invalid cwd) error = %v", err)
	}
	if !strings.Contains(reply, "工作目录不可访问") {
		t.Fatalf("reply = %q, want cwd validation error", reply)
	}
}

func TestHandleScheduleCommandRejectsNonOwner(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Config{
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace, OwnerOpenIDs: []string{testOwnerOpenID}}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: t.TempDir()}}},
	}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, "sessions.json")))

	reply, err := svc.HandleFeishuMessage(context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		MessageID: "om_schedule",
		SenderID:  "ou_other",
		Workspace: workspace,
		Text:      "/schedule list",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(non-owner /schedule) error = %v", err)
	}
	if reply != "只有 bot owner 可以执行斜杠命令。" {
		t.Fatalf("reply = %q, want owner-only rejection", reply)
	}
}

func TestOnceScheduleSpecFiresOnceThenExpires(t *testing.T) {
	at := time.Now().Add(time.Hour)
	spec := onceScheduleSpec{at: at}
	next, ok := spec.Next(time.Now())
	if !ok || !next.Equal(at) {
		t.Fatalf("Next(before) = %s %v, want %s true", next, ok, at)
	}
	if _, ok := spec.Next(at.Add(time.Second)); ok {
		t.Fatal("Next(after at) = ok true, want false")
	}
}

func TestParseOnceAtSpecUsesLocalAndRFC3339(t *testing.T) {
	// RFC3339 保留绝对时刻与时区偏移。
	rfc, err := parseOnceAtSpec("2026-08-05T09:00:00+08:00", "")
	if err != nil {
		t.Fatalf("parseOnceAtSpec(RFC3339) error = %v", err)
	}
	next, ok := rfc.Next(time.Now())
	if !ok {
		t.Fatal("RFC3339 Next = false, want true")
	}
	want, _ := time.Parse(time.RFC3339, "2026-08-05T09:00:00+08:00")
	if !next.Equal(want) {
		t.Fatalf("RFC3339 next = %s, want %s", next, want)
	}

	// "YYYY-MM-DD HH:MM" 按给定（或 local）时区解释。
	local, err := parseOnceAtSpec("2026-08-05 09:00", "")
	if err != nil {
		t.Fatalf("parseOnceAtSpec(local) error = %v", err)
	}
	nextLocal, ok := local.Next(time.Now())
	if !ok {
		t.Fatal("local Next = false, want true")
	}
	wantLocal, _ := time.ParseInLocation("2006-01-02 15:04", "2026-08-05 09:00", time.Local)
	if !nextLocal.Equal(wantLocal) {
		t.Fatalf("local next = %s, want %s", nextLocal, wantLocal)
	}

	if _, err := parseOnceAtSpec("not-a-time", ""); err == nil {
		t.Fatal("parseOnceAtSpec(invalid) error = nil, want error")
	}
}

func TestHandleScheduleOnceCommandFiresAndDeletes(t *testing.T) {
	workspace := t.TempDir()
	cwd := t.TempDir()
	cfg := config.Config{
		Bots: []config.BotConfig{{
			ID:           "bot-a",
			Workspace:    workspace,
			OwnerOpenIDs: []string{testOwnerOpenID},
		}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
	}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, "sessions.json")))
	rt := &fakeRuntime{
		newSessionInfo: acp.SessionInfo{SessionID: "acp-once"},
		promptReply:    "一次性任务完成",
	}
	svc.setRuntime(rt)
	sent := make(chan string, 1)
	svc.scheduleSenders["bot-a"] = func(ctx context.Context, msg feishu.Message, text string, render feishu.OutboundRenderContext) error {
		sent <- text
		return nil
	}

	at := time.Now().Add(2 * time.Second).Format(time.RFC3339)
	msg := feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		MessageID: "om_once",
		SenderID:  testOwnerOpenID,
		Workspace: workspace,
		Text:      "/schedule once " + at + " 生成一次性早报",
	}
	reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/schedule once) error = %v", err)
	}
	if !strings.Contains(reply, "已创建一次性任务：task-") || !strings.Contains(reply, "触发：@at "+at) {
		t.Fatalf("once reply = %q, want one-shot summary with trigger time", reply)
	}
	store := svc.scheduledTaskStoreForBotID("bot-a")
	tasks := store.List()
	if len(tasks) != 1 || !tasks[0].Once || tasks[0].Spec != "@at "+at || tasks[0].Prompt != "生成一次性早报" {
		t.Fatalf("tasks = %+v, want persisted once task", tasks)
	}
	if got := svc.scheduledTaskJobCount(); got != 1 {
		t.Fatalf("job count = %d, want once task registered", got)
	}
	taskID := tasks[0].ID

	// 等到任务执行、结果回传，并确认任务已从存储和内存中删除。
	waitForCondition(t, 3*time.Second, func() bool {
		return rt.promptCallCount() == 1 && len(store.List()) == 0 && svc.scheduledTaskJobCount() == 0
	})
	select {
	case text := <-sent:
		if text != "一次性任务完成" {
			t.Fatalf("sent text = %q, want prompt reply", text)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for once run result sink")
	}
	if _, ok := store.Get(taskID); ok {
		t.Fatal("once task still present after firing")
	}
}

func TestHandleScheduleOnceCommandRejectsPastTime(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Config{
		Bots:      []config.BotConfig{{ID: "bot-a", Workspace: workspace, OwnerOpenIDs: []string{testOwnerOpenID}}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: t.TempDir()}}},
	}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, "sessions.json")))
	msg := feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		MessageID: "om_once",
		SenderID:  testOwnerOpenID,
		Workspace: workspace,
		Text:      "/schedule once 2020-01-01 09:00 过期任务",
	}
	reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
	if err != nil {
		t.Fatalf("HandleFeishuMessage error = %v", err)
	}
	if !strings.Contains(reply, "执行时间必须在将来") {
		t.Fatalf("reply = %q, want future-time rejection", reply)
	}
	if tasks := svc.scheduledTaskStoreForBotID("bot-a").List(); len(tasks) != 0 {
		t.Fatalf("tasks = %+v, want none persisted", tasks)
	}
}
