package feishu

import (
	"strings"
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestParseMessageTextWithMention(t *testing.T) {
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{
					OpenId: ptr("ou_sender"),
				},
			},
			Message: &larkim.EventMessage{
				MessageId:   ptr("om_1"),
				ChatId:      ptr("oc_1"),
				ChatType:    ptr("group"),
				ThreadId:    ptr("omt_1"),
				RootId:      ptr("om_root"),
				ParentId:    ptr("om_parent"),
				MessageType: ptr("text"),
				Content:     ptr(`{"text":"你好 @_user_1 继续测试"}`),
				Mentions: []*larkim.MentionEvent{
					{
						Key: ptr("@_user_1"),
						Id: &larkim.UserId{
							OpenId: ptr("ou_bot"),
						},
						Name:          ptr("我的智能助手"),
						MentionedType: ptr("bot"),
					},
				},
			},
		},
	}

	msg, err := ParseMessage(event)
	if err != nil {
		t.Fatalf("ParseMessage() error = %v", err)
	}
	if msg.MessageID != "om_1" || msg.ChatID != "oc_1" || msg.ThreadID != "omt_1" {
		t.Fatalf("unexpected ids: %+v", msg)
	}
	if msg.RootID != "om_root" || msg.ParentID != "om_parent" {
		t.Fatalf("unexpected reply ids: %+v", msg)
	}
	if msg.SenderID != "ou_sender" {
		t.Fatalf("SenderID = %q, want ou_sender", msg.SenderID)
	}
	if msg.Text != "你好 @我的智能助手 继续测试" {
		t.Fatalf("Text = %q", msg.Text)
	}
	if len(msg.Mentions) != 1 || msg.Mentions[0].Name != "我的智能助手" || msg.Mentions[0].ID != "ou_bot" {
		t.Fatalf("Mentions = %+v", msg.Mentions)
	}
}

func TestReplyInThreadForMessage(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
		want bool
	}{
		{
			name: "private chat quotes without topic mode",
			msg:  Message{ChatType: "p2p", MessageID: "om_private"},
			want: false,
		},
		{
			name: "ordinary group quotes without topic mode",
			msg:  Message{ChatType: "group", MessageID: "om_group"},
			want: false,
		},
		{
			name: "topic group replies in topic",
			msg:  Message{ChatType: "group", MessageID: "om_topic", ThreadID: "omt_topic"},
			want: true,
		},
		{
			name: "private chat root id does not force topic reply",
			msg:  Message{ChatType: "p2p", MessageID: "om_private_reply", RootID: "om_root"},
			want: false,
		},
		{
			name: "private chat thread id does not force topic reply",
			msg:  Message{ChatType: "p2p", MessageID: "om_private_thread", ThreadID: "omt_thread"},
			want: false,
		},
		{
			name: "unknown chat type thread id keeps legacy topic behavior",
			msg:  Message{MessageID: "om_unknown_thread", ThreadID: "omt_thread"},
			want: true,
		},
		{
			name: "unknown chat type root id does not force topic reply",
			msg:  Message{MessageID: "om_unknown_reply", RootID: "om_root"},
			want: false,
		},
		{
			name: "group root id without thread id does not imply topic mode",
			msg:  Message{ChatType: "group", MessageID: "om_topic_reply", RootID: "om_root"},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := replyInThreadForMessage(tt.msg); got != tt.want {
				t.Fatalf("replyInThreadForMessage() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseMessagePostWithImage(t *testing.T) {
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{
				MessageId:   ptr("om_post"),
				ChatId:      ptr("oc_1"),
				ChatType:    ptr("p2p"),
				MessageType: ptr("post"),
				Content:     ptr(`{"zh_cn":{"title":"带图消息","content":[[{"tag":"text","text":"看图"},{"tag":"img","image_key":"img_v3_post"}]]}}`),
			},
		},
	}

	msg, err := ParseMessage(event)
	if err != nil {
		t.Fatalf("ParseMessage() error = %v", err)
	}
	if msg.MsgType != "post" || !strings.Contains(msg.Text, "带图消息") || !strings.Contains(msg.Text, "看图") {
		t.Fatalf("message = %+v, want readable post text", msg)
	}
	if len(msg.Images) != 1 || msg.Images[0].ImageKey != "img_v3_post" || msg.ImageKey != "img_v3_post" {
		t.Fatalf("images = %+v imageKey=%q, want post image key", msg.Images, msg.ImageKey)
	}
	if got := msg.PromptText(); !strings.Contains(got, "image_key: img_v3_post") {
		t.Fatalf("PromptText() = %q, want image key", got)
	}
}

func TestParseMessagePostWithTopLevelContentAndImage(t *testing.T) {
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{
				MessageId:   ptr("om_test_nested_post"),
				ChatId:      ptr("oc_1"),
				ChatType:    ptr("p2p"),
				MessageType: ptr("post"),
				Content:     ptr(`{"title":"","content":[[{"tag":"text","text":"测试图文消息：","style":[]}],[{"tag":"img","image_key":"img_test_nested_post","width":1080,"height":1451}]],"content_v2":[[{"tag":"text","text":"测试图文消息：","style":[]}],[{"tag":"img","image_key":"img_test_nested_post","width":1080,"height":1451}]]}`),
			},
		},
	}

	msg, err := ParseMessage(event)
	if err != nil {
		t.Fatalf("ParseMessage() error = %v", err)
	}
	if msg.MsgType != "post" || msg.Text != "测试图文消息：" {
		t.Fatalf("message = %+v, want readable top-level post text", msg)
	}
	if len(msg.Images) != 1 || msg.ImageKey != "img_test_nested_post" {
		t.Fatalf("images = %+v imageKey=%q, want image key", msg.Images, msg.ImageKey)
	}
	prompt := msg.PromptText()
	for _, want := range []string{"测试图文消息：", "[图片消息]", "img_test_nested_post"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("PromptText() = %q, want %q", prompt, want)
		}
	}
}

func ptr(s string) *string {
	return &s
}
