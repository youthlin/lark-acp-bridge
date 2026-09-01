package feishu

import (
	"strings"
	"testing"
	"time"

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
				CreateTime:  ptr("1700000000123"),
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
	if !msg.CreatedAt.Equal(time.UnixMilli(1700000000123)) {
		t.Fatalf("CreatedAt = %s, want parsed message create_time", msg.CreatedAt)
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

func TestParseMessageTrimsEventStringFields(t *testing.T) {
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{
					OpenId: ptr(" ou_sender "),
				},
				SenderType: ptr(" user "),
			},
			Message: &larkim.EventMessage{
				MessageId:   ptr(" om_1 "),
				ChatId:      ptr(" oc_1 "),
				ChatType:    ptr(" p2p "),
				ThreadId:    ptr(" omt_1 "),
				RootId:      ptr(" om_root "),
				ParentId:    ptr(" om_parent "),
				MessageType: ptr(" text "),
				Content:     ptr(`{"text":"你好 @_user_1"}`),
				Mentions: []*larkim.MentionEvent{
					{
						Key: ptr(" @_user_1 "),
						Id: &larkim.UserId{
							OpenId: ptr(" ou_bot "),
						},
						Name:          ptr(" 智能助手 "),
						MentionedType: ptr(" bot "),
					},
				},
			},
		},
	}

	msg, err := ParseMessage(event)
	if err != nil {
		t.Fatalf("ParseMessage() error = %v", err)
	}
	if msg.MessageID != "om_1" || msg.ChatID != "oc_1" || msg.ChatType != "p2p" || msg.MsgType != "text" {
		t.Fatalf("message ids/types = %+v, want trimmed fields", msg)
	}
	if msg.ThreadID != "omt_1" || msg.RootID != "om_root" || msg.ParentID != "om_parent" {
		t.Fatalf("thread/reply ids = %+v, want trimmed fields", msg)
	}
	if msg.SenderID != "ou_sender" || msg.SenderType != "user" {
		t.Fatalf("sender = %+v, want trimmed fields", msg)
	}
	if !msg.IsPrivateChat() {
		t.Fatalf("IsPrivateChat() = false, want true for trimmed p2p")
	}
	if msg.Text != "你好 @智能助手" {
		t.Fatalf("Text = %q, want mention replaced with trimmed name", msg.Text)
	}
	if len(msg.Mentions) != 1 || msg.Mentions[0].Key != "@_user_1" || msg.Mentions[0].ID != "ou_bot" || msg.Mentions[0].Name != "智能助手" || msg.Mentions[0].Type != "bot" {
		t.Fatalf("Mentions = %+v, want trimmed mention fields", msg.Mentions)
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
			name: "ordinary group thread id does not reply in topic",
			msg:  Message{ChatType: "group", GroupMessageType: "chat", MessageID: "om_group", ThreadID: "omt_thread"},
			want: false,
		},
		{
			name: "topic group replies in topic",
			msg:  Message{ChatType: "group", GroupMessageType: "thread", MessageID: "om_topic", ThreadID: "omt_topic"},
			want: true,
		},
		{
			name: "topic chat mode replies in topic without group message type",
			msg:  Message{ChatType: "group", ChatMode: "topic", MessageID: "om_topic", ThreadID: "omt_topic"},
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
			name: "unknown chat type thread id does not imply topic mode",
			msg:  Message{MessageID: "om_unknown_thread", ThreadID: "omt_thread"},
			want: false,
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

func TestParseMessagePostRichTextElements(t *testing.T) {
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{
				MessageId:   ptr("om_rich_post"),
				ChatId:      ptr("oc_1"),
				ChatType:    ptr("p2p"),
				MessageType: ptr("post"),
				Content: ptr(`{
  "zh_cn": {
    "title": "富文本标题",
    "content": [
      [
        {"tag":"text","text":"请看 "},
        {"tag":"a","text":"文档","href":"https://example.com/doc"},
        {"tag":"text","text":" 并询问 "},
        {"tag":"at","user_name":"张三","user_id":"ou_zhangsan"},
        {"tag":"emotion","emoji_type":"SMILE"}
      ],
      [
        {"tag":"code_block","language":"go","text":"fmt.Println(\"hi\")"}
      ],
      [
        {"tag":"img","image_key":"img_rich_post"}
      ]
    ]
  }
}`),
			},
		},
	}

	msg, err := ParseMessage(event)
	if err != nil {
		t.Fatalf("ParseMessage() error = %v", err)
	}
	wantText := "富文本标题\n请看 [文档](https://example.com/doc) 并询问 @张三[表情: SMILE]\n```go\nfmt.Println(\"hi\")\n```"
	if msg.Text != wantText {
		t.Fatalf("Text = %q, want %q", msg.Text, wantText)
	}
	if len(msg.Images) != 1 || msg.Images[0].ImageKey != "img_rich_post" {
		t.Fatalf("Images = %+v, want rich post image", msg.Images)
	}
	prompt := msg.PromptText()
	for _, want := range []string{wantText, "[图片消息]", "image_key: img_rich_post"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("PromptText() = %q, want %q", prompt, want)
		}
	}
}

