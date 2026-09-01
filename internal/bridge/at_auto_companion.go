package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

type atAutoCompanionKey struct {
	Source    SessionKey `json:"source"`
	AgentName string     `json:"agent_name"`
}

func (s *Service) handleAtAutoPromptMessage(ctx context.Context, incoming incomingPromptMessage, promptText string) (string, error) {
	if s.queueAtAutoMessageIfBusy(incoming.msg) {
		return "", nil
	}
	source, agent, err := s.prepareAtAutoCompanionSource(incoming.msg)
	if err != nil {
		return "", err
	}
	decisionPrompt := s.formatAtAutoDecisionPrompt(incoming.msg, source, promptText)
	if strings.TrimSpace(decisionPrompt) == "" {
		return "", nil
	}
	if !s.beginAtAutoFlowOrQueue(source.Key, pendingAtMessageFromMessage(incoming.msg)) {
		return "", nil
	}
	defer func() {
		s.continuePendingAtAutoAfterFlow(ctx, incoming.msg, source, agent)
	}()
	respond, err := s.shouldAtAutoCompanionRespond(ctx, incoming.msg, source, agent, decisionPrompt)
	if errors.Is(err, errSessionTaskBusy) {
		if s.queueAtAutoMessageIfBusy(incoming.msg) {
			return "", nil
		}
	}
	if err != nil {
		return "", err
	}
	if !respond {
		return "", nil
	}
	return s.promptWithOptions(ctx, incoming.msg, promptText, promptSessionOptions{
		SkipPendingAtAutoDrain: true,
		EnableAtAutoQueue:      true,
	})
}

func (s *Service) beginAtAutoFlow(key SessionKey) bool {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.atAutoFlows[key] {
		return false
	}
	s.atAutoFlows[key] = true
	return true
}

func (s *Service) beginAtAutoFlowOrQueue(key SessionKey, entry pendingAtMessage) bool {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	task := s.tasks[key]
	if s.atAutoFlows[key] || (task != nil && task.queuePendingAtAuto) {
		pending := append(s.pendingAtAuto[key], entry)
		if len(pending) > maxPendingAtAuto {
			pending = append([]pendingAtMessage(nil), pending[len(pending)-maxPendingAtAuto:]...)
		}
		s.pendingAtAuto[key] = pending
		return false
	}
	s.atAutoFlows[key] = true
	return true
}

func (s *Service) takePendingAtAutoForFlow(key SessionKey, releaseCurrent bool) []pendingAtMessage {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if releaseCurrent {
		delete(s.atAutoFlows, key)
	}
	if s.atAutoFlows[key] {
		return nil
	}
	pending := append([]pendingAtMessage(nil), s.pendingAtAuto[key]...)
	if len(pending) == 0 {
		return nil
	}
	delete(s.pendingAtAuto, key)
	s.atAutoFlows[key] = true
	return pending
}

func (s *Service) finishAtAutoFlow(key SessionKey) {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	delete(s.atAutoFlows, key)
	s.taskMu.Unlock()
}

func (s *Service) prepareAtAutoCompanionSource(msg feishu.Message) (Session, config.AgentConfig, error) {
	agentName := strings.TrimSpace(s.chatAgentName(msg))
	agent, ok := s.registry.Get(agentName)
	if !ok {
		return Session{}, config.AgentConfig{}, fmt.Errorf("未找到 agent 配置: %s", agentName)
	}
	key := normalizeSessionKey(sessionKeyFromMessage(msg))
	source := Session{
		Key:       key,
		AgentName: agentName,
		Cwd:       strings.TrimSpace(agent.DefaultCwd),
		Workspace: strings.TrimSpace(firstNonEmpty(msg.Workspace, s.botWorkspace(msg.BotID))),
	}
	if existing, ok := s.findSession(msg); ok {
		existing.Key = normalizeSessionKey(existing.Key)
		if existing.AgentName == agentName {
			source = existing
			source.Workspace = strings.TrimSpace(sessionWorkspace(existing, msg))
			if strings.TrimSpace(source.Cwd) == "" {
				source.Cwd = strings.TrimSpace(agent.DefaultCwd)
			}
		} else {
			source.Key = existing.Key
			if strings.TrimSpace(source.Workspace) == "" {
				source.Workspace = strings.TrimSpace(sessionWorkspace(existing, msg))
			}
		}
	}
	if !source.Key.Valid() {
		return Session{}, config.AgentConfig{}, fmt.Errorf("当前消息无法定位 at-auto 会话")
	}
	if strings.TrimSpace(source.Cwd) == "" {
		return Session{}, config.AgentConfig{}, fmt.Errorf("当前会话还没有会话映射，且当前 agent %s 未配置 default_cwd，无法执行 at-auto 判断", agentName)
	}
	if strings.TrimSpace(source.Workspace) == "" {
		source.Workspace = strings.TrimSpace(s.botWorkspace(msg.BotID))
	}
	return source, agent, nil
}

