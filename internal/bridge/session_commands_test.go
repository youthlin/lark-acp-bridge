package bridge

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestHandleFeishuSessionListSelectionOptionsAreLimited(t *testing.T) {
	items := make([]Session, 0, maxSessionHistoryPerChat+2)
	for i := 0; i < maxSessionHistoryPerChat+2; i++ {
		items = append(items, Session{
			Title:        fmt.Sprintf("会话%d", i),
			ACPSessionID: fmt.Sprintf("session-%d", i),
			Cwd:          "/repo",
		})
	}
	options := sessionSelectionOptions(items, maxSessionHistoryPerChat)
	if len(options) != maxSessionHistoryPerChat {
		t.Fatalf("len(options) = %d, want %d", len(options), maxSessionHistoryPerChat)
	}
	if options[0].ACPSessionID != "session-0" || options[len(options)-1].ACPSessionID != fmt.Sprintf("session-%d", maxSessionHistoryPerChat-1) {
		t.Fatalf("options = %+v, want first %d items", options, maxSessionHistoryPerChat)
	}
}

func TestHandleSessionSelectionRestoresSessionForOwnerOnly(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	rt := &fakeRuntime{newSessionIDs: []string{"acp-session-1", "acp-session-2"}}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	base := feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_private",
		ChatType: "p2p",
	}

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     base.BotID,
		ChatID:    base.ChatID,
		ChatType:  base.ChatType,
		MessageID: "om_first",
		Text:      "/new " + firstDir + " 第一个",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new first) error = %v", err)
	}
	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     base.BotID,
		ChatID:    base.ChatID,
		ChatType:  base.ChatType,
		MessageID: "om_second",
		Text:      "/new " + secondDir + " 第二个",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new second) error = %v", err)
	}

	display, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:        base.BotID,
		ChatID:       base.ChatID,
		RequesterID:  testOwnerOpenID,
		OperatorID:   testOwnerOpenID,
		ACPSessionID: "acp-session-1",
	})
	if err != nil {
		t.Fatalf("HandleSessionSelection(owner) error = %v", err)
	}
	if display != "第一个" {
		t.Fatalf("display = %q, want restored title", display)
	}
	session, ok := store.Get(SessionKey{BotID: base.BotID, ChatID: base.ChatID})
	if !ok {
		t.Fatalf("current session not found")
	}
	if session.ACPSessionID != "acp-session-1" || session.Cwd != firstDir {
		t.Fatalf("session = %+v, want first session restored", session)
	}
	if len(rt.closedKeys) == 0 {
		t.Fatalf("closedKeys = %+v, want runtime closed before resume", rt.closedKeys)
	}

	if _, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:        base.BotID,
		ChatID:       base.ChatID,
		RequesterID:  testOwnerOpenID,
		OperatorID:   "ou_other",
		ACPSessionID: "acp-session-2",
	}); err == nil || !strings.Contains(err.Error(), "只有发起") {
		t.Fatalf("other requester error = %v, want requester validation", err)
	}

	if _, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:        base.BotID,
		ChatID:       base.ChatID,
		RequesterID:  "ou_other",
		OperatorID:   "ou_other",
		ACPSessionID: "acp-session-2",
	}); err == nil || !strings.Contains(err.Error(), "只有 bot owner") {
		t.Fatalf("non-owner error = %v, want owner validation", err)
	}

	if _, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:        base.BotID,
		ChatID:       base.ChatID,
		OperatorID:   testOwnerOpenID,
		ACPSessionID: "acp-session-2",
	}); err == nil || !strings.Contains(err.Error(), "缺少发起人或操作者") {
		t.Fatalf("missing requester error = %v, want requester metadata validation", err)
	}

	if _, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:        base.BotID,
		ChatID:       base.ChatID,
		RequesterID:  testOwnerOpenID,
		ACPSessionID: "acp-session-2",
	}); err == nil || !strings.Contains(err.Error(), "缺少发起人或操作者") {
		t.Fatalf("missing operator error = %v, want operator metadata validation", err)
	}
}
