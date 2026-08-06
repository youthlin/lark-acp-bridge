package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// imageDownloadTimeout 是单张飞书图片下载的超时。
const imageDownloadTimeout = 30 * time.Second

// maxConcurrentImageDownloads 限制单条消息的图片并发下载数。
const maxConcurrentImageDownloads = 4

type larkMessageClient struct {
	client *lark.Client
}

func (c larkMessageClient) GetMessage(ctx context.Context, messageID string, workspace string) (*Message, error) {
	if c.client == nil {
		return nil, fmt.Errorf("飞书客户端未初始化")
	}
	if strings.TrimSpace(messageID) == "" {
		return nil, fmt.Errorf("飞书 message_id 为空")
	}
	req := larkim.NewGetMessageReqBuilder().
		MessageId(messageID).
		UserIdType(larkim.GetMessageContentV1UserIDTypeOpenId).
		Build()
	resp, err := c.client.Im.V1.Message.Get(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("调用飞书获取消息接口: %w", err)
	}
	if !resp.Success() {
		return nil, fmt.Errorf("飞书获取消息接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || len(resp.Data.Items) == 0 || resp.Data.Items[0] == nil {
		return nil, fmt.Errorf("飞书获取消息接口未返回消息")
	}
	msg := messageFromLarkMessage(resp.Data.Items[0])
	if msg == nil {
		return nil, nil
	}
	msg.Workspace = workspace
	msg.Images = hydrateMessageImages(ctx, c, msg.MessageID, workspace, msg.Images)
	if strings.EqualFold(msg.MsgType, "merge_forward") {
		expandMergedForwardMessage(ctx, c, msg, resp.Data.Items[1:], workspace)
	}
	setMessagePrimaryImage(msg)
	return msg, nil
}

func messageFromLarkMessage(item *larkim.Message) *Message {
	if item == nil {
		return nil
	}
	msg := &Message{
		MessageID:      value(item.MessageId),
		MsgType:        value(item.MsgType),
		UpperMessageID: value(item.UpperMessageId),
	}
	if item.Sender != nil {
		msg.SenderID = value(item.Sender.Id)
		msg.SenderType = value(item.Sender.SenderType)
	}
	if item.Body != nil {
		content := value(item.Body.Content)
		msg.Images = parseMessageImages(content)
		setMessagePrimaryImage(msg)
		if strings.EqualFold(msg.MsgType, "image") {
			msg.Text = ""
		} else {
			text, err := parseMessageContent(msg.MsgType, content)
			if err != nil {
				msg.Text = parseMessageTextContent(content)
			} else {
				msg.Text = text
			}
		}
	}
	for _, mention := range item.Mentions {
		if mention == nil {
			continue
		}
		msg.Mentions = append(msg.Mentions, Mention{
			Key:  value(mention.Key),
			ID:   value(mention.Id),
			Name: value(mention.Name),
			Type: value(mention.IdType),
		})
	}
	msg.Text = replaceMentionKeys(msg.Text, msg.Mentions)
	if msg.MessageID == "" && msg.MsgType == "" && msg.Text == "" && msg.ImageKey == "" && len(msg.Images) == 0 {
		return nil
	}
	return msg
}

func messagesFromLarkMessages(items []*larkim.Message) []Message {
	messages := make([]Message, 0, len(items))
	for _, item := range items {
		msg := messageFromLarkMessage(item)
		if msg == nil {
			continue
		}
		messages = append(messages, *msg)
	}
	return messages
}

func expandMergedForwardMessage(ctx context.Context, client messageClient, msg *Message, items []*larkim.Message, workspace string) {
	if msg == nil {
		return
	}
	children := messagesFromLarkMessages(items)
	for i := range children {
		children[i].Images = hydrateMessageImages(ctx, client, children[i].MessageID, workspace, children[i].Images)
		setMessagePrimaryImage(&children[i])
		msg.Images = append(msg.Images, children[i].Images...)
	}
	msg.Text = formatMergedForwardText(msg, children)
}

func formatMergedForwardText(root *Message, children []Message) string {
	if len(children) == 0 {
		if root == nil {
			return "[合并转发消息]"
		}
		text := strings.TrimSpace(root.Text)
		if text != "" && !strings.EqualFold(text, "Merged and Forwarded Message") {
			return text
		}
		return "[合并转发消息]"
	}
	lines := []string{"[合并转发消息]"}
	for _, child := range children {
		text := strings.TrimSpace(child.PromptText())
		if text == "" {
			text = labeledMessageText("暂不支持的子消息", messageFields(map[string]any{"msg_type": child.MsgType}, "msg_type")...)
		}
		lines = append(lines, formatMergedForwardChildText(child, text))
	}
	return strings.TrimSpace(strings.Join(lines, "\n\n"))
}

func formatMergedForwardChildText(child Message, text string) string {
	var prefix string
	if sender := strings.TrimSpace(child.SenderID); sender != "" {
		prefix = "用户(" + sender + ")"
	} else {
		prefix = "子消息"
	}
	if msgType := strings.TrimSpace(child.MsgType); msgType != "" {
		prefix += " [" + msgType + "]"
	}
	return prefix + ":\n" + strings.TrimSpace(text)
}

func (c larkMessageClient) DownloadImage(ctx context.Context, messageID string, imageKey string, workspace string) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("飞书客户端未初始化")
	}
	messageID = strings.TrimSpace(messageID)
	imageKey = strings.TrimSpace(imageKey)
	workspace = strings.TrimSpace(workspace)
	if messageID == "" {
		return "", fmt.Errorf("飞书 message_id 为空")
	}
	if imageKey == "" {
		return "", fmt.Errorf("飞书 image_key 为空")
	}
	if workspace == "" {
		return "", fmt.Errorf("workspace 为空，无法保存飞书图片")
	}
	path := messageImageCachePath(workspace, imageKey)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("检查飞书图片缓存: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("创建飞书图片缓存目录: %w", err)
	}
	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(imageKey).
		Type("image").
		Build()
	resp, err := c.client.Im.V1.MessageResource.Get(ctx, req)
	if err != nil {
		return "", fmt.Errorf("调用飞书获取图片资源接口: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("飞书获取图片资源接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if err := resp.WriteFile(path); err != nil {
		return "", fmt.Errorf("写入飞书图片缓存: %w", err)
	}
	return path, nil
}

func (c larkMessageClient) UploadImage(ctx context.Context, path string) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("飞书客户端未初始化")
	}
	path, err := normalizeReplyImagePath(path)
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("打开图片文件: %w", err)
	}
	defer file.Close()
	body := larkim.NewCreateImageReqBodyBuilder().
		ImageType(larkim.CreateImageImageTypeMessage).
		Image(file).
		Build()
	resp, err := c.client.Im.V1.Image.Create(ctx, larkim.NewCreateImageReqBuilder().Body(body).Build())
	if err != nil {
		return "", fmt.Errorf("调用飞书上传图片接口: %w", err)
	}
	if !resp.Success() {
		return "", fmt.Errorf("飞书上传图片接口返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || strings.TrimSpace(value(resp.Data.ImageKey)) == "" {
		return "", fmt.Errorf("飞书上传图片接口未返回 image_key")
	}
	return value(resp.Data.ImageKey), nil
}

