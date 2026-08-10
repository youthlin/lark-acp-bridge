package feishu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type concurrentMessageClient struct {
	mu         sync.Mutex
	active     int
	maxActive  int
	downloaded []string
	release    chan struct{}
	block      bool
}

func (c *concurrentMessageClient) GetMessage(context.Context, string, string) (*Message, error) {
	return nil, nil
}

func (c *concurrentMessageClient) DownloadImage(_ context.Context, _ string, imageKey string, workspace string) (string, error) {
	c.mu.Lock()
	c.active++
	if c.active > c.maxActive {
		c.maxActive = c.active
	}
	overlap := c.active >= 2
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.active--
		c.mu.Unlock()
	}()
	if c.block {
		<-c.release
	} else if overlap {
		// 第二个并发下载在此停留片刻，确保能观察到并发在途。
		time.Sleep(20 * time.Millisecond)
	}
	c.mu.Lock()
	c.downloaded = append(c.downloaded, imageKey)
	c.mu.Unlock()
	return filepath.Join(workspace, ".local", "cache", imageKey+".png"), nil
}

func (c *concurrentMessageClient) UploadImage(context.Context, string) (string, error) {
	return "", nil
}

func (c *concurrentMessageClient) maxConcurrentDownloads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxActive
}

func TestHydrateMessageImagesAssignsPathsInOrder(t *testing.T) {
	client := &concurrentMessageClient{}
	images := []MessageImage{
		{ImageKey: "img-1"},
		{ImageKey: "img-2"},
		{ImageKey: "img-3"},
	}
	hydrated := hydrateMessageImages(context.Background(), client, "om-1", "/workspace", images)

	if len(hydrated) != len(images) {
		t.Fatalf("hydrated len = %d, want %d", len(hydrated), len(images))
	}
	for i, img := range hydrated {
		want := filepath.Join("/workspace", ".local", "cache", images[i].ImageKey+".png")
		if img.LocalPath != want {
			t.Fatalf("image[%d].LocalPath = %q, want %q", i, img.LocalPath, want)
		}
	}
	if len(client.downloaded) != len(images) {
		t.Fatalf("downloaded = %v, want all images", client.downloaded)
	}
}

