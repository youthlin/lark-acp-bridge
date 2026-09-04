package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestHandleMeetingCommandPersistsConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Config{Bots: []config.BotConfig{{
		ID:           "bot-a",
		AppID:        "cli_a",
		AppSecret:    config.FileSecret("secret.appsecret"),
		Workspace:    t.TempDir(),
		OwnerOpenIDs: []string{testOwnerOpenID},
	}}}
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	svc := NewService(loaded, NewSessionStore("")).WithConfigPath(configPath)
	msg := feishu.Message{BotID: "bot-a", SenderID: testOwnerOpenID}

	got := svc.handleMeetingCommand(context.Background(), "/meeting on", msg)
	if !strings.Contains(got, "静默会议助手：开启") || !strings.Contains(got, testOwnerOpenID) {
		t.Fatalf("on reply = %q", got)
	}
	got = svc.handleMeetingCommand(context.Background(), "/meeting trace on", msg)
	if !strings.Contains(got, "会议整理 ACP trace：开启") {
		t.Fatalf("trace reply = %q", got)
	}
	got = svc.handleMeetingCommand(context.Background(), "/meeting off", msg)
	if !strings.Contains(got, "正在进行的会议仍会整理到结束") {
		t.Fatalf("off reply = %q", got)
	}
	persisted, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	meeting := persisted.Bots[0].Meeting
	if meeting.Enabled || !meeting.TraceEnabled || meeting.RecipientOpenID != testOwnerOpenID {
		t.Fatalf("meeting = %+v", meeting)
	}
}

func TestHandleMeetingCommandRequiresUniqueRecipient(t *testing.T) {
	cfg := config.Config{Bots: []config.BotConfig{{
		ID:           "bot-a",
		Workspace:    t.TempDir(),
		OwnerOpenIDs: []string{testOwnerOpenID, "ou_other"},
	}}}
	svc := NewService(cfg, NewSessionStore("")).WithConfigPath(filepath.Join(t.TempDir(), "config.json"))
	got := svc.handleMeetingCommand(context.Background(), "/meeting on", feishu.Message{BotID: "bot-a", SenderID: testOwnerOpenID})
	if !strings.Contains(got, "需要配置唯一的 meeting.recipient_open_id") {
		t.Fatalf("reply = %q", got)
	}
}

func TestHandleMeetingCommandRequiresOwner(t *testing.T) {
	cfg := config.Config{Bots: []config.BotConfig{{ID: "bot-a", Workspace: t.TempDir(), OwnerOpenIDs: []string{testOwnerOpenID}}}}
	svc := NewService(cfg, NewSessionStore(""))
	got := svc.handleMeetingCommand(context.Background(), "/meeting on", feishu.Message{BotID: "bot-a", SenderID: "ou_other"})
	if !strings.Contains(got, "只有 bot owner") {
		t.Fatalf("reply = %q", got)
	}
}
