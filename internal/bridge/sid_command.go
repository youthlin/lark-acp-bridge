package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func (s *Service) handleSIDCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) < 3 {
		return "请使用 /sid <acp_session_id> <prompt> 指定要继续的 ACP session 和消息内容。"
	}
	acpSessionID := strings.TrimSpace(fields[1])
	userText := commandRemainder(text, 2)
	if acpSessionID == "" || userText == "" {
		return "请使用 /sid <acp_session_id> <prompt> 指定要继续的 ACP session 和消息内容。"
	}
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	session, ok := store.SessionByACPSessionID(msg.BotID, acpSessionID)
	if !ok {
		return fmt.Sprintf("未找到 ACP session：%s。", acpSessionID)
	}
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		return fmt.Sprintf("未找到 agent 配置: %s", session.AgentName)
	}
	if err := s.interruptRunningSessionWork(ctx, session.Key); err != nil {
		slog.ErrorContext(ctx, "取消指定 ACP session 当前任务失败", "session", session.ACPSessionID, "错误", err)
		return "取消指定 ACP session 当前任务失败：" + err.Error()
	}
	session, errText := s.ensureSIDCurrentSession(ctx, store, session)
	if errText != "" {
		return errText
	}
	promptText := promptTextWithReplyContext(msg, userText)
	reply, err := s.promptSession(ctx, msg, session, agent, promptText, userText, promptSessionOptions{})
	if err != nil {
		slog.ErrorContext(ctx, "执行指定 ACP session prompt 失败", "session", session.ACPSessionID, "错误", err)
		return "执行指定 ACP session 失败：" + err.Error()
	}
	return reply
}

func (s *Service) ensureSIDCurrentSession(ctx context.Context, store *SessionStore, session Session) (Session, string) {
	session = normalizeSessionForStore(session)
	if current, ok := store.Get(session.Key); ok && current.ACPSessionID == session.ACPSessionID {
		return current, ""
	}
	expectedSessionID := ""
	if current, ok := store.Get(session.Key); ok {
		expectedSessionID = current.ACPSessionID
	}
	restored, ok, err := s.runtime.TransitionCurrentSession(session.Key, expectedSessionID, func() (Session, bool, error) {
		return store.ResumeSessionIfCurrent(session.Key, expectedSessionID, session.ACPSessionID)
	})
	if err != nil {
		if ok {
			slog.WarnContext(ctx, "恢复指定 ACP session 后关闭旧 ACP runtime 失败", "key", session.Key, "错误", err)
			return restored, ""
		}
		slog.ErrorContext(ctx, "恢复指定 ACP session 失败", "session", session.ACPSessionID, "错误", err)
		return Session{}, "恢复指定 ACP session 失败：" + err.Error()
	}
	if !ok {
		return Session{}, "目标 ACP session 已变化或不存在，请重新确认 session id。"
	}
	return restored, ""
}
