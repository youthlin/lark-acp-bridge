package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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
	if !strings.Contains(reply, "已创建定时任务：task-") || !strings.Contains(reply, "spec：@every 1h") || !strings.Contains(reply, "cwd："+cwd) {
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
	if task.ResultSink.Type != "im" || task.ResultSink.ChatID != "oc_chat" || task.ResultSink.ThreadID != "omt_thread" || task.ResultSink.MessageID != "om_schedule" {
		t.Fatalf("task sink = %+v, want IM result sink", task.ResultSink)
	}
	if got := svc.scheduledTaskJobCount(); got != 1 {
		t.Fatalf("scheduledTaskJobCount() = %d, want created task registered", got)
	}

	list := svc.handleScheduleCommand(context.Background(), "/schedule list", msg)
	if !strings.Contains(list, task.ID) || !strings.Contains(list, "[启用]") || !strings.Contains(list, "生成日报") {
		t.Fatalf("list reply = %q, want created task", list)
	}
	status := svc.handleScheduleCommand(context.Background(), "/schedule status "+task.ID, msg)
	if !strings.Contains(status, "定时任务："+task.ID) || !strings.Contains(status, "状态：启用") || !strings.Contains(status, "回传：IM oc_chat / omt_thread") {
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
	if !strings.Contains(reply, "spec：0 9 * * 1") {
		t.Fatalf("reply = %q, want cron spec", reply)
	}
	tasks := svc.scheduledTaskStoreForBotID("bot-a").List()
	if len(tasks) != 1 || tasks[0].Spec != "0 9 * * 1" || tasks[0].Prompt != "生成周报" {
		t.Fatalf("tasks = %+v, want cron task", tasks)
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
