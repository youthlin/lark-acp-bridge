//go:build unix

package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadWarnsForPermissiveConfigFile(t *testing.T) {
	var logs bytes.Buffer
	restoreDefaultLogger(t, &logs)

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"bots":[]}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	if _, err := Load(configPath); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	got := logs.String()
	if !strings.Contains(got, `"category":"config"`) || !strings.Contains(got, configPath) || !strings.Contains(got, `"mode":"-rw-r--r--"`) {
		t.Fatalf("logs = %s, want config permission warning with path and mode", got)
	}
}

func TestResolveSecretsWarnsForPermissiveSecretAndKeyFiles(t *testing.T) {
	var logs bytes.Buffer
	restoreDefaultLogger(t, &logs)

	secretPath := filepath.Join(t.TempDir(), "secrets", "default.appsecret")
	if _, _, err := writeEncryptedSecretFile(secretPath, "super-secret"); err != nil {
		t.Fatalf("writeEncryptedSecretFile() error = %v", err)
	}
	keyPath := secretKeyPath(secretPath)
	if err := os.Chmod(secretPath, 0o640); err != nil {
		t.Fatalf("Chmod(secret) error = %v", err)
	}
	if err := os.Chmod(keyPath, 0o660); err != nil {
		t.Fatalf("Chmod(key) error = %v", err)
	}

	cfg := Config{Bots: []BotConfig{{
		ID:        "default",
		AppID:     "cli_xxx",
		AppSecret: FileSecret(secretPath),
		Workspace: t.TempDir(),
	}}}
	if err := cfg.ResolveSecrets(); err != nil {
		t.Fatalf("ResolveSecrets() error = %v", err)
	}

	got := logs.String()
	for _, want := range []string{
		`"category":"secret"`,
		secretPath,
		`"mode":"-rw-r-----"`,
		`"category":"key"`,
		keyPath,
		`"mode":"-rw-rw----"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("logs = %s, want %s", got, want)
		}
	}
	if strings.Contains(got, "super-secret") {
		t.Fatalf("logs leaked secret content: %s", got)
	}
}

func TestResolveSecretsDoesNotWarnForStrictSecretFiles(t *testing.T) {
	var logs bytes.Buffer
	restoreDefaultLogger(t, &logs)

	secretPath := filepath.Join(t.TempDir(), "secrets", "default.appsecret")
	if _, _, err := writeEncryptedSecretFile(secretPath, "super-secret"); err != nil {
		t.Fatalf("writeEncryptedSecretFile() error = %v", err)
	}

	cfg := Config{Bots: []BotConfig{{
		ID:        "default",
		AppID:     "cli_xxx",
		AppSecret: FileSecret(secretPath),
		Workspace: t.TempDir(),
	}}}
	if err := cfg.ResolveSecrets(); err != nil {
		t.Fatalf("ResolveSecrets() error = %v", err)
	}
	if got := logs.String(); got != "" {
		t.Fatalf("logs = %s, want no warnings for 0600 files", got)
	}
}

func restoreDefaultLogger(t *testing.T, logs *bytes.Buffer) {
	t.Helper()
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(oldDefault) })
}
