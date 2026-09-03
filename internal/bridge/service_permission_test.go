package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestHandleFeishuMessageSlashCommandRejectsNonOwner(t *testing.T) {
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].OwnerOpenIDs = []string{"ou_owner"}
	svc := NewService(cfg, nil)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		Text:     "/status",
		SenderID: "ou_someone",
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/status) error = %v", err)
	}
	if reply != "只有 bot owner 可以执行斜杠命令。" {
		t.Fatalf("reply = %q, want non-owner warning", reply)
	}
}

func TestHandleFeishuTopicThreadCommandsRemainOwnerOnly(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	svc.setRuntime(&fakeRuntime{newSessionID: "acp-session-1"})
	msg := feishu.Message{
		BotID:            "bot-a",
		ChatID:           "oc_group",
		ChatType:         "topic_group",
		GroupMessageType: "thread",
		ThreadID:         "omt_topic",
		SenderID:         "ou_other",
		Mentions:         []feishu.Mention{testBotMention("智能助手")},
	}

	for _, command := range []string{
		"@智能助手 /new " + t.TempDir(),
		"@智能助手 /session list",
	} {
		msg.MessageID = "om_" + strings.Fields(command)[1]
		msg.Text = command
		reply, err := handleFeishuMessage(t, svc, context.Background(), msg)
		if err != nil {
			t.Fatalf("HandleFeishuMessage(%q) error = %v", command, err)
		}
		if reply != "只有 bot owner 可以执行斜杠命令。" {
			t.Fatalf("HandleFeishuMessage(%q) = %q, want owner-only rejection", command, reply)
		}
	}
}

func TestHandleFeishuMessagePermissionRequestDefaultsToReject(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	session := testReadySession(t, store)
	rt := &fakeRuntime{
		promptReply: "done",
		permissionRequest: &acp.PermissionRequest{
			RequestID: "1",
			SessionID: session.ACPSessionID,
			ToolCall:  acp.PermissionToolCallRef{ToolCallID: "call-1"},
			Options: []acp.PermissionOption{
				{OptionID: "allow-once", Kind: "allow_once"},
				{OptionID: "reject-once", Kind: "reject_once"},
			},
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     session.Key.BotID,
		ChatID:    sessionKeyMainID(session.Key),
		ThreadID:  session.Key.SubID,
		ChatType:  "topic_group",
		Mentions:  testBotMentions(),
		Text:      "run tests",
		Workspace: session.Workspace,
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(prompt) error = %v", err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q, want done", reply)
	}
	if rt.permissionOutcome.Outcome != "selected" || rt.permissionOutcome.OptionID != "reject-once" {
		t.Fatalf("permission outcome = %+v, want reject-once", rt.permissionOutcome)
	}
}

func TestHandleRestartCommandRequiresOwner(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].OwnerOpenIDs = []string{"ou_owner"}
	svc := NewService(cfg, store)
	svc.setRestartCommand(func(ctx context.Context) error {
		t.Fatal("restart command should not run for non-owner")
		return nil
	})

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
		SenderID:  "ou_other",
		Text:      "/restart",
		Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/restart) error = %v", err)
	}
	if reply != "只有 bot owner 可以执行斜杠命令。" {
		t.Fatalf("reply = %q, want owner warning", reply)
	}
}
