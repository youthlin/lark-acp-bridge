package feishu

import (
	"strings"
	"sync"
	"time"
)

const defaultChatInfoCacheTTL = 10 * time.Minute
const defaultChatInfoCacheMaxEntries = 1024

type chatInfoCache struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	entries    map[string]chatInfoCacheEntry
}

type chatInfoCacheEntry struct {
	info      chatInfo
	cachedAt  time.Time
	expiresAt time.Time
}

func newChatInfoCache(ttl time.Duration) *chatInfoCache {
	return newChatInfoCacheWithMaxEntries(ttl, defaultChatInfoCacheMaxEntries)
}

func newChatInfoCacheWithMaxEntries(ttl time.Duration, maxEntries int) *chatInfoCache {
	if ttl <= 0 {
		ttl = defaultChatInfoCacheTTL
	}
	if maxEntries <= 0 {
		maxEntries = defaultChatInfoCacheMaxEntries
	}
	return &chatInfoCache{
		ttl:        ttl,
		maxEntries: maxEntries,
		entries:    make(map[string]chatInfoCacheEntry),
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
	c.evictExpiredLocked(now)
	c.entries[chatID] = chatInfoCacheEntry{
		info:      info,
		cachedAt:  now,
		expiresAt: now.Add(c.ttl),
	}
	c.evictOldestLocked()
}

func (c *chatInfoCache) evictExpiredLocked(now time.Time) {
	for id, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, id)
		}
	}
}

func (c *chatInfoCache) evictOldestLocked() {
	for len(c.entries) > c.maxEntries {
		var oldestID string
		var oldestAt time.Time
		for id, entry := range c.entries {
			cachedAt := entry.cachedAt
			if cachedAt.IsZero() {
				cachedAt = entry.expiresAt.Add(-c.ttl)
			}
			if oldestID == "" || cachedAt.Before(oldestAt) || (cachedAt.Equal(oldestAt) && id < oldestID) {
				oldestID = id
				oldestAt = cachedAt
			}
		}
		if oldestID == "" {
			return
		}
		delete(c.entries, oldestID)
	}
}
