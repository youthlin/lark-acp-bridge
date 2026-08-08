package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestHandleUpdateCommandRequiresOwner(t *testing.T) {
	svc := newTestService(config.Default(), nil)
	msg := feishu.Message{BotID: "default", ChatID: "oc_chat", SenderID: "ou_stranger"}

	got := svc.handleUpdateCommand(context.Background(), "/update", msg)
	if !strings.Contains(got, "只有 bot owner") {
		t.Fatalf("reply = %q, want owner-only rejection", got)
	}
}

func TestHandleUpdateCommandReportsUsageForBadArgs(t *testing.T) {
	svc := newTestService(config.Default(), nil).WithVersion("v1.0.0")
	msg := feishu.Message{BotID: "default", ChatID: "oc_chat", SenderID: testOwnerOpenID}

	got := svc.handleUpdateCommand(context.Background(), "/update --unknown-flag", msg)
	if !strings.Contains(got, "参数解析失败") {
		t.Fatalf("reply = %q, want flag parse error", got)
	}

	got = svc.handleUpdateCommand(context.Background(), "/update extra positional", msg)
	if !strings.Contains(got, "/update") {
		t.Fatalf("reply = %q, want usage", got)
	}
}

func TestHandleUpdateCommandRejectsGoRunBinary(t *testing.T) {
	orig := currentExecutable
	currentExecutable = func() (string, error) {
		return "/tmp/go-build123/b001/exe/lark-acp-bridge", nil
	}
	defer func() { currentExecutable = orig }()

	svc := newTestService(config.Default(), nil).WithVersion("v1.0.0")
	msg := feishu.Message{BotID: "default", ChatID: "oc_chat", SenderID: testOwnerOpenID}

	got := svc.handleUpdateCommand(context.Background(), "/update", msg)
	if !strings.Contains(got, "go run 临时文件") {
		t.Fatalf("reply = %q, want go-run rejection", got)
	}
}

func TestHandleUpdateCommandExeError(t *testing.T) {
	orig := currentExecutable
	currentExecutable = func() (string, error) {
		return "", errors.New("boom")
	}
	defer func() { currentExecutable = orig }()

	svc := newTestService(config.Default(), nil).WithVersion("v1.0.0")
	msg := feishu.Message{BotID: "default", ChatID: "oc_chat", SenderID: testOwnerOpenID}

	got := svc.handleUpdateCommand(context.Background(), "/update", msg)
	if !strings.Contains(got, "定位当前可执行文件失败") {
		t.Fatalf("reply = %q, want executable error", got)
	}
}

func TestHandleUpdateCommandCheckRunsAsync(t *testing.T) {
	svc := newTestService(config.Default(), nil).WithVersion("v999.0.0")
	client := newFakeSentMessageClient("om_update")
	replies := make(chan string, 1)
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		replies <- text
		return nil
	}
	ctx := withFakeSentMessageClient(context.Background(), svc, "default", client)
	msg := feishu.Message{BotID: "default", ChatID: "oc_chat", SenderID: testOwnerOpenID}

	// --check 用一个比当前旧的离线目标版本，避免发起网络请求，且应异步回复“已是最新版本”。
	got := svc.handleUpdateCommand(ctx, "/update --check --version v1.0.0", msg)
	if got != "" {
		t.Fatalf("reply = %q, want empty (async)", got)
	}
	select {
	case reply := <-replies:
		if !strings.Contains(reply, "已是最新版本") {
			t.Fatalf("async reply = %q, want already latest", reply)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for async update reply")
	}
}

func TestHandleUpdateCommandRollbackRunsAsync(t *testing.T) {
	target := filepath.Join(t.TempDir(), "lark-acp-bridge")
	if err := os.WriteFile(target, []byte("new"), 0o755); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}
	backup := target + ".bak"
	if err := os.WriteFile(backup, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(backup) error = %v", err)
	}

	svc := newTestService(config.Default(), nil).WithVersion("v1.0.0")
	client := newFakeSentMessageClient("om_update")
	replies := make(chan string, 1)
	client.replySender = func(ctx context.Context, msg feishu.Message, text string) error {
		replies <- text
		return nil
	}
	ctx := withFakeSentMessageClient(context.Background(), svc, "default", client)
	msg := feishu.Message{BotID: "default", ChatID: "oc_chat", SenderID: testOwnerOpenID}

	got := svc.handleUpdateCommand(ctx, "/update rollback --binary "+target, msg)
	if got != "" {
		t.Fatalf("reply = %q, want empty (async)", got)
	}
	select {
	case reply := <-replies:
		for _, want := range []string{"已回滚到最近一次备份", backup, "请用 /restart 重启 bridge 服务使回滚版本生效"} {
			if !strings.Contains(reply, want) {
				t.Fatalf("async reply = %q, want %q", reply, want)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for async update rollback reply")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if string(data) != "old" {
		t.Fatalf("target = %q, want old", data)
	}
}

func TestHandleUpdateCommandRollbackRejectsUpdateFlags(t *testing.T) {
	svc := newTestService(config.Default(), nil).WithVersion("v1.0.0")
	msg := feishu.Message{BotID: "default", ChatID: "oc_chat", SenderID: testOwnerOpenID}

	got := svc.handleUpdateCommand(context.Background(), "/update rollback --check", msg)
	if !strings.Contains(got, "rollback 只支持 --binary") {
		t.Fatalf("reply = %q, want rollback flag rejection", got)
	}
}

func TestLooksLikeGoRun(t *testing.T) {
	cases := map[string]bool{
		"/tmp/go-build123/b001/exe/lark-acp-bridge": true,
		"/home/me/go/bin/lark-acp-bridge":           false,
		"/usr/local/bin/lark-acp-bridge":            false,
	}
	for path, want := range cases {
		if got := looksLikeGoRun(path); got != want {
			t.Errorf("looksLikeGoRun(%q) = %v, want %v", path, got, want)
		}
	}
}
