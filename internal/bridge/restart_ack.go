package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const (
	restartAckFileName = "restart_ack.json"
	restartAckMaxAge   = 10 * time.Minute
)

type restartAck struct {
	Version     int            `json:"version"`
	ID          string         `json:"id"`
	CreatedAt   time.Time      `json:"created_at"`
	BotID       string         `json:"bot_id"`
	RequestedBy string         `json:"requested_by,omitempty"`
	Message     restartMessage `json:"message"`
}

type restartMessage struct {
	MessageID string `json:"message_id"`
	ChatID    string `json:"chat_id"`
	ChatType  string `json:"chat_type"`
	ThreadID  string `json:"thread_id,omitempty"`
	RootID    string `json:"root_id,omitempty"`
	ParentID  string `json:"parent_id,omitempty"`
}

type restartAckSender interface {
	SendText(context.Context, feishu.Message, string) error
}

func restartAckPath(workspace string) string {
	return filepath.Join(workspace, restartAckFileName)
}

func newRestartAck(msg feishu.Message) restartAck {
	now := time.Now()
	return restartAck{
		Version:     1,
		ID:          fmt.Sprintf("%d", now.UnixNano()),
		CreatedAt:   now,
		BotID:       strings.TrimSpace(msg.BotID),
		RequestedBy: strings.TrimSpace(msg.SenderID),
		Message: restartMessage{
			MessageID: strings.TrimSpace(msg.MessageID),
			ChatID:    strings.TrimSpace(msg.ChatID),
			ChatType:  strings.TrimSpace(msg.ChatType),
			ThreadID:  strings.TrimSpace(msg.ThreadID),
			RootID:    strings.TrimSpace(msg.RootID),
			ParentID:  strings.TrimSpace(msg.ParentID),
		},
	}
}

func writeRestartAck(workspace string, ack restartAck) error {
	path := restartAckPath(workspace)
	if strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("workspace 不能为空")
	}
	if ack.Message.MessageID == "" {
		return fmt.Errorf("重启确认消息缺少 message_id")
	}
	data, err := json.MarshalIndent(ack, "", "  ")
	if err != nil {
		return fmt.Errorf("编码重启确认记录: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建重启确认目录: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时重启确认记录: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换重启确认记录: %w", err)
	}
	return nil
}

func removeRestartAck(workspace string) {
	path := restartAckPath(workspace)
	if strings.TrimSpace(workspace) == "" {
		return
	}
	_ = os.Remove(path)
}

func consumeRestartAck(ctx context.Context, workspace string, sender restartAckSender, botID string) error {
	path := restartAckPath(workspace)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("读取重启确认记录: %w", err)
	}
	var ack restartAck
	if err := json.Unmarshal(data, &ack); err != nil {
		return fmt.Errorf("解析重启确认记录: %w", err)
	}
	if restartAckExpired(ack, time.Now()) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("删除过期重启确认记录: %w", err)
		}
		return nil
	}
	if sender == nil {
		return fmt.Errorf("重启确认 sender 为空")
	}
	msg := feishu.Message{
		BotID:     firstNonEmpty(ack.BotID, botID),
		MessageID: ack.Message.MessageID,
		ChatID:    ack.Message.ChatID,
		ChatType:  ack.Message.ChatType,
		ThreadID:  ack.Message.ThreadID,
		RootID:    ack.Message.RootID,
		ParentID:  ack.Message.ParentID,
	}
	if err := sender.SendText(ctx, msg, restartAckText()); err != nil {
		return fmt.Errorf("发送重启确认消息: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除重启确认记录: %w", err)
	}
	return nil
}

func restartAckExpired(ack restartAck, now time.Time) bool {
	if ack.CreatedAt.IsZero() {
		return true
	}
	return now.Sub(ack.CreatedAt) > restartAckMaxAge
}

func restartAckText() string {
	return "已重启。"
}
