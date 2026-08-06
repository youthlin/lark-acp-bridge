package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

func TestTokenUsageStoreAppendPersistsAndReportsByAgentAndModel(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "token_usage.json")
	store := NewTokenUsageStore(storePath)
	now := time.Date(2026, 7, 31, 10, 20, 30, 0, time.Local)

	if _, err := store.Append(TokenUsageRecord{
		Timestamp: now.Add(-time.Hour),
		BotID:     "bot-a",
		AgentName: "traex",
		Model:     "gpt-5.5",
		Usage: acp.TokenUsage{
			TotalTokens:      1500,
			InputTokens:      1000,
			OutputTokens:     500,
			CachedReadTokens: 200,
		},
	}); err != nil {
		t.Fatalf("Append(first) error = %v", err)
	}
	if _, err := store.Append(TokenUsageRecord{
		Timestamp: now.Add(-30 * time.Minute),
		BotID:     "bot-a",
		AgentName: "traex",
		Model:     "gpt-5.5",
		Usage: acp.TokenUsage{
			InputTokens:  300,
			OutputTokens: 100,
		},
	}); err != nil {
		t.Fatalf("Append(second) error = %v", err)
	}
	if _, err := store.Append(TokenUsageRecord{
		Timestamp: now.AddDate(0, 0, -1),
		BotID:     "bot-a",
		AgentName: "codex",
		Model:     "gpt-5.6",
		Usage: acp.TokenUsage{
			TotalTokens:  999,
			InputTokens:  900,
			OutputTokens: 99,
		},
	}); err != nil {
		t.Fatalf("Append(old) error = %v", err)
	}

	data, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("ReadFile(token_usage.json) error = %v", err)
	}
	var file tokenUsageFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("Unmarshal(token_usage.json) error = %v", err)
	}
	if file.Version != 1 || len(file.Records) != 3 {
		t.Fatalf("file = %+v, want version 1 with three records", file)
	}

	reloaded := NewTokenUsageStore(storePath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	report := reloaded.Report(tokenUsagePeriodDay, now)
	if report.Total.Calls != 2 || report.Total.TotalTokens != 1900 || report.Total.InputTokens != 1300 || report.Total.OutputTokens != 600 {
		t.Fatalf("report total = %+v, want two same-day calls", report.Total)
	}
	if len(report.Aggregates) != 1 {
		t.Fatalf("aggregates = %+v, want one same-day group", report.Aggregates)
	}
	got := report.Aggregates[0]
	if got.AgentName != "traex" || got.Model != "gpt-5.5" || got.Calls != 2 || got.CachedReadTokens != 200 {
		t.Fatalf("aggregate = %+v, want traex/gpt-5.5 totals", got)
	}
}

func TestTokenUsageStoreLoadsLegacyPathAndWritesLocalPath(t *testing.T) {
	workspace := t.TempDir()
	legacyPath := filepath.Join(workspace, "token_usage.json")
	localPath := filepath.Join(workspace, ".local", "token_usage.json")
	now := time.Date(2026, 7, 31, 10, 20, 30, 0, time.Local)
	legacy := NewTokenUsageStore(legacyPath)
	if _, err := legacy.Append(TokenUsageRecord{
		Timestamp: now,
		BotID:     "bot-a",
		AgentName: "traex",
		Model:     "gpt-5.5",
		Usage: acp.TokenUsage{
			InputTokens:  100,
			OutputTokens: 50,
		},
	}); err != nil {
		t.Fatalf("Append(legacy) error = %v", err)
	}

	store := NewTokenUsageStoreWithFallback(localPath, legacyPath)
	if err := store.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if report := store.Report(tokenUsagePeriodDay, now); report.Total.Calls != 1 {
		t.Fatalf("report total = %+v, want one legacy record", report.Total)
	}
	if _, err := store.Append(TokenUsageRecord{
		Timestamp: now.Add(time.Minute),
		BotID:     "bot-a",
		AgentName: "traex",
		Model:     "gpt-5.5",
		Usage: acp.TokenUsage{
			InputTokens:  20,
			OutputTokens: 10,
		},
	}); err != nil {
		t.Fatalf("Append(local) error = %v", err)
	}
	if _, err := os.Stat(localPath); err != nil {
		t.Fatalf("local token usage file err = %v, want created", err)
	}
}

func TestTokenUsageStoreAppendLoadsExistingFileBeforeWriting(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "token_usage.json")
	now := time.Date(2026, 7, 31, 10, 20, 30, 0, time.Local)
	first := NewTokenUsageStore(storePath)
	if _, err := first.Append(TokenUsageRecord{
		Timestamp: now.Add(-time.Hour),
		BotID:     "bot-a",
		AgentName: "traex",
		Model:     "gpt-5.5",
		Usage: acp.TokenUsage{
			InputTokens:  100,
			OutputTokens: 50,
		},
	}); err != nil {
		t.Fatalf("Append(existing) error = %v", err)
	}

	restarted := NewTokenUsageStore(storePath)
	if _, err := restarted.Append(TokenUsageRecord{
		Timestamp: now,
		BotID:     "bot-a",
		AgentName: "traex",
		Model:     "gpt-5.5",
		Usage: acp.TokenUsage{
			InputTokens:  300,
			OutputTokens: 100,
		},
	}); err != nil {
		t.Fatalf("Append(after restart) error = %v", err)
	}

	reloaded := NewTokenUsageStore(storePath)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	report := reloaded.Report(tokenUsagePeriodDay, now)
	if report.Total.Calls != 2 || report.Total.InputTokens != 400 || report.Total.OutputTokens != 150 {
		t.Fatalf("report total = %+v, want both existing and new records", report.Total)
	}
}

func TestFormatTokenUsageReport(t *testing.T) {
	now := time.Date(2026, 7, 31, 10, 20, 30, 0, time.Local)
	start, end := tokenUsagePeriodRange(tokenUsagePeriodDay, now)
	text := formatTokenUsageReport(tokenUsageReport{
		Period: tokenUsagePeriodDay,
		Start:  start,
		End:    end,
		Total: tokenUsageAggregate{
			Calls:        2,
			TotalTokens:  1900,
			InputTokens:  1300,
			OutputTokens: 600,
		},
		Aggregates: []tokenUsageAggregate{
			{
				AgentName:        "traex",
				Model:            "gpt-5.5",
				Calls:            2,
				TotalTokens:      1900,
				InputTokens:      1300,
				OutputTokens:     600,
				CachedReadTokens: 200,
			},
		},
	})
	for _, want := range []string{"Token 用量报告（今日）", "总计：2 次，1.9K tokens", "traex / gpt-5.5", "缓存读 200"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report = %q, want %q", text, want)
		}
	}
}
