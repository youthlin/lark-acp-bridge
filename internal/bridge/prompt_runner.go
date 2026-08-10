package bridge

import (
	"context"
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

const promptCardFinalUpdateLimit = 15 * time.Second

var errCurrentSessionChanged = errors.New("current session changed")

type promptSessionOptions struct {
	SkipPostPromptWork     bool
	SkipPendingAtAutoDrain bool
}

type preparedPrompt struct {
	session Session
	agent   config.AgentConfig
	text    string
	errText string
}

type promptStreamRun struct {
	stream        *promptCardStream
	chunks        *promptChunkAccumulator
	result        acp.PromptResult
	rawResult     acp.PromptResult
	streamedReply string
	streamedSet   bool
	err           error
}

type promptRuntimeResult struct {
	result       acp.PromptResult
	sentProgress bool
	rawResult    acp.PromptResult
	reply        string
	replySet     bool
	err          error
}

type promptRunOutcome struct {
	session      Session
	result       acp.PromptResult
	reply        string
	replySet     bool
	sentProgress bool
	err          error
}

type promptPostWorkOptions struct {
	botID                    string
	msg                      feishu.Message
	agent                    config.AgentConfig
	operation                string
	skipPostPromptWork       bool
	scheduleWiki             bool
	allowReplySuppression    bool
	updateTitle              func(context.Context, Session) Session
	updateTitleOnSuccessOnly bool
	runAutoCompact           bool
	recordACPError           bool
}

type promptPostWorkResult struct {
	session    Session
	reply      string
	replySet   bool
	suppressed bool
}

func (out promptRunOutcome) replyText() string {
	if out.replySet {
		return out.reply
	}
	if out.reply != "" {
		return out.reply
	}
	return out.result.Text
}

func (s *Service) executePromptWithRecovery(ctx context.Context, session Session, run func(context.Context, Session) promptRunOutcome, refresh func(context.Context, Session) (Session, error)) promptRunOutcome {
	out := run(ctx, session)
	if out.session.Key == (SessionKey{}) {
		out.session = session
	}
	if errors.Is(out.err, errACPSessionUnavailable) && !out.sentProgress && refresh != nil {
		refreshed, refreshErr := refresh(ctx, out.session)
		if refreshErr != nil {
			return promptRunOutcome{session: out.session, err: refreshErr}
		}
		out = run(ctx, refreshed)
		if out.session.Key == (SessionKey{}) {
			out.session = refreshed
		}
	}
	return out
}

func (s *Service) runUserPromptWithWorkspaceContext(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, opts runningTaskOptions) promptRunOutcome {
	promptText := s.promptTextWithWorkspaceContextForSession(session, msg, text)
	includedWorkspaceContext := shouldIncludeWorkspaceContextPrompt(session, sessionWorkspace(session, msg))
	run := s.runUserPromptWithOptionsDetailed(ctx, msg, session, agent, promptText, opts)
	if includedWorkspaceContext && (run.err == nil || run.sentProgress) {
		session = s.markWorkspacePrompted(ctx, msg, session)
	}
	return promptRunOutcome{session: session, result: run.result, reply: run.reply, replySet: run.replySet, sentProgress: run.sentProgress, err: run.err}
}

func (s *Service) markWorkspacePrompted(ctx context.Context, msg feishu.Message, session Session) Session {
	if session.WorkspacePrompted || strings.TrimSpace(session.ACPSessionID) == "" || strings.TrimSpace(sessionWorkspace(session, msg)) == "" {
		return session
	}
	store := s.storeForMessage(msg)
	if store == nil {
		session.WorkspacePrompted = true
		return session
	}
	err := store.UpdateCurrentSession(session.Key, session.ACPSessionID, func(current *Session) bool {
		if current.WorkspacePrompted {
			return false
		}
		current.WorkspacePrompted = true
		return true
	})
	if err != nil {
		slog.WarnContext(ctx, "保存 workspace prompt 状态失败", "session", session.ACPSessionID, "错误", err)
		return session
	}
	if latest, ok := store.Get(session.Key); ok && latest.ACPSessionID == session.ACPSessionID {
		return latest
	}
	session.WorkspacePrompted = true
	return session
}

func (s *Service) resetWorkspacePrompted(ctx context.Context, msg feishu.Message, session Session) Session {
	if !session.WorkspacePrompted || strings.TrimSpace(session.ACPSessionID) == "" || strings.TrimSpace(sessionWorkspace(session, msg)) == "" {
		return session
	}
	store := s.storeForMessage(msg)
	if store == nil {
		session.WorkspacePrompted = false
		return session
	}
	err := store.UpdateCurrentSession(session.Key, session.ACPSessionID, func(current *Session) bool {
		if !current.WorkspacePrompted {
			return false
		}
		current.WorkspacePrompted = false
		return true
	})
	if err != nil {
		slog.WarnContext(ctx, "重置 workspace prompt 状态失败", "session", session.ACPSessionID, "错误", err)
		return session
	}
	if latest, ok := store.Get(session.Key); ok && latest.ACPSessionID == session.ACPSessionID {
		return latest
	}
	session.WorkspacePrompted = false
	return session
}

func (s *Service) finishPromptPostWork(ctx context.Context, out promptRunOutcome, opts promptPostWorkOptions) promptPostWorkResult {
	reply := out.replyText()
	session := out.session
	s.recordPromptTokenUsage(ctx, opts.botID, session, out.result)
	if !opts.skipPostPromptWork && opts.scheduleWiki && out.err == nil {
		s.scheduleWikiAfterUserPrompt(session, opts.agent)
	}
	if opts.allowReplySuppression && s.shouldSuppressAtAutoReply(opts.msg, reply) {
		return promptPostWorkResult{session: session, suppressed: true}
	}
	if !opts.skipPostPromptWork && shouldUpdatePromptTitle(out.err, reply, out.sentProgress, opts.updateTitleOnSuccessOnly) && opts.updateTitle != nil {
		session = opts.updateTitle(ctx, session)
		out.session = session
	}
	if !opts.skipPostPromptWork && opts.runAutoCompact {
		s.maybeRunAutoCompact(ctx, opts.msg, session, opts.agent, out.result, out.err)
	}
	if opts.recordACPError && shouldRecordPromptError(out.err) {
		s.recordACPError(session, opts.operation, out.err)
	}
	return promptPostWorkResult{session: session, reply: reply, replySet: out.replySet}
}

func shouldUpdatePromptTitle(err error, reply string, sentProgress bool, successOnly bool) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if successOnly {
		return err == nil
	}
	return err == nil || strings.TrimSpace(reply) != "" || sentProgress
}