func hydrateMessageImages(ctx context.Context, client messageClient, messageID string, workspace string, images []MessageImage) []MessageImage {
	if client == nil || len(images) == 0 {
		return images
	}
	hydrated := make([]MessageImage, len(images))
	copy(hydrated, images)

	sem := make(chan struct{}, maxConcurrentImageDownloads)
	var wg sync.WaitGroup
	for i := range hydrated {
		image := hydrated[i]
		if strings.TrimSpace(image.ImageKey) == "" || strings.TrimSpace(image.LocalPath) != "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(index int, img MessageImage) {
			defer wg.Done()
			defer func() { <-sem }()
			dlCtx, cancel := context.WithTimeout(ctx, imageDownloadTimeout)
			defer cancel()
			path, err := client.DownloadImage(dlCtx, messageID, img.ImageKey, workspace)
			if err != nil {
				slog.WarnContext(ctx, "下载飞书图片失败", "错误", err)
				slog.DebugContext(ctx, "下载飞书图片失败详情", "message_id", messageID, "image_key", img.ImageKey, "错误", err)
				return
			}
			hydrated[index].LocalPath = path
		}(i, image)
	}
	wg.Wait()
	return hydrated
}

func setMessagePrimaryImage(msg *Message) {
	if msg == nil || len(msg.Images) == 0 {
		return
	}
	msg.ImageKey = msg.Images[0].ImageKey
	msg.LocalPath = msg.Images[0].LocalPath
}

func messageImageCachePath(workspace string, imageKey string) string {
	return filepath.Join(workspace, ".local", "cache", safeImageCacheName(imageKey)+".png")
}

func safeImageCacheName(imageKey string) string {
	imageKey = strings.TrimSpace(imageKey)
	if imageKey == "" {
		return "image"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_")
	return replacer.Replace(imageKey)
}

func normalizeReplyImagePath(path string) (string, error) {
	path = trimReplyImagePath(path)
	if path == "" {
		return "", fmt.Errorf("图片路径为空")
	}
	if strings.Contains(path, "://") && !strings.HasPrefix(strings.ToLower(path), "file://") {
		return "", fmt.Errorf("只支持本地图片路径: %s", path)
	}
	if strings.HasPrefix(strings.ToLower(path), "file://") {
		path = strings.TrimPrefix(path, "file://")
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("查找用户主目录: %w", err)
		}
		path = filepath.Join(home, path[2:])
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("图片路径必须是绝对路径: %s", path)
	}
	path = filepath.Clean(path)
	if !hasSupportedImageExtension(path) {
		return "", fmt.Errorf("不支持的图片类型: %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("检查图片文件: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("图片路径是目录: %s", path)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("图片文件为空: %s", path)
	}
	if info.Size() > 10*1024*1024 {
		return "", fmt.Errorf("图片文件超过 10MB: %s", path)
	}
	return path, nil
}

func trimReplyImagePath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.Trim(path, "\"'")
	return strings.TrimSpace(path)
}

func hasSupportedImageExtension(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".tif", ".tiff", ".bmp", ".ico":
		return true
	default:
		return false
	}
}
