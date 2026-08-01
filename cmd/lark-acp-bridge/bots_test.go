package main

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/scene/registration"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func TestRunBotsAddReadsSecretFromStdin(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".lark-acp-bridge", "config.json")
	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	if _, err := w.WriteString("super-secret\n"); err != nil {
		t.Fatalf("WriteString(stdin) error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close(stdin writer) error = %v", err)
	}
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = r.Close()
	})

	if err := runBotsCommand(configPath, []string{"add", "default", "cli_xxx", "--stdin-secret"}); err != nil {
		t.Fatalf("runBotsCommand(add) error = %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}
	if strings.Contains(string(raw), "super-secret") {
		t.Fatalf("config leaked secret:\n%s", raw)
	}
	secretPath := filepath.Join(home, ".lark-acp-bridge", "secrets", "default.appsecret")
	secret, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("ReadFile(secret) error = %v", err)
	}
	if strings.Contains(string(secret), "super-secret") {
		t.Fatalf("secret file leaked plaintext: %q", secret)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(secret)), "lark-acp-bridge-secret:v1:") {
		t.Fatalf("secret = %q, want encrypted secret", secret)
	}
	keyPath := filepath.Join(home, ".lark-acp-bridge", "secrets", "default.key")
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("ReadFile(key) error = %v", err)
	}
	if strings.Contains(string(key), "super-secret") {
		t.Fatalf("key file leaked plaintext: %q", key)
	}
}

func TestIsBotsShorthand(t *testing.T) {
	for _, command := range []string{"list", "add", "register", "remove", "rm"} {
		if !isBotsShorthand(command) {
			t.Fatalf("isBotsShorthand(%q) = false, want true", command)
		}
	}
	for _, command := range []string{"run", "start", "stop", "restart", "unknown"} {
		if isBotsShorthand(command) {
			t.Fatalf("isBotsShorthand(%q) = true, want false", command)
		}
	}
}

func TestRunBotsListPrintsSecretSummary(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".lark-acp-bridge", "config.json")
	if err := config.AddBot(configPath, config.BotConfig{ID: "default", AppID: "cli_xxx"}, "super-secret"); err != nil {
		t.Fatalf("AddBot() error = %v", err)
	}

	var out bytes.Buffer
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = oldStdout
		_ = r.Close()
	})
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(&out, r)
		done <- err
	}()

	if err := runBotsCommand(configPath, []string{"list"}); err != nil {
		t.Fatalf("runBotsCommand(list) error = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close(stdout writer) error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("copy stdout error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "default") || !strings.Contains(got, "cli_xxx") || !strings.Contains(got, "file:$HOME/.lark-acp-bridge/secrets/default.appsecret") {
		t.Fatalf("list output = %q, want bot and file secret summary", got)
	}
	if strings.Contains(got, "super-secret") {
		t.Fatalf("list output leaked secret: %q", got)
	}
}

func TestRunBotsRegisterStoresReturnedSecret(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".lark-acp-bridge", "config.json")

	oldRegisterApp := registerApp
	var gotOpts *registration.Options
	registerApp = func(ctx context.Context, opts *registration.Options) (*registration.RegisterAppResult, error) {
		gotOpts = opts
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("registerApp ctx has no deadline")
		}
		opts.OnQRCode(&registration.QRCodeInfo{URL: "https://example.test/register", ExpireIn: 600})
		opts.OnStatusChange(&registration.StatusChangeInfo{Status: registration.StatusPolling})
		return &registration.RegisterAppResult{
			ClientID:     "cli_registered",
			ClientSecret: "registered-secret",
			UserInfo:     &registration.UserInfo{OpenID: "ou_owner", TenantBrand: "feishu"},
		}, nil
	}
	t.Cleanup(func() {
		registerApp = oldRegisterApp
	})

	out := captureStdout(t, func() {
		if err := runBotsCommand(configPath, []string{"register", "default", "--timeout=1m", "--app-name", "Bridge Bot", "--app-desc=ACP bridge"}); err != nil {
			t.Fatalf("runBotsCommand(register) error = %v", err)
		}
	})
	if !strings.Contains(out, "https://example.test/register") || !strings.Contains(out, "等待用户确认应用创建") {
		t.Fatalf("stdout = %q, want register URL and polling status", out)
	}
	if strings.Contains(out, "registered-secret") {
		t.Fatalf("stdout leaked secret: %q", out)
	}
	if gotOpts == nil || gotOpts.Source != "lark-acp-bridge" || !gotOpts.CreateOnly {
		t.Fatalf("opts = %+v, want source and create-only", gotOpts)
	}
	if gotOpts.AppPreset == nil || gotOpts.AppPreset.Name != "Bridge Bot" || gotOpts.AppPreset.Desc != "ACP bridge" {
		t.Fatalf("AppPreset = %+v, want preset name and desc", gotOpts.AppPreset)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("Load(config) error = %v", err)
	}
	if len(cfg.Bots) != 1 {
		t.Fatalf("bots = %+v, want one bot", cfg.Bots)
	}
	bot := cfg.Bots[0]
	if bot.ID != "default" || bot.AppID != "cli_registered" || len(bot.OwnerOpenIDs) != 1 || bot.OwnerOpenIDs[0] != "ou_owner" {
		t.Fatalf("bot = %+v, want registered bot with owner from user info", bot)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(config) error = %v", err)
	}
	if strings.Contains(string(raw), "registered-secret") {
		t.Fatalf("config leaked secret:\n%s", raw)
	}
	secretPath := filepath.Join(home, ".lark-acp-bridge", "secrets", "default.appsecret")
	secret, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("ReadFile(secret) error = %v", err)
	}
	if strings.Contains(string(secret), "registered-secret") {
		t.Fatalf("secret file leaked plaintext: %q", secret)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(secret)), "lark-acp-bridge-secret:v1:") {
		t.Fatalf("secret = %q, want encrypted secret", secret)
	}
	if _, err := os.Stat(filepath.Join(home, ".lark-acp-bridge", "secrets", "default.key")); err != nil {
		t.Fatalf("Stat(key) error = %v", err)
	}
}

