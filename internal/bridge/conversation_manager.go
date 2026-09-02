package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

var errCurrentSessionChanged = errors.New("current session changed")

// conversationManager 负责飞书对话与 ACP session 之间的映射和生命周期。
// 跨领域行为通过窄回调注入，避免组件反向依赖整个 Service。
type conversationManager struct {
	stores   map[string]*SessionStore
	registry *acp.Registry
	runtime  acpRuntime
	hooks    conversationManagerHooks
}

type conversationManagerHooks struct {
	cancelRunningSessionWork func(context.Context, SessionKey)
	subscribeACPStateUpdates func(context.Context, feishu.Message, SessionKey)
	setSessionMode           func(context.Context, feishu.Message, Session, string) (string, string, error)
	setSessionModel          func(context.Context, feishu.Message, Session, string) (string, string, error)
	clearACPError            func(Session)
}

func newConversationManager(registry *acp.Registry, runtime acpRuntime) conversationManager {
	return conversationManager{
		stores:   make(map[string]*SessionStore),
		registry: registry,
		runtime:  runtime,
	}
}

func (m *conversationManager) setRuntime(runtime acpRuntime) {
	if runtime != nil {
		m.runtime = runtime
	}
}

func (m *conversationManager) setStore(botID string, store *SessionStore) {
	if m.stores == nil {
		m.stores = make(map[string]*SessionStore)
	}
	m.stores[strings.TrimSpace(botID)] = store
}

func (m *conversationManager) loadStores(upgradedWorkspaceFiles map[string][]string) error {
	for botID, store := range m.stores {
		if store == nil {
			continue
		}
		if err := store.Load(); err != nil {
			return err
		}
		if files := upgradedWorkspaceFiles[strings.TrimSpace(botID)]; len(files) > 0 {
			count, err := store.ResetWorkspacePromptedForAllSessions()
			if err != nil {
				return fmt.Errorf("重置 bot %q 的 workspace prompt 状态: %w", botID, err)
			}
			if count > 0 {
				slog.Info("workspace 已升级，重置 bot 会话 workspace prompt 状态", "bot", displayBotID(botID), "数量", count, "files", files)
			}
		}
		slog.Info("已加载持久化会话映射", "bot", displayBotID(botID), "数量", store.Count())
	}
	return nil
}

func (m *conversationManager) storeForBotID(botID string) *SessionStore {
	if m.stores == nil {
		return nil
	}
	if store := m.stores[strings.TrimSpace(botID)]; store != nil {
		return store
	}
	return m.stores[""]
}

func (m *conversationManager) storeForMessage(msg feishu.Message) *SessionStore {
	return m.storeForBotID(msg.BotID)
}

func (m *conversationManager) cancelRunningSessionWork(ctx context.Context, key SessionKey) {
	if m.hooks.cancelRunningSessionWork != nil {
		m.hooks.cancelRunningSessionWork(ctx, key)
	}
}

func (m *conversationManager) subscribeACPStateUpdates(ctx context.Context, msg feishu.Message, key SessionKey) {
	if m.hooks.subscribeACPStateUpdates != nil {
		m.hooks.subscribeACPStateUpdates(ctx, msg, key)
	}
}

func (m *conversationManager) setSessionMode(ctx context.Context, msg feishu.Message, session Session, mode string) error {
	if m.hooks.setSessionMode == nil {
		return nil
	}
	_, _, err := m.hooks.setSessionMode(ctx, msg, session, mode)
	return err
}

func (m *conversationManager) setSessionModel(ctx context.Context, msg feishu.Message, session Session, model string) error {
	if m.hooks.setSessionModel == nil {
		return nil
	}
	_, _, err := m.hooks.setSessionModel(ctx, msg, session, model)
	return err
}

func (m *conversationManager) clearACPError(session Session) {
	if m.hooks.clearACPError != nil {
		m.hooks.clearACPError(session)
	}
}

func (s *conversationManager) newSession(ctx context.Context, fields []string, msg feishu.Message) string {
	session, _, source, errText := s.createSession(ctx, fields, msg)
	if errText != "" {
		return errText
	}
	session = s.waitForNewSessionState(ctx, msg, session.Key, session)
	return formatNewSessionReply(session, source)
}

func (s *conversationManager) createSession(ctx context.Context, fields []string, msg feishu.Message) (Session, config.AgentConfig, string, string) {
	slog.InfoContext(ctx, "准备创建ACP会话", "cmd", fields)
	store := s.storeForMessage(msg)
	if store == nil {
		return Session{}, config.AgentConfig{}, "", "会话持久化未初始化。"
	}
	plan, errText := s.resolveNewSessionPlan(fields, msg)
	if errText != "" {
		return Session{}, config.AgentConfig{}, "", errText
	}
	inheritConfig := s.inheritedSessionConfigForNewSession(msg, plan.AgentName)
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
	session = s.inheritNewSessionConfig(ctx, msg, session, inheritConfig)
	s.afterSessionCommitted(transition, session)
	slog.InfoContext(ctx, "创建 ACP session 成功", "agent", plan.AgentName, "cwd", plan.Cwd)
	return session, plan.Agent, plan.Source, ""
}