func (s *Service) shouldAtAutoCompanionRespond(ctx context.Context, msg feishu.Message, source Session, agent config.AgentConfig, prompt string) (bool, error) {
	result, err := s.runAtAutoCompanionPrompt(ctx, msg, source, agent, prompt)
	if err != nil {
		return false, err
	}
	return isAtAutoDecisionPositive(result.Text), nil
}

func (s *Service) runAtAutoCompanionPrompt(ctx context.Context, msg feishu.Message, source Session, agent config.AgentConfig, prompt string) (acp.PromptResult, error) {
	source.Key = normalizeSessionKey(source.Key)
	source.AgentName = strings.TrimSpace(source.AgentName)
	source.Cwd = filepath.Clean(strings.TrimSpace(source.Cwd))
	source.Workspace = strings.TrimSpace(source.Workspace)
	store := s.atAutoCompanionStateStore(source.Key.BotID, source.Workspace)
	stateKey := atAutoCompanionStateID(source.Key, source.AgentName)
	runtime := atAutoCompanionRuntimeKey(source.Key, source.AgentName)
	companion := s.atAutoCompanionFromState(source, store, stateKey)
	opts := atAutoCompanionPromptTaskOptions(source.Key, runtime)
	taskCtx, finish, task, err := s.startTaskWithOptionsDetailed(ctx, companion, agent, taskKindUser, opts)
	if err != nil {
		return acp.PromptResult{}, err
	}
	defer finish()
	if strings.TrimSpace(companion.ACPSessionID) == "" {
		companion, runtime, err = s.createAtAutoCompanionSession(taskCtx, source, agent, store, stateKey)
		if err != nil {
			return acp.PromptResult{}, err
		}
		s.updateRunningTaskRuntime(source.Key, task, runtime, companion)
	}
	result, err := s.promptAtAutoCompanionRuntime(taskCtx, msg, companion, runtime, agent, prompt, stateKey, store)
	if !errors.Is(err, errACPSessionUnavailable) {
		return result, err
	}
	companion, runtime, err = s.createAtAutoCompanionSession(taskCtx, source, agent, store, stateKey)
	if err != nil {
		return acp.PromptResult{}, err
	}
	s.updateRunningTaskRuntime(source.Key, task, runtime, companion)
	return s.promptAtAutoCompanionRuntime(taskCtx, msg, companion, runtime, agent, prompt, stateKey, store)
}

func (s *Service) promptAtAutoCompanionRuntime(ctx context.Context, msg feishu.Message, companion Session, runtime runtimeKey, agent config.AgentConfig, prompt string, stateKey string, store *wikiStateStore) (acp.PromptResult, error) {
	recorder := s.newTraceRecorderWithMessageID(companion, prompt, atAutoTraceMessageID(msg, companion))
	result, err := s.runtime.PromptWithRuntimeKey(ctx, runtime, companion, agent, prompt, tracePromptOptions(recorder, acp.PromptOptions{}))
	if recorder != nil {
		recorder.Complete(result, err)
	}
	if err == nil {
		s.touchAtAutoCompanionState(store, stateKey)
	}
	return result, err
}

func (s *Service) atAutoCompanionFromState(source Session, store *wikiStateStore, stateKey string) Session {
	if store == nil {
		return atAutoCompanionSession(source, wikiCompanionState{AgentName: source.AgentName})
	}
	state := store.snapshot().AtAutoCompanions[stateKey]
	if strings.TrimSpace(state.AgentName) == "" {
		state.AgentName = source.AgentName
	}
	return atAutoCompanionSession(source, state)
}

