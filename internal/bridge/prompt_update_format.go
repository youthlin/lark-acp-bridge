package bridge

import (
	"encoding/json"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

const maxPromptUpdateRunes = 1800

type toolProgressStatus int

const (
	toolProgressRunning toolProgressStatus = iota
	toolProgressCompleted
	toolProgressFailed
	toolProgressUnknown
)

func formatPromptUpdate(update acp.PromptUpdate) string {
	u := update.Update
	kind := strings.TrimSpace(firstNonEmpty(u.SessionUpdate, rawString(u.Raw, "type"), rawString(u.Raw, "event")))
	switch kind {
	case "agent_message_chunk":
		return ""
	case "agent_message", "assistant_message", "message":
		return formatProcessMessageText(truncateRunes(firstNonEmpty(u.Message, contentText(u.Content), rawText(u.Raw)), maxPromptUpdateRunes))
	case "plan", "thought", "reasoning":
		return formatThoughtProcessText(truncateRunes(firstNonEmpty(u.Message, contentText(u.Content), rawText(u.Raw)), maxPromptUpdateRunes))
	case "status", "progress":
		text := firstNonEmpty(u.Message, u.Status, contentText(u.Content), rawText(u.Raw))
		return formatProcessMessageText(truncateRunes(text, maxPromptUpdateRunes))
	default:
		if text := firstNonEmpty(u.Message, contentText(u.Content), rawText(u.Raw)); text != "" {
			return formatProcessMessageText(truncateRunes(text, maxPromptUpdateRunes))
		}
		return ""
	}
}

func formatProcessMessageText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return "💬 " + text
}

func formatThoughtProcessText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return "🧠 " + text
}

func isToolPromptUpdateKind(kind string) bool {
	switch kind {
	case "function_call", "tool_call", "custom_tool_call",
		"tool_call_update", "function_call_update", "custom_tool_call_update",
		"function_call_output", "tool_call_output", "custom_tool_call_output",
		"tool_call_error", "function_call_error":
		return true
	default:
		return (strings.Contains(kind, "tool") || strings.Contains(kind, "function")) && !isPromptChunkKind(kind)
	}
}

func isToolBoundaryUpdateKind(kind string) bool {
	switch kind {
	case "function_call", "tool_call", "custom_tool_call":
		return true
	default:
		return false
	}
}

func toolStatusFromUpdate(kind, status string) toolProgressStatus {
	if strings.Contains(kind, "error") {
		return toolProgressFailed
	}
	if strings.Contains(kind, "output") {
		return toolProgressCompleted
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "complete", "success", "succeeded", "done":
		return toolProgressCompleted
	case "failed", "failure", "error":
		return toolProgressFailed
	case "in_progress", "running", "pending", "":
		return toolProgressRunning
	default:
		return toolProgressUnknown
	}
}

func toolStatusIcon(status toolProgressStatus) string {
	switch status {
	case toolProgressCompleted:
		return "✅"
	case toolProgressFailed:
		return "❌"
	case toolProgressUnknown:
		return "•"
	default:
		return "⏳"
	}
}

func promptUpdateChunkText(update acp.PromptUpdate) string {
	if update.Update.SessionUpdate != "agent_message_chunk" {
		return ""
	}
	if update.Update.Title != "" {
		return ""
	}
	if update.Update.Content == nil || update.Update.Content.Text == "" {
		return ""
	}
	return update.Update.Content.Text
}

func promptUpdateChunk(update acp.PromptUpdate) (promptChunk, bool) {
	u := update.Update
	kind := promptUpdateKind(update)
	if !isPromptChunkKind(kind) || u.Title != "" {
		return promptChunk{}, false
	}
	text := promptUpdateChunkRawText(update)
	if text == "" {
		return promptChunk{}, false
	}
	target := promptChunkTargetProcess
	key := kind
	if kind == "agent_message_chunk" {
		target = promptChunkTargetText
		key = "agent_message"
	} else if isToolChunkUpdateKind(kind) {
		target = promptChunkTargetTool
	} else if isThoughtUpdateKind(kind) {
		target = promptChunkTargetThought
	} else if streamName := promptChunkStreamName(kind); streamName != "" {
		key = streamName
	}
	return promptChunk{Target: target, Key: key, Text: text}, true
}

