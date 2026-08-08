package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

// TriggerSink 接收非 IM 触发源的执行结果和状态。
type TriggerSink interface {
	OnUpdate(context.Context, TriggerResult) error
	OnComplete(context.Context, TriggerResult) error
	OnError(context.Context, TriggerResult) error
}

// TriggerRequest 描述一次显式 source 触发的 ACP prompt。
type TriggerRequest struct {
	BotID                string
	Key                  SessionKey
	Workspace            string
	AgentName            string
	Cwd                  string
	Title                string
	Prompt               string
	Metadata             map[string]string
	Sink                 TriggerSink
	EnableWikiReflection bool
}

// TriggerResult 描述 trigger prompt 的阶段性或最终结果。
type TriggerResult struct {
	Request        TriggerRequest
	Session        Session
	ACPResult      acp.PromptResult
	Update         acp.PromptUpdate
	Text           string
	TextSet        bool
	SentProgress   bool
	Err            error
	Skipped        bool
	SkipReason     string
	ACPSessionID   string
	ACPSessionMeta map[string]any
}

type preparedTrigger struct {
	request    TriggerRequest
	store      *SessionStore
	session    Session
	hasSession bool
}

func (r TriggerRequest) normalized() TriggerRequest {
	r.BotID = strings.TrimSpace(r.BotID)
	r.Key = normalizeSessionKey(r.Key)
	if r.Key.BotID == "" {
		r.Key.BotID = r.BotID
	}
	if r.BotID == "" {
		r.BotID = r.Key.BotID
	}
	r.Workspace = strings.TrimSpace(r.Workspace)
	r.AgentName = strings.TrimSpace(r.AgentName)
	r.Cwd = strings.TrimSpace(r.Cwd)
	r.Title = strings.TrimSpace(r.Title)
	r.Prompt = strings.TrimSpace(r.Prompt)
	r.Metadata = maps.Clone(r.Metadata)
	return r
}

func (r TriggerRequest) valid() bool {
	r = r.normalized()
	return r.BotID != "" && r.Key.Valid() && r.Prompt != ""
}

func newTriggerResult(req TriggerRequest, session Session, acpResult acp.PromptResult, text string, textSet bool, sentProgress bool, err error) TriggerResult {
	session = normalizeSessionForStore(session)
	if !textSet && text == "" {
		text = acpResult.Text
	}
	return TriggerResult{
		Request:        req.normalized(),
		Session:        session,
		ACPResult:      acpResult,
		Text:           text,
		TextSet:        textSet,
		SentProgress:   sentProgress,
		Err:            err,
		ACPSessionID:   session.ACPSessionID,
		ACPSessionMeta: maps.Clone(session.ACPMeta),
	}
}

func newTriggerUpdateResult(req TriggerRequest, session Session, update acp.PromptUpdate) TriggerResult {
	result := newTriggerResult(req, session, acp.PromptResult{}, triggerUpdateText(update), true, false, nil)
	result.Update = update
	return result
}

func triggerUpdateText(update acp.PromptUpdate) string {
	if text := strings.TrimSpace(formatPromptUpdate(update)); text != "" {
		return text
	}
	if chunk, ok := promptUpdateChunk(update); ok {
		return chunk.Text
	}
	return ""
}