func shouldRecordPromptError(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, errSessionTaskBusy)
}

func (s *Service) prompt(ctx context.Context, msg feishu.Message, text string) (string, error) {
	return s.promptWithOptions(ctx, msg, text, promptSessionOptions{})
}

func (s *Service) promptWithOptions(ctx context.Context, msg feishu.Message, text string, opts promptSessionOptions) (string, error) {
	prepared, err := s.preparePrompt(ctx, msg, text)
	if err != nil {
		return "", err
	}
	if prepared.errText != "" {
		return prepared.errText, nil
	}
	return s.promptSession(ctx, msg, prepared.session, prepared.agent, prepared.text, text, opts)
}

func (s *Service) preparePrompt(ctx context.Context, msg feishu.Message, userText string) (preparedPrompt, error) {
	session, ok := s.findSession(msg)
	if !ok {
		created, agent, _, errText := s.createSession(ctx, []string{"/new"}, msg)
		if errText != "" {
			return preparedPrompt{errText: errText}, nil
		}
		text := promptTextWithReplyContext(msg, userText)
		return preparedPrompt{session: created, agent: agent, text: text}, nil
	}
	if agentName := s.chatAgentName(msg); strings.TrimSpace(agentName) != "" && session.AgentName != agentName {
		created, agent, _, errText := s.createSession(ctx, []string{"/new"}, msg)
		if errText != "" {
			return preparedPrompt{errText: errText}, nil
		}
		text := promptTextWithReplyContext(msg, userText)
		return preparedPrompt{session: created, agent: agent, text: text}, nil
	}
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		return preparedPrompt{}, fmt.Errorf("未找到 agent 配置: %s", session.AgentName)
	}
	text := promptTextWithReplyContext(msg, userText)
	if strings.TrimSpace(session.ACPSessionID) == "" {
		created, _, _, errText := s.createSession(ctx, []string{"/new", session.Cwd}, msg)
		if errText != "" {
			return preparedPrompt{errText: errText}, nil
		}
		session = created
	}
	return preparedPrompt{session: session, agent: agent, text: text}, nil
}

