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
	err           error
}

type promptRunOutcome struct {
	session      Session
	result       acp.PromptResult
	reply        string
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
	suppressed bool
}

func (out promptRunOutcome) replyText() string {
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
	return promptPostWorkResult{session: session, reply: reply}
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
		text := s.promptTextWithWorkspaceContext(sessionWorkspace(created, msg), msg, promptTextWithReplyContext(msg, userText))
		return preparedPrompt{session: created, agent: agent, text: text}, nil
	}
	if agentName := s.chatAgentName(msg); strings.TrimSpace(agentName) != "" && session.AgentName != agentName {
		created, agent, _, errText := s.createSession(ctx, []string{"/new"}, msg)
		if errText != "" {
			return preparedPrompt{errText: errText}, nil
		}
		text := s.promptTextWithWorkspaceContext(sessionWorkspace(created, msg), msg, promptTextWithReplyContext(msg, userText))
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
	text = s.promptTextWithWorkspaceContext(sessionWorkspace(session, msg), msg, text)
	return preparedPrompt{session: session, agent: agent, text: text}, nil
}

func (s *Service) promptSession(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, userText string, opts promptSessionOptions) (string, error) {
	s.subscribeACPStateUpdates(ctx, msg, session.Key)
	out := s.executePromptWithRecovery(ctx, session, func(runCtx context.Context, runSession Session) promptRunOutcome {
		result, sentProgress, err := s.runUserPrompt(runCtx, msg, runSession, agent, text)
		return promptRunOutcome{session: runSession, result: result, sentProgress: sentProgress, err: err}
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
		if out.sentProgress {
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
	if !isAtAutoMode(s.chatAtMode(msg)) || !messageMentionsBot(msg) {
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
	go func() {
		reply, err := s.promptSession(context.WithoutCancel(ctx), autoMsg, session, agent, promptText, promptText, promptSessionOptions{
			SkipPostPromptWork:     true,
			SkipPendingAtAutoDrain: true,
		})
		if err != nil {
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
	}()
}

func (s *Service) refreshACPSession(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig) (Session, error) {
	cwd := strings.TrimSpace(session.Cwd)
	if cwd == "" {
		return Session{}, fmt.Errorf("当前会话缺少工作目录，无法重建 ACP session")
	}
	inheritConfig := inheritedSessionConfigForNewSession(session, true, session.AgentName)
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
	if isAtAutoMode(s.chatAtMode(msg)) && messageMentionsBot(msg) {
		opts = atAutoUserPromptTaskOptions()
	}
	return s.runUserPromptWithOptions(ctx, msg, session, agent, text, opts)
}

func (s *Service) runUserPromptWithOptions(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, opts runningTaskOptions) (acp.PromptResult, bool, error) {
	out, err := runPromptTask(s, ctx, session, agent, opts, func(taskCtx context.Context) (acp.PromptResult, bool, error) {
		if opts.silentPrompt {
			result, err := s.runtime.Prompt(taskCtx, session, agent, text, acp.PromptOptions{})
			return result, false, err
		}
		return s.promptRuntimeWithProgress(taskCtx, msg, session, agent, text)
	})
	if errors.Is(err, context.Canceled) {
		return out.result, out.sentProgress, err
	}
	return out.result, out.sentProgress, err
}

func (s *Service) promptRuntimeWithProgress(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string) (acp.PromptResult, bool, error) {
	result, sentProgress, _, _, err := s.promptRuntimeWithProgressRaw(ctx, msg, session, agent, text)
	return result, sentProgress, err
}

func (s *Service) promptRuntimeWithProgressRaw(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string) (acp.PromptResult, bool, acp.PromptResult, string, error) {
	return s.promptRuntimeWithProgressRawStatusPrefix(ctx, msg, session, agent, text, "")
}

func (s *Service) promptRuntimeWithProgressRawStatusPrefix(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, statusPrefix string) (acp.PromptResult, bool, acp.PromptResult, string, error) {
	if s.shouldDelayAtAutoProgress(msg) {
		run := s.runPromptWithStream(ctx, msg, session, agent, text, statusPrefix, true)
		result := run.result
		if strings.TrimSpace(run.streamedReply) != "" && (run.chunks.hasToolBoundary() || strings.TrimSpace(result.Text) == "") {
			result.Text = run.streamedReply
		}
		if run.err == nil && strings.TrimSpace(result.Text) != "" && !s.shouldSuppressAtAutoReply(msg, result.Text) {
			finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(ctx), promptCardFinalUpdateLimit)
			defer finalCancel()
			if finalReply := strings.TrimSpace(result.Text); finalReply != "" && !run.chunks.hasToolBoundary() {
				run.stream.setFinalTextWithContext(finalCtx, finalReply)
			}
			run.stream.flushDelayedWithContext(finalCtx, result, result.StopReason)
		}
		return result, run.stream.hasStarted(), result, run.streamedReply, run.err
	}

	run := s.runPromptWithStream(ctx, msg, session, agent, text, statusPrefix, false)
	result := run.result
	if run.stream.hasStarted() {
		finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(ctx), promptCardFinalUpdateLimit)
		defer finalCancel()
		if finalReply := strings.TrimSpace(result.Text); finalReply != "" && !run.chunks.hasToolBoundary() {
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
	return result, run.stream.hasStarted(), run.rawResult, run.streamedReply, run.err
}

func (s *Service) runPromptWithStream(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, statusPrefix string, delayed bool) promptStreamRun {
	stream := newPromptCardStreamWithStatusPrefix(ctx, msg, session, s.chatConfigForMessage(msg), statusPrefix, s.streamCardStarterForMessage(msg))
	if delayed {
		stream.delayCardCreation()
	}
	chunks := newPromptChunkAccumulator(stream)
	result, err := s.runtime.Prompt(ctx, session, agent, text, s.promptStreamOptions(msg, stream, chunks))
	rawResult := result
	chunks.close()
	return promptStreamRun{
		stream:        stream,
		chunks:        chunks,
		result:        result,
		rawResult:     rawResult,
		streamedReply: chunks.finalText(),
		err:           err,
	}
}

func (s *Service) promptStreamOptions(msg feishu.Message, stream *promptCardStream, chunks *promptChunkAccumulator) acp.PromptOptions {
	return acp.PromptOptions{
		OnUpdate: func(update acp.PromptUpdate) {
			stream.updatePromptStatusFromUpdate(update)
			if chunk, ok := promptUpdateChunk(update); ok {
				if chunk.ToolBoundary {
					chunks.markToolBoundary()
				}
				chunks.add(chunk)
				return
			}
			if isToolBoundaryUpdateKind(promptUpdateKind(update)) {
				chunks.markToolBoundary()
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