func (s *Service) createAtAutoCompanionSession(ctx context.Context, source Session, agent config.AgentConfig, store *wikiStateStore, stateKey string) (Session, runtimeKey, error) {
	runtime := atAutoCompanionRuntimeKey(source.Key, source.AgentName)
	key := runtime.SessionKey
	candidate, err := s.runtime.NewSessionWithRuntimeKey(ctx, runtime, key, source.AgentName, agent, source.Cwd, source.Workspace)
	if err != nil {
		return Session{}, runtime, fmt.Errorf("创建 at-auto companion session: %w", err)
	}
	defer candidate.Abort()
	info := candidate.Info()
	now := time.Now()
	state := wikiCompanionState{AgentName: source.AgentName, ACPSessionID: info.SessionID, CreatedAt: now, UpdatedAt: now}
	err = candidate.Commit(func() error {
		if store == nil {
			return nil
		}
		return store.update(func(current *wikiState) {
			current.AtAutoCompanions[stateKey] = state
		})
	})
	if err != nil {
		return Session{}, runtime, fmt.Errorf("保存 at-auto companion session: %w", err)
	}
	return atAutoCompanionSession(source, state), runtime, nil
}

func (s *Service) touchAtAutoCompanionState(store *wikiStateStore, stateKey string) {
	if store == nil || strings.TrimSpace(stateKey) == "" {
		return
	}
	if err := store.update(func(state *wikiState) {
		companion := state.AtAutoCompanions[stateKey]
		if strings.TrimSpace(companion.ACPSessionID) == "" {
			return
		}
		companion.UpdatedAt = time.Now()
		state.AtAutoCompanions[stateKey] = companion
	}); err != nil {
		slog.Warn("更新 at-auto companion 状态失败", "错误", err)
	}
}

func (s *Service) atAutoCompanionStateStore(botID string, workspace string) *wikiStateStore {
	workspace = normalizeWorkspaceLockPath(workspace)
	if workspace == "" {
		return nil
	}
	s.traceStoreMu.Lock()
	defer s.traceStoreMu.Unlock()
	if store := s.companionStateStores[workspace]; store != nil {
		return store
	}
	store := newWikiStateStore(workspace)
	if err := store.Load(); err != nil {
		slog.Warn("加载 at-auto companion 状态失败", "bot", displayBotID(botID), "workspace", workspace, "错误", err)
		return nil
	}
	s.companionStateStores[workspace] = store
	return store
}

func atAutoCompanionSession(source Session, companion wikiCompanionState) Session {
	agentName := strings.TrimSpace(firstNonEmpty(companion.AgentName, source.AgentName))
	return Session{
		Key:          atAutoCompanionRuntimeKey(source.Key, agentName).SessionKey,
		Title:        "at-auto companion " + atAutoCompanionTitleSuffix(source.Key, agentName),
		AgentName:    agentName,
		ACPSessionID: strings.TrimSpace(companion.ACPSessionID),
		Cwd:          strings.TrimSpace(source.Cwd),
		Workspace:    strings.TrimSpace(source.Workspace),
	}
}

func atAutoCompanionRuntimeKey(source SessionKey, agentName string) runtimeKey {
	key := normalizeSessionKey(source)
	agentName = strings.TrimSpace(agentName)
	return runtimeKey{
		SessionKey: SessionKey{
			BotID:  key.BotID,
			Source: runtimeScopeAtAuto,
			MainID: sessionKeyMainID(key),
			SubID:  key.SubID,
		},
		Scope: runtimeScopeAtAuto,
		RunID: agentName,
	}
}

func atAutoCompanionStateID(source SessionKey, agentName string) string {
	payload := atAutoCompanionKey{
		Source:    normalizeSessionKey(source),
		AgentName: strings.TrimSpace(agentName),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return strings.Join([]string{payload.Source.BotID, sessionKeySource(payload.Source), sessionKeyMainID(payload.Source), payload.Source.SubID, payload.AgentName}, "|")
	}
	return string(data)
}

func atAutoCompanionTitleSuffix(source SessionKey, agentName string) string {
	key := normalizeSessionKey(source)
	parts := []string{sessionKeyMainID(key)}
	if strings.TrimSpace(key.SubID) != "" {
		parts = append(parts, key.SubID)
	}
	if strings.TrimSpace(agentName) != "" {
		parts = append(parts, agentName)
	}
	return strings.Join(parts, "/")
}

