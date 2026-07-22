package feishu

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

type countingHandler struct {
	count int
	ctx   context.Context
}

func (h *countingHandler) HandleFeishuMessage(ctx context.Context, msg Message) (string, error) {
	h.count++
	h.ctx = ctx
	return "", nil
}

type replyingHandler struct {
	reply string
}

func (h replyingHandler) HandleFeishuMessage(ctx context.Context, msg Message) (string, error) {
	return h.reply, nil
}

type fakeReactionClient struct {
	added   []string
	deleted []fakeReactionDelete
}

type fakeReactionDelete struct {
	MessageID  string
	ReactionID string
}

func (f *fakeReactionClient) AddReaction(ctx context.Context, messageID string) (string, error) {
	f.added = append(f.added, messageID)
	return "reaction-" + messageID, nil
}

func (f *fakeReactionClient) DeleteReaction(ctx context.Context, messageID string, reactionID string) error {
	f.deleted = append(f.deleted, fakeReactionDelete{MessageID: messageID, ReactionID: reactionID})
	return nil
}

func TestAdapterSkipsDuplicateMessageID(t *testing.T) {
	handler := &countingHandler{}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	event := textEvent("om_dup", "oc_1", "hello")

	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage(first) error = %v", err)
	}
	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage(second) error = %v", err)
	}
	if handler.count != 1 {
		t.Fatalf("handler count = %d, want 1", handler.count)
	}
}

func TestAdapterAddsAndDeletesProcessingReaction(t *testing.T) {
	reactions := &fakeReactionClient{}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, replyingHandler{})
	adapter.reaction = reactions
	event := textEvent("om_1", "oc_1", "hello")

	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage() error = %v", err)
	}
	if len(reactions.added) != 1 || reactions.added[0] != "om_1" {
		t.Fatalf("added reactions = %+v, want om_1", reactions.added)
	}
	if len(reactions.deleted) != 1 || reactions.deleted[0] != (fakeReactionDelete{MessageID: "om_1", ReactionID: "reaction-om_1"}) {
		t.Fatalf("deleted reactions = %+v, want matching reaction deletion", reactions.deleted)
	}
}

func TestRandomProcessingReactionEmojiUsesConfiguredSet(t *testing.T) {
	allowed := make(map[string]bool)
	for _, emoji := range processingReactionEmojis {
		allowed[emoji] = true
	}
	if len(allowed) == 0 {
		t.Fatal("processingReactionEmojis should not be empty")
	}
	for i := 0; i < 100; i++ {
		if emoji := randomProcessingReactionEmoji(); !allowed[emoji] {
			t.Fatalf("randomProcessingReactionEmoji() = %q, want one of %v", emoji, processingReactionEmojis)
		}
	}
}

func TestAdapterSkipsProcessingReactionForDuplicateMessage(t *testing.T) {
	reactions := &fakeReactionClient{}
	handler := &countingHandler{}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	adapter.reaction = reactions
	event := textEvent("om_dup", "oc_1", "hello")

	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage(first) error = %v", err)
	}
	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage(second) error = %v", err)
	}
	if handler.count != 1 {
		t.Fatalf("handler count = %d, want 1", handler.count)
	}
	if len(reactions.added) != 1 || len(reactions.deleted) != 1 {
		t.Fatalf("reaction lifecycle = added %+v deleted %+v, want only first message", reactions.added, reactions.deleted)
	}
}

func TestAdapterAddsMessageAttrsToContext(t *testing.T) {
	handler := &countingHandler{}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	event := textEvent("om_1", "oc_1", "hello")
	event.Event.Message.ChatType = ptr("group")
	event.Event.Message.ThreadId = ptr("omt_1")
	event.Event.Message.RootId = ptr("om_root")
	event.Event.Message.ParentId = ptr("om_parent")

	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage() error = %v", err)
	}

	attrs := attrsMap(logging.CtxAttrs(handler.ctx))
	for key, want := range map[string]string{
		"bot":        "bot-a",
		"chat_id":    "oc_1",
		"chat_type":  "group",
		"message_id": "om_1",
		"thread_id":  "omt_1",
		"root_id":    "om_root",
		"parent_id":  "om_parent",
		"sender_id":  "ou_sender",
	} {
		if got := attrs[key]; got != want {
			t.Fatalf("ctx attr %s = %q, want %q; attrs=%v", key, got, want, attrs)
		}
	}
}

