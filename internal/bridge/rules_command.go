package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func (s *Service) handleRulesCommand(ctx context.Context, text string, msg feishu.Message) string {
	store := s.storeForMessage(msg)
	if store == nil {
		return "会话持久化未初始化。"
	}
	fields := strings.Fields(text)
	if len(fields) == 1 || len(fields) == 2 && strings.EqualFold(fields[1], "status") {
		return formatChatRulesStatus(s.chatConfigForMessage(msg))
	}
	if len(fields) < 2 {
		return rulesCommandUsage()
	}

	chat := s.chatConfigForMessage(msg)
	switch strings.ToLower(fields[1]) {
	case "set":
		rules := commandRemainder(text, 2)
		if rules == "" {
			return "请提供补充规则，例如 /rules set 回复默认使用中文。"
		}
		if len(rules) > chatRulesMaxBytes {
			return fmt.Sprintf("补充规则不能超过 %d KiB。", chatRulesMaxBytes/1024)
		}
		if _, err := store.UpdateChatRules(chat, rules); err != nil {
			slog.ErrorContext(ctx, "保存 chat 补充规则失败", "chat", msg.ChatID, "错误", err)
			return "保存当前 chat 补充规则失败：" + err.Error()
		}
		return "已设置当前 chat 的补充规则，将在下一条 prompt 中随 workspace 规则重新注入。"
	case "clear":
		if len(fields) != 2 {
			return rulesCommandUsage()
		}
		if strings.TrimSpace(chat.Rules) == "" {
			return "当前 chat 未配置补充规则。"
		}
		if _, err := store.UpdateChatRules(chat, ""); err != nil {
			slog.ErrorContext(ctx, "清除 chat 补充规则失败", "chat", msg.ChatID, "错误", err)
			return "清除当前 chat 补充规则失败：" + err.Error()
		}
		return "已清除当前 chat 的补充规则。已有 ACP session 仍可能保留旧规则；如需彻底移除，请执行 /new。"
	default:
		return rulesCommandUsage()
	}
}

func formatChatRulesStatus(chat ChatConfig) string {
	rules := strings.TrimSpace(chat.Rules)
	if rules == "" {
		return "当前 chat 未配置补充规则。"
	}
	return "当前 chat 补充规则：\n" + rules
}

func rulesCommandUsage() string {
	return "请使用 /rules、/rules set <规则> 或 /rules clear。"
}