func TestParseMessagePostPrefersContentV2Markdown(t *testing.T) {
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{
				MessageId:   ptr("om_md_post"),
				ChatId:      ptr("oc_1"),
				ChatType:    ptr("p2p"),
				MessageType: ptr("post"),
				Content: ptr(`{
  "zh_cn": {
    "title": "新版 Markdown",
    "content": [[{"tag":"text","text":"降级后的文本"}]],
    "content_v2": [[{"tag":"md","text":"## 原始 Markdown\n\n- [x] 保留任务列表\n\n![截图](img_v3_markdown)"}]]
  }
}`),
			},
		},
	}

	msg, err := ParseMessage(event)
	if err != nil {
		t.Fatalf("ParseMessage() error = %v", err)
	}
	wantText := "新版 Markdown\n## 原始 Markdown\n\n- [x] 保留任务列表\n\n![截图](img_v3_markdown)"
	if msg.Text != wantText {
		t.Fatalf("Text = %q, want content_v2 markdown %q", msg.Text, wantText)
	}
	if strings.Contains(msg.Text, "降级后的文本") {
		t.Fatalf("Text = %q, should not include downgraded content when content_v2 is readable", msg.Text)
	}
	if len(msg.Images) != 1 || msg.ImageKey != "img_v3_markdown" {
		t.Fatalf("Images = %+v ImageKey=%q, want markdown image key", msg.Images, msg.ImageKey)
	}
	if got := msg.PromptText(); !strings.Contains(got, "image_key: img_v3_markdown") {
		t.Fatalf("PromptText() = %q, want markdown image key", got)
	}
}

func TestParseMessagePostPrefersChineseLocale(t *testing.T) {
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{
				MessageId:   ptr("om_i18n_post"),
				ChatId:      ptr("oc_1"),
				ChatType:    ptr("p2p"),
				MessageType: ptr("post"),
				Content:     ptr(`{"en_us":{"title":"English","content":[[{"tag":"text","text":"hello"}]]},"zh_cn":{"title":"中文","content":[[{"tag":"text","text":"你好"}]]}}`),
			},
		},
	}

	msg, err := ParseMessage(event)
	if err != nil {
		t.Fatalf("ParseMessage() error = %v", err)
	}
	if msg.Text != "中文\n你好" {
		t.Fatalf("Text = %q, want Chinese locale text", msg.Text)
	}
}

func TestParseMessageInteractivePrefersUserDSL(t *testing.T) {
	event := &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{
				MessageId:   ptr("om_interactive"),
				ChatId:      ptr("oc_1"),
				ChatType:    ptr("group"),
				MessageType: ptr("interactive"),
				Content: ptr(`{
  "title": null,
  "elements": [
    [
      {"tag":"img","image_key":"img_v3_fallback"},
      {"tag":"text","text":"请升级至最新版本客户端，以查看内容"}
    ]
  ],
  "user_dsl": "{\"schema\":\"2.0\",\"header\":{\"title\":{\"tag\":\"plain_text\",\"content\":\"QA Review\"}},\"body\":{\"elements\":[{\"tag\":\"markdown\",\"content\":\"**Review 结论**\\n- 需要补测试\"},{\"tag\":\"img\",\"img_key\":\"img_v3_card\"},{\"tag\":\"button\",\"text\":{\"tag\":\"plain_text\",\"content\":\"查看详情\"}},{\"tag\":\"collapsible_panel\",\"header\":{\"title\":{\"tag\":\"plain_text\",\"content\":\"执行过程\"}},\"elements\":[{\"tag\":\"markdown\",\"content\":\"工具调用完成\"}]}]}}"
}`),
			},
		},
	}

	msg, err := ParseMessage(event)
	if err != nil {
		t.Fatalf("ParseMessage() error = %v", err)
	}
	for _, want := range []string{"QA Review", "Review 结论", "需要补测试", "查看详情", "执行过程", "工具调用完成"} {
		if !strings.Contains(msg.Text, want) {
			t.Fatalf("Text = %q, want %q", msg.Text, want)
		}
	}
	if strings.Contains(msg.Text, "请升级至最新版本客户端") {
		t.Fatalf("Text = %q, should prefer user_dsl over downgraded fallback text", msg.Text)
	}
	if len(msg.Images) != 2 || msg.Images[0].ImageKey != "img_v3_card" || msg.Images[1].ImageKey != "img_v3_fallback" || msg.ImageKey != "img_v3_card" {
		t.Fatalf("Images = %+v ImageKey=%q, want card image before fallback image", msg.Images, msg.ImageKey)
	}
}