func TestHydrateMessageImagesRespectsConcurrencyLimit(t *testing.T) {
	client := &concurrentMessageClient{block: true, release: make(chan struct{})}
	images := make([]MessageImage, maxConcurrentImageDownloads+3)
	for i := range images {
		images[i] = MessageImage{ImageKey: "img"}
	}
	done := make(chan struct{})
	go func() {
		hydrateMessageImages(context.Background(), client, "om-1", "/workspace", images)
		close(done)
	}()

	deadline := time.After(time.Second)
	for client.maxConcurrentDownloads() < maxConcurrentImageDownloads {
		select {
		case <-deadline:
			close(client.release)
			t.Fatalf("active downloads never reached %d, got %d", maxConcurrentImageDownloads, client.maxConcurrentDownloads())
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(client.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("hydrate did not finish after releasing downloads")
	}
	if got := client.maxConcurrentDownloads(); got > maxConcurrentImageDownloads {
		t.Fatalf("max concurrent downloads = %d, want <= %d", got, maxConcurrentImageDownloads)
	}
}

func TestHydrateMessageImagesKeepsOrderOnFailure(t *testing.T) {
	// 已有 LocalPath 的图片不应被重新下载，且失败的图片保留空 LocalPath 但不影响其他图片。
	client := &concurrentMessageClient{}
	images := []MessageImage{
		{ImageKey: "img-1", LocalPath: "/existing.png"},
		{ImageKey: "img-2"},
	}
	hydrated := hydrateMessageImages(context.Background(), client, "om-1", "/workspace", images)
	if hydrated[0].LocalPath != "/existing.png" {
		t.Fatalf("image[0].LocalPath = %q, want preserved /existing.png", hydrated[0].LocalPath)
	}
	if hydrated[1].LocalPath == "" {
		t.Fatal("image[1].LocalPath empty, want downloaded path")
	}
	if len(client.downloaded) != 1 || client.downloaded[0] != "img-2" {
		t.Fatalf("downloaded = %v, want only [img-2]", client.downloaded)
	}
}

func TestMessageFromLarkMessageKeepsMergedForwardUpperID(t *testing.T) {
	msg := messageFromLarkMessage(&larkim.Message{
		MessageId:      ptr("om_child"),
		MsgType:        ptr("text"),
		UpperMessageId: ptr("om_root"),
		Sender: &larkim.Sender{
			Id:         ptr("ou_sender"),
			SenderType: ptr("user"),
		},
		Body: &larkim.MessageBody{
			Content: ptr(`{"text":"你好 @_user_1"}`),
		},
		Mentions: []*larkim.Mention{
			{
				Key:    ptr("@_user_1"),
				Id:     ptr("ou_bot"),
				IdType: ptr("open_id"),
				Name:   ptr("智能助手"),
			},
		},
	})

	if msg == nil {
		t.Fatal("messageFromLarkMessage() = nil")
	}
	if msg.UpperMessageID != "om_root" {
		t.Fatalf("UpperMessageID = %q, want om_root", msg.UpperMessageID)
	}
	if msg.Text != "你好 @智能助手" {
		t.Fatalf("Text = %q, want mention replaced", msg.Text)
	}
}

func TestFormatMergedForwardTextIncludesChildren(t *testing.T) {
	got := formatMergedForwardText(&Message{
		MessageID: "om_root",
		MsgType:   "merge_forward",
		Text:      "Merged and Forwarded Message",
	}, []Message{
		{
			MessageID: "om_child_text",
			SenderID:  "ou_alice",
			MsgType:   "text",
			Text:      "第一条",
		},
		{
			MessageID: "om_child_image",
			SenderID:  "ou_bob",
			MsgType:   "image",
			Images: []MessageImage{
				{ImageKey: "img_forwarded", LocalPath: "/workspace/.local/cache/img_forwarded.png"},
			},
		},
	})

	for _, want := range []string{
		"[合并转发消息]",
		"用户(ou_alice) [text]:",
		"第一条",
		"用户(ou_bob) [image]:",
		"image_key: img_forwarded",
		"local_path: /workspace/.local/cache/img_forwarded.png",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatMergedForwardText() = %q, want %q", got, want)
		}
	}
}

func TestExpandMergedForwardMessagePropagatesChildImages(t *testing.T) {
	client := &concurrentMessageClient{}
	msg := &Message{
		MessageID: "om_root",
		MsgType:   "merge_forward",
		Text:      "Merged and Forwarded Message",
	}
	expandMergedForwardMessage(context.Background(), client, msg, []*larkim.Message{
		larkim.NewMessageBuilder().
			MessageId("om_child_text").
			MsgType("text").
			UpperMessageId("om_root").
			Sender(larkim.NewSenderBuilder().Id("ou_alice").SenderType("user").Build()).
			Body(larkim.NewMessageBodyBuilder().Content(`{"text":"第一条"}`).Build()).
			Build(),
		larkim.NewMessageBuilder().
			MessageId("om_child_image").
			MsgType("image").
			UpperMessageId("om_root").
			Sender(larkim.NewSenderBuilder().Id("ou_bob").SenderType("user").Build()).
			Body(larkim.NewMessageBodyBuilder().Content(`{"image_key":"img_forwarded"}`).Build()).
			Build(),
	}, "/workspace")

	if len(msg.Images) != 1 {
		t.Fatalf("root images = %+v, want one propagated child image", msg.Images)
	}
	if msg.Images[0].ImageKey != "img_forwarded" || msg.Images[0].LocalPath != filepath.Join("/workspace", ".local", "cache", "img_forwarded.png") {
		t.Fatalf("root image = %+v, want hydrated child image propagated", msg.Images[0])
	}
	for _, want := range []string{
		"用户(ou_alice) [text]:",
		"第一条",
		"用户(ou_bob) [image]:",
		"local_path: " + filepath.Join("/workspace", ".local", "cache", "img_forwarded.png"),
	} {
		if !strings.Contains(msg.Text, want) {
			t.Fatalf("message text = %q, want %q", msg.Text, want)
		}
	}
	if len(client.downloaded) != 1 || client.downloaded[0] != "img_forwarded" {
		t.Fatalf("downloaded = %+v, want forwarded child image downloaded", client.downloaded)
	}
}

func TestCleanupMessageImageCacheRemovesExpiredAndOldestOverLimit(t *testing.T) {
	workspace := t.TempDir()
	cacheDir := filepath.Join(workspace, ".local", "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(cache) error = %v", err)
	}
	now := time.Date(2026, 8, 10, 10, 0, 0, 0, time.Local)
	files := map[string]struct {
		size    int
		modTime time.Time
	}{
		"expired.png": {size: 3, modTime: now.Add(-48 * time.Hour)},
		"old.png":     {size: 4, modTime: now.Add(-4 * time.Hour)},
		"new.png":     {size: 4, modTime: now.Add(-2 * time.Hour)},
	}
	for name, file := range files {
		path := filepath.Join(cacheDir, name)
		if err := os.WriteFile(path, []byte(strings.Repeat("x", file.size)), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
		if err := os.Chtimes(path, file.modTime, file.modTime); err != nil {
			t.Fatalf("Chtimes(%s) error = %v", name, err)
		}
	}

	if err := cleanupMessageImageCache(context.Background(), workspace, now, 24*time.Hour, 5); err != nil {
		t.Fatalf("cleanupMessageImageCache() error = %v", err)
	}
	for _, name := range []string{"expired.png", "old.png"} {
		if _, err := os.Stat(filepath.Join(cacheDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s stat err = %v, want removed", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "new.png")); err != nil {
		t.Fatalf("new.png stat err = %v, want kept", err)
	}
}
