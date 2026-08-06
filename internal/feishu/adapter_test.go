package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
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

type reactionStartingHandler struct{}

func (h reactionStartingHandler) HandleFeishuMessage(ctx context.Context, msg Message) (string, error) {
	return "", nil
}

func (h reactionStartingHandler) HandleFeishuMessageWithOutbound(ctx context.Context, msg Message, outbound Outbound) (string, error) {
	starter, _ := outbound.(interface {
		StartProcessingReaction(context.Context, Message) func()
	})
	if starter == nil {
		return "", nil
	}
	cleanup := starter.StartProcessingReaction(ctx, msg)
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
	uploadCalls   []string
	imageKey      string
	err           error
	downloadErr   error
	uploadErr     error
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
	return filepath.Join(workspace, ".local", "cache", imageKey+".png"), nil
}

func (f *fakeMessageClient) UploadImage(ctx context.Context, path string) (string, error) {
	f.uploadCalls = append(f.uploadCalls, path)
	if f.uploadErr != nil {
		return "", f.uploadErr
	}
	if f.imageKey != "" {
		return f.imageKey, nil
	}
	return "img_uploaded", nil
}

type fakeChatInfoClient struct {
	infos map[string]chatInfo
	calls []string
	err   error
}

func (f *fakeChatInfoClient) GetChatInfo(ctx context.Context, chatID string) (chatInfo, error) {
	f.calls = append(f.calls, chatID)
	if f.err != nil {
		return chatInfo{}, f.err
	}
	return f.infos[chatID], nil
}

type fakeApplicationClient struct {
	app                  applicationOwnerCandidates
	appErr               error
	collaborators        []applicationCollaborator
	collaboratorsErr     error
	botOpenID            string
	botName              string
	botOpenIDErr         error
	appCalls             int
	collaboratorGetCalls int
	botOpenIDCalls       int
}

func TestNewAdapterStoresDeduperUnderWorkspaceLocal(t *testing.T) {
	workspace := t.TempDir()

	adapter := NewAdapter(config.BotConfig{Workspace: workspace}, nil)

	if adapter.deduper == nil {
		t.Fatal("deduper is nil")
	}
	want := filepath.Join(workspace, ".local", "processed_messages.json")
	if adapter.deduper.path != want {
		t.Fatalf("deduper path = %q, want %q", adapter.deduper.path, want)
	}
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

func (f *fakeApplicationClient) GetBotInfo(ctx context.Context) (BotInfo, error) {
	f.botOpenIDCalls++
	if f.botOpenIDErr != nil {
		return BotInfo{}, f.botOpenIDErr
	}
	return BotInfo{OpenID: f.botOpenID, Name: f.botName}, nil
}

func TestParseBotInfoResponseUsesDisplayNameFallbacks(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want BotInfo
	}{
		{
			name: "name",
			raw:  `{"code":0,"bot":{"open_id":" ou_bot ","name":" 智能助手 "}}`,
			want: BotInfo{OpenID: "ou_bot", Name: "智能助手"},
		},
		{
			name: "display name",
			raw:  `{"code":0,"bot":{"open_id":"ou_bot","display_name":"展示名","bot_name":"机器人名","app_name":"应用名"}}`,
			want: BotInfo{OpenID: "ou_bot", Name: "展示名"},
		},
		{
			name: "bot name",
			raw:  `{"code":0,"bot":{"open_id":"ou_bot","bot_name":"机器人名","app_name":"应用名"}}`,
			want: BotInfo{OpenID: "ou_bot", Name: "机器人名"},
		},
		{
			name: "app name",
			raw:  `{"code":0,"bot":{"open_id":"ou_bot","app_name":"应用名"}}`,
			want: BotInfo{OpenID: "ou_bot", Name: "应用名"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBotInfoResponse([]byte(tt.raw))
			if err != nil {
				t.Fatalf("parseBotInfoResponse() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseBotInfoResponse() = %+v, want %+v", got, tt.want)
			}
		})
	}
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

func TestAdapterSkipsStaleMessage(t *testing.T) {
	handler := &countingHandler{}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	event := textEvent("om_stale", "oc_1", "hello")
	setEventCreateTime(event, time.Now().Add(-maxIncomingMessageAge-time.Second))

	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage() error = %v", err)
	}
	if handler.count != 0 {
		t.Fatalf("handler count = %d, want stale message skipped", handler.count)
	}
	if allowed, err := adapter.deduper.Allow("bot-a", "om_stale"); err != nil || !allowed {
		t.Fatalf("stale message should not be recorded in deduper, Allow = %v, %v", allowed, err)
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
	chatInfo := &fakeChatInfoClient{
		infos: map[string]chatInfo{
			"oc_1": {ChatMode: "group", GroupMessageType: "thread"},
		},
	}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	adapter.chatInfo = chatInfo
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
		"bot":                "bot-a",
		"chat_id":            "oc_1",
		"chat_type":          "group",
		"chat_mode":          "group",
		"group_message_type": "thread",
		"message_id":         "om_1",
		"thread_id":          "omt_1",
		"root_id":            "om_root",
		"parent_id":          "om_parent",
		"sender_id":          "ou_sender",
	} {
		if got := attrs[key]; got != want {
			t.Fatalf("ctx attr %s = %q, want %q; attrs=%v", key, got, want, attrs)
		}
	}
}

