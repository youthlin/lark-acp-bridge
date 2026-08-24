package bridge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestHandleNewChatCreatesPrivateGroupForSender(t *testing.T) {
	svc := newTestService(config.Config{Bots: []config.BotConfig{{ID: "bot-a", OwnerOpenIDs: []string{testOwnerOpenID}}}}, NewSessionStore(""))
	client := newFakeSentMessageClient("")
	var gotReq feishu.CreateChatRequest
	client.chatCreator = func(_ context.Context, req feishu.CreateChatRequest) (feishu.CreatedChat, error) {
		gotReq = req
		return feishu.CreatedChat{ChatID: "oc_created", Name: "(无主题)", ChatType: "private", GroupMessageType: "chat"}, nil
	}
	svc.setOutbound("bot-a", client)

	got := svc.handleNewCommand(context.Background(), "/new chat", feishu.Message{
		BotID:     "bot-a",
		BotOpenID: testBotOpenID,
		SenderID:  testOwnerOpenID,
	})

	if !strings.Contains(got, "已创建群聊。") || !strings.Contains(got, "类型：普通群") || !strings.Contains(got, "额外成员：无") {
		t.Fatalf("reply = %q, want created ordinary chat without extra members", got)
	}
	wantUsers := []string{testOwnerOpenID}
	if gotReq.Name != "" || gotReq.Mode != newChatModeGroup || gotReq.ChatType != "private" || gotReq.OwnerOpenID != testOwnerOpenID ||
		!gotReq.SetBotManager || !reflect.DeepEqual(gotReq.UserOpenIDs, wantUsers) {
		t.Fatalf("CreateChat request = %+v, want sender-only private group", gotReq)
	}
}

func TestHandleNewChatCreatesTopicAndAddsMentionedUsers(t *testing.T) {
	svc := newTestService(config.Config{Bots: []config.BotConfig{{ID: "bot-a", OwnerOpenIDs: []string{testOwnerOpenID}}}}, NewSessionStore(""))
	client := newFakeSentMessageClient("")
	var createReq feishu.CreateChatRequest
	var addReq feishu.AddChatMembersRequest
	client.chatCreator = func(_ context.Context, req feishu.CreateChatRequest) (feishu.CreatedChat, error) {
		createReq = req
		return feishu.CreatedChat{ChatID: "oc_topic", Name: "专项群", ChatType: "private", GroupMessageType: "thread"}, nil
	}
	client.chatMemberAdder = func(_ context.Context, req feishu.AddChatMembersRequest) (feishu.AddChatMembersResult, error) {
		addReq = req
		return feishu.AddChatMembersResult{}, nil
	}
	svc.setOutbound("bot-a", client)

	got := svc.handleNewCommand(context.Background(), "/new chat topic 专项群 @Alice @Bob", feishu.Message{
		BotID:     "bot-a",
		BotOpenID: testBotOpenID,
		SenderID:  testOwnerOpenID,
		Mentions: []feishu.Mention{
			{Key: "@_user_1", ID: testBotOpenID, Name: "智能助手", Type: "bot"},
			{Key: "@_user_2", ID: "ou_alice", Name: "Alice", Type: "user"},
			{Key: "@_user_3", ID: "ou_bob", Name: "Bob", Type: "user"},
			{Key: "@_user_4", ID: "ou_alice", Name: "Alice", Type: "user"},
			{Key: "@_user_5", ID: testOwnerOpenID, Name: "Owner", Type: "user"},
		},
	})

	if !strings.Contains(got, "类型：话题群") || !strings.Contains(got, "群名：专项群") || !strings.Contains(got, "额外成员：成功 2/2") {
		t.Fatalf("reply = %q, want topic chat success summary", got)
	}
	if createReq.Name != "专项群" || createReq.Mode != newChatModeTopic || !reflect.DeepEqual(createReq.UserOpenIDs, []string{testOwnerOpenID}) {
		t.Fatalf("CreateChat request = %+v, want title, topic mode and sender-only initial members", createReq)
	}
	if addReq.ChatID != "oc_topic" || !reflect.DeepEqual(addReq.UserOpenIDs, []string{"ou_alice", "ou_bob"}) {
		t.Fatalf("AddChatMembers request = %+v, want deduped non-bot mentions", addReq)
	}
}

func TestHandleNewChatThroughFeishuMessageStripsNormalizedMentionIDsFromName(t *testing.T) {
	svc := newTestService(config.Config{Bots: []config.BotConfig{{ID: "bot-a", OwnerOpenIDs: []string{testOwnerOpenID}}}}, NewSessionStore(""))
	client := newFakeSentMessageClient("")
	var createReq feishu.CreateChatRequest
	var addReq feishu.AddChatMembersRequest
	client.chatCreator = func(_ context.Context, req feishu.CreateChatRequest) (feishu.CreatedChat, error) {
		createReq = req
		return feishu.CreatedChat{ChatID: "oc_created", Name: req.Name, ChatType: "private", ChatMode: "group"}, nil
	}
	client.chatMemberAdder = func(_ context.Context, req feishu.AddChatMembersRequest) (feishu.AddChatMembersResult, error) {
		addReq = req
		return feishu.AddChatMembersResult{}, nil
	}
	svc.setOutbound("bot-a", client)

	got, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_source",
		ChatType: "p2p",
		SenderID: testOwnerOpenID,
		Text:     "/new chat 项目 @Alice",
		Mentions: []feishu.Mention{
			{ID: "ou_alice", Name: "Alice", Type: "user"},
		},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new chat) error = %v", err)
	}

	if !strings.Contains(got, "群名：项目") || !strings.Contains(got, "额外成员：成功 1/1") {
		t.Fatalf("reply = %q, want cleaned title and added member summary", got)
	}
	if createReq.Name != "项目" {
		t.Fatalf("CreateChat name = %q, want 项目", createReq.Name)
	}
	if !reflect.DeepEqual(addReq.UserOpenIDs, []string{"ou_alice"}) {
		t.Fatalf("AddChatMembers users = %v, want [ou_alice]", addReq.UserOpenIDs)
	}
}

