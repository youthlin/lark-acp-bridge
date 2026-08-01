package bridge

import (
	"context"
	"log/slog"
	"maps"
	"os"
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
	Key            SessionKey
	PendingWiki    pendingWikiRun
	HasPendingWiki bool
}

func (s *Service) newSession(ctx context.Context, fields []string, msg feishu.Message) string {
	session, _, source, errText := s.createSession(ctx, fields, msg)
	if errText != "" {
		return errText
	}
	session = s.waitForNewSessionState(ctx, msg, session.Key, session)
	return formatNewSessionReply(session, source)
}

func (s *Service) createSession(ctx context.Context, fields []string, msg feishu.Message) (Session, config.AgentConfig, string, string) {
	slog.InfoContext(ctx, "准备创建ACP会话", "cmd", fields)
	store := s.storeForMessage(msg)
	if store == nil {
		return Session{}, config.AgentConfig{}, "", "会话持久化未初始化。"
	}
	plan, errText := s.resolveNewSessionPlan(fields, msg)
	if errText != "" {
		return Session{}, config.AgentConfig{}, "", errText
	}
	if _, err := ensureWorkspace(msg.Workspace, msg.BotID); err != nil {
		slog.ErrorContext(ctx, "初始化 workspace 失败", "workspace", msg.Workspace, "错误", err)
		return Session{}, config.AgentConfig{}, "", "初始化 workspace 失败：" + err.Error()
	}
	transition := s.prepareSessionTransition(ctx, msg)
	candidate, err := s.startACPSessionCandidate(ctx, transition.Key, msg, plan)
	if err != nil {
		s.restoreSessionTransition(transition)
		slog.ErrorContext(ctx, "创建 ACP session 失败", "agent", plan.AgentName, "cwd", plan.Cwd, "错误", err)
		return Session{}, config.AgentConfig{}, "", "创建 ACP session 失败：" + err.Error()
	}
	defer candidate.Abort()
	session := newSessionFromCandidate(transition.Key, msg, plan, candidate)
	if err := commitNewSessionCandidate(candidate, store, plan, &session); err != nil {
		s.restoreSessionTransition(transition)
		slog.ErrorContext(ctx, "保存会话映射失败", "错误", err)
		return Session{}, config.AgentConfig{}, "", "保存会话映射失败：" + err.Error()
	}
	s.afterSessionCommitted(transition, session)
	slog.InfoContext(ctx, "创建 ACP session 成功", "agent", plan.AgentName, "cwd", plan.Cwd)
	return session, plan.Agent, plan.Source, ""
}

func (s *Service) resolveNewSessionPlan(fields []string, msg feishu.Message) (newSessionPlan, string) {
	req, source, errText := s.resolveNewSessionRequest(fields, msg)
	if errText != "" {
		return newSessionPlan{}, errText
	}
	cwd := req.Cwd
	if !filepath.IsAbs(cwd) {
		return newSessionPlan{}, "工作目录必须是绝对路径，可使用 /absolute/path 或 ~/path。"
	}
	if info, err := os.Stat(cwd); err != nil {
		return newSessionPlan{}, "工作目录不可访问：" + err.Error()
	} else if !info.IsDir() {
		return newSessionPlan{}, "工作目录不是目录：" + cwd
	}
	agentName := s.chatAgentName(msg)
	agent, ok := s.registry.Get(agentName)
	if !ok {
		return newSessionPlan{}, "未找到当前聊天选择的 agent 配置：" + agentName
	}
	return newSessionPlan{
		Req:             req,
		Source:          source,
		UseDefaultTitle: req.Title == "",
		Cwd:             cwd,
		AgentName:       agentName,
		Agent:           agent,
	}, ""
}

func (s *Service) prepareSessionTransition(ctx context.Context, msg feishu.Message) sessionTransition {
	key := sessionKeyFromMessage(msg)
	s.migrateSessionShowConfigToChat(ctx, msg)
	pendingWiki, hasPendingWiki := s.takePendingWiki(key)
	s.cancelRunningSessionWork(ctx, key)
	s.subscribeACPStateUpdates(ctx, msg, key)
	return sessionTransition{Key: key, PendingWiki: pendingWiki, HasPendingWiki: hasPendingWiki}
}

func (s *Service) restoreSessionTransition(transition sessionTransition) {
	if transition.HasPendingWiki {
		s.restorePendingWiki(transition.PendingWiki)
	}
}