func TestAdapterHydratesChatInfoAndCachesByChatID(t *testing.T) {
	handler := &countingHandler{}
	chatInfo := &fakeChatInfoClient{
		infos: map[string]chatInfo{
			"oc_topic": {Name: "话题群", ChatMode: "group", ChatType: "private", GroupMessageType: "thread"},
		},
	}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	adapter.chatInfo = chatInfo

	first := textEvent("om_topic_1", "oc_topic", "hello")
	first.Event.Message.ChatType = ptr("group")
	first.Event.Message.ThreadId = ptr("omt_topic")
	if err := adapter.handleMessage(context.Background(), first); err != nil {
		t.Fatalf("handleMessage(first) error = %v", err)
	}
	if handler.msg.GroupMessageType != "thread" || !handler.msg.IsTopicThread() {
		t.Fatalf("message = %+v, want topic thread from chat info", handler.msg)
	}

	second := textEvent("om_topic_2", "oc_topic", "hello again")
	second.Event.Message.ChatType = ptr("group")
	second.Event.Message.ThreadId = ptr("omt_topic_2")
	if err := adapter.handleMessage(context.Background(), second); err != nil {
		t.Fatalf("handleMessage(second) error = %v", err)
	}
	if len(chatInfo.calls) != 1 || chatInfo.calls[0] != "oc_topic" {
		t.Fatalf("chat info calls = %+v, want one cached lookup", chatInfo.calls)
	}
	if handler.msg.GroupMessageType != "thread" || !handler.msg.IsTopicThread() {
		t.Fatalf("second message = %+v, want cached topic thread info", handler.msg)
	}
}