func (s *conversationManager) resolveNewSessionPlan(fields []string, msg feishu.Message) (newSessionPlan, string) {
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

func (s *conversationManager) prepareSessionTransition(ctx context.Context, msg feishu.Message) sessionTransition {
	key := sessionKeyFromMessage(msg)
	s.migrateSessionShowConfigToChat(ctx, msg)
	s.cancelRunningSessionWork(ctx, key)
	s.subscribeACPStateUpdates(ctx, msg, key)
	return sessionTransition{Key: key}
}

func (s *conversationManager) restoreSessionTransition(sessionTransition) {}

func (s *conversationManager) startACPSessionCandidate(ctx context.Context, key SessionKey, msg feishu.Message, plan newSessionPlan) (acpSessionCandidate, error) {
	return s.runtime.NewSession(ctx, key, plan.AgentName, plan.Agent, filepath.Clean(plan.Cwd), msg.Workspace)
}

func (s *conversationManager) inheritedSessionConfigForNewSession(msg feishu.Message, agentName string) inheritedSessionConfig {
	chat := s.chatConfigForMessage(msg)
	if cfg, ok := chatAgentSessionConfig(chat, agentName); ok {
		return inheritedSessionConfig{
			Mode:  cfg.Mode,
			Model: cfg.Model,
		}
	}
	previous, ok := s.findSession(msg)
	return inheritedSessionConfigFromPreviousSession(previous, ok, agentName)
}

func (s *conversationManager) inheritNewSessionConfig(ctx context.Context, msg feishu.Message, session Session, inherited inheritedSessionConfig) Session {
	if inherited.empty() {
		return session
	}
	session = s.waitForNewSessionState(ctx, msg, session.Key, session)
	return s.applyInheritedSessionConfig(ctx, msg, session, inherited)
}

func (s *conversationManager) applyInheritedSessionConfig(ctx context.Context, msg feishu.Message, session Session, inherited inheritedSessionConfig) Session {
	if inherited.empty() {
		return session
	}
	store := s.storeForMessage(msg)
	if inherited.Mode != "" {
		if err := s.setSessionMode(ctx, msg, session, inherited.Mode); err != nil {
			slog.WarnContext(ctx, "继承上次 ACP session mode 失败", "mode", inherited.Mode, "session", session.ACPSessionID, "错误", err)
		} else {
			session = latestSessionForKey(store, session.Key, session)
		}
	}
	if inherited.Model != "" {
		if err := s.setSessionModel(ctx, msg, session, inherited.Model); err != nil {
			slog.WarnContext(ctx, "继承上次 ACP session model 失败", "model", inherited.Model, "session", session.ACPSessionID, "错误", err)
		} else {
			session = latestSessionForKey(store, session.Key, session)
		}
	}
	return latestSessionForKey(store, session.Key, session)
}

func (s *conversationManager) afterSessionCommitted(transition sessionTransition, session Session) {
	s.clearACPError(session)
}

func (s *conversationManager) waitForNewSessionState(ctx context.Context, msg feishu.Message, key SessionKey, session Session) Session {
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

func (s *conversationManager) resolveNewSessionRequest(fields []string, msg feishu.Message) (newSessionRequest, string, string) {
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

func (s *conversationManager) defaultNewSessionCwd(msg feishu.Message) (string, string, string) {
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

func (s *conversationManager) resolveNewSessionCwdArg(arg string, msg feishu.Message) (string, bool, string) {
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

func (s *conversationManager) updateAutomaticSessionTitle(ctx context.Context, msg feishu.Message, session Session, userText string) Session {
	return updateAutomaticSessionTitleInStore(ctx, s.storeForMessage(msg), session, userText)
}

func (s *conversationManager) defaultAgentName() string {
	names := s.registry.Names()
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func (s *conversationManager) chatAgentName(msg feishu.Message) string {
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

func (s *conversationManager) findSession(msg feishu.Message) (Session, bool) {
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

func (s *conversationManager) resumeSessionByID(ctx context.Context, msg feishu.Message, acpSessionID string, expectedCurrentACPSessionID *string) (Session, string) {
	store := s.storeForMessage(msg)
	if store == nil {
		return Session{}, "会话持久化未初始化。"
	}
	acpSessionID = strings.TrimSpace(acpSessionID)
	if acpSessionID == "" {
		return Session{}, "会话 ID 不能为空。"
	}
	key := sessionKeyFromMessage(msg)
	var (
		session  Session
		restored bool
		err      error
	)
	expectedSessionID := ""
	if current, ok := store.Get(key); ok {
		expectedSessionID = current.ACPSessionID
	}
	if expectedCurrentACPSessionID != nil {
		expectedSessionID = strings.TrimSpace(*expectedCurrentACPSessionID)
	}
	session, restored, err = s.runtime.TransitionCurrentSession(key, expectedSessionID, func() (Session, bool, error) {
		if expectedCurrentACPSessionID == nil {
			return store.ResumeSessionIfCurrent(key, expectedSessionID, acpSessionID)
		}
		return store.ResumeSessionIfCurrent(key, *expectedCurrentACPSessionID, acpSessionID)
	})
	if err != nil {
		if restored {
			slog.WarnContext(ctx, "恢复会话后关闭旧 ACP runtime 失败", "key", key, "错误", err)
			return session, ""
		}
		slog.ErrorContext(ctx, "恢复会话映射失败", "错误", err)
		return Session{}, "恢复会话失败：" + err.Error()
	}
	if !restored {
		return Session{}, "当前会话已变化，或选择的会话不存在，请重新发送 /session list。"
	}
	return session, ""
}

func (s *conversationManager) refreshACPSession(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig) (Session, error) {
	cwd := strings.TrimSpace(session.Cwd)
	if cwd == "" {
		return Session{}, fmt.Errorf("当前会话缺少工作目录，无法重建 ACP session")
	}
	inheritConfig := inheritedSessionConfigFromPreviousSession(session, true, session.AgentName)
	previousACPSessionID := session.ACPSessionID
	slog.WarnContext(ctx, "持久化 ACP session 不可恢复，准备重建", "session", session.ACPSessionID, "cwd", cwd)
	candidate, err := s.runtime.NewSession(ctx, session.Key, session.AgentName, agent, filepath.Clean(cwd), sessionWorkspace(session, msg))
	if err != nil {
		return Session{}, fmt.Errorf("重建 ACP session 失败: %w", err)
	}
	defer candidate.Abort()
	sessionInfo := candidate.Info()
	workspace := sessionWorkspace(session, msg)
	session = sessionWithACPInfo(session, sessionInfo, cwd, workspace)
	store := s.storeForMessage(msg)
	session, err = commitCurrentACPSessionReplacement(candidate, store, previousACPSessionID, session)
	if err != nil {
		if errors.Is(err, errCurrentSessionChanged) {
			return Session{}, fmt.Errorf("当前会话已变化，忽略旧会话的重建结果")
		}
		if store == nil {
			return Session{}, fmt.Errorf("激活重建后的 ACP session 失败: %w", err)
		}
		return Session{}, fmt.Errorf("保存重建后的 ACP session 失败: %w", err)
	}
	session = s.inheritNewSessionConfig(ctx, msg, session, inheritConfig)
	slog.InfoContext(ctx, "已重建 ACP session", "session", session.ACPSessionID, "cwd", session.Cwd)
	return session, nil
}

func (s *conversationManager) chatConfigForMessage(msg feishu.Message) ChatConfig {
	chat := ChatConfig{Key: chatKeyFromMessage(msg)}
	store := s.storeForMessage(msg)
	if store == nil {
		return chat
	}
	if existing, ok := store.GetChat(chat.Key); ok {
		return existing
	}
	if session, ok := s.findSession(msg); ok {
		chat.WikiDisabled = session.WikiDisabled
		chat.WikiIntervalSec = session.WikiIntervalSec
		chat.HideStepMessages = session.HideStepMessages
		chat.HidePlans = session.HidePlans
		chat.ShowThoughts = session.ShowThoughts
		chat.HideThoughts = session.HideThoughts
		chat.HideTools = session.HideTools
		chat.HideStatusBar = session.HideStatusBar
		chat.HideUsageDetail = session.HideUsageDetail
	}
	return chat
}

func (s *conversationManager) migrateSessionShowConfigToChat(ctx context.Context, msg feishu.Message) {
	store := s.storeForMessage(msg)
	if store == nil {
		return
	}
	chatKey := chatKeyFromMessage(msg)
	session, ok := s.findSession(msg)
	if !ok || !sessionHasShowConfig(session) {
		return
	}
	chat := ChatConfig{
		Key:              chatKey,
		WikiDisabled:     session.WikiDisabled,
		WikiIntervalSec:  session.WikiIntervalSec,
		HideStepMessages: session.HideStepMessages,
		HidePlans:        session.HidePlans,
		ShowThoughts:     session.ShowThoughts,
		HideThoughts:     session.HideThoughts,
		HideTools:        session.HideTools,
		HideStatusBar:    session.HideStatusBar,
		HideUsageDetail:  session.HideUsageDetail,
	}
	if _, err := store.InsertChatIfAbsent(chat); err != nil {
		slog.ErrorContext(ctx, "迁移会话展示配置到 chat 配置失败", "chat", msg.ChatID, "错误", err)
	}
}

func (s *conversationManager) selectionSession(msg feishu.Message, acpSessionID string, expiredMessage string) (Session, error) {
	store := s.storeForMessage(msg)
	if store == nil {
		return Session{}, fmt.Errorf("会话持久化未初始化")
	}
	for _, key := range callbackSessionKeys(msg) {
		session, ok := store.Get(key)
		if ok && session.ACPSessionID == acpSessionID {
			return session, nil
		}
	}
	return Session{}, errors.New(expiredMessage)
}