func (s *Service) startACPSessionCandidate(ctx context.Context, key SessionKey, msg feishu.Message, plan newSessionPlan) (acpSessionCandidate, error) {
	return s.runtime.NewSession(ctx, key, plan.AgentName, plan.Agent, filepath.Clean(plan.Cwd), msg.Workspace)
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

func (s *Service) afterSessionCommitted(transition sessionTransition, session Session) {
	if transition.HasPendingWiki {
		s.runPendingWikiAsync(transition.PendingWiki)
	}
	s.clearACPError(session)
}

func (s *Service) waitForNewSessionState(ctx context.Context, msg feishu.Message, key SessionKey, session Session) Session {
	store := s.storeForMessage(msg)
	session = latestSessionForKey(store, key, session)
	if newSessionStateReady(session) {
		return session
	}
	timer := time.NewTimer(newSessionStateWait)
	defer timer.Stop()
	var partialTimer *time.Timer
	var partialTimerC <-chan time.Time
	defer func() {
		if partialTimer != nil {
			partialTimer.Stop()
		}
	}()
	if newSessionStatePartial(session) {
		partialTimer = time.NewTimer(newSessionPartialStateWait)
		partialTimerC = partialTimer.C
	}
	updated := make(chan struct{}, 1)
	unsub := s.runtime.SubscribeUpdates(key, func(sessionID string, update acp.SessionUpdate) {
		if sessionID != session.ACPSessionID || !isACPStateUpdate(update) {
			return
		}
		select {
		case updated <- struct{}{}:
		default:
		}
	})
	defer unsub()
	for {
		select {
		case <-ctx.Done():
			return latestSessionForKey(store, key, session)
		case <-timer.C:
			return latestSessionForKey(store, key, session)
		case <-partialTimerC:
			return latestSessionForKey(store, key, session)
		case <-updated:
			current := latestSessionForKey(store, key, session)
			if newSessionStateReady(current) {
				return current
			}
			if newSessionStatePartial(current) && partialTimer == nil {
				partialTimer = time.NewTimer(newSessionPartialStateWait)
				partialTimerC = partialTimer.C
			}
		}
	}
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

func (s *Service) resolveNewSessionRequest(fields []string, msg feishu.Message) (newSessionRequest, string, string) {
	args := fields[1:]
	req := newSessionRequest{}
	if len(args) > 0 {
		if args[0] == "--title" || args[0] == "-t" {
			req.Title = normalizeSessionTitle(strings.Join(args[1:], " "))
			req.ManualTitle = req.Title != ""
		} else {
			candidate, isPath, errText := s.resolveNewSessionCwdArg(args[0], msg)
			if errText != "" {
				return newSessionRequest{}, "", errText
			}
			if isPath {
				req.Cwd = candidate
				req.Title = normalizeSessionTitle(strings.Join(args[1:], " "))
				req.ManualTitle = req.Title != ""
			} else {
				req.Title = normalizeSessionTitle(strings.Join(args, " "))
				req.ManualTitle = req.Title != ""
			}
		}
	}
	if req.Cwd != "" {
		return req, "命令参数", ""
	}
	cwd, source, errText := s.defaultNewSessionCwd(msg)
	if errText != "" {
		return newSessionRequest{}, "", errText
	}
	req.Cwd = cwd
	return req, source, ""
}

func (s *Service) defaultNewSessionCwd(msg feishu.Message) (string, string, string) {
	if session, ok := s.findSession(msg); ok && session.Cwd != "" {
		return session.Cwd, "当前会话已有会话", ""
	}
	agentName := s.chatAgentName(msg)
	agent, ok := s.registry.Get(agentName)
	if !ok || strings.TrimSpace(agent.DefaultCwd) == "" {
		if agentName == "" {
			return "", "", "当前会话还没有会话映射，且未配置 ACP agent。请使用 /new <cwd> 指定工作目录。"
		}
		return "", "", "当前会话还没有会话映射，且当前 agent " + agentName + " 未配置 default_cwd。请使用 /new <cwd> 指定工作目录。"
	}
	return agent.DefaultCwd, "默认配置", ""
}

func (s *Service) resolveNewSessionCwdArg(arg string, msg feishu.Message) (string, bool, string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", false, ""
	}
	looksPath := isExplicitPathArg(arg)
	if !looksPath {
		return "", false, ""
	}
	candidate, err := config.ExpandPath(arg)
	if err != nil {
		return "", false, "展开工作目录失败：" + err.Error()
	}
	if !filepath.IsAbs(candidate) {
		base, _, errText := s.defaultNewSessionCwd(msg)
		if errText != "" {
			return "", false, errText
		}
		candidate = filepath.Join(base, candidate)
	}
	info, statErr := os.Stat(candidate)
	if statErr == nil {
		if !info.IsDir() {
			return "", false, "工作目录不是目录：" + candidate
		}
		return candidate, true, ""
	}
	return "", false, "工作目录不可访问：" + statErr.Error()
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

func (s *Service) updateAutomaticSessionTitle(ctx context.Context, msg feishu.Message, session Session, userText string) Session {
	return updateAutomaticSessionTitleInStore(ctx, s.storeForMessage(msg), session, userText)
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

func (s *Service) defaultAgentName() string {
	names := s.registry.Names()
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func (s *Service) chatAgentName(msg feishu.Message) string {
	chat := s.chatConfigForMessage(msg)
	if strings.TrimSpace(chat.AgentName) != "" {
		return chat.AgentName
	}
	if session, ok := s.findSession(msg); ok {
		if _, ok := s.registry.Get(session.AgentName); ok {
			return session.AgentName
		}
	}
	return s.defaultAgentName()
}

func (s *Service) storeForMessage(msg feishu.Message) *SessionStore {
	if s.stores == nil {
		return nil
	}
	if store := s.stores[msg.BotID]; store != nil {
		return store
	}
	return s.stores[""]
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

func (s *Service) findSession(msg feishu.Message) (Session, bool) {
	store := s.storeForMessage(msg)
	if store == nil {
		return Session{}, false
	}
	for _, messageID := range messageSessionBindingLookupIDs(msg) {
		if session, _, ok := store.SessionForMessage(msg.BotID, msg.ChatID, messageID); ok {
			return session, true
		}
	}
	for _, key := range sessionKeysFromMessage(msg) {
		if session, ok := store.Get(key); ok {
			return session, true
		}
	}
	return Session{}, false
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
		ChatID: chatID,
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
