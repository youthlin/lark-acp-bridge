package bridge

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const (
	secretInputsDirName       = "secret-inputs"
	secretInputPlaceholder    = "[已隐藏]"
	secretInputNoticeTemplate = "## Sensitive Input Handling\n用户消息中的明显敏感值已隐藏为本地文件路径。不要为了展示、推理或回答而打开、打印、复述这些文件内容；如需把值写入配置或传给命令，只在 shell 命令中通过管道、重定向、命令替换或环境变量消费对应文件。"
)

var secretInputPatterns = []struct {
	re          *regexp.Regexp
	valueGroup  int
	description string
}{
	{
		re:          regexp.MustCompile(`(?i)(\bAuthorization\s*:\s*(?:Bearer|Basic)\s+)([^\s\r\n]+)`),
		valueGroup:  2,
		description: "authorization",
	},
	{
		re:          regexp.MustCompile(`(?i)(["']?\b(?:openai[_-]?api[_-]?key|ark[_-]?api[_-]?key|aws[_-]?access[_-]?key[_-]?id|awsaccesskeyid|aws[_-]?secret[_-]?access[_-]?key|awssecretaccesskey|api[_-]?key|apikey|app[_-]?secret|appsecret|client[_-]?secret|clientsecret|secret[_-]?key|secretkey|access[_-]?token|accesstoken|refresh[_-]?token|refreshtoken|secret|token|ak|sk)\b["']?\s*[:=]\s*)(?:"([^"\r\n]*)"|'([^'\r\n]*)'|([^\s\r\n,;#]+))`),
		valueGroup:  -1,
		description: "key-value",
	},
}

var secretInputFilePlaceholderRE = regexp.MustCompile(`\[已隐藏:[^\]]+\]`)

type sanitizedPromptText struct {
	Text      string
	TitleText string
	Count     int
}

type secretTextMatch struct {
	start int
	end   int
}

func (s *Service) sanitizePromptSecretsForModel(msg feishu.Message, session Session, promptText string, titleText string) (sanitizedPromptText, error) {
	sanitized, count, err := s.redactSensitiveValuesToFiles(msg, session, promptText)
	if err != nil {
		return sanitizedPromptText{}, err
	}
	if needsSecretInputNotice(sanitized, count) {
		sanitized = secretInputNoticeTemplate + "\n\n" + sanitized
	}
	return sanitizedPromptText{
		Text:      sanitized,
		TitleText: redactSensitiveValuesForDisplay(titleText),
		Count:     count,
	}, nil
}

func (s *Service) sanitizeACPCommandSecretsForModel(msg feishu.Message, session Session, command string) (string, error) {
	sanitized, count, err := s.redactSensitiveValuesToFiles(msg, session, command)
	if err != nil {
		return "", err
	}
	if !needsSecretInputNotice(sanitized, count) {
		return sanitized, nil
	}
	return strings.TrimSpace(sanitized) + "\n\n" + secretInputNoticeTemplate, nil
}

func (s *Service) sanitizePromptSecretsToFiles(msg feishu.Message, session Session, promptText string) (string, error) {
	sanitized, _, err := s.redactSensitiveValuesToFiles(msg, session, promptText)
	return sanitized, err
}

func (s *Service) sanitizeTriggerRequestSecrets(req TriggerRequest, session Session) (TriggerRequest, error) {
	sanitized, count, err := s.redactSensitiveValuesToFiles(triggerRequestSecretMessage(req), session, req.Prompt)
	if err != nil {
		return req, err
	}
	req.Title = redactSensitiveValuesForDisplay(req.Title)
	req.Prompt = sanitized
	if !needsSecretInputNotice(sanitized, count) {
		return req, nil
	}
	req.Prompt = secretInputNoticeTemplate + "\n\n" + req.Prompt
	return req, nil
}

