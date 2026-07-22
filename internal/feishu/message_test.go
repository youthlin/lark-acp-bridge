package feishu

import (
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

func ptr(s string) *string {
	return &s
}
