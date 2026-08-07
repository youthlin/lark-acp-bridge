package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const (
	defaultAutoCompactPct = 80
	minAutoCompactPct     = 1
	maxAutoCompactPct     = 99
)

func (s *Service) handleCompactCommand(ctx context.Context, text string, msg feishu.Message) string {
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	session, ok := s.findSession(msg)
	if !ok || strings.TrimSpace(session.ACPSessionID) == "" {
		return "当前会话还没有 ACP session，发送普通文本或 /new 后再配置自动 compact。"
	}
	fields := strings.Fields(text)
	if len(fields) == 1 || len(fields) == 2 && strings.EqualFold(fields[1], "status") {
		return formatCompactStatus(session)
	}
	switch strings.ToLower(strings.TrimSpace(fields[1])) {
	case "on":
		if len(fields) > 3 {
			return compactCommandUsage()
		}
		percent := defaultAutoCompactPct
		if len(fields) >= 3 {
			parsed, err := parseCompactPercent(fields[2])
			if err != nil {
				return err.Error()
			}
			percent = parsed
		}
		updated, err := s.updateCompactConfig(ctx, store, session, true, percent)
		if err != nil {
			return "保存 compact 配置失败：" + err.Error()
		}
		return "已开启自动 compact，阈值：" + strconv.Itoa(updated.AutoCompactPct) + "%。\n" + formatCompactStatus(updated)
	case "off":
		if len(fields) != 2 {
			return compactCommandUsage()
		}
		updated, err := s.updateCompactConfig(ctx, store, session, false, 0)
		if err != nil {
			return "保存 compact 配置失败：" + err.Error()
		}
		return "已关闭自动 compact。\n" + formatCompactStatus(updated)
	default:
		return compactCommandUsage()
	}
}

func compactCommandUsage() string {
	return "请使用 /compact、/compact on 80% 或 /compact off。手动执行 agent compact 请使用 //compact。"
}

func parseCompactPercent(value string) (int, error) {
	value = strings.TrimSpace(strings.TrimSuffix(value, "%"))
	if value == "" {
		return 0, fmt.Errorf("compact 阈值不能为空，请使用 /compact on 80%%。")
	}
	percent, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("compact 阈值必须是百分比，例如 /compact on 80%%。")
	}
	if percent < minAutoCompactPct || percent > maxAutoCompactPct {
		return 0, fmt.Errorf("compact 阈值必须在 %d%%-%d%% 之间。", minAutoCompactPct, maxAutoCompactPct)
	}
	return percent, nil
}

func (s *Service) updateCompactConfig(ctx context.Context, store *SessionStore, session Session, enabled bool, percent int) (Session, error) {
	session = normalizeSessionForStore(session)
	err := store.UpdateCurrentSession(session.Key, session.ACPSessionID, func(current *Session) bool {
		if current.AutoCompact == enabled && current.AutoCompactPct == percent && (!enabled || !current.AutoCompacting) {
			return false
		}
		current.AutoCompact = enabled
		current.AutoCompactPct = percent
		if !enabled {
			current.AutoCompacting = false
		}
		return true
	})
	if err != nil {
		slog.WarnContext(ctx, "保存 compact 配置失败", "session", session.ACPSessionID, "错误", err)
		return Session{}, err
	}
	if latest, ok := store.Get(session.Key); ok {
		return latest, nil
	}
	return session, nil
}

func formatCompactStatus(session Session) string {
	state := "关闭"
	if session.AutoCompact {
		state = "开启，阈值 " + strconv.Itoa(session.AutoCompactPct) + "%"
	}
	lines := []string{"自动 compact：" + state}
	if session.ContextWindow != nil {
		lines = append(lines, "上下文窗口："+formatContextUsage(*session.ContextWindow))
		if percent := contextUsagePercent(*session.ContextWindow); percent > 0 {
			lines[len(lines)-1] += fmt.Sprintf("（%.1f%%）", percent)
		}
	}
	if session.AutoCompacting {
		lines = append(lines, "状态：正在 compact")
	}
	if session.LastAutoCompactAt != nil && !session.LastAutoCompactAt.IsZero() {
		lines = append(lines, "最近自动 compact："+session.LastAutoCompactAt.Format(time.RFC3339))
	}
	return strings.Join(lines, "\n")
}

