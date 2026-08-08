package feishu

import (
	"testing"
	"time"
)

func TestChatInfoCacheSetEvictsExpiredEntries(t *testing.T) {
	c := newChatInfoCache(time.Hour)
	now := time.Now()
	// 直接构造一条已过期条目，模拟长期不再访问的群。
	c.entries["stale"] = chatInfoCacheEntry{
		info:      chatInfo{Name: "stale"},
		cachedAt:  now.Add(-2 * time.Hour),
		expiresAt: now.Add(-time.Minute),
	}
	c.entries["fresh"] = chatInfoCacheEntry{
		info:      chatInfo{Name: "fresh"},
		cachedAt:  now.Add(-time.Minute),
		expiresAt: now.Add(time.Minute),
	}

	c.Set("new", chatInfo{Name: "new"})

	if _, ok := c.entries["stale"]; ok {
		t.Fatalf("Set 后过期条目 stale 应被淘汰")
	}
	if _, ok := c.entries["fresh"]; !ok {
		t.Fatalf("Set 不应淘汰未过期条目 fresh")
	}
	if _, ok := c.entries["new"]; !ok {
		t.Fatalf("Set 应写入新条目")
	}
}

func TestChatInfoCacheGetExpiredTreatsAsMiss(t *testing.T) {
	c := newChatInfoCache(time.Hour)
	c.entries["stale"] = chatInfoCacheEntry{
		info:      chatInfo{Name: "stale"},
		cachedAt:  time.Now().Add(-2 * time.Hour),
		expiresAt: time.Now().Add(-time.Minute),
	}

	if _, ok := c.Get("stale"); ok {
		t.Fatalf("过期条目 Get 应返回 miss")
	}
	if _, ok := c.entries["stale"]; ok {
		t.Fatalf("Get miss 过期条目应同步删除")
	}
}

func TestChatInfoCacheIgnoresNilAndEmpty(t *testing.T) {
	var c *chatInfoCache
	if _, ok := c.Get("x"); ok {
		t.Fatalf("nil 缓存 Get 应返回 miss")
	}
	c.Set("x", chatInfo{Name: "x"}) // 不应 panic

	c = newChatInfoCache(time.Hour)
	c.Set("  ", chatInfo{Name: "blank"})
	if len(c.entries) != 0 {
		t.Fatalf("空白 chatID 不应写入缓存，实际有 %d 条", len(c.entries))
	}
}

func TestChatInfoCacheSetEvictsOldestWhenOverCapacity(t *testing.T) {
	c := newChatInfoCacheWithMaxEntries(time.Hour, 2)
	now := time.Now()
	c.entries["old"] = chatInfoCacheEntry{
		info:      chatInfo{Name: "old"},
		cachedAt:  now.Add(-2 * time.Hour),
		expiresAt: now.Add(time.Hour),
	}
	c.entries["newer"] = chatInfoCacheEntry{
		info:      chatInfo{Name: "newer"},
		cachedAt:  now.Add(-time.Hour),
		expiresAt: now.Add(time.Hour),
	}

	c.Set("newest", chatInfo{Name: "newest"})

	if _, ok := c.Get("old"); ok {
		t.Fatalf("容量超限时应淘汰最旧条目 old")
	}
	for _, id := range []string{"newer", "newest"} {
		if _, ok := c.Get(id); !ok {
			t.Fatalf("容量淘汰后应保留 %s", id)
		}
	}
	if len(c.entries) != 2 {
		t.Fatalf("cache entries = %d, want 2", len(c.entries))
	}
}

func TestChatInfoCacheSetEvictsExpiredBeforeOldest(t *testing.T) {
	c := newChatInfoCacheWithMaxEntries(time.Hour, 2)
	now := time.Now()
	c.entries["expired"] = chatInfoCacheEntry{
		info:      chatInfo{Name: "expired"},
		cachedAt:  now.Add(-2 * time.Hour),
		expiresAt: now.Add(-time.Minute),
	}
	c.entries["old-live"] = chatInfoCacheEntry{
		info:      chatInfo{Name: "old-live"},
		cachedAt:  now.Add(-time.Hour),
		expiresAt: now.Add(time.Hour),
	}

	c.Set("new", chatInfo{Name: "new"})

	if _, ok := c.entries["expired"]; ok {
		t.Fatalf("写入前应优先淘汰过期条目")
	}
	if _, ok := c.entries["old-live"]; !ok {
		t.Fatalf("有过期条目可淘汰时不应淘汰仍有效的最旧条目")
	}
	if _, ok := c.entries["new"]; !ok {
		t.Fatalf("新条目应写入缓存")
	}
}
