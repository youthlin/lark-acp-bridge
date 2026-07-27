package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

type countingHandler struct {
	count int
	ctx   context.Context
	msg   Message
}

func (h *countingHandler) HandleFeishuMessage(ctx context.Context, msg Message) (string, error) {
	h.count++
	h.ctx = ctx
	h.msg = msg
	return "", nil
}

type replyingHandler struct {
	reply string
}

func (h replyingHandler) HandleFeishuMessage(ctx context.Context, msg Message) (string, error) {
	return h.reply, nil
}

type reactionStartingHandler struct{}

func (h reactionStartingHandler) HandleFeishuMessage(ctx context.Context, msg Message) (string, error) {
	cleanup, _ := StartProcessingReaction(ctx, msg)
	if cleanup != nil {
		defer cleanup()
	}
	return "", nil
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

type fakeMessageClient struct {
	messages      map[string]*Message
	calls         []string
	downloadCalls []fakeImageDownloadCall
	err           error
	downloadErr   error
}

type fakeImageDownloadCall struct {
	MessageID string
	ImageKey  string
	Workspace string
}

func (f *fakeMessageClient) GetMessage(ctx context.Context, messageID string, workspace string) (*Message, error) {
	f.calls = append(f.calls, messageID)
	if f.err != nil {
		return nil, f.err
	}
	return f.messages[messageID], nil
}

func (f *fakeMessageClient) DownloadImage(ctx context.Context, messageID string, imageKey string, workspace string) (string, error) {
	f.downloadCalls = append(f.downloadCalls, fakeImageDownloadCall{MessageID: messageID, ImageKey: imageKey, Workspace: workspace})
	if f.downloadErr != nil {
		return "", f.downloadErr
	}
	return filepath.Join(workspace, "cache", imageKey+".png"), nil
}

type fakeApplicationClient struct {
	app                  applicationOwnerCandidates
	appErr               error
	collaborators        []applicationCollaborator
	collaboratorsErr     error
	botOpenID            string
	botOpenIDErr         error
	appCalls             int
	collaboratorGetCalls int
	botOpenIDCalls       int
}

func (f *fakeApplicationClient) GetApplication(ctx context.Context) (applicationOwnerCandidates, error) {
	f.appCalls++
	if f.appErr != nil {
		return applicationOwnerCandidates{}, f.appErr
	}
	return f.app, nil
}

func (f *fakeApplicationClient) GetCollaborators(ctx context.Context) ([]applicationCollaborator, error) {
	f.collaboratorGetCalls++
	if f.collaboratorsErr != nil {
		return nil, f.collaboratorsErr
	}
	return f.collaborators, nil
}

func (f *fakeApplicationClient) GetBotOpenID(ctx context.Context) (string, error) {
	f.botOpenIDCalls++
	if f.botOpenIDErr != nil {
		return "", f.botOpenIDErr
	}
	return f.botOpenID, nil
}

func TestResolveBotOpenIDSkipsConfiguredValue(t *testing.T) {
	applications := &fakeApplicationClient{botOpenID: "ou_from_api"}
	adapter := NewAdapter(config.BotConfig{
		ID:        "bot-a",
		BotOpenID: " ou_configured ",
	}, nil)
	adapter.applications = applications

	adapter.resolveBotOpenID(context.Background())

	if applications.botOpenIDCalls != 0 {
		t.Fatalf("bot open_id calls = %d, want no lookup when configured", applications.botOpenIDCalls)
	}
	if got, want := adapter.cfg.BotOpenID, "ou_configured"; got != want {
		t.Fatalf("BotOpenID = %q, want %q", got, want)
	}
}

func TestResolveBotOpenIDUsesApplicationAPI(t *testing.T) {
	applications := &fakeApplicationClient{botOpenID: " ou_bot "}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, nil)
	adapter.applications = applications

	adapter.resolveBotOpenID(context.Background())

	if applications.botOpenIDCalls != 1 {
		t.Fatalf("bot open_id calls = %d, want 1", applications.botOpenIDCalls)
	}
	if got, want := adapter.cfg.BotOpenID, "ou_bot"; got != want {
		t.Fatalf("BotOpenID = %q, want %q", got, want)
	}
}

