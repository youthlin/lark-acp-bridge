package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestHandleRulesCommandPersistsAndClearsRules(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "sessions.json")
	store := NewSessionStore(storePath)
	svc := newTestService(config.Default(), store)
	msg := feishu.Message{BotID: "bot-a", ChatID: "oc_chat"}

	if got := svc.handleRulesCommand(context.Background(), "/rules", msg); got != "当前 chat 未配置补充规则。" {
		t.Fatalf("/rules reply = %q", got)
	}
	rules := "默认使用中文。\n修改代码后运行测试。"
	if got := svc.handleRulesCommand(context.Background(), "/rules set "+rules, msg); !strings.Contains(got, "已设置") {
		t.Fatalf("/rules set reply = %q", got)
	}
	chat, ok := store.GetChat(chatKeyFromMessage(msg))
	if !ok || chat.Rules != rules {
		t.Fatalf("chat = %+v ok=%v, want rules %q", chat, ok, rules)
	}
	if got := svc.handleRulesCommand(context.Background(), "/rules status", msg); !strings.Contains(got, rules) {
		t.Fatalf("/rules status reply = %q, want rules", got)
	}

	reloaded := NewSessionStore(storePath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if chat, ok := reloaded.GetChat(chatKeyFromMessage(msg)); !ok || chat.Rules != rules {
		t.Fatalf("reloaded chat = %+v ok=%v, want persisted rules", chat, ok)
	} else if chat.RulesRevision != 1 {
		t.Fatalf("reloaded rules revision = %d, want 1", chat.RulesRevision)
	}

	if got := svc.handleRulesCommand(context.Background(), "/rules clear", msg); !strings.Contains(got, "/new") {
		t.Fatalf("/rules clear reply = %q, want stale context warning", got)
	}
	if chat, ok := store.GetChat(chatKeyFromMessage(msg)); !ok || chat.Rules != "" {
		t.Fatalf("chat after clear = %+v ok=%v, want empty rules", chat, ok)
	}
}

func TestHandleRulesCommandRejectsMissingAndOversizedRules(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	svc := newTestService(config.Default(), store)
	msg := feishu.Message{BotID: "bot-a", ChatID: "oc_chat"}

	if got := svc.handleRulesCommand(context.Background(), "/rules set", msg); !strings.Contains(got, "请提供补充规则") {
		t.Fatalf("missing rules reply = %q", got)
	}
	if got := svc.handleRulesCommand(context.Background(), "/rules set "+strings.Repeat("a", chatRulesMaxBytes+1), msg); !strings.Contains(got, "不能超过 16 KiB") {
		t.Fatalf("oversized rules reply = %q", got)
	}
}

func TestUpdateChatRulesResetsAllSessionsInChat(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	sessions := []Session{
		{Key: imSessionKey("bot-a", "oc_chat", "omt_a"), AgentName: "traex", ACPSessionID: "acp-a", Cwd: "/repo", WorkspacePrompted: true},
		{Key: imSessionKey("bot-a", "oc_chat", "omt_b"), AgentName: "traex", ACPSessionID: "acp-b", Cwd: "/repo", WorkspacePrompted: true},
		{Key: imSessionKey("bot-a", "oc_other", ""), AgentName: "traex", ACPSessionID: "acp-other", Cwd: "/repo", WorkspacePrompted: true},
	}
	for _, session := range sessions {
		if err := store.Upsert(session); err != nil {
			t.Fatalf("Upsert(%s) error = %v", session.ACPSessionID, err)
		}
	}
	chat, err := store.UpdateChatRules(ChatConfig{Key: ChatKey{BotID: "bot-a", ChatID: "oc_chat"}}, "新规则")
	if err != nil {
		t.Fatalf("UpdateChatRules() error = %v", err)
	}
	if chat.Rules != "新规则" {
		t.Fatalf("chat rules = %q, want 新规则", chat.Rules)
	}
	for _, session := range sessions[:2] {
		got, ok := store.Get(session.Key)
		if !ok || got.WorkspacePrompted {
			t.Fatalf("session %s = %+v ok=%v, want prompt reset", session.ACPSessionID, got, ok)
		}
	}
	other, ok := store.Get(sessions[2].Key)
	if !ok || !other.WorkspacePrompted {
		t.Fatalf("other session = %+v ok=%v, want unchanged", other, ok)
	}
}

func TestStalePromptCannotMarkUpdatedChatRulesInjected(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := imSessionKey("bot-a", "oc_chat", "")
	chatKey := ChatKey{BotID: "bot-a", ChatID: "oc_chat"}
	session := Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-a",
		Cwd:          "/repo",
	}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	// 模拟旧 prompt 已读取 revision 0，但尚未执行完成。
	chat, err := store.UpdateChatRules(ChatConfig{Key: chatKey}, "新规则")
	if err != nil {
		t.Fatalf("UpdateChatRules() error = %v", err)
	}
	if chat.RulesRevision != 1 {
		t.Fatalf("rules revision = %d, want 1", chat.RulesRevision)
	}
	if err := store.MarkWorkspacePromptedIfRulesRevision(key, session.ACPSessionID, chatKey, 0); err != nil {
		t.Fatalf("MarkWorkspacePromptedIfRulesRevision(stale) error = %v", err)
	}
	if got, ok := store.Get(key); !ok || got.WorkspacePrompted {
		t.Fatalf("session after stale prompt = %+v ok=%v, want prompt still pending", got, ok)
	}

	if err := store.MarkWorkspacePromptedIfRulesRevision(key, session.ACPSessionID, chatKey, chat.RulesRevision); err != nil {
		t.Fatalf("MarkWorkspacePromptedIfRulesRevision(current) error = %v", err)
	}
	if got, ok := store.Get(key); !ok || !got.WorkspacePrompted {
		t.Fatalf("session after current prompt = %+v ok=%v, want prompt marked", got, ok)
	}
}
