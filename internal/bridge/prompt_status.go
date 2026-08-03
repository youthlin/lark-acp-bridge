package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

type promptStatusState string

const (
	promptStatusRunning   promptStatusState = "running"
	promptStatusCompleted promptStatusState = "completed"
	promptStatusCancelled promptStatusState = "cancelled"
	promptStatusFailed    promptStatusState = "failed"
	promptStatusStopped   promptStatusState = "stopped"
)

type promptStatusBar struct {
	state       promptStatusState
	stopReason  string
	prefix      string
	startedAt   time.Time
	endedAt     time.Time
	input       int64
	cachedInput int64
	output      int64
	Context     acp.ContextWindowUsage
}

func (s *promptStatusBar) applyPromptResult(result acp.PromptResult) {
	if tokenUsage := result.Meta.TraeTokenUsage; tokenUsage != nil {
		if result.Usage.InputTokens <= 0 && tokenUsage.TurnDisplay.InputTokens > 0 {
			s.input = tokenUsage.TurnDisplay.InputTokens
		}
		if result.Usage.OutputTokens <= 0 && tokenUsage.TurnDisplay.OutputTokens > 0 {
			s.output = tokenUsage.TurnDisplay.OutputTokens
		}
		if s.Context.Used <= 0 && s.Context.Size <= 0 && (tokenUsage.ContextWindow.Used > 0 || tokenUsage.ContextWindow.Size > 0) {
			s.Context = tokenUsage.ContextWindow
		}
	}
	if result.Usage.InputTokens > 0 {
		s.input = result.Usage.InputTokens
	}
	if result.Usage.CachedReadTokens > 0 {
		s.cachedInput = result.Usage.CachedReadTokens
	}
	if result.Usage.OutputTokens > 0 {
		s.output = result.Usage.OutputTokens
	}
}

func (s promptStatusBar) text() string {
	return s.textAt(time.Now())
}

func (s promptStatusBar) textAt(now time.Time) string {
	label := promptStatusStateLabel(s.state, s.stopReason, s.elapsed(now))
	if prefix := strings.TrimSpace(s.prefix); prefix != "" {
		label = prefix + " " + label
	}
	parts := []string{label}
	if tokenUsage := formatPromptTokenUsage(s.input, s.cachedInput, s.output); tokenUsage != "" {
		parts = append(parts, tokenUsage)
	}
	if s.Context.Used > 0 || s.Context.Size > 0 {
		parts = append(parts, formatContextUsage(s.Context))
	}
	return strings.Join(parts, " | ")
}

func (s promptStatusBar) elapsed(now time.Time) time.Duration {
	if s.startedAt.IsZero() {
		return 0
	}
	end := now
	if !s.endedAt.IsZero() {
		end = s.endedAt
	}
	if end.Before(s.startedAt) {
		return 0
	}
	return end.Sub(s.startedAt)
}

func (s *promptStatusBar) finish(state promptStatusState, stopReason string, endedAt time.Time) {
	s.state = state
	s.stopReason = strings.TrimSpace(stopReason)
	if s.endedAt.IsZero() {
		s.endedAt = endedAt
	}
}

func promptStatusFromStopReason(stopReason string) promptStatusState {
	switch strings.TrimSpace(stopReason) {
	case "", "end_turn":
		return promptStatusCompleted
	case "cancelled":
		return promptStatusCancelled
	default:
		return promptStatusStopped
	}
}

func promptStatusStateLabel(state promptStatusState, stopReason string, elapsed time.Duration) string {
	duration := formatPromptElapsedDuration(elapsed)
	switch state {
	case promptStatusCompleted:
		return "✅ " + duration
	case promptStatusCancelled:
		return "🚫 " + duration
	case promptStatusFailed:
		return "❌ " + duration
	case promptStatusStopped:
		if stopReason = strings.TrimSpace(stopReason); stopReason != "" {
			return fmt.Sprintf("⏹️ %v %v", duration, stopReason)
		}
		return "⏹️ " + duration
	default:
		return "⏳ " + duration
	}
}

func formatPromptElapsedDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalSeconds := int64(d.Truncate(time.Second) / time.Second)
	hours := totalSeconds / 3600
	minutes := totalSeconds % 3600 / 60
	seconds := totalSeconds % 60
	if hours > 0 {
		parts := []string{fmt.Sprintf("%dh", hours)}
		if minutes > 0 {
			parts = append(parts, fmt.Sprintf("%dm", minutes))
		}
		if seconds > 0 {
			parts = append(parts, fmt.Sprintf("%ds", seconds))
		}
		return strings.Join(parts, "")
	}
	if minutes > 0 {
		if seconds > 0 {
			return fmt.Sprintf("%dm%ds", minutes, seconds)
		}
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%ds", seconds)
}

func formatContextUsage(usage acp.ContextWindowUsage) string {
	if usage.Used <= 0 && usage.Size <= 0 {
		return ""
	}
	if usage.Size <= 0 {
		return formatContextTokenCount(usage.Used)
	}
	if usage.Used <= 0 {
		return formatContextTokenCount(usage.Size)
	}
	return fmt.Sprintf("%v/%v%v",
		formatContextTokenCount(usage.Used),
		formatContextTokenCount(usage.Size),
		formatPercent(usage.Used, usage.Size))
}

func formatTokenCountWithUnit(value int64) string {
	if value <= 0 {
		return "0"
	}
	if value >= 1_000_000 {
		return fmt.Sprintf("%sM", formatDecimal(float64(value)/1_000_000, 1))
	}
	if value >= 1000 {
		return fmt.Sprintf("%sK", formatDecimal(float64(value)/1000, 1))
	}
	return strconv.FormatInt(value, 10)
}

func formatContextTokenCount(value int64) string {
	if value <= 0 {
		return "0K"
	}
	if value >= 1_000_000 {
		return fmt.Sprintf("%dM", value/1_000_000)
	}
	return fmt.Sprintf("%dK", value/1000)
}

func formatTokenCount(value int64) string {
	if value <= 0 {
		return "0"
	}
	return formatTokenCountWithUnit(value)
}

func formatPromptTokenUsage(input, cachedInput, output int64) string {
	items := make([]string, 0, 2)
	if input > 0 {
		items = append(items, formatTokenCount(input)+formatPercent(cachedInput, input))
	}
	if output > 0 {
		items = append(items, formatTokenCount(output))
	}
	return strings.Join(items, ", ")
}

func formatPromptResultDetail(result acp.PromptResult) string {
	raw := append(json.RawMessage(nil), result.Raw...)
	if len(raw) == 0 || string(raw) == "null" {
		var err error
		raw, err = json.Marshal(result)
		if err != nil {
			return ""
		}
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		pretty.Write(raw)
	}
	text := pretty.String()
	if strings.TrimSpace(text) == "" {
		return ""
	}
	fence := markdownCodeFence(text)
	return fence + "json\n" + text + "\n" + fence
}

func promptResultHasUsageDetail(result acp.PromptResult) bool {
	if promptTokenUsagePresent(result.Usage) || result.Meta.TraeTokenUsage != nil {
		return true
	}
	raw := bytes.TrimSpace(result.Raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	var payload struct {
		Usage json.RawMessage `json:"usage"`
		Meta  json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false
	}
	return rawJSONObjectHasFields(payload.Usage) || rawJSONObjectHasFields(payload.Meta)
}

func promptTokenUsagePresent(usage acp.TokenUsage) bool {
	return usage.TotalTokens > 0 ||
		usage.InputTokens > 0 ||
		usage.OutputTokens > 0 ||
		usage.ThoughtTokens > 0 ||
		usage.CachedReadTokens > 0 ||
		usage.CachedWriteTokens > 0
}

func rawJSONObjectHasFields(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	return len(obj) > 0
}

func markdownCodeFence(text string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence
}

// formatPercent 返回 numerator/denominator 的百分比,形如 "(27%)";
// 任一值 <=0 或取整后为 0 时返回空串,超过 100 截断为 100。
func formatPercent(numerator, denominator int64) string {
	if numerator <= 0 || denominator <= 0 {
		return ""
	}
	percent := int64(math.Round(float64(numerator) / float64(denominator) * 100))
	if percent <= 0 {
		return ""
	}
	if percent > 100 {
		percent = 100
	}
	return fmt.Sprintf("(%d%%)", percent)
}

func formatDecimal(value float64, precision int) string {
	text := strconv.FormatFloat(value, 'f', precision, 64)
	text = strings.TrimRight(text, "0")
	text = strings.TrimRight(text, ".")
	if text == "" {
		return "0"
	}
	return text
}
