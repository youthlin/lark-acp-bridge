package feishu

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type Message struct {
	BotID     string
	BotOpenID string
	Workspace string
	MessageID string
	ChatID    string
	ChatType  string
	ChatMode  string
	// GroupMessageType comes from the chat info API. "chat" means ordinary
	// group messages, while "thread" means topic-group messages.
	GroupMessageType string
	ThreadID         string
	RootID           string
	ParentID         string
	CreatedAt        time.Time
	SenderID         string
	SenderType       string
	MsgType          string
	Text             string
	ImageKey         string
	LocalPath        string
	Images           []MessageImage
	Mentions         []Mention
	Reply            *ReplyContext
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

func (m Message) IsTopicGroup() bool {
	groupMessageType := strings.TrimSpace(m.GroupMessageType)
	if strings.EqualFold(groupMessageType, "thread") {
		return true
	}
	return false
}

func (m Message) IsTopicThread() bool {
	if strings.TrimSpace(m.ThreadID) == "" || m.IsPrivateChat() {
		return false
	}
	return m.IsTopicGroup()
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
		CreatedAt: parseMessageCreateTime(value(raw.CreateTime)),
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
	text, err := parseMessageContent(msg.MsgType, content)
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

func parseMessageCreateTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	millis, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || millis <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(millis)
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

func parseMessageContent(msgType string, content string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(msgType)) {
	case "image":
		return "", nil
	case "text":
		return parseTextContent(content)
	case "post":
		return parsePostContent(content), nil
	case "interactive":
		return parseInteractiveContent(content), nil
	default:
		return parseStructuredMessageContent(msgType, content), nil
	}
}

func parseMessageTextContent(content string) string {
	text, err := parseTextContent(content)
	if err != nil || strings.TrimSpace(text) == "" {
		return extractReadableMessageText(content)
	}
	return strings.TrimSpace(text)
}

func parseStructuredMessageContent(msgType string, content string) string {
	msgType = strings.ToLower(strings.TrimSpace(msgType))
	if strings.TrimSpace(content) == "" {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return parseMessageTextContent(content)
	}
	switch msgType {
	case "file":
		return labeledMessageText("文件消息", messageFields(payload, "file_name", "file_key")...)
	case "folder":
		return labeledMessageText("文件夹消息", messageFields(payload, "file_name", "file_key")...)
	case "audio":
		return labeledMessageText("音频消息", messageFields(payload, "file_key", "duration")...)
	case "media":
		return labeledMessageText("视频消息", messageFields(payload, "file_name", "file_key", "image_key", "duration")...)
	case "sticker":
		return labeledMessageText("表情包消息", messageFields(payload, "file_key")...)
	case "share_chat":
		return labeledMessageText("群名片", messageFields(payload, "chat_id")...)
	case "share_user":
		return labeledMessageText("个人名片", messageFields(payload, "user_id")...)
	case "share_calendar_event", "calendar", "general_calendar":
		return labeledMessageText("日程消息", messageFields(payload, "summary", "start_time", "end_time")...)
	case "location":
		return labeledMessageText("位置消息", messageFields(payload, "name", "longitude", "latitude")...)
	case "video_chat":
		return labeledMessageText("视频通话消息", messageFields(payload, "topic", "start_time")...)
	case "todo":
		fields := messageFields(payload, "task_id", "due_time")
		if summary := postFieldText(payload, "summary"); summary != "" {
			fields = append([]string{"summary: " + summary}, fields...)
		}
		return labeledMessageText("任务消息", fields...)
	case "vote":
		return labeledMessageText("投票消息", messageFields(payload, "topic", "options")...)
	case "system":
		if text := systemMessageText(payload); text != "" {
			return text
		}
	case "hongbao", "merge_forward":
		return parseMessageTextContent(content)
	}
	return extractReadableMessageText(content)
}

func parseInteractiveContent(content string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return ""
	}
	if userDSL := firstString(payload, "user_dsl"); userDSL != "" {
		if text := parseInteractiveCardDSL(userDSL); text != "" {
			return text
		}
	}
	if text := interactiveCardText(payload); text != "" {
		return text
	}
	return extractReadableMessageText(content)
}

func parseInteractiveCardDSL(raw string) string {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return ""
	}
	return interactiveCardText(value)
}

