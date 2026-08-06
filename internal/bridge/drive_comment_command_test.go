package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestHandleDriveCommentCommandUpdatesBotConfig(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Config{
		Bots: []config.BotConfig{{
			ID:           "bot-a",
			AppID:        "cli_a",
			AppSecret:    config.FileSecret("$HOME/.lark-acp-bridge/secrets/bot-a.appsecret"),
			Workspace:    workspace,
			OwnerOpenIDs: []string{testOwnerOpenID},
		}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: t.TempDir()}}},
	}
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	svc := NewService(loaded, NewSessionStore(filepath.Join(workspace, "sessions.json"))).WithConfigPath(configPath)
	msg := feishu.Message{BotID: "bot-a", ChatID: "oc_trace", SenderID: testOwnerOpenID}

	got := svc.handleDriveCommentCommand(context.Background(), "/drive_comment on", msg)
	if !strings.Contains(got, "云文档评论监听处理：开启") {
		t.Fatalf("on reply = %q, want enabled status", got)
	}
	cfgAfterOn, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load(after on) error = %v", err)
	}
	if !cfgAfterOn.Bots[0].DriveComment.Enabled || !svc.cfg.Bots[0].DriveComment.Enabled {
		t.Fatalf("drive_comment enabled not persisted/in-memory: cfg=%+v svc=%+v", cfgAfterOn.Bots[0].DriveComment, svc.cfg.Bots[0].DriveComment)
	}

	got = svc.handleDriveCommentCommand(context.Background(), "/drive_comment trace on", msg)
	if !strings.Contains(got, "处理过程展示：开启") || !strings.Contains(got, "处理过程卡片目的地：oc_trace") {
		t.Fatalf("trace on reply = %q, want trace chat status", got)
	}
	cfgAfterTrace, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load(after trace) error = %v", err)
	}
	if !cfgAfterTrace.Bots[0].DriveComment.TraceEnabled || cfgAfterTrace.Bots[0].DriveComment.TraceChatID != "oc_trace" {
		t.Fatalf("drive_comment trace = %+v, want enabled oc_trace", cfgAfterTrace.Bots[0].DriveComment)
	}

	got = svc.handleDriveCommentCommand(context.Background(), "/drive_comment off", msg)
	if !strings.Contains(got, "云文档评论监听处理：关闭") || !strings.Contains(got, "已有处理过程群里的对话仍可继续") {
		t.Fatalf("off reply = %q, want disabled status with continuation note", got)
	}
}

func TestHandleDriveCommentTraceNewCreatesTopicChat(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Config{
		Bots: []config.BotConfig{{
			ID:           "bot-a",
			AppID:        "cli_a",
			AppSecret:    config.FileSecret("$HOME/.lark-acp-bridge/secrets/bot-a.appsecret"),
			Workspace:    workspace,
			OwnerOpenIDs: []string{testOwnerOpenID},
		}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: t.TempDir()}}},
	}
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	svc := NewService(loaded, NewSessionStore(filepath.Join(workspace, "sessions.json"))).WithConfigPath(configPath)
	client := newFakeSentMessageClient("")
	var gotReq feishu.CreateDriveCommentTraceChatRequest
	client.traceChatCreator = func(ctx context.Context, req feishu.CreateDriveCommentTraceChatRequest) (feishu.CreatedChat, error) {
		gotReq = req
		return feishu.CreatedChat{ChatID: "oc_new_trace", ChatType: "private", GroupMessageType: "thread"}, nil
	}
	client.traceBotNameProvider = func(ctx context.Context) (string, error) {
		return "智能助手", nil
	}
	svc.setOutbound("bot-a", client)
	msg := feishu.Message{BotID: "bot-a", ChatID: "oc_current", SenderID: testOwnerOpenID}

	got := svc.handleDriveCommentCommand(context.Background(), "/drive_comment trace new", msg)
	if !strings.Contains(got, "chat_id：oc_new_trace") || !strings.Contains(got, "处理过程卡片目的地：oc_new_trace") {
		t.Fatalf("trace new reply = %q, want new chat status", got)
	}
	if gotReq.Name != "云文档评论处理通知群-智能助手" || gotReq.OwnerOpenID != testOwnerOpenID || len(gotReq.UserOpenIDs) != 1 || gotReq.UserOpenIDs[0] != testOwnerOpenID {
		t.Fatalf("create chat request = %+v, want topic trace chat request", gotReq)
	}
	cfgAfter, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load(after trace new) error = %v", err)
	}
	if !cfgAfter.Bots[0].DriveComment.TraceEnabled || cfgAfter.Bots[0].DriveComment.TraceChatID != "oc_new_trace" {
		t.Fatalf("drive_comment trace = %+v, want new trace chat", cfgAfter.Bots[0].DriveComment)
	}
}