func (s *Service) runTriggerPrompt(ctx context.Context, req TriggerRequest) (TriggerResult, error) {
	req = req.normalized()
	ctx = triggerTraceContext(ctx, req)
	slog.InfoContext(ctx, "开始执行 trigger prompt", triggerLogArgs(req, Session{})...)
	prepared, err := s.prepareTrigger(req)
	if err != nil {
		result := TriggerResult{Request: req.normalized(), Err: err}
		slog.ErrorContext(ctx, "trigger prompt 准备失败", append(triggerLogArgs(result.Request, Session{}), slog.Any("错误", err))...)
		if req.Sink != nil {
			_ = req.Sink.OnError(ctx, result)
		}
		return result, err
	}
	req = prepared.request
	if !prepared.hasSession {
		session, err := s.createTriggerSession(ctx, prepared)
		if err != nil {
			result := TriggerResult{Request: req, Err: err}
			slog.ErrorContext(ctx, "创建 trigger session 失败", append(triggerLogArgs(req, Session{}), slog.Any("错误", err))...)
			if req.Sink != nil {
				_ = req.Sink.OnError(ctx, result)
			}
			return result, err
		}
		prepared.session = session
		prepared.hasSession = true
	}
	s.subscribeTriggerStateUpdates(ctx, prepared)

	out := s.executePromptWithRecovery(ctx, prepared.session, func(runCtx context.Context, runSession Session) promptRunOutcome {
		prepared.session = runSession
		result, err := s.runPreparedTriggerPrompt(runCtx, prepared)
		return promptRunOutcome{
			session:      result.Session,
			result:       result.ACPResult,
			reply:        result.Text,
			replySet:     result.TextSet,
			sentProgress: result.SentProgress,
			err:          err,
		}
	}, func(refreshCtx context.Context, refreshSession Session) (Session, error) {
		slog.WarnContext(refreshCtx, "trigger ACP session 不可恢复，准备重建", triggerLogArgs(req, refreshSession)...)
		prepared.session = refreshSession
		return s.refreshTriggerSession(refreshCtx, prepared)
	})
	agent, agentOK := s.registry.Get(out.session.AgentName)
	post := s.finishPromptPostWork(ctx, out, promptPostWorkOptions{
		botID:                    req.BotID,
		operation:                "trigger prompt",
		agent:                    agent,
		scheduleWiki:             req.EnableWikiReflection && agentOK,
		updateTitleOnSuccessOnly: true,
		recordACPError:           true,
		updateTitle: func(titleCtx context.Context, current Session) Session {
			prepared.session = current
			return s.updateTriggerAutomaticTitle(titleCtx, prepared, current)
		},
	})
	result := newTriggerResult(req, post.session, out.result, post.reply, post.replySet, out.sentProgress, out.err)
	err = out.err
	if req.Sink != nil {
		if err != nil {
			_ = req.Sink.OnError(ctx, result)
		} else {
			_ = req.Sink.OnComplete(ctx, result)
		}
	}
	if err != nil {
		slog.ErrorContext(ctx, "trigger prompt 执行失败", append(triggerLogArgs(req, result.Session), slog.Any("错误", err))...)
	} else {
		slog.InfoContext(ctx, "trigger prompt 执行完成", triggerLogArgs(req, result.Session)...)
	}
	return result, err
}

func triggerTraceContext(ctx context.Context, req TriggerRequest) context.Context {
	req = req.normalized()
	key := normalizeSessionKey(req.Key)
	ctx = logging.CtxAddMissingAttr(ctx,
		slog.String("bot", req.BotID),
		slog.String("source", sessionKeySource(key)),
		slog.String("main_id", sessionKeyMainID(key)),
		slog.String("sub_id", strings.TrimSpace(key.SubID)),
		slog.String("task_id", strings.TrimSpace(req.Metadata["task_id"])),
		slog.String("run_id", strings.TrimSpace(req.Metadata["run_id"])),
		slog.String("comment_id", strings.TrimSpace(req.Metadata["comment_id"])),
	)
	ctx, _ = logging.EnsureTraceID(ctx, triggerTraceParts(req)...)
	return ctx
}

func triggerTraceParts(req TriggerRequest) []string {
	req = req.normalized()
	key := normalizeSessionKey(req.Key)
	return []string{
		"trigger",
		req.BotID,
		sessionKeySource(key),
		sessionKeyMainID(key),
		strings.TrimSpace(key.SubID),
		strings.TrimSpace(req.Metadata["task_id"]),
		strings.TrimSpace(req.Metadata["run_id"]),
		strings.TrimSpace(req.Metadata["comment_id"]),
	}
}

func (s *Service) subscribeTriggerStateUpdates(ctx context.Context, prepared preparedTrigger) {
	key := normalizeSessionKey(prepared.request.Key)
	s.acpUpdateMu.Lock()
	if old := s.acpUpdateUnsub[key]; old != nil {
		old()
	}
	unsub := s.runtime.SubscribeUpdates(key, func(sessionID string, update acp.SessionUpdate) {
		s.handleTriggerStateUpdate(ctx, prepared.store, key, sessionID, update)
	})
	s.acpUpdateUnsub[key] = unsub
	s.acpUpdateMu.Unlock()
}

func (s *Service) handleTriggerStateUpdate(ctx context.Context, store *SessionStore, key SessionKey, sessionID string, update acp.SessionUpdate) {
	if !isACPStateUpdate(update) || store == nil {
		return
	}
	if err := store.UpdateCurrentSession(key, sessionID, func(session *Session) bool {
		return applyACPStateUpdate(session, update)
	}); err != nil {
		slog.WarnContext(ctx, "保存 trigger ACP session 状态失败", "session", sessionID, "update", update.SessionUpdate, "错误", err)
	}
}

