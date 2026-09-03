package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestHandleRestartCommandWritesAckSendsPreparingReplyAndRunsCommand(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].OwnerOpenIDs = []string{"ou_owner"}
	svc := NewService(cfg, store)
	restartCalled := make(chan struct{}, 1)
	svc.setRestartCommand(func(ctx context.Context) error {
		restartCalled <- struct{}{}
		return nil
	})
	var intermediate []string
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		intermediate = append(intermediate, text)
		return nil
	}
	svc.setOutbound("bot-a", client)
	workspace := t.TempDir()

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		SenderID:  "ou_owner",
		Text:      "/restart",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/restart) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after intermediate preparing reply", reply)
	}
	if got, want := intermediate, []string{"收到，准备重启 bridge 服务。"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("intermediate replies = %#v, want %#v", got, want)
	}
	select {
	case <-restartCalled:
	case <-time.After(time.Second):
		t.Fatal("restart command was not called")
	}
	data, err := os.ReadFile(restartAckPath(workspace))
	if err != nil {
		t.Fatalf("ReadFile(restart ack) error = %v", err)
	}
	var ack restartAck
	if err := json.Unmarshal(data, &ack); err != nil {
		t.Fatalf("Unmarshal(restart ack) error = %v", err)
	}
	if ack.BotID != "bot-a" || ack.Message.MessageID != "om_restart" || ack.Message.ChatID != "oc_chat" || ack.RequestedBy != "ou_owner" {
		t.Fatalf("restart ack = %+v, want original message target and requester", ack)
	}
}

func TestHandleRestartCommandRejectsDefaultRestartOutsideDaemon(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].OwnerOpenIDs = []string{"ou_owner"}
	svc := NewService(cfg, store)
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		t.Fatal("intermediate reply should not be sent when restart command is unavailable")
		return nil
	}
	workspace := t.TempDir()

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		SenderID:  "ou_owner",
		Text:      "/restart",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/restart) error = %v", err)
	}
	if !strings.Contains(reply, "未配置 restart_command") || !strings.Contains(reply, "systemd") {
		t.Fatalf("reply = %q, want restart_command guidance", reply)
	}
	if _, err := os.Stat(restartAckPath(workspace)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart ack file err = %v, want no ack when restart is rejected", err)
	}
}

func TestRestartCommandAllowsAdapterResolvedOwner(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	svc := NewService(cfg, store)
	adapter := feishu.NewAdapter(config.BotConfig{
		ID:           "bot-a",
		BotOpenID:    " ou_bot_resolved ",
		OwnerOpenIDs: []string{" ou_owner ", "ou_owner"},
	}, svc)
	if !svc.syncResolvedBotConfig(0, adapter) {
		t.Fatal("syncResolvedBotConfig() = false, want resolved fields copied")
	}
	if got, want := svc.botOpenID("bot-a"), "ou_bot_resolved"; got != want {
		t.Fatalf("botOpenID() = %q, want %q", got, want)
	}
	if got, want := svc.ownerOpenIDs("bot-a"), []string{"ou_owner"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ownerOpenIDs() = %#v, want %#v", got, want)
	}

	restartCalled := make(chan struct{}, 1)
	svc.setRestartCommand(func(ctx context.Context) error {
		restartCalled <- struct{}{}
		return nil
	})
	ctx := context.Background()
	client := newFakeSentMessageClient("")
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		return nil
	}
	svc.setOutbound("bot-a", client)

	reply, err := handleFeishuMessage(t, svc, ctx, feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		SenderID:  "ou_owner",
		Text:      "/restart",
		Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/restart) error = %v", err)
	}
	if reply != "" {
		t.Fatalf("reply = %q, want empty after accepted restart command", reply)
	}
	select {
	case <-restartCalled:
	case <-time.After(time.Second):
		t.Fatal("restart command was not called for adapter-resolved owner")
	}
}
