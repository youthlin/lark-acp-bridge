package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

type tokenUsagePeriod string

const (
	tokenUsagePeriodDay   tokenUsagePeriod = "day"
	tokenUsagePeriodWeek  tokenUsagePeriod = "week"
	tokenUsagePeriodMonth tokenUsagePeriod = "month"
	tokenUsagePeriodYear  tokenUsagePeriod = "year"
)

const (
	tokenUsageRetention  = 400 * 24 * time.Hour
	tokenUsageMaxRecords = 100000
)

type TokenUsageRecord struct {
	Timestamp    time.Time      `json:"timestamp"`
	BotID        string         `json:"bot_id,omitempty"`
	Source       string         `json:"source,omitempty"`
	MainID       string         `json:"main_id,omitempty"`
	SubID        string         `json:"sub_id,omitempty"`
	SessionID    string         `json:"session_id,omitempty"`
	AgentName    string         `json:"agent_name"`
	Model        string         `json:"model,omitempty"`
	Cwd          string         `json:"cwd,omitempty"`
	StopReason   string         `json:"stop_reason,omitempty"`
	InputTokens  int64          `json:"input_tokens,omitempty"`
	OutputTokens int64          `json:"output_tokens,omitempty"`
	Usage        acp.TokenUsage `json:"usage"`
}

type TokenUsageStore struct {
	path         string
	fallbackPath string
	mu           sync.Mutex
	records      []TokenUsageRecord
	loaded       bool
}

type tokenUsageStoreSnapshot struct {
	records []TokenUsageRecord
}

type tokenUsageAggregate struct {
	AgentName         string
	Model             string
	Calls             int
	TotalTokens       int64
	InputTokens       int64
	OutputTokens      int64
	ThoughtTokens     int64
	CachedReadTokens  int64
	CachedWriteTokens int64
}

type tokenUsageReport struct {
	Period     tokenUsagePeriod
	Start      time.Time
	End        time.Time
	Aggregates []tokenUsageAggregate
	Total      tokenUsageAggregate
}

func NewTokenUsageStore(path string) *TokenUsageStore {
	return &TokenUsageStore{path: path}
}

func NewTokenUsageStoreWithFallback(path string, fallbackPath string) *TokenUsageStore {
	store := NewTokenUsageStore(path)
	if strings.TrimSpace(fallbackPath) != strings.TrimSpace(path) {
		store.fallbackPath = fallbackPath
	}
	return store
}

func (s *TokenUsageStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *TokenUsageStore) Append(record TokenUsageRecord) (TokenUsageRecord, error) {
	record = normalizeTokenUsageRecord(record)
	if !validTokenUsageRecord(record) {
		return TokenUsageRecord{}, fmt.Errorf("token 用量记录字段不完整")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		if err := s.loadLocked(); err != nil {
			return TokenUsageRecord{}, err
		}
	}

	snapshot := s.snapshotLocked()
	s.records = append(s.records, record)
	s.records = pruneTokenUsageRecords(s.records, time.Now(), tokenUsageRetention, tokenUsageMaxRecords)
	sortTokenUsageRecords(s.records)
	if err := s.writeOrRestoreLocked(snapshot); err != nil {
		return TokenUsageRecord{}, err
	}
	return record, nil
}

func (s *TokenUsageStore) Report(period tokenUsagePeriod, now time.Time) tokenUsageReport {
	s.mu.Lock()
	defer s.mu.Unlock()

	if now.IsZero() {
		now = time.Now()
	}
	start, end := tokenUsagePeriodRange(period, now)
	groups := make(map[string]*tokenUsageAggregate)
	total := tokenUsageAggregate{}
	for _, record := range s.records {
		if record.Timestamp.Before(start) || !record.Timestamp.Before(end) {
			continue
		}
		agentName := firstNonEmpty(record.AgentName, "(未知 agent)")
		model := firstNonEmpty(record.Model, "(未知模型)")
		key := agentName + "\x00" + model
		aggregate := groups[key]
		if aggregate == nil {
			aggregate = &tokenUsageAggregate{AgentName: agentName, Model: model}
			groups[key] = aggregate
		}
		addTokenUsageRecord(aggregate, record)
		addTokenUsageRecord(&total, record)
	}
	aggregates := make([]tokenUsageAggregate, 0, len(groups))
	for _, aggregate := range groups {
		aggregates = append(aggregates, *aggregate)
	}
	sort.Slice(aggregates, func(i, j int) bool {
		if aggregates[i].TotalTokens != aggregates[j].TotalTokens {
			return aggregates[i].TotalTokens > aggregates[j].TotalTokens
		}
		if aggregates[i].AgentName != aggregates[j].AgentName {
			return aggregates[i].AgentName < aggregates[j].AgentName
		}
		return aggregates[i].Model < aggregates[j].Model
	})
	return tokenUsageReport{
		Period:     period,
		Start:      start,
		End:        end,
		Aggregates: aggregates,
		Total:      total,
	}
}