func interactiveCardText(value any) string {
	parts := collectInteractiveCardText(value, nil)
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func collectInteractiveCardText(value any, parts []string) []string {
	switch v := value.(type) {
	case map[string]any:
		return collectInteractiveCardObjectText(v, parts)
	case []any:
		if line, ok := interactiveInlineLine(v); ok {
			return appendReadableText(parts, line)
		}
		for _, item := range v {
			parts = collectInteractiveCardText(item, parts)
		}
	case string:
		parts = appendReadableText(parts, v)
	}
	return parts
}

func collectInteractiveCardObjectText(value map[string]any, parts []string) []string {
	if text := interactiveCardElementText(value); text != "" {
		parts = appendReadableText(parts, text)
	}
	for _, key := range []string{"header", "title", "subtitle", "text", "body", "elements", "columns", "items", "children", "fields", "options", "placeholder"} {
		child, ok := value[key]
		if !ok {
			continue
		}
		if _, isString := child.(string); isString {
			continue
		}
		parts = collectInteractiveCardText(child, parts)
	}
	return parts
}

func interactiveInlineLine(items []any) (string, bool) {
	var builder strings.Builder
	foundElement := false
	for _, item := range items {
		elem, ok := item.(map[string]any)
		if !ok {
			return "", false
		}
		tag := strings.TrimSpace(firstString(elem, "tag"))
		if tag == "" {
			return "", false
		}
		if !isInteractiveInlineTag(tag) {
			return "", false
		}
		foundElement = true
		text := interactiveCardElementText(elem)
		if text == "" {
			continue
		}
		builder.WriteString(text)
	}
	return strings.TrimSpace(builder.String()), foundElement
}

func isInteractiveInlineTag(tag string) bool {
	switch strings.ToLower(strings.TrimSpace(tag)) {
	case "text", "plain_text", "a", "link", "at", "mention", "img", "image", "hr", "divider", "br", "button":
		return true
	default:
		return false
	}
}

func interactiveCardElementText(elem map[string]any) string {
	tag := strings.ToLower(strings.TrimSpace(firstString(elem, "tag")))
	switch tag {
	case "markdown", "md", "lark_md":
		return firstRawString(elem, "content", "text")
	case "plain_text", "text":
		return firstRawString(elem, "content", "text")
	case "a", "link":
		text := interactiveTextValue(firstAny(elem, "text", "content", "title"))
		href := firstString(elem, "href", "url")
		switch {
		case text != "" && href != "":
			return "[" + text + "](" + href + ")"
		case href != "":
			return href
		default:
			return text
		}
	case "at", "mention":
		name := firstString(elem, "user_name", "name", "text")
		if name == "" {
			name = firstString(elem, "user_id", "open_id", "id")
		}
		if name == "" {
			return ""
		}
		if strings.HasPrefix(name, "@") {
			return name
		}
		return "@" + name
	case "img", "image":
		return ""
	case "hr", "divider":
		return "---"
	case "br":
		return "\n"
	}
	if text := interactiveTextValue(firstAny(elem, "title", "subtitle", "text", "content", "name", "value", "alt", "placeholder")); text != "" {
		return text
	}
	return ""
}

func interactiveTextValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		if text := firstRawString(v, "content", "text"); strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
		return interactiveCardText(v)
	case []any:
		return interactiveCardText(v)
	default:
		return messageFieldString(v)
	}
}

func firstAny(value map[string]any, keys ...string) any {
	for _, key := range keys {
		if raw, ok := value[key]; ok {
			return raw
		}
	}
	return nil
}