func TestHandleNewChatThroughFeishuMessageStripsBotMentionFromNameAndReportsSkipped(t *testing.T) {
	svc := newTestService(config.Config{Bots: []config.BotConfig{{ID: "bot-a", OwnerOpenIDs: []string{testOwnerOpenID}}}}, NewSessionStore(""))
	client := newFakeSentMessageClient("")
	var createReq feishu.CreateChatRequest
	client.chatCreator = func(_ context.Context, req feishu.CreateChatRequest) (feishu.CreatedChat, error) {
		createReq = req
		return feishu.CreatedChat{ChatID: "oc_topic", Name: req.Name, ChatType: "private", ChatMode: "topic"}, nil
	}
	client.chatMemberAdder = func(_ context.Context, req feishu.AddChatMembersRequest) (feishu.AddChatMembersResult, error) {
		t.Fatalf("AddChatMembers should not be called for bot mentions: %+v", req)
		return feishu.AddChatMembersResult{}, nil
	}
	svc.setOutbound("bot-a", client)

	got, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_source",
		ChatType: "p2p",
		SenderID: testOwnerOpenID,
		Text:     "/new chat topic @QA Claw",
		Mentions: []feishu.Mention{
			{ID: "ou_qa_claw", Name: "QA Claw", Type: "bot"},
		},
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/new chat topic) error = %v", err)
	}

	if createReq.Name != "" {
		t.Fatalf("CreateChat name = %q, want empty after stripping bot mention", createReq.Name)
	}
	if !strings.Contains(got, "类型：话题群") || strings.Contains(got, "群名：") ||
		!strings.Contains(got, "额外成员：无") ||
		!strings.Contains(got, "跳过非用户 mention：QA Claw(ou_qa_claw)") {
		t.Fatalf("reply = %q, want topic chat with skipped bot mention notice", got)
	}
}

func TestHandleNewChatReportsPartialAddFailures(t *testing.T) {
	svc := newTestService(config.Config{Bots: []config.BotConfig{{ID: "bot-a", OwnerOpenIDs: []string{testOwnerOpenID}}}}, NewSessionStore(""))
	client := newFakeSentMessageClient("")
	client.chatCreator = func(_ context.Context, req feishu.CreateChatRequest) (feishu.CreatedChat, error) {
		return feishu.CreatedChat{ChatID: "oc_created", ChatType: "private", GroupMessageType: "chat"}, nil
	}
	client.chatMemberAdder = func(_ context.Context, req feishu.AddChatMembersRequest) (feishu.AddChatMembersResult, error) {
		return feishu.AddChatMembersResult{
			InvalidOpenIDs:         []string{"ou_bad"},
			NotExistedOpenIDs:      []string{"ou_missing"},
			PendingApprovalOpenIDs: []string{"ou_pending", "ou_bad"},
		}, nil
	}
	svc.setOutbound("bot-a", client)

	got := svc.handleNewCommand(context.Background(), "/new chat @Bad @Missing @Pending", feishu.Message{
		BotID:     "bot-a",
		BotOpenID: testBotOpenID,
		SenderID:  testOwnerOpenID,
		Mentions: []feishu.Mention{
			{ID: "ou_bad", Name: "Bad", Type: "user"},
			{ID: "ou_missing", Name: "Missing", Type: "user"},
			{ID: "ou_pending", Name: "Pending", Type: "user"},
		},
	})

	if !strings.Contains(got, "已创建群聊。") || !strings.Contains(got, "额外成员：成功 0/3") ||
		!strings.Contains(got, "未拉入：ou_bad, ou_missing, ou_pending") {
		t.Fatalf("reply = %q, want partial failure summary", got)
	}
}

func TestHandleNewChatKeepsCreatedChatWhenAddMembersErrors(t *testing.T) {
	svc := newTestService(config.Config{Bots: []config.BotConfig{{ID: "bot-a", OwnerOpenIDs: []string{testOwnerOpenID}}}}, NewSessionStore(""))
	client := newFakeSentMessageClient("")
	client.chatCreator = func(_ context.Context, req feishu.CreateChatRequest) (feishu.CreatedChat, error) {
		return feishu.CreatedChat{ChatID: "oc_created", ChatType: "private"}, nil
	}
	client.chatMemberAdder = func(_ context.Context, req feishu.AddChatMembersRequest) (feishu.AddChatMembersResult, error) {
		return feishu.AddChatMembersResult{}, errors.New("forbidden")
	}
	svc.setOutbound("bot-a", client)

	got := svc.handleNewCommand(context.Background(), "/new chat @Alice", feishu.Message{
		BotID:     "bot-a",
		BotOpenID: testBotOpenID,
		SenderID:  testOwnerOpenID,
		Mentions: []feishu.Mention{
			{ID: "ou_alice", Name: "Alice", Type: "user"},
		},
	})

	if !strings.Contains(got, "群已创建，但拉人入群失败：forbidden") ||
		!strings.Contains(got, "chat_id：oc_created") ||
		!strings.Contains(got, "额外成员：未完成 0/1") ||
		!strings.Contains(got, "待确认：ou_alice") {
		t.Fatalf("reply = %q, want created chat with pending add summary", got)
	}
}