func triggerLogArgs(req TriggerRequest, session Session) []any {
	req = req.normalized()
	key := normalizeSessionKey(req.Key)
	source := sessionKeySource(key)
	mainID := sessionKeyMainID(key)
	acpSessionID := strings.TrimSpace(session.ACPSessionID)
	if acpSessionID == "" {
		acpSessionID = strings.TrimSpace(req.Metadata["acp_session_id"])
	}
	args := []any{
		slog.String("bot", req.BotID),
		slog.String("source", source),
		slog.String("main_id", mainID),
		slog.String("sub_id", strings.TrimSpace(key.SubID)),
	}
	if acpSessionID != "" {
		args = append(args, slog.String("acp_session_id", acpSessionID))
	}
	for _, key := range []string{"task_id", "run_id", "comment_id"} {
		if value := strings.TrimSpace(req.Metadata[key]); value != "" {
			args = append(args, slog.String(key, value))
		}
	}
	return args
}

func (s *Service) prepareTrigger(req TriggerRequest) (preparedTrigger, error) {
	req, store, err := s.prepareTriggerRequest(req)
	if err != nil {
		return preparedTrigger{request: req}, err
	}
	session, ok := store.Get(req.Key)
	return preparedTrigger{
		request:    req,
		store:      store,
		session:    session,
		hasSession: ok,
	}, nil
}

func (s *Service) createTriggerSession(ctx context.Context, prepared preparedTrigger) (Session, error) {
	req := prepared.request
	agent, ok := s.registry.Get(req.AgentName)
	if !ok {
		return Session{}, fmt.Errorf("未找到 trigger agent 配置: %s", req.AgentName)
	}
	cwd, err := cleanExistingDirectory(req.Cwd)
	if err != nil {
		return Session{}, err
	}
	if _, err := ensureWorkspace(req.Workspace, req.BotID); err != nil {
		return Session{}, fmt.Errorf("初始化 workspace 失败: %w", err)
	}
	candidate, err := s.runtime.NewSession(ctx, req.Key, req.AgentName, agent, cwd, req.Workspace)
	if err != nil {
		return Session{}, fmt.Errorf("创建 trigger ACP session 失败: %w", err)
	}
	defer candidate.Abort()
	sessionInfo := candidate.Info()
	session := Session{
		Key:               req.Key,
		Title:             normalizeSessionTitle(req.Title),
		ManualTitle:       normalizeSessionTitle(req.Title) != "",
		AgentName:         req.AgentName,
		ACPSessionID:      sessionInfo.SessionID,
		ACPMeta:           maps.Clone(sessionInfo.Meta),
		Cwd:               cwd,
		Workspace:         req.Workspace,
		AvailableCommands: sessionInfo.AvailableCommands,
		ConfigOptions:     sessionInfo.ConfigOptions,
		Models:            sessionInfo.Models,
		Mode:              sessionInfo.Mode,
	}
	if err := candidate.Commit(func() error {
		return prepared.store.Upsert(session)
	}); err != nil {
		return Session{}, fmt.Errorf("保存 trigger 会话映射失败: %w", err)
	}
	return session, nil
}

func (s *Service) refreshTriggerSession(ctx context.Context, prepared preparedTrigger) (Session, error) {
	session := prepared.session
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		return Session{}, fmt.Errorf("未找到 trigger session 的 agent 配置: %s", session.AgentName)
	}
	cwd, err := cleanExistingDirectory(session.Cwd)
	if err != nil {
		return Session{}, err
	}
	workspace := strings.TrimSpace(session.Workspace)
	if workspace == "" {
		workspace = prepared.request.Workspace
	}
	if _, err := ensureWorkspace(workspace, prepared.request.BotID); err != nil {
		return Session{}, fmt.Errorf("初始化 workspace 失败: %w", err)
	}
	previousACPSessionID := session.ACPSessionID
	candidate, err := s.runtime.NewSession(ctx, session.Key, session.AgentName, agent, cwd, workspace)
	if err != nil {
		return Session{}, fmt.Errorf("重建 trigger ACP session 失败: %w", err)
	}
	defer candidate.Abort()
	sessionInfo := candidate.Info()
	session = sessionWithACPInfo(session, sessionInfo, cwd, workspace)
	session, err = commitCurrentACPSessionReplacement(candidate, prepared.store, previousACPSessionID, session)
	if err != nil {
		if errors.Is(err, errCurrentSessionChanged) {
			return Session{}, fmt.Errorf("当前 trigger 会话已变化，忽略旧会话的重建结果")
		}
		return Session{}, fmt.Errorf("保存重建后的 trigger ACP session 失败: %w", err)
	}
	return session, nil
}