func (s *TokenUsageStore) loadLocked() error {
	data, err := os.ReadFile(s.readPathLocked())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.records = nil
			s.loaded = true
			return nil
		}
		return fmt.Errorf("读取 token 用量文件: %w", err)
	}
	var file tokenUsageFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("解析 token 用量文件: %w", err)
	}
	records := make([]TokenUsageRecord, 0, len(file.Records))
	for _, record := range file.Records {
		record = normalizeTokenUsageRecord(record)
		if validTokenUsageRecord(record) {
			records = append(records, record)
		}
	}
	originalLen := len(records)
	records = pruneTokenUsageRecords(records, time.Now(), tokenUsageRetention, tokenUsageMaxRecords)
	sortTokenUsageRecords(records)
	s.records = records
	s.loaded = true
	if originalLen != len(records) {
		if err := s.writeLocked(); err != nil {
			return err
		}
	}
	return nil
}

func (s *TokenUsageStore) readPathLocked() string {
	path := strings.TrimSpace(s.path)
	if path == "" {
		return s.path
	}
	if _, err := os.Stat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return path
	}
	fallbackPath := strings.TrimSpace(s.fallbackPath)
	if fallbackPath == "" || fallbackPath == path {
		return path
	}
	if _, err := os.Stat(fallbackPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return fallbackPath
	}
	return path
}

func (s *TokenUsageStore) snapshotLocked() tokenUsageStoreSnapshot {
	return tokenUsageStoreSnapshot{records: append([]TokenUsageRecord(nil), s.records...)}
}

func (s *TokenUsageStore) writeOrRestoreLocked(snapshot tokenUsageStoreSnapshot) error {
	if err := s.writeLocked(); err != nil {
		s.records = snapshot.records
		return err
	}
	return nil
}

func (s *TokenUsageStore) writeLocked() error {
	file := tokenUsageFile{
		Version: 1,
		Records: append([]TokenUsageRecord(nil), s.records...),
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("编码 token 用量文件: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("创建 token 用量目录: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时 token 用量文件: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换 token 用量文件: %w", err)
	}
	return nil
}

type tokenUsageFile struct {
	Version int                `json:"version"`
	Records []TokenUsageRecord `json:"records"`
}

func newTokenUsageRecord(session Session, result acp.PromptResult) TokenUsageRecord {
	usage := promptResultTokenUsage(result)
	key := normalizeSessionKey(session.Key)
	return normalizeTokenUsageRecord(TokenUsageRecord{
		Timestamp:    time.Now(),
		BotID:        key.BotID,
		Source:       sessionKeySource(key),
		MainID:       sessionKeyMainID(key),
		SubID:        key.SubID,
		SessionID:    session.ACPSessionID,
		AgentName:    session.AgentName,
		Model:        currentModelDisplay(session),
		Cwd:          session.Cwd,
		StopReason:   result.StopReason,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		Usage:        usage,
	})
}

func promptResultTokenUsage(result acp.PromptResult) acp.TokenUsage {
	usage := result.Usage
	if tokenUsage := result.Meta.TraeTokenUsage; tokenUsage != nil {
		if usage.InputTokens <= 0 && tokenUsage.TurnDisplay.InputTokens > 0 {
			usage.InputTokens = tokenUsage.TurnDisplay.InputTokens
		}
		if usage.OutputTokens <= 0 && tokenUsage.TurnDisplay.OutputTokens > 0 {
			usage.OutputTokens = tokenUsage.TurnDisplay.OutputTokens
		}
		if usage.TotalTokens <= 0 && tokenUsage.TurnDisplay.TotalTokens > 0 {
			usage.TotalTokens = tokenUsage.TurnDisplay.TotalTokens
		}
		if usage.ThoughtTokens <= 0 && tokenUsage.TurnDisplay.ThoughtTokens > 0 {
			usage.ThoughtTokens = tokenUsage.TurnDisplay.ThoughtTokens
		}
		if usage.CachedReadTokens <= 0 && tokenUsage.TurnDisplay.CachedReadTokens > 0 {
			usage.CachedReadTokens = tokenUsage.TurnDisplay.CachedReadTokens
		}
		if usage.CachedWriteTokens <= 0 && tokenUsage.TurnDisplay.CachedWriteTokens > 0 {
			usage.CachedWriteTokens = tokenUsage.TurnDisplay.CachedWriteTokens
		}
	}
	if usage.TotalTokens <= 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens + usage.ThoughtTokens
	}
	return usage
}