func (s *Service) formatAtAutoDecisionPrompt(msg feishu.Message, source Session, promptText string) string {
	message := formatCurrentAtUserMessage(msg, promptTextWithReplyContext(msg, promptText))
	if message == "" {
		return ""
	}
	return s.formatAtAutoDecisionPromptWithMessages(source, []string{
		"## 待判断群消息",
		message,
	}, false)
}

func (s *Service) formatAtAutoPendingDecisionPrompt(source Session, messages []pendingAtMessage) string {
	history := formatPendingAtMessageBlock(pendingAtAutoHeader, messages)
	if history == "" {
		return ""
	}
	return s.formatAtAutoDecisionPromptWithMessages(source, []string{history}, true)
}

func (s *Service) formatAtAutoDecisionPromptWithMessages(source Session, messageSections []string, batch bool) string {
	rules := []string{
		"当前群聊已启用 /at off auto 或 /at off auto-reaction。",
		"你是 at-auto 伴生判断会话，只判断是否需要让主会话响应，不要执行用户请求。",
		"请结合你已知的本群上下文、主会话信息，以及必要时主会话 trace 判断。",
		"如果消息与当前会话、bot 职责或正在处理的任务无关，最终只输出 SILENT。",
		"如果需要主会话响应，最终只输出 RESPOND。",
		"不要输出解释、引用、代码或其它文本。",
	}
	if batch {
		rules = append(rules, "多条消息中只要任意一条需要主会话响应，就输出 RESPOND。")
	}
	sections := []string{
		"# 群聊自动响应判断",
		strings.Join(rules, "\n"),
		s.formatAtAutoSourceContext(source),
	}
	sections = append(sections, messageSections...)
	return strings.Join(nonEmptySections(sections), "\n\n")
}

func (s *Service) formatAtAutoSourceContext(source Session) string {
	lines := []string{
		"## 主会话信息",
		"bot_id：" + source.Key.BotID,
		"source：" + sessionKeySource(source.Key),
		"main_id：" + sessionKeyMainID(source.Key),
	}
	if strings.TrimSpace(source.Key.SubID) != "" {
		lines = append(lines, "sub_id："+strings.TrimSpace(source.Key.SubID))
	}
	if strings.TrimSpace(source.AgentName) != "" {
		lines = append(lines, "agent："+strings.TrimSpace(source.AgentName))
	}
	if strings.TrimSpace(source.ACPSessionID) != "" {
		lines = append(lines, "acp_session："+strings.TrimSpace(source.ACPSessionID))
	}
	if tracePath := s.atAutoSourceTracePath(source); tracePath != "" {
		lines = append(lines, "trace_file："+tracePath)
	}
	return strings.Join(lines, "\n")
}

func (s *Service) atAutoSourceTracePath(source Session) string {
	if strings.TrimSpace(source.ACPSessionID) == "" {
		return ""
	}
	store := s.traceStoreForSession(source)
	if store == nil {
		return ""
	}
	return store.sessionPath(source)
}

func formatAtAutoPendingResponsePrompt(messages []pendingAtMessage) string {
	history := formatPendingAtMessageBlock("下面是需要处理的群消息：", messages)
	if history == "" {
		return ""
	}
	return promptWithUserMessage(nil, history+"\n\n请结合上下文综合处理，并只回复一次。")
}

func pendingAtAutoReplyMessage(base feishu.Message, messages []pendingAtMessage) feishu.Message {
	reply := base
	if len(messages) == 0 {
		return reply
	}
	last := messages[len(messages)-1]
	reply.MessageID = strings.TrimSpace(last.MessageID)
	reply.ThreadID = strings.TrimSpace(last.ThreadID)
	reply.RootID = strings.TrimSpace(last.RootID)
	reply.ParentID = strings.TrimSpace(last.ParentID)
	reply.SenderID = strings.TrimSpace(last.SenderID)
	reply.Reply = nil
	reply.ForceReplyInThread = last.ForceReplyInThread
	return reply
}

func atAutoTraceMessageID(msg feishu.Message, companion Session) string {
	return traceMessageID("at_auto", companion.ACPSessionID, msg.MessageID)
}

func isAtAutoDecisionPositive(reply string) bool {
	reply = strings.TrimSpace(reply)
	return hasTrailingReplySentinel(reply, "RESPOND")
}