func TestParseMessageInteractiveDefaultContent(t *testing.T) {
	got, err := parseMessageContent("interactive", `{
  "title": "卡片标题",
  "elements": [
    [
      {"tag":"button","text":"主按钮","type":"primary"},
      {"tag":"button","text":"次按钮","type":"default"}
    ],
    [
      {"tag":"a","href":"https://www.feishu.cn","text":"飞书"},
      {"tag":"text","text":"更高效、更愉悦。"},
      {"tag":"at","user_id":"@_user_1","user_name":"张三"}
    ],
    [
      {"tag":"note","elements":[
        {"tag":"img","image_key":"img_v3_note"},
        {"tag":"text","text":"备注信息"}
      ]}
    ]
  ]
}`)
	if err != nil {
		t.Fatalf("parseMessageContent() error = %v", err)
	}
	for _, want := range []string{"卡片标题", "主按钮", "次按钮", "[飞书](https://www.feishu.cn)", "更高效、更愉悦。", "@张三", "备注信息"} {
		if !strings.Contains(got, want) {
			t.Fatalf("parseMessageContent() = %q, want %q", got, want)
		}
	}
}

func TestParseMessageStructuredInboundTypes(t *testing.T) {
	tests := []struct {
		name    string
		msgType string
		content string
		want    []string
	}{
		{
			name:    "file",
			msgType: "file",
			content: `{"file_key":"file_v2_doc","file_name":"需求文档.txt"}`,
			want:    []string{"[文件消息]", "file_name: 需求文档.txt", "file_key: file_v2_doc"},
		},
		{
			name:    "location",
			msgType: "location",
			content: `{"name":"浙江省杭州市","longitude":"120.1","latitude":"30.2"}`,
			want:    []string{"[位置消息]", "name: 浙江省杭州市", "longitude: 120.1", "latitude: 30.2"},
		},
		{
			name:    "todo",
			msgType: "todo",
			content: `{"task_id":"task_1","summary":{"title":"","content":[[{"tag":"text","text":"多吃水果"}]]},"due_time":"1623124318000"}`,
			want:    []string{"[任务消息]", "summary: 多吃水果", "task_id: task_1", "due_time: 1623124318000"},
		},
		{
			name:    "vote",
			msgType: "vote",
			content: `{"topic":"午餐投票","options":["米饭","面条"]}`,
			want:    []string{"[投票消息]", "topic: 午餐投票", "options: 米饭, 面条"},
		},
		{
			name:    "system divider",
			msgType: "system",
			content: `{"template":"{divider_text}","divider_text":{"text":"新会话","i18n_text":{"zh_cn":"新话题","en_us":"New Session"}}}`,
			want:    []string{"[系统消息]", "新会话"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMessageContent(tt.msgType, tt.content)
			if err != nil {
				t.Fatalf("parseMessageContent() error = %v", err)
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("parseMessageContent() = %q, want %q", got, want)
				}
			}
		})
	}
}

func TestParseMessageImagesUsesStableStructuralOrder(t *testing.T) {
	images := parseMessageImages(`{
  "z_extra": {"image_key": "img_extra"},
  "content": [
    [{"tag": "img", "image_key": "img_content_1"}],
    [{"tag": "img", "image_key": "img_content_2"}]
  ],
  "a_extra": {"image_key": "img_alpha"},
  "content_v2": [[{"tag": "img", "image_key": "img_content_2"}]]
}`)
	got := make([]string, 0, len(images))
	for _, image := range images {
		got = append(got, image.ImageKey)
	}
	want := []string{"img_content_1", "img_content_2", "img_alpha", "img_extra"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("image keys = %#v, want %#v", got, want)
	}
}

func ptr(s string) *string {
	return &s
}
