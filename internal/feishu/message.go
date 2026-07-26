package feishu

import (
	"encoding/json"
	"fmt"
	"strings"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type Message struct {
	BotID      string
	BotOpenID  string
	Workspace  string
	MessageID  string
	ChatID     string
	ChatType   string
	ThreadID   string
	RootID     string
	ParentID   string
	SenderID   string
	SenderType string
	MsgType    string
	Text       string
	ImageKey   string
	LocalPath  string
	Images     []MessageImage
	Mentions   []Mention
	Reply      *ReplyContext
	// ForceReplyInThread forces replies to use Feishu topic/thread mode even
	// when the source message is not itself a topic-thread event.
	ForceReplyInThread bool
}

type SentMessage struct {
	MessageID string
	ChatID    string
	ChatType  string
	ThreadID  string
	RootID    string
	ParentID  string
}

type Mention struct {
	Key  string
	ID   string
	Name string
	Type string
}

type ReplyContext struct {
	MessageID  string
	SenderID   string
	SenderType string
	MsgType    string
	Text       string
	ImageKey   string
	LocalPath  string
	Images     []MessageImage
}

type MessageImage struct {
	ImageKey  string
	LocalPath string
}

func (m Message) IsPrivateChat() bool {
	return strings.EqualFold(m.ChatType, "p2p")
}

func (m Message) IsTopicThread() bool {
	return !m.IsPrivateChat() && strings.TrimSpace(m.ThreadID) != ""
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
		MsgType:   value(raw.MessageType),
	}
	if event.Event.Sender != nil && event.Event.Sender.SenderId != nil {
		msg.SenderID = value(event.Event.Sender.SenderId.OpenId)
	}
	if event.Event.Sender != nil {
		msg.SenderType = value(event.Event.Sender.SenderType)
	}
	content := value(raw.Content)
	msg.Images = parseMessageImages(content)
	if len(msg.Images) > 0 {
		msg.ImageKey = msg.Images[0].ImageKey
	}
	if strings.EqualFold(msg.MsgType, "image") {
		msg.Text = ""
	} else if strings.EqualFold(msg.MsgType, "text") {
		text, err := parseTextContent(content)
		if err != nil {
			return Message{}, err
		}
		msg.Text = text
	} else {
		msg.Text = parseMessageTextContent(content)
	}
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

func (m Message) PromptText() string {
	return messageTextWithImages(m.Text, m.Images)
}

func (r ReplyContext) PromptText() string {
	return messageTextWithImages(r.Text, r.Images)
}

// replyContextFromMessage 将一条消息转换成被引用的上下文
func replyContextFromMessage(msg *Message) *ReplyContext {
	if msg == nil {
		return nil
	}
	reply := &ReplyContext{
		MessageID:  msg.MessageID,
		SenderID:   msg.SenderID,
		SenderType: msg.SenderType,
		MsgType:    msg.MsgType,
		Text:       msg.Text,
		ImageKey:   msg.ImageKey,
		LocalPath:  msg.LocalPath,
		Images:     append([]MessageImage(nil), msg.Images...),
	}
	if reply.MessageID == "" && reply.SenderID == "" && reply.SenderType == "" && reply.MsgType == "" && reply.Text == "" && reply.ImageKey == "" && len(reply.Images) == 0 {
		return nil
	}
	return reply
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

func parseMessageTextContent(content string) string {
	text, err := parseTextContent(content)
	if err != nil || strings.TrimSpace(text) == "" {
		return extractReadableMessageText(content)
	}
	return strings.TrimSpace(text)
}

func messageTextWithImages(text string, images []MessageImage) string {
	parts := make([]string, 0, len(images)+1)
	if text = strings.TrimSpace(text); text != "" {
		parts = append(parts, text)
	}
	for _, image := range images {
		if imageText := imageMessageText(image.ImageKey, image.LocalPath); imageText != "" {
			parts = append(parts, imageText)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func imageMessageText(imageKey string, localPath string) string {
	imageKey = strings.TrimSpace(imageKey)
	localPath = strings.TrimSpace(localPath)
	if imageKey == "" && localPath == "" {
		return "[图片消息]"
	}
	lines := []string{"[图片消息]"}
	if imageKey != "" {
		lines = append(lines, "image_key: "+imageKey)
	}
	if localPath != "" {
		lines = append(lines, "local_path: "+localPath)
	}
	return strings.Join(lines, "\n")
}

func parseMessageImages(content string) []MessageImage {
	var value any
	if err := json.Unmarshal([]byte(content), &value); err != nil {
		return nil
	}
	keys := collectImageKeys(value, nil)
	images := make([]MessageImage, 0, len(keys))
	for _, key := range keys {
		images = append(images, MessageImage{ImageKey: key})
	}
	return images
}

func collectImageKeys(value any, keys []string) []string {
	switch v := value.(type) {
	case map[string]any:
		for _, keyName := range []string{"image_key", "imageKey"} {
			if key, ok := v[keyName].(string); ok {
				keys = appendImageKey(keys, key)
			}
		}
		for _, child := range v {
			if _, isString := child.(string); isString {
				continue
			}
			keys = collectImageKeys(child, keys)
		}
	case []any:
		for _, item := range v {
			keys = collectImageKeys(item, keys)
		}
	}
	return keys
}

func appendImageKey(keys []string, key string) []string {
	key = strings.TrimSpace(key)
	if key == "" {
		return keys
	}
	for _, existing := range keys {
		if existing == key {
			return keys
		}
	}
	return append(keys, key)
}

func extractReadableMessageText(content string) string {
	var value any
	if err := json.Unmarshal([]byte(content), &value); err != nil {
		return ""
	}
	parts := collectReadableText(value, nil)
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func collectReadableText(value any, parts []string) []string {
	switch v := value.(type) {
	case map[string]any:
		for _, key := range []string{"title", "subtitle", "text", "content", "name", "value", "alt"} {
			if text, ok := v[key].(string); ok {
				parts = appendReadableText(parts, text)
			}
		}
		for _, key := range []string{"body", "header", "title", "text", "content", "content_v2", "elements", "fields", "columns", "children", "items", "zh_cn", "en_us"} {
			if child, ok := v[key]; ok {
				if _, isString := child.(string); !isString {
					parts = collectReadableText(child, parts)
				}
			}
		}
	case []any:
		for _, item := range v {
			parts = collectReadableText(item, parts)
		}
	case string:
		parts = appendReadableText(parts, v)
	}
	return parts
}

func appendReadableText(parts []string, text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return parts
	}
	if len(parts) > 0 && parts[len(parts)-1] == text {
		return parts
	}
	return append(parts, text)
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
