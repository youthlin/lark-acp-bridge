package bridge

import (
	"strings"
	"unicode/utf8"
)

func normalizeStreamMarkdown(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	var out strings.Builder
	inCodeBlock := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			appendLineBreak(&out)
			out.WriteString(line)
			inCodeBlock = !inCodeBlock
			if i < len(lines)-1 {
				out.WriteByte('\n')
			}
			continue
		}
		if inCodeBlock || trimmed == "" || isMarkdownBlockStart(trimmed) {
			appendLineBreak(&out)
			out.WriteString(line)
			if i < len(lines)-1 {
				out.WriteByte('\n')
			}
			continue
		}
		if out.Len() > 0 {
			current := out.String()
			last, _ := utf8.DecodeLastRuneInString(current)
			if last == '\n' || isSentenceEnd(last) {
				appendLineBreak(&out)
			} else if !isCJKContinuation(current, trimmed) {
				out.WriteByte(' ')
			}
		}
		out.WriteString(trimmed)
	}
	return strings.TrimSpace(out.String())
}

func truncateProcessText(text string) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= maxPromptProcessRunes {
		return text
	}
	return "（前面过程内容已省略）\n" + string(runes[len(runes)-maxPromptProcessRunes:])
}

func appendLineBreak(out *strings.Builder) {
	if out.Len() == 0 {
		return
	}
	if out.String()[out.Len()-1] != '\n' {
		out.WriteByte('\n')
	}
}

func isMarkdownBlockStart(line string) bool {
	if isProcessMarkerLine(line) {
		return true
	}
	if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ">") || strings.HasPrefix(line, "|") {
		return true
	}
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") || strings.HasPrefix(line, "+ ") {
		return true
	}
	if len(line) >= 3 && line[0] >= '0' && line[0] <= '9' {
		for i := 1; i < len(line)-1 && i < 4; i++ {
			if line[i] == '.' && line[i+1] == ' ' {
				return true
			}
			if line[i] < '0' || line[i] > '9' {
				return false
			}
		}
	}
	return false
}

func isProcessMarkerLine(line string) bool {
	for _, marker := range []string{"💬 ", "📌 ", "🧠 ", "⏳ ", "✅ ", "❌ "} {
		if strings.HasPrefix(line, marker) {
			return true
		}
	}
	return false
}

func isCJKContinuation(current, next string) bool {
	if current == "" || next == "" {
		return false
	}
	prev, _ := utf8.DecodeLastRuneInString(current)
	first, _ := utf8.DecodeRuneInString(next)
	return isCJK(prev) || isCJK(first)
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x3040 && r <= 0x30FF) ||
		(r >= 0xAC00 && r <= 0xD7AF)
}

func isSentenceEnd(r rune) bool {
	switch r {
	case '.', '!', '?', ':', ';', '。', '！', '？', '：', '；':
		return true
	default:
		return false
	}
}
