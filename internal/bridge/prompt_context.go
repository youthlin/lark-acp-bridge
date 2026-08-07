package bridge

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func promptTextWithReplyContext(msg feishu.Message, text string) string {
	replyText := ""
	if msg.Reply != nil {
		replyText = strings.TrimSpace(msg.Reply.PromptText())
	}
	if replyText == "" {
		return text
	}
	sections := []string{
		replyMetadataPrompt(msg.Reply),
		"## Replied Message Context",
		replyText,
		"",
		"请结合上面的被回复消息理解下面的用户消息。",
		"",
		"## User Message",
		strings.TrimSpace(text),
	}
	return strings.Join(nonEmptySections(sections), "\n")
}

func messageMetadataPrompt(msg feishu.Message) string {
	metadata := orderedPromptMetadata{
		{"bot_id", msg.BotID},
		{"message_id", msg.MessageID},
		{"chat_id", msg.ChatID},
		{"chat_type", msg.ChatType},
		{"thread_id", msg.ThreadID},
		{"root_id", msg.RootID},
		{"parent_id", msg.ParentID},
		{"sender_id", msg.SenderID},
		{"sender_type", msg.SenderType},
		{"msg_type", msg.MsgType},
	}
	return promptMetadataSection("## Message Metadata", metadata)
}

func replyMetadataPrompt(reply *feishu.ReplyContext) string {
	if reply == nil {
		return ""
	}
	metadata := orderedPromptMetadata{
		{"message_id", reply.MessageID},
		{"sender_id", reply.SenderID},
		{"sender_type", reply.SenderType},
		{"msg_type", reply.MsgType},
	}
	return promptMetadataSection("## Replied Message Metadata", metadata)
}

type promptMetadataField struct {
	Key   string
	Value string
}

type orderedPromptMetadata []promptMetadataField

func (m orderedPromptMetadata) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	written := 0
	for _, field := range m {
		key := strings.TrimSpace(field.Key)
		value := strings.TrimSpace(field.Value)
		if key == "" || value == "" {
			continue
		}
		if written > 0 {
			b.WriteByte(',')
		}
		keyJSON, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		valueJSON, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		b.Write(keyJSON)
		b.WriteByte(':')
		b.Write(valueJSON)
		written++
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

func promptMetadataSection(title string, metadata orderedPromptMetadata) string {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil || string(data) == "{}" {
		return ""
	}
	return strings.Join([]string{
		title,
		"```json",
		string(data),
		"```",
	}, "\n")
}

func nonEmptySections(sections []string) []string {
	out := sections[:0]
	for _, section := range sections {
		if strings.TrimSpace(section) != "" {
			out = append(out, section)
		}
	}
	return out
}

func sessionWorkspace(session Session, msg feishu.Message) string {
	if strings.TrimSpace(session.Workspace) != "" {
		return session.Workspace
	}
	return msg.Workspace
}

func promptTextWithWorkspaceContext(workspace string, msg feishu.Message, text string) string {
	return promptTextWithWorkspaceContextOptions(workspace, msg, text, workspacePromptOptions{
		IncludeWorkspaceContext: true,
		IncludeMemoryPolicy:     true,
	})
}

type workspacePromptOptions struct {
	IncludeWorkspaceContext bool
	IncludeMemoryPolicy     bool
	IncludeReactionPrompt   bool
}

func (s *Service) promptTextWithWorkspaceContextForSession(session Session, msg feishu.Message, text string) string {
	workspace := sessionWorkspace(session, msg)
	includeWorkspaceContext := shouldIncludeWorkspaceContextPrompt(session, workspace)
	return promptTextWithWorkspaceContextOptions(workspace, msg, text, workspacePromptOptions{
		IncludeWorkspaceContext: includeWorkspaceContext,
		IncludeMemoryPolicy:     includeWorkspaceContext,
		IncludeReactionPrompt:   s.messageReactionEnabled(),
	})
}

func promptTextWithWorkspaceContextOptions(workspace string, msg feishu.Message, text string, opts workspacePromptOptions) string {
	workspace = strings.TrimSpace(workspace)
	var workspaceContext string
	if opts.IncludeWorkspaceContext {
		workspaceContext = workspaceContextPrompt(workspace)
	}
	var memoryPolicy string
	if opts.IncludeMemoryPolicy {
		memoryPolicy = workspaceMemoryPolicyPrompt(workspace)
	}
	return promptWithUserMessage([]string{
		workspaceContext,
		memoryPolicy,
		messageMetadataPrompt(msg),
		feishuMessageReactionPrompt(msg, opts.IncludeReactionPrompt),
	}, text)
}

func shouldIncludeWorkspaceContextPrompt(session Session, workspace string) bool {
	return strings.TrimSpace(workspace) != "" && !session.WorkspacePrompted
}

var feishuMessageReactionEmojiTypes = []string{
	"THUMBSUP",    // 赞
	"APPLAUSE",    // 鼓掌
	"FISTBUMP",    // 碰拳
	"FINGERHEART", // 比心
	"MUSCLE",      // 加油
	"LAUGH",       // 笑
	"LOL",         // 笑哭
	"FACEPALM",    // 捂脸
	"SPEECHLESS",  // 无语
	"WOW",         // 惊讶
	"HEART",       // 爱心
	"Fire",        // 火
}

func feishuMessageReactionPrompt(msg feishu.Message, enabled bool) string {
	if !enabled || strings.TrimSpace(msg.MessageID) == "" {
		return ""
	}
	return strings.Join([]string{
		"## Feishu Message Reaction",
		"",
		"- 如果你认为本次收到的飞书消息适合用一个轻量 reaction 表达判断、认可、惊讶、好笑、无语或鼓励，可以给该消息添加一个表情 reaction。",
		"- reaction 是可选动作；只有在自然、合适且不会替代必要文字回复时才添加。",
		"- 目标消息 ID 使用 Message Metadata 中的 `message_id`。",
		"- 可用 emoji_type 建议从以下列表选择：" + strings.Join(feishuMessageReactionEmojiTypes, ", ") + "。",
		"- 可使用 `lark-cli im reactions create --message-id <message_id> --data '{\"reaction_type\":{\"emoji_type\":\"THUMBSUP\"}}' --as bot --profile <profile>`，或调用飞书 IM MessageReaction Create API。",
	}, "\n")
}

func formatNewSessionReply(session Session, source string) string {
	mode := currentModeDisplay(session)
	if mode == "" {
		mode = "未知"
	}
	model := currentModelDisplay(session)
	if model == "" {
		model = "未知"
	}
	return fmt.Sprintf("已为当前会话创建 ACP 会话。\n标题：%s\nmode：%s\nmodel：%s\nagent：%s\ncwd：%s\ncwd 来源：%s\nsession：%s",
		displaySessionTitle(session), mode, model, session.AgentName, session.Cwd, source, session.ACPSessionID)
}

func promptWithUserMessage(prefixes []string, text string) string {
	sections := make([]string, 0, len(prefixes)+2)
	for _, prefix := range prefixes {
		if strings.TrimSpace(prefix) != "" {
			sections = append(sections, prefix)
		}
	}
	if len(sections) == 0 {
		return text
	}
	sections = append(sections, "## User Message", text)
	return strings.Join(sections, "\n\n")
}