func TestAdapterOrdinaryGroupThreadIDStaysNonTopic(t *testing.T) {
	handler := &countingHandler{}
	chatInfo := &fakeChatInfoClient{
		infos: map[string]chatInfo{
			"oc_group": {Name: "普通群", ChatMode: "group", ChatType: "private", GroupMessageType: "chat"},
		},
	}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	adapter.chatInfo = chatInfo
	event := textEvent("om_group_reply", "oc_group", "hello")
	event.Event.Message.ChatType = ptr("group")
	event.Event.Message.ThreadId = ptr("omt_plain_group")

	if err := adapter.handleMessage(context.Background(), event); err != nil {
		t.Fatalf("handleMessage() error = %v", err)
	}
	if handler.msg.GroupMessageType != "chat" || handler.msg.IsTopicThread() {
		t.Fatalf("message = %+v, want ordinary group despite thread_id", handler.msg)
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
	wantPath := filepath.Join(workspace, ".local", "cache", "img_test_reply_image.png")
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

func TestAdapterExpandsMergedForwardMessage(t *testing.T) {
	handler := &countingHandler{}
	messages := &fakeMessageClient{
		messages: map[string]*Message{
			"om_forward": {
				MessageID: "om_forward",
				MsgType:   "merge_forward",
				Text: strings.Join([]string{
					"[合并转发消息]",
					"用户(ou_alice) [text]:\n第一条",
					"用户(ou_bob) [image]:\n[图片消息]\nimage_key: img_forwarded\nlocal_path: /workspace/.local/cache/img_forwarded.png",
				}, "\n\n"),
				Images: []MessageImage{
					{ImageKey: "img_forwarded", LocalPath: "/workspace/.local/cache/img_forwarded.png"},
				},
			},
		},
	}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a", Workspace: "/workspace"}, handler)
	adapter.messages = messages

	if err := adapter.handleMessage(context.Background(), mergeForwardEvent("om_forward", "oc_1")); err != nil {
		t.Fatalf("handleMessage() error = %v", err)
	}

	if len(messages.calls) != 1 || messages.calls[0] != "om_forward" {
		t.Fatalf("message get calls = %+v, want merge_forward root", messages.calls)
	}
	for _, want := range []string{
		"[合并转发消息]",
		"用户(ou_alice) [text]:",
		"第一条",
		"用户(ou_bob) [image]:",
		"local_path: /workspace/.local/cache/img_forwarded.png",
	} {
		if !strings.Contains(handler.msg.Text, want) {
			t.Fatalf("message text = %q, want %q", handler.msg.Text, want)
		}
	}
	if handler.msg.ImageKey != "img_forwarded" || handler.msg.LocalPath != "/workspace/.local/cache/img_forwarded.png" {
		t.Fatalf("message images = %+v image/local = %q/%q, want forwarded image as primary", handler.msg.Images, handler.msg.ImageKey, handler.msg.LocalPath)
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

func TestReplyContextFromLarkTextMessageReplacesMentionKeys(t *testing.T) {
	item := larkim.NewMessageBuilder().
		MessageId("om_parent").
		MsgType("text").
		Body(larkim.NewMessageBodyBuilder().
			Content(`{"text":"@_user_1 进入bridge项目"}`).
			Build()).
		Mentions([]*larkim.Mention{
			larkim.NewMentionBuilder().
				Key("@_user_1").
				Id("ou_real_user").
				IdType("open_id").
				Name("真实mention的名称").
				Build(),
		}).
		Build()

	msg := messageFromLarkMessage(item)
	if msg == nil {
		t.Fatal("message = nil, want text message")
	}
	if msg.Text != "@真实mention的名称 进入bridge项目" {
		t.Fatalf("message text = %q, want mention display name", msg.Text)
	}
	if len(msg.Mentions) != 1 || msg.Mentions[0].ID != "ou_real_user" || msg.Mentions[0].Name != "真实mention的名称" {
		t.Fatalf("mentions = %+v, want parsed mention metadata", msg.Mentions)
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

func TestReplyContextFromLarkInteractiveMessagePrefersUserDSL(t *testing.T) {
	item := larkim.NewMessageBuilder().
		MessageId("om_parent").
		MsgType("interactive").
		Sender(larkim.NewSenderBuilder().
			Id("cli_other_bot").
			SenderType("app").
			Build()).
		Body(larkim.NewMessageBodyBuilder().
			Content(`{"title":null,"elements":[[{"tag":"text","text":"请升级至最新版本客户端，以查看内容"}]],"user_dsl":"{\"schema\":\"2.0\",\"body\":{\"elements\":[{\"tag\":\"markdown\",\"content\":\"真实卡片正文\"},{\"tag\":\"button\",\"text\":{\"tag\":\"plain_text\",\"content\":\"查看详情\"}}]},\"header\":{\"title\":{\"tag\":\"plain_text\",\"content\":\"真实标题\"}}}"}`).
			Build()).
		Build()

	msg := messageFromLarkMessage(item)
	if msg == nil || msg.SenderType != "app" {
		t.Fatalf("message = %+v, want app interactive text", msg)
	}
	for _, want := range []string{"真实标题", "真实卡片正文", "查看详情"} {
		if !strings.Contains(msg.Text, want) {
			t.Fatalf("message text = %q, want %q", msg.Text, want)
		}
	}
	if strings.Contains(msg.Text, "请升级至最新版本客户端") {
		t.Fatalf("message text = %q, should prefer user_dsl over downgraded fallback text", msg.Text)
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
	wantPath := filepath.Join(workspace, ".local", "cache", "img_v3_reply.png")
	if got.LocalPath != wantPath || !strings.Contains(got.PromptText(), "local_path: "+wantPath) {
		t.Fatalf("message = %+v prompt=%q, want hydrated local path %q", got, got.PromptText(), wantPath)
	}
}

func TestParseOutboundMarkdownExtractsMarkdownImages(t *testing.T) {
	dir := t.TempDir()
	text := strings.Join([]string{
		"处理完成。",
		"![截图](result.png)",
		"local_path: /tmp/trace.webp",
		"```",
		"local_path: /tmp/ignored.png",
		"```",
		"结论保留。",
	}, "\n")

	blocks := parseOutboundMarkdown(text, outboundRenderContext{BaseDir: dir})
	if len(blocks) != 3 {
		t.Fatalf("blocks = %+v, want text/image/text", blocks)
	}
	if blocks[0].Kind != outboundBlockMarkdown || !strings.Contains(blocks[0].Text, "处理完成。") {
		t.Fatalf("first block = %+v, want markdown text", blocks[0])
	}
	if blocks[1].Kind != outboundBlockImage || blocks[1].Alt != "截图" || blocks[1].Path != filepath.Join(dir, "result.png") {
		t.Fatalf("image block = %+v, want relative image resolved from base dir", blocks[1])
	}
	if blocks[2].Kind != outboundBlockMarkdown || !strings.Contains(blocks[2].Text, "local_path: /tmp/trace.webp") || !strings.Contains(blocks[2].Text, "local_path: /tmp/ignored.png") || !strings.Contains(blocks[2].Text, "结论保留。") {
		t.Fatalf("last block = %+v, want local_path kept as plain markdown text", blocks[2])
	}
}

func TestParseOutboundMarkdownIgnoresRemoteMarkdownImages(t *testing.T) {
	text := "![remote](https://example.com/a.png)\n![local](/tmp/a.png)"

	blocks := parseOutboundMarkdown(text, outboundRenderContext{})
	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v, want remote markdown text plus local image", blocks)
	}
	if blocks[0].Kind != outboundBlockMarkdown || !strings.Contains(blocks[0].Text, "https://example.com/a.png") {
		t.Fatalf("first block = %+v, want remote image left as markdown text", blocks[0])
	}
	if blocks[1].Kind != outboundBlockImage || blocks[1].Path != "/tmp/a.png" {
		t.Fatalf("second block = %+v, want local image", blocks[1])
	}
}

func TestParseOutboundMarkdownPreservesInlineTextAroundImages(t *testing.T) {
	dir := t.TempDir()
	text := "before ![local](a.png) after"

	blocks := parseOutboundMarkdown(text, outboundRenderContext{BaseDir: dir})
	if len(blocks) != 3 {
		t.Fatalf("blocks = %+v, want text/image/text", blocks)
	}
	if blocks[0].Kind != outboundBlockMarkdown || blocks[0].Text != "before" {
		t.Fatalf("first block = %+v, want prefix text", blocks[0])
	}
	if blocks[1].Kind != outboundBlockImage || blocks[1].Path != filepath.Join(dir, "a.png") {
		t.Fatalf("image block = %+v, want relative image", blocks[1])
	}
	if blocks[2].Kind != outboundBlockMarkdown || blocks[2].Text != "after" {
		t.Fatalf("last block = %+v, want suffix text", blocks[2])
	}
}

func TestParseOutboundMarkdownKeepsMarkdownImagesInsideCodeFence(t *testing.T) {
	text := strings.Join([]string{
		"```markdown",
		"![example](/tmp/should-stay.png)",
		"```",
		"![local](/tmp/should-upload.png)",
	}, "\n")

	blocks := parseOutboundMarkdown(text, outboundRenderContext{})
	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v, want fenced text plus image", blocks)
	}
	if blocks[0].Kind != outboundBlockMarkdown || !strings.Contains(blocks[0].Text, "![example](/tmp/should-stay.png)") {
		t.Fatalf("first block = %+v, want fenced markdown image preserved", blocks[0])
	}
	if blocks[1].Kind != outboundBlockImage || blocks[1].Path != "/tmp/should-upload.png" {
		t.Fatalf("second block = %+v, want unfenced local image", blocks[1])
	}
}

func TestOutboundBlocksRenderPostAndCardImages(t *testing.T) {
	blocks := []outboundBlock{
		{Kind: outboundBlockMarkdown, Text: "说明"},
		{Kind: outboundBlockImage, Alt: "截图", ImageKey: "img_v3_uploaded"},
	}

	post, err := outboundBlocksPostContent(blocks)
	if err != nil {
		t.Fatalf("outboundBlocksPostContent() error = %v", err)
	}
	for _, want := range []string{`"tag":"md"`, `"text":"说明"`, `"tag":"img"`, `"image_key":"img_v3_uploaded"`} {
		if !strings.Contains(post, want) {
			t.Fatalf("post = %s, want %s", post, want)
		}
	}

	var card any
	data := newStreamCardJSONFromBlocks(blocks, "", "done", "", false, true, false, false, StreamCardMeta{})
	if err := json.Unmarshal([]byte(data), &card); err != nil {
		t.Fatalf("newStreamCardJSONFromBlocks() invalid JSON: %v", err)
	}
	for _, want := range []string{"说明", "img_v3_uploaded", "截图"} {
		if !jsonContainsValue(card, want) {
			t.Fatalf("card = %#v, want %q", card, want)
		}
	}
	if !jsonContainsKey(card, "img_key") {
		t.Fatalf("card = %#v, want card img_key field", card)
	}
}

func TestOutboundBlocksStreamCardPreservesTextImageOrder(t *testing.T) {
	blocks := []outboundBlock{
		{Kind: outboundBlockMarkdown, Text: "before txt"},
		{Kind: outboundBlockImage, ImageKey: "img_v3_uploaded"},
		{Kind: outboundBlockMarkdown, Text: "after txt"},
	}

	var card map[string]any
	data := newStreamCardJSONFromBlocks(blocks, "", "", "", false, false, false, false, StreamCardMeta{})
	if err := json.Unmarshal([]byte(data), &card); err != nil {
		t.Fatalf("newStreamCardJSONFromBlocks() invalid JSON: %v", err)
	}
	body, ok := card["body"].(map[string]any)
	if !ok {
		t.Fatalf("card body = %#v, want object", card["body"])
	}
	elements, ok := body["elements"].([]any)
	if !ok {
		t.Fatalf("body elements = %#v, want array", body["elements"])
	}
	if len(elements) != 3 {
		t.Fatalf("elements = %#v, want before/image/after", elements)
	}
	first, ok := elements[0].(map[string]any)
	if !ok || first["tag"] != "markdown" || first["content"] != "before txt" || first["element_id"] != streamCardTextElementID {
		t.Fatalf("first element = %#v, want anchored before text", elements[0])
	}
	second, ok := elements[1].(map[string]any)
	if !ok || second["tag"] != "img" || second["img_key"] != "img_v3_uploaded" {
		t.Fatalf("second element = %#v, want image", elements[1])
	}
	third, ok := elements[2].(map[string]any)
	if !ok || third["tag"] != "markdown" || third["content"] != "after txt" {
		t.Fatalf("third element = %#v, want after text", elements[2])
	}
}

func TestNormalizeReplyImagePathValidatesLocalImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.png")
	if err := os.WriteFile(path, []byte("png"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := normalizeReplyImagePath(path)
	if err != nil {
		t.Fatalf("normalizeReplyImagePath() error = %v", err)
	}
	if got != path {
		t.Fatalf("normalizeReplyImagePath() = %q, want %q", got, path)
	}
}

func TestNormalizeReplyImagePathRejectsUnsupportedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.txt")
	if err := os.WriteFile(path, []byte("text"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := normalizeReplyImagePath(path); err == nil || !strings.Contains(err.Error(), "不支持的图片类型") {
		t.Fatalf("normalizeReplyImagePath() error = %v, want unsupported image type", err)
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

func TestNewLoggerHonorsMinLevelWhenDefaultLoggerIsDebug(t *testing.T) {
	oldDefault := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(oldDefault)
	})

	var buf bytes.Buffer
	level := &slog.LevelVar{}
	level.Set(slog.LevelDebug)
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})))

	logger := NewLogger(slog.LevelInfo, "test", "lark-sdk")
	logger.Debug(context.Background(), "debug message")
	if strings.Contains(buf.String(), "debug message") {
		t.Fatalf("debug log was written despite min level info: %s", buf.String())
	}

	logger.Info(context.Background(), "info message")
	if !strings.Contains(buf.String(), "info message") {
		t.Fatalf("info log was not written: %s", buf.String())
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

func setEventCreateTime(event *larkim.P2MessageReceiveV1, t time.Time) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return
	}
	event.Event.Message.CreateTime = ptr(strconv.FormatInt(t.UnixMilli(), 10))
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

func mergeForwardEvent(messageID, chatID string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: ptr("ou_sender")},
			},
			Message: &larkim.EventMessage{
				MessageId:   ptr(messageID),
				ChatId:      ptr(chatID),
				ChatType:    ptr("p2p"),
				MessageType: ptr("merge_forward"),
				Content:     ptr(`{"content":"Merged and Forwarded Message"}`),
			},
		},
	}
}
