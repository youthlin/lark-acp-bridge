package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestCtxHandlerAddsContextAttrsToJSONLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewCtxHandler(slog.NewJSONHandler(&buf, nil)))
	ctx := CtxAddAttr(context.Background(),
		slog.String("bot", "bot-a"),
		slog.String("message_id", "om_1"),
	)

	logger.InfoContext(ctx, "test log", "extra", "value")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal(log) error = %v, log=%s", err, buf.String())
	}
	for key, want := range map[string]string{
		"msg":        "test log",
		"extra":      "value",
		"bot":        "bot-a",
		"message_id": "om_1",
	} {
		if got[key] != want {
			t.Fatalf("log[%s] = %#v, want %q; full log=%v", key, got[key], want, got)
		}
	}
}

func TestLevelFromEnv(t *testing.T) {
	t.Setenv(LevelEnv, "debug")
	if got := LevelFromEnv(); got.Level() != slog.LevelDebug {
		t.Fatalf("LevelFromEnv(debug) = %v, want debug", got.Level())
	}

	t.Setenv(LevelEnv, "error")
	if got := LevelFromEnv(); got.Level() != slog.LevelError {
		t.Fatalf("LevelFromEnv(error) = %v, want error", got.Level())
	}

	t.Setenv(LevelEnv, "")
	if got := LevelFromEnv(); got.Level() != slog.LevelInfo {
		t.Fatalf("LevelFromEnv(empty) = %v, want info", got.Level())
	}
}

func TestSetDebug(t *testing.T) {
	orig := ProgramLevel().Level()
	t.Cleanup(func() {
		ProgramLevel().Set(orig)
	})

	SetDebug(true)
	if !DebugEnabled() {
		t.Fatal("DebugEnabled() = false, want true after SetDebug(true)")
	}
	if got := ProgramLevel().Level(); got != slog.LevelDebug {
		t.Fatalf("ProgramLevel() = %v, want debug", got)
	}

	SetDebug(false)
	if DebugEnabled() {
		t.Fatal("DebugEnabled() = true, want false after SetDebug(false)")
	}
	if got := ProgramLevel().Level(); got != slog.LevelInfo {
		t.Fatalf("ProgramLevel() = %v, want info", got)
	}
}

func TestSetDebugOffRestoresEnvLevel(t *testing.T) {
	orig := ProgramLevel().Level()
	t.Cleanup(func() {
		ProgramLevel().Set(orig)
	})
	t.Setenv(LevelEnv, "warn")

	SetDebug(true)
	SetDebug(false)

	if got := ProgramLevel().Level(); got != slog.LevelWarn {
		t.Fatalf("ProgramLevel() = %v, want warn", got)
	}
}
