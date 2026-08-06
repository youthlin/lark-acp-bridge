package bridge

import (
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func newPendingTestService() *Service {
	return NewService(config.Config{}, nil)
}

func testPendingKey() SessionKey {
	return normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat"})
}

func TestRestorePendingAtAutoMessagesPrepends(t *testing.T) {
	s := newPendingTestService()
	key := testPendingKey()

	restored := []pendingAtMessage{
		{SenderID: "u1", Text: "restored-1"},
		{SenderID: "u2", Text: "restored-2"},
	}
	existing := []pendingAtMessage{
		{SenderID: "u3", Text: "existing-1"},
	}
	s.pendingAtAuto[key] = existing

	s.restorePendingAtAutoMessages(key, restored)

	got := s.pendingAtAuto[key]
	if len(got) != 3 {
		t.Fatalf("pending len = %d, want 3", len(got))
	}
	if got[0].Text != "restored-1" || got[1].Text != "restored-2" || got[2].Text != "existing-1" {
		t.Fatalf("pending order = %+v, want restored first then existing", got)
	}
}

func TestRestorePendingAtAutoMessagesCapsAtMax(t *testing.T) {
	s := newPendingTestService()
	key := testPendingKey()

	restored := make([]pendingAtMessage, maxPendingAtAuto+2)
	for i := range restored {
		restored[i] = pendingAtMessage{SenderID: "r", Text: "r"}
	}
	s.restorePendingAtAutoMessages(key, restored)

	if got := len(s.pendingAtAuto[key]); got != maxPendingAtAuto {
		t.Fatalf("pending len = %d, want capped at %d", got, maxPendingAtAuto)
	}
}

func TestTakeThenRestorePendingAtAutoRoundTrip(t *testing.T) {
	s := newPendingTestService()
	key := testPendingKey()
	s.pendingAtAuto[key] = []pendingAtMessage{
		{SenderID: "u1", Text: "one"},
		{SenderID: "u2", Text: "two"},
	}

	taken := s.takePendingAtAutoMessages(key)
	if len(taken) != 2 {
		t.Fatalf("taken len = %d, want 2", len(taken))
	}
	if len(s.pendingAtAuto[key]) != 0 {
		t.Fatalf("pending not drained after take")
	}

	s.restorePendingAtAutoMessages(key, taken)
	if got := len(s.pendingAtAuto[key]); got != 2 {
		t.Fatalf("pending len after restore = %d, want 2", got)
	}
}
