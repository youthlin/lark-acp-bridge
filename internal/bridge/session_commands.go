package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func (s *Service) handleSessionCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return "可用命令：/session list、/session resume <index> 或 /session title <title>"
	}
	switch strings.ToLower(strings.TrimSpace(fields[1])) {
	case "list":
		return s.sendSessionList(ctx, msg)
	case "resume":
		if len(fields) < 3 {
			return "请使用 /session resume <index> 指定要恢复的会话序号。"
		}
		index, err := strconv.Atoi(fields[2])
		if err != nil || index <= 0 {
			return "会话序号必须是正整数。"
		}
		return s.resumeSession(ctx, msg, index)
	case "title":
		title := commandRemainder(text, 2)
		if title == "" {
			return "请使用 /session title <title> 设置当前会话标题。"
		}
		return s.setSessionTitle(ctx, msg, title)
	default:
		return "暂不支持这个 session 命令。可用 /session list、/session resume <index> 或 /session title <title>。"
	}
}

func (s *Service) sendSessionList(ctx context.Context, msg feishu.Message) string {
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	items := store.ListByChat(msg.BotID, msg.ChatID)
	if len(items) == 0 {
		return "当前聊天还没有历史 ACP 会话。发送普通文本会自动创建，或用 /new <cwd> 指定工作目录。"
	}
	sent, err := feishu.SendSessionSelectionCard(ctx, msg, feishu.SessionSelectionCard{
		BotID:               msg.BotID,
		ChatID:              msg.ChatID,
		ThreadID:            msg.ThreadID,
		RequesterID:         msg.SenderID,
		CurrentACPSessionID: currentACPSessionID(s, msg),
		Options:             sessionSelectionOptions(items, maxSessionHistoryPerChat),
	})
	if err != nil {
		slog.ErrorContext(ctx, "发送会话选择卡片失败", "错误", err)
		return "发送会话选择卡片失败：" + err.Error()
	}
	if sent {
		return ""
	}
	return s.formatSessionList(msg, 0)
}

func (s *Service) formatSessionList(msg feishu.Message, limit int) string {
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	items := store.ListByChat(msg.BotID, msg.ChatID)
	if len(items) == 0 {
		return "当前聊天还没有历史 ACP 会话。发送普通文本会自动创建，或用 /new <cwd> 指定工作目录。"
	}
	total := len(items)
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	current, hasCurrent := s.findSession(msg)
	lines := []string{"当前聊天的历史 ACP 会话："}
	for i, item := range items {
		marker := ""
		if hasCurrent && item.ACPSessionID == current.ACPSessionID {
			marker = " *"
		}
		lines = append(lines, fmt.Sprintf("%d. %s%s\n   标题：%s\n   cwd：%s", i+1, item.ACPSessionID, marker, displaySessionTitle(item), item.Cwd))
	}
	if limit > 0 && total > limit {
		lines = append(lines, fmt.Sprintf("仅显示最近 %d 个，共 %d 个历史会话。", limit, total))
	}
	lines = append(lines, "使用 /session resume <index> 恢复指定会话。")
	return strings.Join(lines, "\n")
}

func currentACPSessionID(svc *Service, msg feishu.Message) string {
	if svc == nil {
		return ""
	}
	session, ok := svc.findSession(msg)
	if !ok {
		return ""
	}
	return session.ACPSessionID
}

func sessionSelectionOptions(items []Session, limit int) []feishu.SessionOption {
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	options := make([]feishu.SessionOption, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.ACPSessionID) == "" {
			continue
		}
		options = append(options, feishu.SessionOption{
			ACPSessionID: item.ACPSessionID,
			Title:        displaySessionTitle(item),
			Cwd:          item.Cwd,
		})
	}
	return options
}

func (s *Service) resumeSession(ctx context.Context, msg feishu.Message, index int) string {
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	items := store.ListByChat(msg.BotID, msg.ChatID)
	if len(items) == 0 {
		return "当前聊天还没有历史 ACP 会话。"
	}
	if index > len(items) {
		return fmt.Sprintf("会话序号超出范围。当前共有 %d 个历史会话。", len(items))
	}
	session, errText := s.resumeSessionByID(ctx, msg, items[index-1].ACPSessionID)
	if errText != "" {
		return errText
	}
	return fmt.Sprintf("已恢复会话 %d。\n标题：%s\nagent：%s\ncwd：%s\nsession：%s", index, displaySessionTitle(session), session.AgentName, session.Cwd, session.ACPSessionID)
}

func (s *Service) resumeSessionByID(ctx context.Context, msg feishu.Message, acpSessionID string) (Session, string) {
	store := s.storeForMessage(msg)
	if store == nil {
		return Session{}, "会话持久化未初始化。"
	}
	acpSessionID = strings.TrimSpace(acpSessionID)
	if acpSessionID == "" {
		return Session{}, "会话 ID 不能为空。"
	}
	for _, item := range store.ListByChat(msg.BotID, msg.ChatID) {
		if item.ACPSessionID != acpSessionID {
			continue
		}
		session := item
		session.Key = sessionKeyFromMessage(msg)
		if err := store.Upsert(session); err != nil {
			slog.ErrorContext(ctx, "恢复会话映射失败", "错误", err)
			return Session{}, "恢复会话失败：" + err.Error()
		}
		if err := s.runtime.CloseSession(session.Key); err != nil {
			slog.WarnContext(ctx, "关闭旧 ACP runtime 失败", "key", session.Key, "错误", err)
		}
		return session, ""
	}
	return Session{}, "选择的会话不存在或已过期，请重新发送 /session list。"
}

func (s *Service) HandleSessionSelection(ctx context.Context, selection feishu.SessionSelection) (string, error) {
	if err := validateSelectionRequester(selection.RequesterID, selection.OperatorID, "会话", "session list", "恢复会话"); err != nil {
		return "", err
	}
	msg := feishu.Message{
		BotID:    selection.BotID,
		ChatID:   selection.ChatID,
		ThreadID: selection.ThreadID,
		SenderID: selection.OperatorID,
	}
	if !s.slashCommandAllowed(msg) {
		if len(s.ownerOpenIDs(selection.BotID)) == 0 {
			return "", fmt.Errorf("未配置 bot owner，不能恢复会话")
		}
		return "", fmt.Errorf("只有 bot owner 可以恢复会话")
	}
	session, errText := s.resumeSessionByID(ctx, msg, selection.ACPSessionID)
	if errText != "" {
		return "", fmt.Errorf("%s", strings.TrimSuffix(errText, "。"))
	}
	return displaySessionTitle(session), nil
}

func (s *Service) setSessionTitle(ctx context.Context, msg feishu.Message, title string) string {
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	session, ok := s.findSession(msg)
	if !ok {
		return "当前会话还没有 ACP session，无法设置标题。"
	}
	session.Title = normalizeSessionTitle(title)
	session.ManualTitle = true
	if err := store.Upsert(session); err != nil {
		slog.ErrorContext(ctx, "设置会话标题失败", "错误", err)
		return "设置会话标题失败：" + err.Error()
	}
	return "已设置当前会话标题：" + session.Title
}