func (s *Service) promptSession(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, userText string, opts promptSessionOptions) (string, error) {
	s.subscribeACPStateUpdates(ctx, msg, session.Key)
	out := s.executePromptWithRecovery(ctx, session, func(runCtx context.Context, runSession Session) promptRunOutcome {
		runOpts := userPromptTaskOptions()
		if s.shouldHandleAtAutoMessage(msg) {
			runOpts = atAutoPromptTaskOptions()
		} else if isAtAutoMode(s.chatAtMode(msg)) && messageMentionsBot(msg) {
			runOpts = atAutoUserPromptTaskOptions()
		}
		return s.runUserPromptWithWorkspaceContext(runCtx, msg, runSession, agent, text, runOpts)
	}, func(refreshCtx context.Context, refreshSession Session) (Session, error) {
		return s.refreshACPSession(refreshCtx, msg, refreshSession, agent)
	})
	post := s.finishPromptPostWork(ctx, out, promptPostWorkOptions{
		botID:                 msg.BotID,
		msg:                   msg,
		agent:                 agent,
		operation:             "prompt",
		skipPostPromptWork:    opts.SkipPostPromptWork,
		scheduleWiki:          true,
		allowReplySuppression: true,
		updateTitle: func(titleCtx context.Context, current Session) Session {
			return s.updateAutomaticSessionTitle(titleCtx, msg, current, userText)
		},
		runAutoCompact: true,
		recordACPError: true,
	})
	session = post.session
	reply := post.reply
	if post.suppressed {
		if !opts.SkipPendingAtAutoDrain {
			s.runPendingAtAutoAsync(ctx, msg, session, agent)
		}
		return "", nil
	}
	if out.sentProgress {
		reply = ""
	}
	if out.err != nil {
		if errors.Is(out.err, context.Canceled) {
			return "", nil
		}
		if strings.TrimSpace(reply) != "" {
			return reply, nil
		}
		if out.sentProgress {
			return "", nil
		}
		return "", out.err
	}
	if strings.TrimSpace(reply) == "" {
		if out.sentProgress || post.replySet {
			return "", nil
		}
		return "ACP session 已完成，但没有返回文本。", nil
	}
	if !opts.SkipPendingAtAutoDrain {
		s.runPendingAtAutoAsync(ctx, msg, session, agent)
	}
	s.clearACPError(session)
	return reply, nil
}

func (s *Service) runPendingAtAutoAsync(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig) {
	if !isAtAutoMode(s.chatAtMode(msg)) {
		return
	}
	pending := s.takePendingAtAutoMessages(session.Key)
	if len(pending) == 0 {
		return
	}
	promptText := formatAtAutoPendingPrompt(pending)
	if strings.TrimSpace(promptText) == "" {
		return
	}
	autoMsg := msg
	autoMsg.Text = promptText
	autoMsg.Mentions = nil
	autoMsg.ForceReplyInThread = true
	sessionKey := session.Key
	s.goBackground("pending-at-auto", func() {
		reply, err := s.promptSession(context.WithoutCancel(ctx), autoMsg, session, agent, promptText, promptText, promptSessionOptions{
			SkipPostPromptWork:     true,
			SkipPendingAtAutoDrain: true,
		})
		if err != nil {
			// session 正忙（通常是新消息刚好占用）时，把待处理消息放回，等待下次处理，避免丢失。
			if errors.Is(err, errSessionTaskBusy) {
				s.restorePendingAtAutoMessages(sessionKey, pending)
				return
			}
			slog.WarnContext(context.WithoutCancel(ctx), "执行待处理群聊 auto 判断失败", "错误", err)
			return
		}
		if strings.TrimSpace(reply) == "" {
			return
		}
		if ok, err := s.sendIntermediateReply(context.WithoutCancel(ctx), autoMsg, reply); err != nil {
			slog.WarnContext(context.WithoutCancel(ctx), "发送待处理群聊 auto 回复失败", "错误", err)
		} else if !ok {
			slog.WarnContext(context.WithoutCancel(ctx), "缺少待处理群聊 auto 回复发送器")
		}
	})
}