func TestResolveOwnerOpenIDsSkipsConfiguredOwners(t *testing.T) {
	applications := &fakeApplicationClient{
		app: applicationOwnerCandidates{OwnerID: "ou_from_api"},
	}
	adapter := NewAdapter(config.BotConfig{
		ID:           "bot-a",
		OwnerOpenIDs: []string{"ou_configured"},
	}, nil)
	adapter.applications = applications

	adapter.resolveOwnerOpenIDs(context.Background())

	if applications.appCalls != 0 || applications.collaboratorGetCalls != 0 {
		t.Fatalf("application calls = app:%d collaborators:%d, want no lookup when configured", applications.appCalls, applications.collaboratorGetCalls)
	}
	if got, want := adapter.cfg.OwnerOpenIDs, []string{"ou_configured"}; !slices.Equal(got, want) {
		t.Fatalf("OwnerOpenIDs = %#v, want %#v", got, want)
	}
}

func TestFetchOwnerOpenIDsCombinesApplicationOwnersAndCollaborators(t *testing.T) {
	adapter := &Adapter{
		cfg: config.BotConfig{ID: "bot-a"},
		applications: &fakeApplicationClient{
			app: applicationOwnerCandidates{
				OwnerID:   " ou_owner ",
				CreatorID: "ou_creator",
			},
			collaborators: []applicationCollaborator{
				{Type: "administrator", UserID: "ou_admin"},
				{Type: "developer", UserID: "ou_dev"},
				{Type: "Developer", UserID: "ou_dev_upper_duplicate"},
				{Type: "operator", UserID: "ou_operator"},
				{Type: "owner", UserID: "ou_owner"},
				{Type: "administrator", UserID: " "},
			},
		},
	}

	got, err := adapter.fetchOwnerOpenIDs(context.Background())
	if err != nil {
		t.Fatalf("fetchOwnerOpenIDs() error = %v", err)
	}
	want := []string{"ou_owner", "ou_creator", "ou_admin", "ou_dev", "ou_dev_upper_duplicate"}
	if !slices.Equal(got, want) {
		t.Fatalf("fetchOwnerOpenIDs() = %#v, want %#v", got, want)
	}
}

func TestFetchOwnerOpenIDsUsesCollaboratorsWhenApplicationLookupFails(t *testing.T) {
	adapter := &Adapter{
		cfg: config.BotConfig{ID: "bot-a"},
		applications: &fakeApplicationClient{
			appErr: errors.New("app info denied"),
			collaborators: []applicationCollaborator{
				{Type: "administrator", UserID: "ou_admin"},
			},
		},
	}

	got, err := adapter.fetchOwnerOpenIDs(context.Background())
	if err != nil {
		t.Fatalf("fetchOwnerOpenIDs() error = %v", err)
	}
	want := []string{"ou_admin"}
	if !slices.Equal(got, want) {
		t.Fatalf("fetchOwnerOpenIDs() = %#v, want %#v", got, want)
	}
}

func TestResolveOwnerOpenIDsLeavesEmptyWhenLookupFails(t *testing.T) {
	applications := &fakeApplicationClient{
		appErr:           errors.New("app info denied"),
		collaboratorsErr: errors.New("collaborators denied"),
	}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, nil)
	adapter.applications = applications

	adapter.resolveOwnerOpenIDs(context.Background())

	if applications.appCalls != 1 || applications.collaboratorGetCalls != 1 {
		t.Fatalf("application calls = app:%d collaborators:%d, want both lookups attempted", applications.appCalls, applications.collaboratorGetCalls)
	}
	if len(adapter.cfg.OwnerOpenIDs) != 0 {
		t.Fatalf("OwnerOpenIDs = %#v, want empty after failed lookup", adapter.cfg.OwnerOpenIDs)
	}
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

func TestAdapterSkipsMessageWithoutMessageID(t *testing.T) {
	handler := &countingHandler{}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	event := textEvent("", "oc_1", "hello")

	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage() error = %v", err)
	}
	if handler.count != 0 {
		t.Fatalf("handler count = %d, want message without id skipped", handler.count)
	}
}

func TestAdapterAddsAndDeletesProcessingReaction(t *testing.T) {
	reactions := &fakeReactionClient{}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, reactionStartingHandler{})
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
	handler := reactionStartingHandler{}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	adapter.reaction = reactions
	event := textEvent("om_dup", "oc_1", "hello")

	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage(first) error = %v", err)
	}
	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage(second) error = %v", err)
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

