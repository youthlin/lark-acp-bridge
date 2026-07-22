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
