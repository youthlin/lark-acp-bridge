package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestHandleWikiTraceCommandUpdatesBotConfig(t *testing.T) {
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

	got := svc.handleWikiCommand(context.Background(), "/wiki trace on", msg)
	if !strings.Contains(got, "自动知识沉淀过程展示：开启") || !strings.Contains(got, "过程卡片目的地：oc_trace") || strings.Contains(got, "展示模式") {
		t.Fatalf("trace on reply = %q, want enabled status without mode", got)
	}
	persisted, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load(after trace on) error = %v", err)
	}
	if cfg := persisted.Bots[0].WikiTrace; !cfg.Enabled || cfg.ChatID != "oc_trace" {
		t.Fatalf("persisted wiki_trace = %+v, want enabled oc_trace", cfg)
	}
	runtimeBot, ok := svc.botConfig("bot-a")
	if !ok || runtimeBot.WikiTrace != persisted.Bots[0].WikiTrace {
		t.Fatalf("runtime wiki_trace = %+v, persisted = %+v", runtimeBot.WikiTrace, persisted.Bots[0].WikiTrace)
	}
}

func TestHandleWikiTraceNewCreatesTopicChat(t *testing.T) {
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
	client := newFakeSentMessageClient("")
	var request feishu.CreateDriveCommentTraceChatRequest
	client.traceChatCreator = func(_ context.Context, req feishu.CreateDriveCommentTraceChatRequest) (feishu.CreatedChat, error) {
		request = req
		return feishu.CreatedChat{ChatID: "oc_wiki_trace", ChatType: "private", GroupMessageType: "thread"}, nil
	}
	client.traceBotNameProvider = func(context.Context) (string, error) {
		return "智能助手", nil
	}
	svc.setOutbound("bot-a", client)

	got := svc.handleWikiCommand(context.Background(), "/wiki trace new", feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_current",
		SenderID: testOwnerOpenID,
	})
	if !strings.Contains(got, "chat_id：oc_wiki_trace") || !strings.Contains(got, "过程卡片目的地：oc_wiki_trace") {
		t.Fatalf("trace new reply = %q, want new trace chat status", got)
	}
	if request.Name != "知识沉淀过程通知群-智能助手" || request.OwnerOpenID != testOwnerOpenID {
		t.Fatalf("create chat request = %+v, want wiki trace topic chat", request)
	}
}

func TestHandleWikiTraceCommandRequiresOwner(t *testing.T) {
	cfg := config.Config{Bots: []config.BotConfig{{ID: "bot-a", Workspace: t.TempDir(), OwnerOpenIDs: []string{testOwnerOpenID}}}}
	svc := NewService(cfg, NewSessionStore(""))
	got := svc.handleWikiCommand(context.Background(), "/wiki trace on", feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		SenderID: "ou_other",
	})
	if !strings.Contains(got, "只有 bot owner") {
		t.Fatalf("reply = %q, want owner-only rejection", got)
	}
}