func (s *Service) updateTriggerAutomaticTitle(ctx context.Context, prepared preparedTrigger, session Session) Session {
	return updateAutomaticSessionTitleInStore(ctx, prepared.store, session, prepared.request.Prompt)
}

func (s *Service) runPreparedTriggerPrompt(ctx context.Context, prepared preparedTrigger) (TriggerResult, error) {
	req := prepared.request
	session := prepared.session
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		err := fmt.Errorf("未找到 trigger session 的 agent 配置: %s", session.AgentName)
		return newTriggerResult(req, session, acp.PromptResult{}, "", false, false, err), err
	}
	chunks := &triggerTextAccumulator{}
	out, err := runPromptTask(s, ctx, session, agent, triggerPromptTaskOptions(), func(taskCtx context.Context) (acp.PromptResult, bool, error) {
		result, err := s.runtime.Prompt(taskCtx, session, agent, req.Prompt, acp.PromptOptions{
			OnUpdate: func(update acp.PromptUpdate) {
				chunks.add(update)
				if req.Sink != nil {
					_ = req.Sink.OnUpdate(taskCtx, newTriggerUpdateResult(req, session, update))
				}
			},
			OnPermissionRequest: func(permCtx context.Context, permission acp.PermissionRequest) (acp.PermissionOutcome, error) {
				return s.requestTriggerPermission(permCtx, req, session, permission), nil
			},
		})
		return result, false, err
	})
	text, textSet := chunks.finalText()
	if text != "" && strings.TrimSpace(out.result.Text) == "" {
		out.result.Text = text
	}
	result := newTriggerResult(req, session, out.result, text, textSet, out.sentProgress, err)
	return result, err
}

// triggerPermissionTimeout 限制非 IM source 等待 owner 审批的最长时间，避免 cron/评论 run 永久挂起。
const triggerPermissionTimeout = 10 * time.Minute

// requestTriggerPermission 向 bot owner 发送私聊权限卡片并等待审批。
// 没有配置 owner、出站不支持私聊发卡、发送失败或超时时，统一按默认策略拒绝（不放行）。
func (s *Service) requestTriggerPermission(ctx context.Context, req TriggerRequest, session Session, permission acp.PermissionRequest) acp.PermissionOutcome {
	requester, ok := s.outboundForBot(req.BotID).(triggerPermissionRequester)
	if !ok || requester == nil {
		slog.WarnContext(ctx, "trigger 权限请求无法发送：出站不支持私聊权限卡片，已拒绝",
			triggerPermissionLogArgs(req, session, permission)...)
		return defaultPermissionOutcome(permission)
	}
	owners := s.ownerOpenIDs(req.BotID)
	if len(owners) == 0 {
		slog.WarnContext(ctx, "trigger 权限请求无法发送：未配置 bot owner，已拒绝",
			triggerPermissionLogArgs(req, session, permission)...)
		return defaultPermissionOutcome(permission)
	}
	source := triggerPermissionSourceLabel(req)
	waitCtx, cancel := context.WithTimeout(ctx, triggerPermissionTimeout)
	defer cancel()
	for _, ownerID := range owners {
		ownerID = strings.TrimSpace(ownerID)
		if ownerID == "" {
			continue
		}
		outcome, err := requester.RequestPermissionForOpenID(waitCtx, ownerID, source, permission)
		if err != nil {
			slog.WarnContext(ctx, "trigger 权限卡片发送失败，已拒绝",
				append(triggerPermissionLogArgs(req, session, permission), "owner", ownerID, "err", err)...)
			return defaultPermissionOutcome(permission)
		}
		if outcome.Outcome == "cancelled" {
			continue // 该 owner 超时或取消，尝试下一个
		}
		return outcome
	}
	slog.WarnContext(ctx, "trigger 权限请求未获审批（无 owner 响应或全部超时），已拒绝",
		triggerPermissionLogArgs(req, session, permission)...)
	return defaultPermissionOutcome(permission)
}

func triggerPermissionLogArgs(req TriggerRequest, session Session, permission acp.PermissionRequest) []any {
	return []any{
		"source", req.Key.Source,
		"main_id", req.Key.MainID,
		"sub_id", req.Key.SubID,
		"session", session.ACPSessionID,
		"tool_call_id", strings.TrimSpace(permission.ToolCall.ToolCallID),
		"tool_title", strings.TrimSpace(permission.ToolCall.Title),
	}
}