func (s *Service) refreshACPSession(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig) (Session, error) {
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

func (s *Service) runUserPrompt(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string) (acp.PromptResult, bool, error) {
	opts := userPromptTaskOptions()
	if s.shouldHandleAtAutoMessage(msg) {
		opts = atAutoPromptTaskOptions()
	} else if isAtAutoMode(s.chatAtMode(msg)) && messageMentionsBot(msg) {
		opts = atAutoUserPromptTaskOptions()
	}
	return s.runUserPromptWithOptions(ctx, msg, session, agent, text, opts)
}

func (s *Service) runUserPromptWithOptions(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, opts runningTaskOptions) (acp.PromptResult, bool, error) {
	run := s.runUserPromptWithOptionsDetailed(ctx, msg, session, agent, text, opts)
	return run.result, run.sentProgress, run.err
}

func (s *Service) runUserPromptWithOptionsDetailed(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, opts runningTaskOptions) promptRuntimeResult {
	if !opts.silentPrompt {
		delayed := s.shouldDelayAtAutoProgress(msg)
		stream := newPromptCardStreamWithStatusPrefix(ctx, msg, session, s.chatConfigForMessage(msg), "", s.streamCardStarterForMessage(msg))
		if delayed {
			stream.delayCardCreation()
		}
		opts.replacementWait = stream
		return s.runUserPromptWithStreamOptionsDetailed(ctx, msg, session, agent, text, opts, stream, delayed)
	}
	out, err := runPromptTaskDetailed(s, ctx, session, agent, opts, func(taskCtx context.Context) (promptRuntimeResult, error) {
		result, err := s.runtime.Prompt(taskCtx, session, agent, text, acp.PromptOptions{})
		return promptRuntimeResult{result: result, err: err}, err
	})
	return promptRuntimeResult{result: out.result, sentProgress: out.sentProgress, err: err}
}

func (s *Service) runUserPromptWithStreamOptionsDetailed(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, opts runningTaskOptions, stream *promptCardStream, delayed bool) promptRuntimeResult {
	out, err := runPromptTaskDetailed(s, ctx, session, agent, opts, func(taskCtx context.Context) (promptRuntimeResult, error) {
		run := s.promptRuntimeWithProgressRawStatusPrefixAndStream(taskCtx, msg, session, agent, text, stream, delayed)
		return run, run.err
	})
	return promptRuntimeResult{result: out.result, sentProgress: out.sentProgress, reply: out.reply, replySet: out.replySet, err: err}
}

func (s *Service) promptRuntimeWithProgress(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string) (acp.PromptResult, bool, error) {
	result, sentProgress, _, _, err := s.promptRuntimeWithProgressRaw(ctx, msg, session, agent, text)
	return result, sentProgress, err
}

func (s *Service) promptRuntimeWithProgressRaw(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string) (acp.PromptResult, bool, acp.PromptResult, string, error) {
	run := s.promptRuntimeWithProgressRawStatusPrefix(ctx, msg, session, agent, text, "")
	return run.result, run.sentProgress, run.rawResult, run.reply, run.err
}

func (s *Service) promptRuntimeWithProgressRawStatusPrefix(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, statusPrefix string) promptRuntimeResult {
	delayed := s.shouldDelayAtAutoProgress(msg)
	stream := newPromptCardStreamWithStatusPrefix(ctx, msg, session, s.chatConfigForMessage(msg), statusPrefix, s.streamCardStarterForMessage(msg))
	if delayed {
		stream.delayCardCreation()
	}
	return s.promptRuntimeWithProgressRawStatusPrefixAndStream(ctx, msg, session, agent, text, stream, delayed)
}

func (s *Service) promptRuntimeWithProgressRawStatusPrefixAndStream(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, stream *promptCardStream, delayed bool) promptRuntimeResult {
	if delayed {
		run := s.runPromptWithStream(ctx, msg, session, agent, text, stream)
		result := run.result
		if run.chunks.hasFinalBoundary() {
			result.Text = run.streamedReply
		} else if strings.TrimSpace(run.streamedReply) != "" && strings.TrimSpace(result.Text) == "" {
			result.Text = run.streamedReply
		}
		if run.err == nil && strings.TrimSpace(result.Text) != "" && !s.shouldSuppressAtAutoReply(msg, result.Text) {
			finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(ctx), promptCardFinalUpdateLimit)
			defer finalCancel()
			if finalReply := promptFinalCardText(run, result); finalReply != "" {
				slog.InfoContext(finalCtx, "准备更新 ACP 流式卡片最终文本", "session", session.ACPSessionID, "final_boundary", run.chunks.hasFinalBoundary(), "delayed", true)
				run.stream.setFinalTextWithContext(finalCtx, finalReply)
			}
			run.stream.flushDelayedWithContext(finalCtx, result, result.StopReason)
		}
		reply, replySet := delayedPromptReply(run, result)
		return promptRuntimeResult{result: result, sentProgress: run.stream.hasStarted(), rawResult: result, reply: reply, replySet: replySet, err: run.err}
	}

	run := s.runPromptWithStream(ctx, msg, session, agent, text, stream)
	result := run.result
	if run.stream.hasStarted() {
		finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(ctx), promptCardFinalUpdateLimit)
		defer finalCancel()
		if finalReply := promptFinalCardText(run, result); finalReply != "" {
			slog.InfoContext(finalCtx, "准备更新 ACP 流式卡片最终文本", "session", session.ACPSessionID, "final_boundary", run.chunks.hasFinalBoundary(), "delayed", false)
			run.stream.setFinalTextWithContext(finalCtx, finalReply)
		}
		if run.err != nil {
			if errors.Is(run.err, context.Canceled) {
				run.stream.updateProcessMessageWithContext(finalCtx, "已取消")
				run.stream.finishPromptStatusWithContext(finalCtx, "cancelled")
			} else {
				run.stream.updateProcessMessageWithContext(finalCtx, "执行失败："+run.err.Error())
				run.stream.failPromptStatusWithContext(finalCtx)
			}
		} else {
			run.stream.updatePromptStatusFromResultWithContext(finalCtx, result)
			run.stream.updatePromptResult(result)
			run.stream.finishPromptStatusWithContext(finalCtx, result.StopReason)
		}
		run.stream.closeWithContext(finalCtx)
	}
	if run.stream.hasStarted() {
		result.Text = ""
	}
	return promptRuntimeResult{result: result, sentProgress: run.stream.hasStarted(), rawResult: run.rawResult, reply: run.streamedReply, replySet: run.streamedSet, err: run.err}
}

