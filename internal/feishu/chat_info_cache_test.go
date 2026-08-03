package feishu

import (
	"testing"
	"time"
)

func TestChatInfoCacheSetEvictsExpiredEntries(t *testing.T) {
	c := newChatInfoCache(time.Hour)
	// 直接构造一条已过期条目，模拟长期不再访问的群。
	c.entries["stale"] = chatInfoCacheEntry{
		info:      chatInfo{Name: "stale"},
		expiresAt: time.Now().Add(-time.Minute),
	}
	c.entries["fresh"] = chatInfoCacheEntry{
		info:      chatInfo{Name: "fresh"},
		expiresAt: time.Now().Add(time.Minute),
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
