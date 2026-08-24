package feishu

import "testing"

func TestBuildCreateChatReqBodyUsesTopicChatMode(t *testing.T) {
	body := buildCreateChatReqBody(CreateChatRequest{
		Name:        "专项群",
		Mode:        "topic",
		ChatType:    "private",
		OwnerOpenID: "ou_owner",
		UserOpenIDs: []string{"ou_owner"},
	}, "ou_owner", []string{"ou_owner"})

	if body == nil {
		t.Fatal("body is nil")
	}
	if body.ChatMode == nil || *body.ChatMode != "topic" {
		t.Fatalf("ChatMode = %v, want topic", body.ChatMode)
	}
	if body.GroupMessageType != nil {
		t.Fatalf("GroupMessageType = %v, want nil for topic chat", *body.GroupMessageType)
	}
	if body.Name == nil || *body.Name != "专项群" {
		t.Fatalf("Name = %v, want 专项群", body.Name)
	}
}

func TestBuildCreateChatReqBodyKeepsGroupMessageTypeForTraceChat(t *testing.T) {
	body := buildCreateChatReqBody(CreateChatRequest{
		Name:             "过程通知群",
		Mode:             "group",
		ChatType:         "private",
		GroupMessageType: "thread",
		OwnerOpenID:      "ou_owner",
		UserOpenIDs:      []string{"ou_owner"},
	}, "ou_owner", []string{"ou_owner"})

	if body == nil {
		t.Fatal("body is nil")
	}
	if body.ChatMode == nil || *body.ChatMode != "group" {
		t.Fatalf("ChatMode = %v, want group", body.ChatMode)
	}
	if body.GroupMessageType == nil || *body.GroupMessageType != "thread" {
		t.Fatalf("GroupMessageType = %v, want thread", body.GroupMessageType)
	}
}