// triggerPermissionSourceLabel 生成权限卡片上展示的来源说明。
func triggerPermissionSourceLabel(req TriggerRequest) string {
	switch req.Key.Source {
	case sessionSourceSchedule:
		taskID := strings.TrimSpace(req.Metadata["task_id"])
		runID := strings.TrimSpace(req.Metadata["run_id"])
		label := "定时任务"
		if taskID != "" {
			label += " " + taskID
		}
		if runID != "" {
			label += "（run " + runID + "）"
		}
		return label
	case sessionSourceDriveComment:
		fileType := strings.TrimSpace(req.Metadata["file_type"])
		fileToken := strings.TrimSpace(req.Metadata["file_token"])
		commentID := strings.TrimSpace(req.Metadata["comment_id"])
		label := "云文档评论"
		if fileType != "" || fileToken != "" {
			label += " " + fileType + ":" + fileToken
		}
		if commentID != "" {
			label += "#" + commentID
		}
		return label
	default:
		if req.Key.Source != "" {
			return req.Key.Source
		}
		return req.Title
	}
}

type triggerTextAccumulator struct {
	reply          strings.Builder
	finalCandidate strings.Builder
	lastFinalText  string
	hasBoundary    bool
}

func (a *triggerTextAccumulator) add(update acp.PromptUpdate) {
	chunk, ok := promptUpdateChunk(update)
	if ok {
		if chunk.FinalBoundary {
			a.markBoundary()
		}
		if chunk.Target == promptChunkTargetText {
			a.reply.WriteString(chunk.Text)
			a.finalCandidate.WriteString(chunk.Text)
		}
		return
	}
	if isFinalTextBoundaryUpdateKind(promptUpdateKind(update)) {
		a.markBoundary()
	}
}

func (a *triggerTextAccumulator) markBoundary() {
	if text := strings.TrimSpace(a.finalCandidate.String()); text != "" {
		a.lastFinalText = text
		a.finalCandidate.Reset()
	}
	a.hasBoundary = true
}

func (a *triggerTextAccumulator) finalText() (string, bool) {
	if text := strings.TrimSpace(a.finalCandidate.String()); text != "" {
		return text, true
	}
	if a.hasBoundary {
		return strings.TrimSpace(a.lastFinalText), true
	}
	if text := strings.TrimSpace(a.reply.String()); text != "" {
		return text, true
	}
	return "", false
}

func (s *Service) prepareTriggerRequest(req TriggerRequest) (TriggerRequest, *SessionStore, error) {
	req = req.normalized()
	if req.BotID == "" {
		return req, nil, fmt.Errorf("trigger bot_id 不能为空")
	}
	if sessionKeySource(req.Key) == sessionSourceIM {
		return req, nil, fmt.Errorf("trigger source 不能使用 IM")
	}
	if !req.Key.Valid() {
		return req, nil, fmt.Errorf("trigger session key 不完整")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return req, nil, fmt.Errorf("trigger prompt 不能为空")
	}
	if strings.TrimSpace(req.AgentName) == "" {
		return req, nil, fmt.Errorf("trigger agent_name 不能为空")
	}
	if strings.TrimSpace(req.Cwd) == "" {
		return req, nil, fmt.Errorf("trigger cwd 不能为空")
	}
	if strings.TrimSpace(req.Workspace) == "" {
		return req, nil, fmt.Errorf("trigger workspace 不能为空")
	}
	store := s.storeForBotID(req.BotID)
	if store == nil {
		return req, nil, fmt.Errorf("未找到 bot %s 的会话存储", displayBotID(req.BotID))
	}
	return req, store, nil
}

func (s *Service) storeForBotID(botID string) *SessionStore {
	if s.stores == nil {
		return nil
	}
	if store := s.stores[strings.TrimSpace(botID)]; store != nil {
		return store
	}
	return s.stores[""]
}

func cleanExistingDirectory(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("工作目录不能为空")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("工作目录必须是绝对路径: %s", path)
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("工作目录不可访问: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("工作目录不是目录: %s", path)
	}
	return path, nil
}

type noopTriggerSink struct{}

func (noopTriggerSink) OnUpdate(context.Context, TriggerResult) error {
	return nil
}

func (noopTriggerSink) OnComplete(context.Context, TriggerResult) error {
	return nil
}

func (noopTriggerSink) OnError(context.Context, TriggerResult) error {
	return nil
}