func normalizeTokenUsageRecord(record TokenUsageRecord) TokenUsageRecord {
	record.BotID = strings.TrimSpace(record.BotID)
	record.Source = strings.TrimSpace(record.Source)
	record.MainID = strings.TrimSpace(record.MainID)
	record.SubID = strings.TrimSpace(record.SubID)
	record.SessionID = strings.TrimSpace(record.SessionID)
	record.AgentName = strings.TrimSpace(record.AgentName)
	record.Model = strings.TrimSpace(record.Model)
	record.Cwd = strings.TrimSpace(record.Cwd)
	record.StopReason = strings.TrimSpace(record.StopReason)
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now()
	}
	if record.Source == "" {
		record.Source = sessionSourceIM
	}
	if record.InputTokens <= 0 {
		record.InputTokens = record.Usage.InputTokens
	}
	if record.OutputTokens <= 0 {
		record.OutputTokens = record.Usage.OutputTokens
	}
	if record.Usage.TotalTokens <= 0 {
		record.Usage.TotalTokens = record.Usage.InputTokens + record.Usage.OutputTokens + record.Usage.ThoughtTokens
	}
	return record
}

func validTokenUsageRecord(record TokenUsageRecord) bool {
	return strings.TrimSpace(record.AgentName) != "" && promptTokenUsagePresent(record.Usage)
}

func addTokenUsageRecord(aggregate *tokenUsageAggregate, record TokenUsageRecord) {
	aggregate.Calls++
	aggregate.TotalTokens += record.Usage.TotalTokens
	aggregate.InputTokens += record.Usage.InputTokens
	aggregate.OutputTokens += record.Usage.OutputTokens
	aggregate.ThoughtTokens += record.Usage.ThoughtTokens
	aggregate.CachedReadTokens += record.Usage.CachedReadTokens
	aggregate.CachedWriteTokens += record.Usage.CachedWriteTokens
}

func sortTokenUsageRecords(records []TokenUsageRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		return records[i].Timestamp.Before(records[j].Timestamp)
	})
}

func pruneTokenUsageRecords(records []TokenUsageRecord, now time.Time, retention time.Duration, maxRecords int) []TokenUsageRecord {
	if len(records) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	pruned := records[:0]
	if retention > 0 {
		cutoff := now.Add(-retention)
		for _, record := range records {
			if record.Timestamp.IsZero() || record.Timestamp.Before(cutoff) {
				continue
			}
			pruned = append(pruned, record)
		}
	} else {
		pruned = records
	}
	sortTokenUsageRecords(pruned)
	if maxRecords > 0 && len(pruned) > maxRecords {
		pruned = pruned[len(pruned)-maxRecords:]
	}
	out := make([]TokenUsageRecord, len(pruned))
	copy(out, pruned)
	return out
}

func tokenUsagePeriodRange(period tokenUsagePeriod, now time.Time) (time.Time, time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	switch period {
	case tokenUsagePeriodWeek:
		year, month, day := now.Date()
		dayStart := time.Date(year, month, day, 0, 0, 0, 0, now.Location())
		weekday := int(dayStart.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		start := dayStart.AddDate(0, 0, -(weekday - 1))
		return start, start.AddDate(0, 0, 7)
	case tokenUsagePeriodMonth:
		year, month, _ := now.Date()
		start := time.Date(year, month, 1, 0, 0, 0, 0, now.Location())
		return start, start.AddDate(0, 1, 0)
	case tokenUsagePeriodYear:
		year, _, _ := now.Date()
		start := time.Date(year, 1, 1, 0, 0, 0, 0, now.Location())
		return start, start.AddDate(1, 0, 0)
	default:
		year, month, day := now.Date()
		start := time.Date(year, month, day, 0, 0, 0, 0, now.Location())
		return start, start.AddDate(0, 0, 1)
	}
}

func parseTokenUsagePeriod(value string) (tokenUsagePeriod, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "day", "daily", "d", "日", "天":
		return tokenUsagePeriodDay, true
	case "week", "weekly", "w", "周", "星期":
		return tokenUsagePeriodWeek, true
	case "month", "monthly", "m", "月":
		return tokenUsagePeriodMonth, true
	case "year", "yearly", "y", "年":
		return tokenUsagePeriodYear, true
	default:
		return "", false
	}
}