func TestAdapterHydratesReplyContextFromParentMessage(t *testing.T) {
	handler := &countingHandler{}
	messages := &fakeMessageClient{
		messages: map[string]*Message{
			"om_parent": {
				MessageID:  "om_parent",
				SenderID:   "ou_other",
				SenderType: "user",
				MsgType:    "text",
				Text:       "我先发一条消息",
			},
		},
	}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	adapter.messages = messages
	event := textEvent("om_reply", "oc_1", "继续处理")
	event.Event.Message.ChatType = ptr("group")
	event.Event.Message.ParentId = ptr("om_parent")
	event.Event.Message.RootId = ptr("om_root")

	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage() error = %v", err)
	}
	if len(messages.calls) != 1 || messages.calls[0] != "om_parent" {
		t.Fatalf("message get calls = %+v, want parent id", messages.calls)
	}
	if handler.msg.Reply == nil || handler.msg.Reply.Text != "我先发一条消息" {
		t.Fatalf("reply context = %+v, want hydrated parent text", handler.msg.Reply)
	}
}

func TestAdapterHydratesCurrentImageMessage(t *testing.T) {
	handler := &countingHandler{}
	messages := &fakeMessageClient{}
	workspace := t.TempDir()
	adapter := NewAdapter(config.BotConfig{ID: "bot-a", Workspace: workspace}, handler)
	adapter.messages = messages
	event := imageEvent("om_image", "oc_1", "img_test_reply_image")

	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage() error = %v", err)
	}
	if handler.msg.MsgType != "image" || handler.msg.ImageKey != "img_test_reply_image" {
		t.Fatalf("message = %+v, want image type and key", handler.msg)
	}
	wantPath := filepath.Join(workspace, "cache", "img_test_reply_image.png")
	if handler.msg.LocalPath != wantPath {
		t.Fatalf("LocalPath = %q, want %q", handler.msg.LocalPath, wantPath)
	}
	if got := handler.msg.PromptText(); !strings.Contains(got, "local_path: "+wantPath) {
		t.Fatalf("PromptText() = %q, want local path", got)
	}
	if len(messages.downloadCalls) != 1 || messages.downloadCalls[0] != (fakeImageDownloadCall{
		MessageID: "om_image",
		ImageKey:  "img_test_reply_image",
		Workspace: workspace,
	}) {
		t.Fatalf("download calls = %+v, want current image download", messages.downloadCalls)
	}
}

func TestAdapterHydratesAppReplyContext(t *testing.T) {
	handler := &countingHandler{}
	messages := &fakeMessageClient{
		messages: map[string]*Message{
			"om_parent": {
				MessageID:  "om_parent",
				SenderID:   "cli_bot",
				SenderType: "app",
				MsgType:    "text",
				Text:       "bot output",
			},
		},
	}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	adapter.messages = messages
	event := textEvent("om_reply", "oc_1", "继续处理")
	event.Event.Message.ChatType = ptr("group")
	event.Event.Message.ParentId = ptr("om_parent")

	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage() error = %v", err)
	}
	if handler.msg.Reply == nil || handler.msg.Reply.Text != "bot output" || handler.msg.Reply.SenderType != "app" {
		t.Fatalf("reply context = %+v, want hydrated app sender text", handler.msg.Reply)
	}
}

func TestAdapterHydratesBotReplyContext(t *testing.T) {
	handler := &countingHandler{}
	messages := &fakeMessageClient{
		messages: map[string]*Message{
			"om_parent": {
				MessageID:  "om_parent",
				SenderID:   "ou_bot",
				SenderType: "bot",
				MsgType:    "text",
				Text:       "bot output",
			},
		},
	}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	adapter.messages = messages
	event := textEvent("om_reply", "oc_1", "继续处理")
	event.Event.Message.ChatType = ptr("group")
	event.Event.Message.ParentId = ptr("om_parent")

	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage() error = %v", err)
	}
	if handler.msg.Reply == nil || handler.msg.Reply.Text != "bot output" || handler.msg.Reply.SenderType != "bot" {
		t.Fatalf("reply context = %+v, want hydrated bot sender text", handler.msg.Reply)
	}
}