func promptFinalCardText(run promptStreamRun, result acp.PromptResult) string {
	if run.chunks != nil && run.chunks.hasFinalBoundary() {
		return strings.TrimSpace(run.streamedReply)
	}
	return strings.TrimSpace(result.Text)
}

func delayedPromptReply(run promptStreamRun, result acp.PromptResult) (string, bool) {
	if run.chunks != nil && run.chunks.hasFinalBoundary() {
		return run.streamedReply, true
	}
	return result.Text, false
}

func (s *Service) runPromptWithStream(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, stream *promptCardStream) promptStreamRun {
	if stream == nil {
		stream = newPromptCardStream(ctx, msg, session, s.chatConfigForMessage(msg), s.streamCardStarterForMessage(msg))
	}
	chunks := newPromptChunkAccumulator(stream)
	stopStatusRefresh := stream.startStatusRefresh(ctx)
	result, err := s.runtime.Prompt(ctx, session, agent, text, s.promptStreamOptions(msg, stream, chunks))
	stopStatusRefresh()
	rawResult := result
	chunks.close()
	streamedReply := chunks.finalText()
	return promptStreamRun{
		stream:        stream,
		chunks:        chunks,
		result:        result,
		rawResult:     rawResult,
		streamedReply: streamedReply,
		streamedSet:   chunks.hasFinalBoundary() || strings.TrimSpace(streamedReply) != "",
		err:           err,
	}
}

func (s *Service) promptStreamOptions(msg feishu.Message, stream *promptCardStream, chunks *promptChunkAccumulator) acp.PromptOptions {
	return acp.PromptOptions{
		OnUpdate: func(update acp.PromptUpdate) {
			sessionID := strings.TrimSpace(update.SessionID)
			if sessionID == "" {
				sessionID = stream.session.ACPSessionID
			}
			s.handleACPStateUpdate(stream.ctx, msg, stream.session.Key, sessionID, update.Update)
			stream.updatePromptStatusFromUpdate(update)
			if chunk, ok := promptUpdateChunk(update); ok {
				if chunk.FinalBoundary {
					chunks.markFinalBoundary()
				}
				chunks.add(chunk)
				return
			}
			if isFinalTextBoundaryUpdateKind(promptUpdateKind(update)) {
				chunks.markFinalBoundary()
			} else {
				chunks.finishStream()
			}
			stream.updatePromptUpdate(update)
		},
		OnPermissionRequest: func(reqCtx context.Context, req acp.PermissionRequest) (acp.PermissionOutcome, error) {
			outcome, ok, err := s.requestPermission(reqCtx, msg, req)
			if err != nil {
				return acp.PermissionOutcome{}, err
			}
			if ok {
				return outcome, nil
			}
			return defaultPermissionOutcome(req), nil
		},
	}
}

func defaultPermissionOutcome(req acp.PermissionRequest) acp.PermissionOutcome {
	for _, option := range req.Options {
		switch option.Kind {
		case "reject_once", "reject_always":
			if strings.TrimSpace(option.OptionID) != "" {
				return acp.PermissionOutcome{Outcome: "selected", OptionID: option.OptionID}
			}
		}
	}
	return acp.PermissionOutcome{Outcome: "cancelled"}
}