func TestMessageDeduperAllowsSameMessageIDForDifferentBots(t *testing.T) {
	deduper := newMessageDeduper(time.Minute, 100)
	if allowed, err := deduper.Allow("bot-a", "om_1"); err != nil || !allowed {
		t.Fatalf("Allow(bot-a first) = %v, %v; want true, nil", allowed, err)
	}
	if allowed, err := deduper.Allow("bot-b", "om_1"); err != nil || !allowed {
		t.Fatalf("Allow(bot-b first) = %v, %v; want true, nil", allowed, err)
	}
	if allowed, err := deduper.Allow("bot-a", "om_1"); err != nil || allowed {
		t.Fatalf("Allow(bot-a duplicate) = %v, %v; want false, nil", allowed, err)
	}
}

func TestMessageDeduperPersistsProcessedMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "processed_messages.json")
	first := newMessageDeduper(time.Minute, 100).WithPath(path)
	if allowed, err := first.Allow("bot-a", "om_1"); err != nil || !allowed {
		t.Fatalf("Allow(first) = %v, %v; want true, nil", allowed, err)
	}

	second := newMessageDeduper(time.Minute, 100).WithPath(path)
	if err := second.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if allowed, err := second.Allow("bot-a", "om_1"); err != nil || allowed {
		t.Fatalf("Allow(after load duplicate) = %v, %v; want false, nil", allowed, err)
	}
	if allowed, err := second.Allow("bot-a", "om_2"); err != nil || !allowed {
		t.Fatalf("Allow(new message) = %v, %v; want true, nil", allowed, err)
	}
}

func TestMessageDeduperSkipsExpiredPersistedMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "processed_messages.json")
	expired := newMessageDeduper(time.Millisecond, 100).WithPath(path)
	if allowed, err := expired.Allow("bot-a", "om_1"); err != nil || !allowed {
		t.Fatalf("Allow(first) = %v, %v; want true, nil", allowed, err)
	}
	time.Sleep(2 * time.Millisecond)

	fresh := newMessageDeduper(time.Minute, 100).WithPath(path)
	if err := fresh.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if allowed, err := fresh.Allow("bot-a", "om_1"); err != nil || !allowed {
		t.Fatalf("Allow(expired message) = %v, %v; want true, nil", allowed, err)
	}
}

func TestMessageDeduperAllowsEmptyMessageID(t *testing.T) {
	deduper := newMessageDeduper(time.Minute, 100)
	if allowed, err := deduper.Allow("bot-a", ""); err != nil || !allowed {
		t.Fatalf("Allow(empty) = %v, %v; want true, nil", allowed, err)
	}
	if allowed, err := deduper.Allow("bot-a", " "); err != nil || !allowed {
		t.Fatalf("Allow(blank) = %v, %v; want true, nil", allowed, err)
	}
}

func TestMessageDeduperLoadMissingFile(t *testing.T) {
	deduper := newMessageDeduper(time.Minute, 100).WithPath(filepath.Join(t.TempDir(), "missing.json"))
	if err := deduper.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if allowed, err := deduper.Allow("bot-a", "om_1"); err != nil || !allowed {
		t.Fatalf("Allow(after missing load) = %v, %v; want true, nil", allowed, err)
	}
}

func TestMessageDeduperEvictsWhenOverCapacity(t *testing.T) {
	deduper := newMessageDeduper(time.Minute, 1)
	if allowed, err := deduper.Allow("bot-a", "om_1"); err != nil || !allowed {
		t.Fatalf("first bot-a message was rejected")
	}
	if allowed, err := deduper.Allow("bot-a", "om_2"); err != nil || !allowed {
		t.Fatalf("second bot-a message was rejected")
	}
	if allowed, err := deduper.Allow("bot-a", "om_2"); err != nil || allowed {
		t.Fatalf("newest duplicate = %v, %v; want false, nil", allowed, err)
	}
}

func attrsMap(attrs []slog.Attr) map[string]string {
	result := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		result[attr.Key] = attr.Value.String()
	}
	return result
}

func textEvent(messageID, chatID, text string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: ptr("ou_sender")},
			},
			Message: &larkim.EventMessage{
				MessageId:   ptr(messageID),
				ChatId:      ptr(chatID),
				ChatType:    ptr("p2p"),
				MessageType: ptr("text"),
				Content:     ptr(`{"text":"` + text + `"}`),
			},
		},
	}
}