func TestReplyContextFromLarkTextMessage(t *testing.T) {
	item := larkim.NewMessageBuilder().
		MessageId("om_parent").
		MsgType("text").
		Sender(larkim.NewSenderBuilder().
			Id("ou_user").
			SenderType("user").
			Build()).
		Body(larkim.NewMessageBodyBuilder().
			Content(`{"text":"hello reply"}`).
			Build()).
		Build()

	msg := messageFromLarkMessage(item)
	if msg == nil || msg.MessageID != "om_parent" || msg.SenderID != "ou_user" || msg.SenderType != "user" || msg.Text != "hello reply" {
		t.Fatalf("message = %+v", msg)
	}
}

func TestReplyContextFromLarkPostMessage(t *testing.T) {
	item := larkim.NewMessageBuilder().
		MessageId("om_parent").
		MsgType("post").
		Sender(larkim.NewSenderBuilder().
			Id("ou_user").
			SenderType("user").
			Build()).
		Body(larkim.NewMessageBodyBuilder().
			Content(`{"zh_cn":{"title":"复盘结论","content":[[{"tag":"text","text":"第一段"},{"tag":"a","text":"链接文字"}],[{"tag":"text","text":"第二段"}]]}}`).
			Build()).
		Build()

	msg := messageFromLarkMessage(item)
	if msg == nil {
		t.Fatal("message = nil, want post text")
	}
	for _, want := range []string{"复盘结论", "第一段", "链接文字", "第二段"} {
		if !strings.Contains(msg.Text, want) {
			t.Fatalf("message text = %q, want %q", msg.Text, want)
		}
	}
}

func TestReplyContextFromLarkPostMessageWithImage(t *testing.T) {
	item := larkim.NewMessageBuilder().
		MessageId("om_parent").
		MsgType("post").
		Body(larkim.NewMessageBodyBuilder().
			Content(`{"zh_cn":{"title":"带图消息","content":[[{"tag":"text","text":"看图"},{"tag":"img","image_key":"img_v3_post"}]]}}`).
			Build()).
		Build()

	msg := messageFromLarkMessage(item)
	if msg == nil {
		t.Fatal("message = nil, want post image context")
	}
	if len(msg.Images) != 1 || msg.Images[0].ImageKey != "img_v3_post" || msg.ImageKey != "img_v3_post" {
		t.Fatalf("message images = %+v imageKey=%q, want post image", msg.Images, msg.ImageKey)
	}
	if !strings.Contains(msg.PromptText(), "image_key: img_v3_post") {
		t.Fatalf("PromptText() = %q, want image key", msg.PromptText())
	}
}

func TestReplyContextFromLarkInteractiveMessage(t *testing.T) {
	item := larkim.NewMessageBuilder().
		MessageId("om_parent").
		MsgType("interactive").
		Sender(larkim.NewSenderBuilder().
			Id("cli_other_bot").
			SenderType("app").
			Build()).
		Body(larkim.NewMessageBodyBuilder().
			Content(`{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"**Review 结论**\n- 需要补测试"},{"tag":"button","text":{"tag":"plain_text","content":"查看详情"}}]},"header":{"title":{"tag":"plain_text","content":"QA Review"}}}`).
			Build()).
		Build()

	msg := messageFromLarkMessage(item)
	if msg == nil || msg.SenderType != "app" {
		t.Fatalf("message = %+v, want app interactive text", msg)
	}
	for _, want := range []string{"QA Review", "Review 结论", "需要补测试", "查看详情"} {
		if !strings.Contains(msg.Text, want) {
			t.Fatalf("message text = %q, want %q", msg.Text, want)
		}
	}
}

func TestReplyContextFromLarkImageMessage(t *testing.T) {
	item := larkim.NewMessageBuilder().
		MessageId("om_parent").
		MsgType("image").
		Sender(larkim.NewSenderBuilder().
			Id("ou_user").
			SenderType("user").
			Build()).
		Body(larkim.NewMessageBodyBuilder().
			Content(`{"image_key":"img_test_reply_image"}`).
			Build()).
		Build()

	msg := messageFromLarkMessage(item)
	if msg == nil {
		t.Fatal("message = nil, want image context")
	}
	if msg.MsgType != "image" || msg.ImageKey != "img_test_reply_image" {
		t.Fatalf("message = %+v, want image key", msg)
	}
	for _, want := range []string{"[图片消息]", "img_test_reply_image"} {
		if !strings.Contains(msg.PromptText(), want) {
			t.Fatalf("message prompt text = %q, want %q", msg.PromptText(), want)
		}
	}
}