func TestHandleDriveCommentTraceNewReportsCreatedChatWhenConfigWriteFails(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Config{
		Bots: []config.BotConfig{{
			ID:           "bot-a",
			AppID:        "cli_a",
			AppSecret:    config.FileSecret("$HOME/.lark-acp-bridge/secrets/bot-a.appsecret"),
			Workspace:    workspace,
			OwnerOpenIDs: []string{testOwnerOpenID},
		}},
	}
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	svc := NewService(loaded, NewSessionStore(filepath.Join(workspace, "sessions.json"))).WithConfigPath(filepath.Join(configPath, "missing.json"))
	client := newFakeSentMessageClient("")
	client.traceChatCreator = func(context.Context, feishu.CreateDriveCommentTraceChatRequest) (feishu.CreatedChat, error) {
		return feishu.CreatedChat{ChatID: "oc_orphan", ChatType: "private", GroupMessageType: "thread"}, nil
	}
	client.traceBotNameProvider = func(context.Context) (string, error) {
		return "智能助手", nil
	}
	svc.setOutbound("bot-a", client)

	got := svc.handleDriveCommentCommand(context.Background(), "/drive_comment trace new", feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_current",
		SenderID: testOwnerOpenID,
	})
	for _, want := range []string{"群已创建，但设置为卡片目的地失败", "请手动删除该群或重新配置", "群名：云文档评论处理通知群-智能助手", "chat_id：oc_orphan"} {
		if !strings.Contains(got, want) {
			t.Fatalf("trace new failure reply = %q, want %q", got, want)
		}
	}
}

func TestHandleDriveCommentCommandConcurrentUpdatesRemainConsistent(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Config{
		Bots: []config.BotConfig{{
			ID:           "bot-a",
			AppID:        "cli_a",
			AppSecret:    config.FileSecret("$HOME/.lark-acp-bridge/secrets/bot-a.appsecret"),
			Workspace:    workspace,
			OwnerOpenIDs: []string{testOwnerOpenID},
		}},
	}
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	svc := NewService(loaded, NewSessionStore(filepath.Join(workspace, "sessions.json"))).WithConfigPath(configPath)
	msg := feishu.Message{BotID: "bot-a", ChatID: "oc_trace", SenderID: testOwnerOpenID}

	var wait sync.WaitGroup
	for range 20 {
		wait.Add(2)
		go func() {
			defer wait.Done()
			svc.handleDriveCommentCommand(context.Background(), "/drive_comment on", msg)
		}()
		go func() {
			defer wait.Done()
			svc.handleDriveCommentCommand(context.Background(), "/drive_comment trace on", msg)
		}()
	}
	wait.Wait()

	persisted, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load(after concurrent updates) error = %v", err)
	}
	runtimeBot, ok := svc.botConfig("bot-a")
	if !ok {
		t.Fatal("runtime bot config missing")
	}
	if !persisted.Bots[0].DriveComment.Enabled || !persisted.Bots[0].DriveComment.TraceEnabled || persisted.Bots[0].DriveComment.TraceChatID != "oc_trace" {
		t.Fatalf("persisted drive comment = %+v, want merged concurrent updates", persisted.Bots[0].DriveComment)
	}
	if runtimeBot.DriveComment != persisted.Bots[0].DriveComment {
		t.Fatalf("runtime drive comment = %+v, persisted = %+v", runtimeBot.DriveComment, persisted.Bots[0].DriveComment)
	}
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(configPath), "."+filepath.Base(configPath)+".tmp-*")); err != nil {
		t.Fatalf("Glob(temp files) error = %v", err)
	} else if len(matches) != 0 {
		t.Fatalf("temporary config files remain: %v", matches)
	}
	if _, err := os.Stat(configPath + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("legacy temporary config file remains or stat failed: %v", err)
	}
}

func TestHandleDriveCommentCommandRequiresOwner(t *testing.T) {
	cfg := config.Config{Bots: []config.BotConfig{{ID: "bot-a", Workspace: t.TempDir(), OwnerOpenIDs: []string{testOwnerOpenID}}}}
	svc := NewService(cfg, NewSessionStore(""))
	msg := feishu.Message{BotID: "bot-a", ChatID: "oc_chat", SenderID: "ou_other"}

	got := svc.handleDriveCommentCommand(context.Background(), "/drive_comment on", msg)
	if !strings.Contains(got, "只有 bot owner") {
		t.Fatalf("reply = %q, want owner-only rejection", got)
	}
}
