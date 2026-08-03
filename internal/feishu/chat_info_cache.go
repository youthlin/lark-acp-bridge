package feishu

import (
	"strings"
	"sync"
	"time"
)

const defaultChatInfoCacheTTL = 10 * time.Minute

type chatInfoCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]chatInfoCacheEntry
}

type chatInfoCacheEntry struct {
	info      chatInfo
	expiresAt time.Time
}

func newChatInfoCache(ttl time.Duration) *chatInfoCache {
	if ttl <= 0 {
		ttl = defaultChatInfoCacheTTL
	}
	return &chatInfoCache{
		ttl:     ttl,
		entries: make(map[string]chatInfoCacheEntry),
	}
}

func (c *chatInfoCache) Get(chatID string) (chatInfo, bool) {
	if c == nil {
		return chatInfo{}, false
	}
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return chatInfo{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[chatID]
	if !ok {
		return chatInfo{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.entries, chatID)
		return chatInfo{}, false
	}
	return entry.info, true
}

func (c *chatInfoCache) Set(chatID string, info chatInfo) {
	if c == nil {
		return
	}
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	// 写入时顺带淘汰已过期的条目，避免长期不再访问的群信息无限残留。
	for id, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, id)
		}
	}
	c.entries[chatID] = chatInfoCacheEntry{
		info:      info,
		expiresAt: now.Add(c.ttl),
	}
}