func TestRunBotsRegisterRequiresPositiveTimeout(t *testing.T) {
	oldRegisterApp := registerApp
	registerApp = func(context.Context, *registration.Options) (*registration.RegisterAppResult, error) {
		t.Fatal("registerApp should not be called")
		return nil, nil
	}
	t.Cleanup(func() {
		registerApp = oldRegisterApp
	})

	err := runBotsCommand(filepath.Join(t.TempDir(), "config.json"), []string{"register", "default", "--timeout=0s"})
	if err == nil || !strings.Contains(err.Error(), "--timeout 必须大于 0") {
		t.Fatalf("runBotsCommand(register) error = %v, want timeout validation", err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	var out bytes.Buffer
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = oldStdout
		_ = r.Close()
	})
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(&out, r)
		done <- err
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("Close(stdout writer) error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("copy stdout error = %v", err)
	}
	return out.String()
}

func TestRegistrationStatusLine(t *testing.T) {
	tests := []struct {
		name string
		info *registration.StatusChangeInfo
		want string
	}{
		{name: "nil"},
		{name: "polling", info: &registration.StatusChangeInfo{Status: registration.StatusPolling}, want: "等待用户确认应用创建..."},
		{name: "slow down", info: &registration.StatusChangeInfo{Status: registration.StatusSlowDown, Interval: 15}, want: "服务端要求降低轮询频率，下次轮询间隔：15 秒"},
		{name: "domain switched", info: &registration.StatusChangeInfo{Status: registration.StatusDomainSwitched}, want: "已切换到 Lark 域名继续注册..."},
		{name: "unknown", info: &registration.StatusChangeInfo{Status: "custom"}, want: "注册状态：custom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := registrationStatusLine(tt.info); got != tt.want {
				t.Fatalf("registrationStatusLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSplitBotsRegisterArgsKeepsIDPositionIndependent(t *testing.T) {
	flagArgs, positional, err := splitBotsRegisterArgs([]string{"default", "--app-name", "Bridge Bot", "--timeout=1m", "--create-only=false", "--owner-open-ids", "ou_a,ou_b"})
	if err != nil {
		t.Fatalf("splitBotsRegisterArgs() error = %v", err)
	}
	if strings.Join(positional, ",") != "default" {
		t.Fatalf("positional = %+v, want default", positional)
	}
	if !containsAll(flagArgs, []string{"--app-name", "Bridge Bot", "--timeout=1m", "--create-only=false", "--owner-open-ids", "ou_a,ou_b"}) {
		t.Fatalf("flagArgs = %+v, missing expected flags", flagArgs)
	}
}

func containsAll(values []string, wants []string) bool {
	counts := make(map[string]int, len(values))
	for _, value := range values {
		counts[value]++
	}
	for _, want := range wants {
		if counts[want] == 0 {
			return false
		}
		counts[want]--
	}
	return true
}

func TestRunBotsRegisterTimeoutFlagAcceptsDuration(t *testing.T) {
	flagArgs, positional, err := splitBotsRegisterArgs([]string{"--timeout", (2 * time.Minute).String(), "default"})
	if err != nil {
		t.Fatalf("splitBotsRegisterArgs() error = %v", err)
	}
	if strings.Join(positional, ",") != "default" {
		t.Fatalf("positional = %+v, want default", positional)
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	timeout := fs.Duration("timeout", time.Minute, "")
	if err := fs.Parse(flagArgs); err != nil {
		t.Fatalf("Parse(flagArgs) error = %v", err)
	}
	if *timeout != 2*time.Minute {
		t.Fatalf("timeout = %v, want 2m", *timeout)
	}
}
