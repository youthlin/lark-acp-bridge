package feishu

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultMessageDeduperTTL = 30 * time.Minute
	defaultMessageDeduperMax = 10000
)

type messageDeduper struct {
	mu        sync.Mutex
	ttl       time.Duration
	max       int
	path      string
	seen      map[string]time.Time
	nextSweep time.Time
}

func newMessageDeduper(ttl time.Duration, max int) *messageDeduper {
	if ttl <= 0 {
		ttl = defaultMessageDeduperTTL
	}
	if max <= 0 {
		max = defaultMessageDeduperMax
	}
	return &messageDeduper{
		ttl:  ttl,
		max:  max,
		seen: make(map[string]time.Time),
	}
}

func (d *messageDeduper) WithPath(path string) *messageDeduper {
	d.path = strings.TrimSpace(path)
	return d
}

func (d *messageDeduper) Load() error {
	if d.path == "" {
		return nil
	}

	data, err := os.ReadFile(d.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			d.mu.Lock()
			d.seen = make(map[string]time.Time)
			d.nextSweep = time.Time{}
			d.mu.Unlock()
			return nil
		}
		return fmt.Errorf("读取消息去重文件: %w", err)
	}
	var file processedMessageFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("解析消息去重文件: %w", err)
	}

	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = make(map[string]time.Time, len(file.Messages))
	for _, item := range file.Messages {
		if strings.TrimSpace(item.MessageID) == "" || !item.ExpiresAt.After(now) {
			continue
		}
		d.seen[dedupeKey(item.BotID, item.MessageID)] = item.ExpiresAt
	}
	d.sweepLocked(now)
	return nil
}

func (d *messageDeduper) Allow(botID, messageID string) (bool, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return true, nil
	}
	now := time.Now()
	key := dedupeKey(botID, messageID)

	d.mu.Lock()
	defer d.mu.Unlock()

	if expiresAt, ok := d.seen[key]; ok && expiresAt.After(now) {
		return false, nil
	}
	d.seen[key] = now.Add(d.ttl)
	if d.nextSweep.IsZero() || now.After(d.nextSweep) || len(d.seen) > d.max {
		d.sweepLocked(now)
	}
	if err := d.writeLocked(); err != nil {
		return true, err
	}
	return true, nil
}

func (d *messageDeduper) sweepLocked(now time.Time) {
	for key, expiresAt := range d.seen {
		if !expiresAt.After(now) {
			delete(d.seen, key)
		}
	}
	for len(d.seen) > d.max {
		var oldestKey string
		var oldestExpiresAt time.Time
		for key, expiresAt := range d.seen {
			if oldestKey == "" || expiresAt.Before(oldestExpiresAt) {
				oldestKey = key
				oldestExpiresAt = expiresAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(d.seen, oldestKey)
	}
	d.nextSweep = now.Add(d.ttl / 2)
}

func (d *messageDeduper) writeLocked() error {
	if d.path == "" {
		return nil
	}
	file := processedMessageFile{
		Version:  1,
		Messages: make([]processedMessage, 0, len(d.seen)),
	}
	now := time.Now()
	for key, expiresAt := range d.seen {
		if !expiresAt.After(now) {
			continue
		}
		botID, messageID := splitDedupeKey(key)
		file.Messages = append(file.Messages, processedMessage{
			BotID:     botID,
			MessageID: messageID,
			ExpiresAt: expiresAt,
		})
	}
	sort.Slice(file.Messages, func(i, j int) bool {
		if file.Messages[i].BotID != file.Messages[j].BotID {
			return file.Messages[i].BotID < file.Messages[j].BotID
		}
		return file.Messages[i].MessageID < file.Messages[j].MessageID
	})
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("编码消息去重文件: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return fmt.Errorf("创建消息去重目录: %w", err)
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时消息去重文件: %w", err)
	}
	if err := os.Rename(tmp, d.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换消息去重文件: %w", err)
	}
	return nil
}

func dedupeKey(botID, messageID string) string {
	return strings.TrimSpace(botID) + "\x00" + strings.TrimSpace(messageID)
}

func splitDedupeKey(key string) (string, string) {
	botID, messageID, ok := strings.Cut(key, "\x00")
	if !ok {
		return "", key
	}
	return botID, messageID
}

type processedMessageFile struct {
	Version  int                `json:"version"`
	Messages []processedMessage `json:"messages"`
}

type processedMessage struct {
	BotID     string    `json:"bot_id,omitempty"`
	MessageID string    `json:"message_id"`
	ExpiresAt time.Time `json:"expires_at"`
}
