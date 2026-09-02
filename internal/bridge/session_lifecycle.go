package bridge

import (
	"context"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const (
	newSessionStateWait        = 600 * time.Millisecond
	newSessionPartialStateWait = 120 * time.Millisecond
)

type newSessionPlan struct {
	Req             newSessionRequest
	Source          string
	UseDefaultTitle bool
	Cwd             string
	AgentName       string
	Agent           config.AgentConfig
}

type sessionTransition struct {
	Key SessionKey
}

func newSessionFromCandidate(key SessionKey, msg feishu.Message, plan newSessionPlan, candidate acpSessionCandidate) Session {
	sessionInfo := candidate.Info()
	return Session{
		Key:               key,
		Title:             plan.Req.Title,
		ManualTitle:       plan.Req.ManualTitle,
		AgentName:         plan.AgentName,
		ACPSessionID:      sessionInfo.SessionID,
		ACPMeta:           maps.Clone(sessionInfo.Meta),
		Cwd:               filepath.Clean(plan.Cwd),
		Workspace:         msg.Workspace,
		AvailableCommands: sessionInfo.AvailableCommands,
		ConfigOptions:     sessionInfo.ConfigOptions,
		Models:            sessionInfo.Models,
		Mode:              sessionInfo.Mode,
	}
}

func commitNewSessionCandidate(candidate acpSessionCandidate, store *SessionStore, plan newSessionPlan, session *Session) error {
	if plan.UseDefaultTitle {
		return candidate.Commit(func() error {
			var persistErr error
			*session, persistErr = store.UpsertWithDefaultTitle(*session)
			return persistErr
		})
	}
	return candidate.Commit(func() error {
		return store.Upsert(*session)
	})
}

type inheritedSessionConfig struct {
	Mode  string
	Model string
}

func chatAgentSessionConfig(chat ChatConfig, agentName string) (ChatAgentConfig, bool) {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" || len(chat.AgentConfigs) == 0 {
		return ChatAgentConfig{}, false
	}
	cfg := chat.AgentConfigs[agentName]
	config := inheritedSessionConfig{
		Mode:  strings.TrimSpace(cfg.Mode),
		Model: strings.TrimSpace(cfg.Model),
	}
	if config.empty() {
		return ChatAgentConfig{}, false
	}
	return ChatAgentConfig{Mode: config.Mode, Model: config.Model}, true
}

func inheritedSessionConfigFromPreviousSession(previous Session, ok bool, agentName string) inheritedSessionConfig {
	if !ok || strings.TrimSpace(previous.AgentName) != strings.TrimSpace(agentName) {
		return inheritedSessionConfig{}
	}
	return inheritedSessionConfig{
		Mode:  strings.TrimSpace(currentModeDisplay(previous)),
		Model: strings.TrimSpace(currentModelDisplay(previous)),
	}
}

func (c inheritedSessionConfig) empty() bool {
	return c.Mode == "" && c.Model == ""
}

func newSessionStateReady(session Session) bool {
	return currentModeDisplay(session) != "" && currentModelDisplay(session) != ""
}

func newSessionStatePartial(session Session) bool {
	return currentModeDisplay(session) != "" || currentModelDisplay(session) != ""
}

func latestSessionForKey(store *SessionStore, key SessionKey, fallback Session) Session {
	if store == nil {
		return fallback
	}
	if session, ok := store.Get(key); ok {
		return session
	}
	return fallback
}

func sessionWithACPInfo(session Session, info acp.SessionInfo, cwd string, workspace string) Session {
	session.ACPSessionID = info.SessionID
	session.Cwd = filepath.Clean(cwd)
	session.Workspace = strings.TrimSpace(workspace)
	session.ACPMeta = maps.Clone(info.Meta)
	session.AvailableCommands = append([]acp.AvailableCommand(nil), info.AvailableCommands...)
	session.ConfigOptions = append([]acp.SessionConfigOption(nil), info.ConfigOptions...)
	session.Models = info.Models
	session.Mode = info.Mode
	return session
}

func commitCurrentACPSessionReplacement(candidate acpSessionCandidate, store *SessionStore, previousACPSessionID string, replacement Session) (Session, error) {
	if store == nil {
		if err := candidate.Commit(nil); err != nil {
			return Session{}, err
		}
		return replacement, nil
	}
	session := replacement
	replaced := false
	if err := candidate.Commit(func() error {
		var persistErr error
		session, replaced, persistErr = store.ReplaceCurrentACPSession(previousACPSessionID, replacement)
		if persistErr == nil && !replaced {
			return errCurrentSessionChanged
		}
		return persistErr
	}); err != nil {
		return Session{}, err
	}
	return session, nil
}

