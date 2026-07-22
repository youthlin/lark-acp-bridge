package feishu

import (
	"encoding/json"
	"fmt"
	"strings"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type Message struct {
	BotID     string
	Workspace string
	MessageID string
	ChatID    string
	ChatType  string
	ThreadID  string
	RootID    string
	ParentID  string
	SenderID  string
	Text      string
	Mentions  []Mention
}

type Mention struct {
	Key  string
	ID   string
	Name string
	Type string
}

func (m Message) IsPrivateChat() bool {
	return strings.EqualFold(m.ChatType, "p2p")
}

func ParseMessage(event *larkim.P2MessageReceiveV1) (Message, error) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return Message{}, fmt.Errorf("飞书消息事件为空")
	}
	raw := event.Event.Message
	msg := Message{
		MessageID: value(raw.MessageId),
		ChatID:    value(raw.ChatId),
		ChatType:  value(raw.ChatType),
		ThreadID:  value(raw.ThreadId),
		RootID:    value(raw.RootId),
		ParentID:  value(raw.ParentId),
	}
	if event.Event.Sender != nil && event.Event.Sender.SenderId != nil {
		msg.SenderID = value(event.Event.Sender.SenderId.OpenId)
	}
	text, err := parseTextContent(value(raw.Content))
	if err != nil {
		return Message{}, err
	}
	msg.Text = text
	for _, mention := range raw.Mentions {
		if mention == nil {
			continue
		}
		item := Mention{
			Key:  value(mention.Key),
			Name: value(mention.Name),
			Type: value(mention.MentionedType),
		}
		if mention.Id != nil {
			item.ID = value(mention.Id.OpenId)
		}
		msg.Mentions = append(msg.Mentions, item)
	}
	msg.Text = replaceMentionKeys(msg.Text, msg.Mentions)
	return msg, nil
}

func parseTextContent(content string) (string, error) {
	if strings.TrimSpace(content) == "" {
		return "", nil
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return "", fmt.Errorf("解析飞书文本消息: %w", err)
	}
	return payload.Text, nil
}

func replaceMentionKeys(text string, mentions []Mention) string {
	for _, mention := range mentions {
		if mention.Key == "" || mention.Name == "" {
			continue
		}
		text = strings.ReplaceAll(text, mention.Key, "@"+mention.Name)
	}
	return strings.TrimSpace(text)
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