func promptUpdateKind(update acp.PromptUpdate) string {
	u := update.Update
	return strings.TrimSpace(firstNonEmpty(u.SessionUpdate, rawString(u.Raw, "type"), rawString(u.Raw, "event")))
}

func isPromptChunkKind(kind string) bool {
	return kind == "agent_message_chunk" || strings.HasSuffix(kind, "_chunk")
}

func isToolChunkUpdateKind(kind string) bool {
	if !isPromptChunkKind(kind) {
		return false
	}
	return strings.Contains(kind, "tool") || strings.Contains(kind, "function")
}

func isThoughtUpdateKind(kind string) bool {
	switch kind {
	case "agent_thought_chunk", "thought_chunk", "reasoning_chunk", "plan_chunk",
		"thought", "reasoning", "plan":
		return true
	default:
		return strings.Contains(kind, "thought") || strings.Contains(kind, "reasoning")
	}
}

func promptChunkStreamName(kind string) string {
	kind = strings.TrimSuffix(kind, "_chunk")
	kind = strings.TrimSuffix(kind, ".chunk")
	return strings.TrimSpace(kind)
}

func promptUpdateChunkRawText(update acp.PromptUpdate) string {
	u := update.Update
	if u.Content != nil && (u.Content.Type == "" || u.Content.Type == "text" || u.Content.Type == "output_text") {
		return u.Content.Text
	}
	if u.Message != "" {
		return u.Message
	}
	return rawChunkText(u.Raw)
}

func rawChunkText(raw json.RawMessage) string {
	value := rawValue(raw)
	return rawChunkTextValue(value)
}

func rawChunkTextValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		for _, item := range v {
			if text := rawChunkTextValue(item); text != "" {
				return text
			}
		}
	case map[string]any:
		for _, key := range []string{"text", "delta", "output_text", "message"} {
			if text := rawChunkTextValue(v[key]); text != "" {
				return text
			}
		}
		for _, key := range []string{"content", "payload"} {
			if text := rawChunkTextValue(v[key]); text != "" {
				return text
			}
		}
	}
	return ""
}

func toolDisplayName(u acp.SessionUpdate) string {
	return firstNonEmpty(u.Title, u.Name, rawName(u.Raw), rawString(u.Raw, "title"))
}

func contentText(content *acp.ContentBlock) string {
	if content == nil {
		return ""
	}
	if content.Type != "" && content.Type != "text" && content.Type != "output_text" {
		return ""
	}
	return strings.TrimSpace(content.Text)
}

func rawText(raw json.RawMessage) string {
	value := rawValue(raw)
	return strings.TrimSpace(rawTextValue(value))
}

func rawTextValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		for _, item := range v {
			if text := rawTextValue(item); text != "" {
				return text
			}
		}
	case map[string]any:
		for _, key := range []string{"message", "text", "output_text", "delta"} {
			if text := rawTextValue(v[key]); text != "" {
				return text
			}
		}
		if content := rawTextValue(v["content"]); content != "" {
			return content
		}
		if payload := rawTextValue(v["payload"]); payload != "" {
			return payload
		}
	}
	return ""
}

func rawName(raw json.RawMessage) string {
	value := rawValue(raw)
	return strings.TrimSpace(rawNameValue(value))
}

func rawNameValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		for _, item := range v {
			if name := rawNameValue(item); name != "" {
				return name
			}
		}
	case map[string]any:
		for _, key := range []string{"title", "name", "toolName", "command"} {
			if name := rawNameValue(v[key]); name != "" {
				return name
			}
		}
		for _, key := range []string{"toolCall", "tool_call", "function", "payload", "item"} {
			if name := rawNameValue(v[key]); name != "" {
				return name
			}
		}
	}
	return ""
}

func rawString(raw json.RawMessage, key string) string {
	value, ok := rawValue(raw).(map[string]any)
	if !ok {
		return ""
	}
	text, _ := value[key].(string)
	return strings.TrimSpace(text)
}

func rawValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func truncateRunes(text string, limit int) string {
	text = strings.TrimSpace(text)
	if text == "" || limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}