type newSessionRequest struct {
	Cwd         string
	Title       string
	ManualTitle bool
}

func isExplicitPathArg(arg string) bool {
	arg = strings.TrimSpace(arg)
	return filepath.IsAbs(arg) ||
		arg == "~" ||
		strings.HasPrefix(arg, "~/") ||
		arg == "." ||
		arg == ".." ||
		strings.HasPrefix(arg, "./") ||
		strings.HasPrefix(arg, "../") ||
		strings.HasPrefix(arg, `.\\`) ||
		strings.HasPrefix(arg, `..\\`) ||
		strings.HasPrefix(arg, "$HOME/") ||
		strings.HasPrefix(arg, "${HOME}/")
}

const maxSessionTitleRunes = 40

func normalizeSessionTitle(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return ""
	}
	runes := []rune(title)
	if len(runes) <= maxSessionTitleRunes {
		return title
	}
	return string(runes[:maxSessionTitleRunes]) + "..."
}

func titleFromPrompt(text string) string {
	if strings.TrimSpace(text) == mentionOnlyPromptText {
		return ""
	}
	return normalizeSessionTitle(text)
}

func updateAutomaticSessionTitleInStore(ctx context.Context, store *SessionStore, session Session, userText string) Session {
	if session.ManualTitle {
		return session
	}
	title := titleFromPrompt(userText)
	if title == "" {
		return session
	}
	if store == nil {
		session.Title = title
		return session
	}
	updated, err := store.UpdateAutomaticTitle(session, title)
	if err != nil {
		slog.WarnContext(ctx, "保存自动会话标题失败", "session", session.ACPSessionID, "错误", err)
		return session
	}
	return updated
}

func displaySessionTitle(session Session) string {
	if strings.TrimSpace(session.Title) != "" {
		return session.Title
	}
	return "(未命名)"
}

func sessionKeyFromMessage(msg feishu.Message) SessionKey {
	keys := sessionKeysFromMessage(msg)
	if len(keys) == 0 {
		return imSessionKey(msg.BotID, msg.ChatID, "")
	}
	return keys[0]
}

func chatKeyFromMessage(msg feishu.Message) ChatKey {
	return ChatKey{BotID: msg.BotID, ChatID: msg.ChatID}
}

func messageSessionBindingLookupIDs(msg feishu.Message) []string {
	candidates := []string{
		msg.RootID,
		msg.ParentID,
	}
	if msg.Reply != nil {
		candidates = append(candidates, msg.Reply.MessageID)
	}
	candidates = append(candidates, msg.MessageID)
	out := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, id := range candidates {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func sessionKeysFromMessage(msg feishu.Message) []SessionKey {
	if msg.IsPrivateChat() {
		if msg.ChatID == "" {
			return nil
		}
		return []SessionKey{imSessionKey(msg.BotID, msg.ChatID, "")}
	}
	if !msg.IsTopicThread() && !msg.IsTopicGroup() {
		if msg.ChatID == "" {
			return nil
		}
		return []SessionKey{imSessionKey(msg.BotID, msg.ChatID, "")}
	}
	id := strings.TrimSpace(msg.ThreadID)
	if id == "" {
		id = strings.TrimSpace(msg.MessageID)
	}
	if msg.ChatID == "" || id == "" {
		return nil
	}
	return []SessionKey{imSessionKey(msg.BotID, msg.ChatID, id)}
}

func imSessionKey(botID, chatID, threadID string) SessionKey {
	return normalizeSessionKey(SessionKey{
		BotID:  botID,
		Source: sessionSourceIM,
		MainID: chatID,
		SubID:  threadID,
	})
}

func sessionLabel(msg feishu.Message) string {
	if msg.IsPrivateChat() {
		return "当前私聊会话"
	}
	if !msg.IsTopicThread() {
		return "当前群聊会话"
	}
	return "当前话题会话"
}

func displayBotID(botID string) string {
	if strings.TrimSpace(botID) == "" {
		return "default"
	}
	return botID
}

func stripMentionNames(text string, mentions []feishu.Mention) string {
	for _, mention := range mentions {
		if mention.Name == "" {
			continue
		}
		text = strings.ReplaceAll(text, "@"+mention.Name, "")
	}
	return strings.TrimSpace(text)
}
