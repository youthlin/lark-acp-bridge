package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestConsumeRestartAckSendsConfirmationAndRemovesFile(t *testing.T) {
	workspace := t.TempDir()
	ack := newRestartAck(feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "group",
		ThreadID:  "omt_thread",
		SenderID:  "ou_owner",
	})
	if err := writeRestartAck(workspace, ack); err != nil {
		t.Fatalf("writeRestartAck() error = %v", err)
	}
	sender := &fakeRestartAckSender{}

	if err := consumeRestartAck(context.Background(), workspace, sender, "bot-a"); err != nil {
		t.Fatalf("consumeRestartAck() error = %v", err)
	}
	if _, err := os.Stat(restartAckPath(workspace)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart ack file err = %v, want removed", err)
	}
	if got := sender.messages; len(got) != 1 || got[0].MessageID != "om_restart" || got[0].ThreadID != "omt_thread" || sender.texts[0] != restartAckText() {
		t.Fatalf("sent messages = %+v texts = %+v, want restart confirmation to original message", sender.messages, sender.texts)
	}
}

func TestConsumeRestartAckLoadsLegacyPath(t *testing.T) {
	workspace := t.TempDir()
	ack := newRestartAck(feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "group",
		SenderID:  "ou_owner",
	})
	data, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	legacyPath := legacyRestartAckPath(workspace)
	if err := os.WriteFile(legacyPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile(legacy restart ack) error = %v", err)
	}
	sender := &fakeRestartAckSender{}

	if err := consumeRestartAck(context.Background(), workspace, sender, "bot-a"); err != nil {
		t.Fatalf("consumeRestartAck() error = %v", err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy restart ack err = %v, want removed", err)
	}
	if got := sender.messages; len(got) != 1 || got[0].MessageID != "om_restart" {
		t.Fatalf("sent messages = %+v, want legacy restart confirmation", got)
	}
}

func TestConsumeRestartAckKeepsFileWhenSendFails(t *testing.T) {
	workspace := t.TempDir()
	if err := writeRestartAck(workspace, newRestartAck(feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_restart",
		ChatID:    "oc_chat",
		ChatType:  "p2p",
	})); err != nil {
		t.Fatalf("writeRestartAck() error = %v", err)
	}
	sender := &fakeRestartAckSender{err: fmt.Errorf("send failed")}

	err := consumeRestartAck(context.Background(), workspace, sender, "bot-a")
	if err == nil || !strings.Contains(err.Error(), "发送重启确认消息") {
		t.Fatalf("consumeRestartAck() error = %v, want send error", err)
	}
	if _, statErr := os.Stat(restartAckPath(workspace)); statErr != nil {
		t.Fatalf("restart ack file should remain after send failure, stat err = %v", statErr)
	}
}

func TestConsumeRestartAckRemovesInvalidJSONFile(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(restartAckPath(workspace)), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(restartAckPath(workspace), []byte("{invalid json"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	sender := &fakeRestartAckSender{}

	if err := consumeRestartAck(context.Background(), workspace, sender, "bot-a"); err != nil {
		t.Fatalf("consumeRestartAck() error = %v", err)
	}
	if _, err := os.Stat(restartAckPath(workspace)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart ack file err = %v, want removed", err)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("sent messages = %+v, want none for invalid ack", sender.messages)
	}
}

func TestConsumeRestartAckRemovesMissingTargetFile(t *testing.T) {
	workspace := t.TempDir()
	ack := newRestartAck(feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "group",
	})
	data, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(restartAckPath(workspace)), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(restartAckPath(workspace), data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	sender := &fakeRestartAckSender{}

	if err := consumeRestartAck(context.Background(), workspace, sender, "bot-a"); err != nil {
		t.Fatalf("consumeRestartAck() error = %v", err)
	}
	if _, err := os.Stat(restartAckPath(workspace)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restart ack file err = %v, want removed", err)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("sent messages = %+v, want none for ack without message target", sender.messages)
	}
}

type fakeRestartAckSender struct {
	messages []feishu.Message
	texts    []string
	err      error
}

func (f *fakeRestartAckSender) SendText(ctx context.Context, msg feishu.Message, text string) error {
	if f.err != nil {
		return f.err
	}
	f.messages = append(f.messages, msg)
	f.texts = append(f.texts, text)
	return nil
}
