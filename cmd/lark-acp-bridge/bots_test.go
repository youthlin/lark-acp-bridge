package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	for _, command := range []string{"list", "add", "remove", "rm"} {
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