func parsePostContent(content string) string {
	var value any
	if err := json.Unmarshal([]byte(content), &value); err != nil {
		return ""
	}
	parts := collectPostText(value, nil)
	if len(parts) == 0 {
		return extractReadableMessageText(content)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func collectPostText(value any, parts []string) []string {
	switch v := value.(type) {
	case map[string]any:
		if localized := collectLocalizedPostText(v, parts); len(localized) > len(parts) {
			return localized
		}
		return collectPostObjectText(v, parts)
	case []any:
		for _, item := range v {
			parts = collectPostBlockText(item, parts)
		}
	}
	return parts
}

func collectLocalizedPostText(value map[string]any, parts []string) []string {
	for _, key := range []string{"zh_cn", "en_us"} {
		child, ok := value[key]
		if !ok {
			continue
		}
		before := len(parts)
		parts = collectPostText(child, parts)
		if len(parts) > before {
			return parts
		}
	}
	return parts
}

func collectPostObjectText(value map[string]any, parts []string) []string {
	if tag := strings.TrimSpace(firstString(value, "tag")); tag != "" {
		return appendPostText(parts, postElementText(tag, value))
	}
	parts = appendPostText(parts, firstString(value, "title"))
	parts = collectPreferredPostChildText(value, parts, "content_v2", "content")
	for _, key := range []string{"elements", "children", "items"} {
		child, ok := value[key]
		if !ok {
			continue
		}
		parts = collectPostBlockText(child, parts)
	}
	if len(parts) == 0 {
		parts = collectReadableText(value, parts)
	}
	return parts
}

func collectPreferredPostChildText(value map[string]any, parts []string, keys ...string) []string {
	for _, key := range keys {
		child, ok := value[key]
		if !ok {
			continue
		}
		before := len(parts)
		parts = collectPostBlockText(child, parts)
		if len(parts) > before {
			return parts
		}
	}
	return parts
}

func collectPostBlockText(value any, parts []string) []string {
	switch v := value.(type) {
	case map[string]any:
		return collectPostObjectText(v, parts)
	case []any:
		if line, ok := postInlineLine(v); ok {
			return appendPostText(parts, line)
		}
		for _, item := range v {
			parts = collectPostBlockText(item, parts)
		}
	case string:
		parts = appendPostText(parts, v)
	}
	return parts
}

func postInlineLine(items []any) (string, bool) {
	var builder strings.Builder
	foundElement := false
	for _, item := range items {
		elem, ok := item.(map[string]any)
		if !ok {
			return "", false
		}
		tag := strings.TrimSpace(firstString(elem, "tag"))
		if tag == "" {
			return "", false
		}
		foundElement = true
		text := postElementText(tag, elem)
		if text == "" {
			continue
		}
		builder.WriteString(text)
	}
	return strings.TrimSpace(builder.String()), foundElement
}

func postElementText(tag string, elem map[string]any) string {
	switch strings.ToLower(strings.TrimSpace(tag)) {
	case "text", "plain_text", "md", "markdown", "lark_md":
		return firstRawString(elem, "text", "content")
	case "a", "link":
		text := firstString(elem, "text", "content", "title")
		href := firstString(elem, "href", "url")
		switch {
		case text != "" && href != "":
			return "[" + text + "](" + href + ")"
		case href != "":
			return href
		default:
			return text
		}
	case "at", "mention":
		name := firstString(elem, "user_name", "name", "text")
		if name == "" {
			name = firstString(elem, "user_id", "open_id", "id")
		}
		if name == "" {
			return ""
		}
		if strings.HasPrefix(name, "@") {
			return name
		}
		return "@" + name
	case "emotion", "emoji":
		name := firstString(elem, "emoji_type", "text", "name", "key")
		if name == "" {
			return "[表情]"
		}
		return "[表情: " + name + "]"
	case "code_block", "code":
		text := firstString(elem, "text", "content")
		if text == "" {
			return ""
		}
		language := firstString(elem, "language")
		return "```" + language + "\n" + text + "\n```"
	case "img", "image":
		return ""
	case "media", "file":
		name := firstString(elem, "file_name", "name", "text")
		if name == "" {
			if strings.EqualFold(tag, "media") {
				return "[视频]"
			}
			return "[文件]"
		}
		if strings.EqualFold(tag, "media") {
			return "[视频: " + name + "]"
		}
		return "[文件: " + name + "]"
	case "hr":
		return "---"
	case "br":
		return "\n"
	default:
		return firstString(elem, "text", "content", "name", "value", "alt", "title")
	}
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		raw, ok := value[key]
		if !ok {
			continue
		}
		if text, ok := raw.(string); ok {
			if text = strings.TrimSpace(text); text != "" {
				return text
			}
		}
	}
	return ""
}

func firstRawString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		raw, ok := value[key]
		if !ok {
			continue
		}
		if text, ok := raw.(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func appendPostText(parts []string, text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return parts
	}
	if len(parts) > 0 && parts[len(parts)-1] == text {
		return parts
	}
	return append(parts, text)
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
		for _, keyName := range []string{"image_key", "imageKey", "img_key", "imgKey"} {
			if key, ok := v[keyName].(string); ok {
				keys = appendImageKey(keys, key)
			}
		}
		if userDSL := firstString(v, "user_dsl"); userDSL != "" {
			var child any
			if err := json.Unmarshal([]byte(userDSL), &child); err == nil {
				keys = collectImageKeys(child, keys)
			}
		}
		if tag := strings.ToLower(strings.TrimSpace(firstString(v, "tag"))); tag == "md" || tag == "markdown" || tag == "lark_md" {
			keys = appendMarkdownImageKeys(keys, firstRawString(v, "text", "content"))
		}
		visited := make(map[string]struct{}, len(v))
		for _, childKey := range []string{"body", "header", "title", "text", "content", "content_v2", "elements", "fields", "columns", "children", "items", "zh_cn", "en_us"} {
			if child, ok := v[childKey]; ok {
				keys = collectImageKeysValue(child, keys)
				visited[childKey] = struct{}{}
			}
		}
		remaining := make([]string, 0, len(v))
		for childKey := range v {
			if _, ok := visited[childKey]; ok {
				continue
			}
			if childKey == "image_key" || childKey == "imageKey" || childKey == "img_key" || childKey == "imgKey" || childKey == "user_dsl" {
				continue
			}
			remaining = append(remaining, childKey)
		}
		slices.Sort(remaining)
		for _, childKey := range remaining {
			keys = collectImageKeysValue(v[childKey], keys)
		}
	case []any:
		for _, item := range v {
			keys = collectImageKeys(item, keys)
		}
	}
	return keys
}