func (s *Service) redactSensitiveValuesToFiles(msg feishu.Message, session Session, text string) (string, int, error) {
	matches := collectSecretTextMatches(text)
	if len(matches) == 0 {
		return text, 0, nil
	}
	dir, err := s.secretInputMessageDir(msg, session)
	if err != nil {
		return "", 0, err
	}
	if err := ensureSecretInputDir(dir); err != nil {
		return "", 0, err
	}
	return replaceSensitiveText(text, matches, func(index int, value string) (string, error) {
		path := filepath.Join(dir, fmt.Sprintf("secret-%d.txt", index))
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			return "", fmt.Errorf("写入敏感输入文件 %s: %w", path, err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return "", fmt.Errorf("设置敏感输入文件权限 %s: %w", path, err)
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		return "[已隐藏: " + abs + "]", nil
	})
}

func redactSensitiveValuesForDisplay(text string) string {
	matches := collectSecretTextMatches(text)
	if len(matches) == 0 {
		return secretInputFilePlaceholderRE.ReplaceAllString(text, secretInputPlaceholder)
	}
	sanitized, _, err := replaceSensitiveText(text, matches, func(int, string) (string, error) {
		return secretInputPlaceholder, nil
	})
	if err != nil {
		return secretInputFilePlaceholderRE.ReplaceAllString(text, secretInputPlaceholder)
	}
	return secretInputFilePlaceholderRE.ReplaceAllString(sanitized, secretInputPlaceholder)
}

func needsSecretInputNotice(text string, count int) bool {
	if count == 0 && !strings.Contains(text, "[已隐藏:") {
		return false
	}
	return !strings.Contains(text, secretInputNoticeTemplate)
}

func (s *Service) secretInputMessageDir(msg feishu.Message, session Session) (string, error) {
	workspace := strings.TrimSpace(sessionWorkspace(session, msg))
	if workspace == "" {
		workspace = strings.TrimSpace(s.botWorkspace(msg.BotID))
	}
	if workspace == "" {
		return "", fmt.Errorf("检测到敏感输入，但当前 bot workspace 未初始化，无法安全保存原始值")
	}
	expanded, err := config.ExpandPath(workspace)
	if err != nil {
		return "", fmt.Errorf("解析 workspace 路径 %s: %w", workspace, err)
	}
	messageID := safeSecretInputComponent(firstNonEmpty(msg.MessageID, msg.RootID, msg.ThreadID, time.Now().UTC().Format("20060102T150405.000000000Z")))
	return workspaceLocalPath(filepath.Clean(expanded), secretInputsDirName, messageID), nil
}

func triggerRequestSecretMessage(req TriggerRequest) feishu.Message {
	req = req.normalized()
	return feishu.Message{
		BotID:     req.BotID,
		MessageID: req.TraceMessageID,
		Workspace: req.Workspace,
	}
}

func ensureSecretInputDir(dir string) error {
	root := filepath.Dir(dir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("创建敏感输入目录 %s: %w", root, err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("设置敏感输入目录权限 %s: %w", root, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建敏感输入消息目录 %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("设置敏感输入消息目录权限 %s: %w", dir, err)
	}
	return nil
}

func collectSecretTextMatches(text string) []secretTextMatch {
	var matches []secretTextMatch
	for _, pattern := range secretInputPatterns {
		for _, idx := range pattern.re.FindAllStringSubmatchIndex(text, -1) {
			start, end := secretMatchValueRange(idx, pattern.valueGroup)
			if start < 0 || end <= start {
				continue
			}
			if !shouldRedactSecretValue(text[start:end]) {
				continue
			}
			matches = append(matches, secretTextMatch{start: start, end: end})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].start == matches[j].start {
			return matches[i].end > matches[j].end
		}
		return matches[i].start < matches[j].start
	})
	out := matches[:0]
	lastEnd := -1
	for _, match := range matches {
		if match.start < lastEnd {
			continue
		}
		out = append(out, match)
		lastEnd = match.end
	}
	return out
}

func secretMatchValueRange(idx []int, valueGroup int) (int, int) {
	if valueGroup > 0 {
		return submatchRange(idx, valueGroup)
	}
	for group := 2; group <= 4; group++ {
		start, end := submatchRange(idx, group)
		if start >= 0 && end >= start {
			return start, end
		}
	}
	return -1, -1
}

func submatchRange(idx []int, group int) (int, int) {
	pos := group * 2
	if pos+1 >= len(idx) {
		return -1, -1
	}
	return idx[pos], idx[pos+1]
}

func shouldRedactSecretValue(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	return !strings.HasPrefix(value, secretInputPlaceholder) && !strings.HasPrefix(value, "[已隐藏:")
}

func replaceSensitiveText(text string, matches []secretTextMatch, replacement func(index int, value string) (string, error)) (string, int, error) {
	var b strings.Builder
	b.Grow(len(text))
	cursor := 0
	count := 0
	for _, match := range matches {
		if match.start < cursor || match.end > len(text) {
			continue
		}
		count++
		b.WriteString(text[cursor:match.start])
		repl, err := replacement(count, text[match.start:match.end])
		if err != nil {
			return "", count - 1, err
		}
		b.WriteString(repl)
		cursor = match.end
	}
	b.WriteString(text[cursor:])
	return b.String(), count, nil
}

func safeSecretInputComponent(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "message"
	}
	var b strings.Builder
	b.Grow(len(value))
	for _, r := range value {
		if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return "message"
	}
	return out
}