func TestReplyContextFromMessage(t *testing.T) {
	msg := &Message{
		MessageID:  "om_parent",
		SenderID:   "ou_user",
		SenderType: "user",
		MsgType:    "text",
		Text:       "hello",
		Images:     []MessageImage{{ImageKey: "img_v3_reply", LocalPath: "/tmp/img.png"}},
	}
	reply := replyContextFromMessage(msg)
	if reply == nil || reply.MessageID != msg.MessageID || reply.SenderID != msg.SenderID || reply.SenderType != msg.SenderType || reply.Text != msg.Text {
		t.Fatalf("reply context = %+v, want copied message fields", reply)
	}
	if len(reply.Images) != 1 || reply.Images[0].LocalPath != "/tmp/img.png" {
		t.Fatalf("reply images = %+v, want copied images", reply.Images)
	}
	msg.Images[0].LocalPath = "/tmp/changed.png"
	if reply.Images[0].LocalPath != "/tmp/img.png" {
		t.Fatalf("reply images share backing array with message: %+v", reply.Images)
	}
}

func TestLarkMessageClientHydratesMessageImageLocalPath(t *testing.T) {
	msg := &Message{
		MessageID: "om_parent",
		MsgType:   "image",
		Images:    []MessageImage{{ImageKey: "img_v3_reply"}},
	}
	setMessagePrimaryImage(msg)
	messages := &fakeMessageClient{
		messages: map[string]*Message{"om_parent": msg},
	}
	workspace := t.TempDir()
	got, err := messages.GetMessage(context.Background(), "om_parent", workspace)
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	// fakeMessageClient intentionally does not hydrate by itself; hydrateMessageImages
	// is the shared helper used by the real lark client and event path.
	got.Images = hydrateMessageImages(context.Background(), messages, got.MessageID, workspace, got.Images)
	setMessagePrimaryImage(got)
	wantPath := filepath.Join(workspace, "cache", "img_v3_reply.png")
	if got.LocalPath != wantPath || !strings.Contains(got.PromptText(), "local_path: "+wantPath) {
		t.Fatalf("message = %+v prompt=%q, want hydrated local path %q", got, got.PromptText(), wantPath)
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

func TestMessageDeduperPersistsMessagesInStableOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "processed_messages.json")
	deduper := newMessageDeduper(time.Minute, 100).WithPath(path)
	for _, item := range []struct {
		botID     string
		messageID string
	}{
		{botID: "bot-b", messageID: "om_2"},
		{botID: "bot-a", messageID: "om_2"},
		{botID: "bot-a", messageID: "om_1"},
	} {
		if allowed, err := deduper.Allow(item.botID, item.messageID); err != nil || !allowed {
			t.Fatalf("Allow(%s, %s) = %v, %v; want true, nil", item.botID, item.messageID, allowed, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(processed messages) error = %v", err)
	}
	var file processedMessageFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("Unmarshal(processed messages) error = %v", err)
	}
	got := make([]string, 0, len(file.Messages))
	for _, item := range file.Messages {
		got = append(got, item.BotID+"/"+item.MessageID)
	}
	want := []string{"bot-a/om_1", "bot-a/om_2", "bot-b/om_2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("message order = %+v, want %+v", got, want)
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

func TestMessageDeduperLoadMissingFileClearsState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "processed_messages.json")
	deduper := newMessageDeduper(time.Minute, 100).WithPath(path)
	if allowed, err := deduper.Allow("bot-a", "om_1"); err != nil || !allowed {
		t.Fatalf("Allow(first) = %v, %v; want true, nil", allowed, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove(processed messages) error = %v", err)
	}
	if err := deduper.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if allowed, err := deduper.Allow("bot-a", "om_1"); err != nil || !allowed {
		t.Fatalf("Allow(after missing reload) = %v, %v; want true, nil", allowed, err)
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

func imageEvent(messageID, chatID, imageKey string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: ptr("ou_sender")},
			},
			Message: &larkim.EventMessage{
				MessageId:   ptr(messageID),
				ChatId:      ptr(chatID),
				ChatType:    ptr("p2p"),
				MessageType: ptr("image"),
				Content:     ptr(`{"image_key":"` + imageKey + `"}`),
			},
		},
	}
}