func collectImageKeysValue(value any, keys []string) []string {
	if _, isString := value.(string); isString {
		return keys
	}
	return collectImageKeys(value, keys)
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

var markdownImagePattern = regexp.MustCompile(`!\[[^\]]*]\(([^)\s]+)\)`)

func appendMarkdownImageKeys(keys []string, text string) []string {
	for _, group := range markdownImagePattern.FindAllStringSubmatch(text, -1) {
		if len(group) < 2 {
			continue
		}
		keys = appendImageKey(keys, group[1])
	}
	return keys
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

func labeledMessageText(label string, fields ...string) string {
	label = strings.TrimSpace(label)
	fields = nonEmptyStrings(fields)
	if label == "" {
		return strings.Join(fields, "\n")
	}
	if len(fields) == 0 {
		return "[" + label + "]"
	}
	return "[" + label + "]\n" + strings.Join(fields, "\n")
}

func messageFields(payload map[string]any, keys ...string) []string {
	fields := make([]string, 0, len(keys))
	for _, key := range keys {
		if text := messageFieldString(payload[key]); text != "" {
			fields = append(fields, key+": "+text)
		}
	}
	return fields
}

func messageFieldString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if text := messageFieldString(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		if text := firstString(v, "text", "title", "name", "content"); text != "" {
			return text
		}
		if post := postObjectText(v); post != "" {
			return post
		}
	}
	return ""
}

func postFieldText(payload map[string]any, key string) string {
	child, ok := payload[key]
	if !ok {
		return ""
	}
	switch v := child.(type) {
	case map[string]any:
		return postObjectText(v)
	case string:
		return strings.TrimSpace(v)
	default:
		return messageFieldString(v)
	}
}

func postObjectText(value map[string]any) string {
	parts := collectPostText(value, nil)
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func systemMessageText(payload map[string]any) string {
	if text := messageFieldString(payload["text"]); text != "" {
		return text
	}
	if divider, ok := payload["divider_text"].(map[string]any); ok {
		if text := localizedText(divider); text != "" {
			return "[系统消息]\n" + text
		}
	}
	if params, ok := payload["params"].(map[string]any); ok {
		if divider, ok := params["divider_text"].(map[string]any); ok {
			if text := localizedText(divider); text != "" {
				return "[系统消息]\n" + text
			}
		}
	}
	if template := messageFieldString(payload["template"]); template != "" {
		return labeledMessageText("系统消息", systemTemplateFields(template, payload)...)
	}
	return ""
}

func localizedText(value map[string]any) string {
	if text := firstString(value, "text"); text != "" {
		return text
	}
	if texts, ok := value["i18n_text"].(map[string]any); ok {
		for _, key := range []string{"zh_cn", "zh_CN", "en_us", "en_US"} {
			if text := messageFieldString(texts[key]); text != "" {
				return text
			}
		}
	}
	return ""
}

func systemTemplateFields(template string, payload map[string]any) []string {
	fields := []string{"template: " + template}
	keys := make([]string, 0, len(payload))
	for key := range payload {
		if key == "template" {
			continue
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if text := messageFieldString(payload[key]); text != "" {
			fields = append(fields, key+": "+text)
		}
	}
	return fields
}

func nonEmptyStrings(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
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
	return strings.TrimSpace(*p)
}

func valueInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func valueBool(p *bool) bool {
	return p != nil && *p
}