func formatCompactStatusInline(session Session) string {
	parts := make([]string, 0, 4)
	if session.AutoCompact {
		parts = append(parts, "开启 "+strconv.Itoa(session.AutoCompactPct)+"%")
	} else {
		parts = append(parts, "关闭")
	}
	if session.ContextWindow != nil {
		usage := *session.ContextWindow
		contextText := formatContextUsage(usage)
		if percent := contextUsagePercent(usage); percent > 0 {
			contextText += fmt.Sprintf(" %.1f%%", percent)
		}
		parts = append(parts, contextText)
	}
	if session.AutoCompacting {
		parts = append(parts, "正在 compact")
	}
	if session.LastAutoCompactAt != nil && !session.LastAutoCompactAt.IsZero() {
		parts = append(parts, "最近 "+session.LastAutoCompactAt.Format(time.RFC3339))
	}
	return strings.Join(parts, "，")
}

func normalizeContextWindowUsagePtr(usage *acp.ContextWindowUsage) *acp.ContextWindowUsage {
	if usage == nil {
		return nil
	}
	normalized := normalizeContextWindowUsage(*usage)
	if normalized.Used <= 0 && normalized.Size <= 0 {
		return nil
	}
	return &normalized
}

func normalizeContextWindowUsage(usage acp.ContextWindowUsage) acp.ContextWindowUsage {
	if usage.Used < 0 {
		usage.Used = 0
	}
	if usage.Size < 0 {
		usage.Size = 0
	}
	if usage.AutoCompactTokenLimit < 0 {
		usage.AutoCompactTokenLimit = 0
	}
	return usage
}

func contextWindowFromPromptResult(result acp.PromptResult) (acp.ContextWindowUsage, bool) {
	if tokenUsage := result.Meta.TraeTokenUsage; tokenUsage != nil {
		usage := normalizeContextWindowUsage(tokenUsage.ContextWindow)
		if usage.Used > 0 || usage.Size > 0 {
			return usage, true
		}
	}
	return acp.ContextWindowUsage{}, false
}

func contextWindowFromUpdate(update acp.SessionUpdate) (acp.ContextWindowUsage, bool) {
	if update.Used <= 0 && update.Size <= 0 {
		return acp.ContextWindowUsage{}, false
	}
	return normalizeContextWindowUsage(acp.ContextWindowUsage{Used: update.Used, Size: update.Size}), true
}

func contextUsagePercent(usage acp.ContextWindowUsage) float64 {
	if usage.Used <= 0 || usage.Size <= 0 {
		return 0
	}
	return float64(usage.Used) * 100 / float64(usage.Size)
}

func contextWindowUsageDropped(previous *acp.ContextWindowUsage, next acp.ContextWindowUsage) bool {
	if previous == nil {
		return false
	}
	prev := normalizeContextWindowUsage(*previous)
	next = normalizeContextWindowUsage(next)
	if prev.Used <= 0 || next.Used <= 0 {
		return false
	}
	if prev.Size > 0 && next.Size > 0 && prev.Size != next.Size {
		return false
	}
	return next.Used < prev.Used
}

