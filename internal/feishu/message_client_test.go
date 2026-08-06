package feishu

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
	return filepath.Join(workspace, "cache", imageKey+".png"), nil
}

func (c *concurrentMessageClient) UploadImage(context.Context, string) (string, error) {
	return "", nil
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
		want := filepath.Join("/workspace", "cache", images[i].ImageKey+".png")
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
	for client.maxActive < maxConcurrentImageDownloads {
		select {
		case <-deadline:
			close(client.release)
			t.Fatalf("active downloads never reached %d, got %d", maxConcurrentImageDownloads, client.maxActive)
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(client.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("hydrate did not finish after releasing downloads")
	}
	if client.maxActive > maxConcurrentImageDownloads {
		t.Fatalf("max concurrent downloads = %d, want <= %d", client.maxActive, maxConcurrentImageDownloads)
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
