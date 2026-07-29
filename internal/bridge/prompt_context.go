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
	return promptWithUserMessage([]string{
		workspaceContextPrompt(workspace),
		workspaceMemoryPolicyPrompt(workspace),
		messageMetadataPrompt(msg),
	}, text)
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