func tokenUsagePeriodLabel(period tokenUsagePeriod) string {
	switch period {
	case tokenUsagePeriodWeek:
		return "本周"
	case tokenUsagePeriodMonth:
		return "本月"
	case tokenUsagePeriodYear:
		return "今年"
	default:
		return "今日"
	}
}

func (s *Service) tokenUsageStoreForBotID(botID string) *TokenUsageStore {
	if s.usageStores == nil {
		return nil
	}
	if store := s.usageStores[strings.TrimSpace(botID)]; store != nil {
		return store
	}
	return s.usageStores[""]
}

func (s *Service) recordPromptTokenUsage(ctx context.Context, botID string, session Session, result acp.PromptResult) {
	record := newTokenUsageRecord(session, result)
	if strings.TrimSpace(record.BotID) == "" {
		record.BotID = strings.TrimSpace(botID)
	}
	if !validTokenUsageRecord(record) {
		return
	}
	store := s.tokenUsageStoreForBotID(record.BotID)
	if store == nil {
		return
	}
	if _, err := store.Append(record); err != nil {
		slog.WarnContext(ctx, "保存 token 用量记录失败", "agent", record.AgentName, "model", record.Model, "session", record.SessionID, "错误", err)
	}
}

func (s *Service) handleUsageCommand(text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) > 2 {
		return usageCommandUsage()
	}
	period := tokenUsagePeriodDay
	if len(fields) == 2 {
		parsed, ok := parseTokenUsagePeriod(fields[1])
		if !ok {
			return usageCommandUsage()
		}
		period = parsed
	}
	store := s.tokenUsageStoreForBotID(msg.BotID)
	if store == nil {
		return "当前 bot workspace 未初始化，无法读取 token 用量。"
	}
	if err := store.Load(); err != nil {
		return "读取 token 用量失败：" + err.Error()
	}
	return formatTokenUsageReport(store.Report(period, time.Now()))
}

func usageCommandUsage() string {
	return strings.Join([]string{
		"请使用 /usage [day|week|month|year]。",
		"不指定周期时默认查看今日 token 用量。",
	}, "\n")
}

func formatTokenUsageReport(report tokenUsageReport) string {
	lines := []string{
		fmt.Sprintf("Token 用量报告（%s）", tokenUsagePeriodLabel(report.Period)),
		fmt.Sprintf("范围：%s - %s", formatTokenUsageReportTime(report.Start), formatTokenUsageReportTime(report.End.Add(-time.Nanosecond))),
	}
	if report.Total.Calls == 0 {
		lines = append(lines, "当前周期暂无 token 用量记录。")
		return strings.Join(lines, "\n")
	}
	lines = append(lines,
		fmt.Sprintf("总计：%d 次，%s tokens（输入 %s，输出 %s）",
			report.Total.Calls,
			formatTokenCount(report.Total.TotalTokens),
			formatTokenCount(report.Total.InputTokens),
			formatTokenCount(report.Total.OutputTokens),
		),
		"",
		"按 agent / model：",
	)
	for i, aggregate := range report.Aggregates {
		lines = append(lines, fmt.Sprintf("%d. %s / %s：%d 次，%s tokens（输入 %s，输出 %s%s）",
			i+1,
			aggregate.AgentName,
			aggregate.Model,
			aggregate.Calls,
			formatTokenCount(aggregate.TotalTokens),
			formatTokenCount(aggregate.InputTokens),
			formatTokenCount(aggregate.OutputTokens),
			formatTokenUsageExtras(aggregate),
		))
	}
	return strings.Join(lines, "\n")
}

func formatTokenUsageExtras(aggregate tokenUsageAggregate) string {
	parts := make([]string, 0, 3)
	if aggregate.ThoughtTokens > 0 {
		parts = append(parts, "思考 "+formatTokenCount(aggregate.ThoughtTokens))
	}
	if aggregate.CachedReadTokens > 0 {
		parts = append(parts, "缓存读 "+formatTokenCount(aggregate.CachedReadTokens))
	}
	if aggregate.CachedWriteTokens > 0 {
		parts = append(parts, "缓存写 "+formatTokenCount(aggregate.CachedWriteTokens))
	}
	if len(parts) == 0 {
		return ""
	}
	return "，" + strings.Join(parts, "，")
}

func formatTokenUsageReportTime(value time.Time) string {
	return value.Format("2006-01-02 15:04:05")
}
