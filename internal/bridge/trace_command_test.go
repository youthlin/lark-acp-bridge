package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestHandleTraceCommandUpdatesBotConfig(t *testing.T) {
	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Config{
		Bots: []config.BotConfig{{
			ID:           "bot-a",
			AppID:        "cli_a",
			AppSecret:    config.FileSecret("$HOME/.lark-acp-bridge/secrets/bot-a.appsecret"),
			Workspace:    workspace,
			OwnerOpenIDs: []string{testOwnerOpenID},
		}},
	}
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	svc := NewService(loaded, NewSessionStore(filepath.Join(workspace, ".local", "sessions.json"))).WithConfigPath(configPath)
	msg := feishu.Message{BotID: "bot-a", ChatID: "oc_trace", SenderID: testOwnerOpenID}

	got := svc.handleTraceCommand(context.Background(), "/trace off", msg)
	if !strings.Contains(got, "ACP trace：关闭") {
		t.Fatalf("trace off reply = %q, want disabled status", got)
	}
	persisted, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load(after trace off) error = %v", err)
	}
	if cfg := persisted.Bots[0].Trace; cfg.Enabled || !cfg.Disabled {
		t.Fatalf("persisted trace = %+v, want disabled", cfg)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(config after trace off) error = %v", err)
	}
	if !strings.Contains(string(data), `"enabled": false`) || strings.Contains(string(data), `"disabled"`) {
		t.Fatalf("config after trace off = %s, want enabled false without internal disabled field", data)
	}

	got = svc.handleTraceCommand(context.Background(), "/trace on 14d", msg)
	if !strings.Contains(got, "ACP trace：开启") || !strings.Contains(got, "保留期：14d") {
		t.Fatalf("trace on reply = %q, want enabled 14d status", got)
	}
	persisted, err = config.Load(configPath)
	if err != nil {
		t.Fatalf("Load(after trace on) error = %v", err)
	}
	if cfg := persisted.Bots[0].Trace; !cfg.Enabled || cfg.Disabled || cfg.RetentionDays != 14 {
		t.Fatalf("persisted trace = %+v, want enabled 14d", cfg)
	}
}

func TestHandleTraceCommandRequiresOwner(t *testing.T) {
	cfg := config.Config{Bots: []config.BotConfig{{ID: "bot-a", Workspace: t.TempDir(), OwnerOpenIDs: []string{testOwnerOpenID}}}}
	svc := NewService(cfg, NewSessionStore(""))
	got := svc.handleTraceCommand(context.Background(), "/trace off", feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		SenderID: "ou_other",
	})
	if !strings.Contains(got, "只有 bot owner") {
		t.Fatalf("reply = %q, want owner-only rejection", got)
	}
}

func TestTraceStoreForSessionConcurrentWithTraceConfigUpdate(t *testing.T) {
	workspace := t.TempDir()
	svc := NewService(config.Config{Bots: []config.BotConfig{{
		ID:        "bot-a",
		Workspace: workspace,
		Trace:     config.TraceConfig{Enabled: true, RetentionDays: 7},
	}}}, NewSessionStore(""))
	session := Session{Key: imSessionKey("bot-a", "oc_chat", ""), ACPSessionID: "acp-session-1"}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = svc.newTraceRecorder(session, "hello")
			}
		}()
	}
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if (i+j)%2 == 0 {
					svc.setTraceStore("bot-a", nil)
					continue
				}
				svc.setTraceStore("bot-a", newTraceStore(workspace, config.TraceConfig{Enabled: true, RetentionDays: 7}))
			}
		}(i)
	}
	wg.Wait()
}