func (s *Service) recordSessionContextWindow(ctx context.Context, msg feishu.Message, session Session, usage acp.ContextWindowUsage) Session {
	usage = normalizeContextWindowUsage(usage)
	if usage.Used <= 0 && usage.Size <= 0 {
		return session
	}
	store := s.storeForMessage(msg)
	if store == nil {
		return session
	}
	if err := store.UpdateCurrentSession(session.Key, session.ACPSessionID, func(current *Session) bool {
		if current.ContextWindow != nil &&
			current.ContextWindow.Used == usage.Used &&
			current.ContextWindow.Size == usage.Size &&
			current.ContextWindow.AutoCompactTokenLimit == usage.AutoCompactTokenLimit {
			return false
		}
		if contextWindowUsageDropped(current.ContextWindow, usage) {
			current.WorkspacePrompted = false
		}
		current.ContextWindow = &usage
		return true
	}); err != nil {
		slog.WarnContext(ctx, "保存上下文窗口用量失败", "session", session.ACPSessionID, "错误", err)
		return session
	}
	if latest, ok := store.Get(session.Key); ok {
		return latest
	}
	session.ContextWindow = &usage
	return session
}

func (s *Service) markAutoCompacting(ctx context.Context, msg feishu.Message, session Session, value bool) (Session, bool) {
	store := s.storeForMessage(msg)
	if store == nil {
		return session, false
	}
	changed := false
	if err := store.UpdateCurrentSession(session.Key, session.ACPSessionID, func(current *Session) bool {
		if current.AutoCompacting == value {
			return false
		}
		current.AutoCompacting = value
		changed = true
		return true
	}); err != nil {
		slog.WarnContext(ctx, "保存自动 compact 状态失败", "session", session.ACPSessionID, "错误", err)
		return session, false
	}
	if latest, ok := store.Get(session.Key); ok {
		return latest, changed
	}
	return session, changed
}

func (s *Service) finishAutoCompact(ctx context.Context, msg feishu.Message, session Session, success bool) {
	store := s.storeForMessage(msg)
	if store == nil {
		return
	}
	now := time.Now()
	if err := store.UpdateCurrentSession(session.Key, session.ACPSessionID, func(current *Session) bool {
		changed := current.AutoCompacting
		current.AutoCompacting = false
		if success {
			current.LastAutoCompactAt = &now
			current.WorkspacePrompted = false
			changed = true
		}
		return changed
	}); err != nil {
		slog.WarnContext(ctx, "保存自动 compact 完成状态失败", "session", session.ACPSessionID, "错误", err)
	}
}

func (s *Service) maybeRunAutoCompact(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, result acp.PromptResult, err error) {
	if err != nil {
		return
	}
	if usage, ok := contextWindowFromPromptResult(result); ok {
		session = s.recordSessionContextWindow(ctx, msg, session, usage)
	} else if store := s.storeForMessage(msg); store != nil {
		if latest, ok := store.Get(session.Key); ok && latest.ACPSessionID == session.ACPSessionID {
			session = latest
		}
	}
	if !shouldAutoCompact(session) {
		return
	}
	if !sessionSupportsCompact(session) {
		return
	}
	session, changed := s.markAutoCompacting(ctx, msg, session, true)
	if !changed {
		return
	}
	s.goBackground("auto-compact", func() { s.runAutoCompact(context.WithoutCancel(ctx), msg, session, agent) })
}

func sessionSupportsCompact(session Session) bool {
	if len(session.AvailableCommands) == 0 {
		return false
	}
	return sessionHasCommand(session, "compact")
}

func shouldAutoCompact(session Session) bool {
	if !session.AutoCompact || session.AutoCompacting || session.AutoCompactPct <= 0 || session.ContextWindow == nil {
		return false
	}
	usage := normalizeContextWindowUsage(*session.ContextWindow)
	if usage.Used <= 0 || usage.Size <= 0 {
		return false
	}
	return usage.Used*100 >= int64(session.AutoCompactPct)*usage.Size
}

func (s *Service) runAutoCompact(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig) {
	_, _, err := s.runUserPromptWithOptions(ctx, msg, session, agent, "/compact", autoCompactTaskOptions())
	if err != nil && !errors.Is(err, context.Canceled) {
		s.recordACPError(session, "auto compact", err)
		slog.WarnContext(ctx, "自动 compact 失败", "session", session.ACPSessionID, "错误", err)
		s.finishAutoCompact(ctx, msg, session, false)
		return
	}
	s.finishAutoCompact(ctx, msg, session, err == nil)
}
