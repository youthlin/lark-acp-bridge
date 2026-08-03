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
	result, sentProgress, err := s.runUserPrompt(ctx, msg, session, agent, text)
	if errors.Is(err, errACPSessionUnavailable) && !sentProgress {
		refreshed, refreshErr := s.refreshACPSession(ctx, msg, session, agent)
		if refreshErr != nil {
			return "", refreshErr
		}
		session = refreshed
		result, sentProgress, err = s.runUserPrompt(ctx, msg, session, agent, text)
	}
	reply := result.Text
	s.recordPromptTokenUsage(ctx, msg.BotID, session, result)
	if !opts.SkipPostPromptWork && err == nil {
		s.scheduleWikiAfterUserPrompt(session, agent)
	}
	if s.shouldSuppressAtAutoReply(msg, reply) {
		return "", nil
	}
	if !opts.SkipPostPromptWork && !errors.Is(err, context.Canceled) && (err == nil || strings.TrimSpace(reply) != "" || sentProgress) {
		session = s.updateAutomaticSessionTitle(ctx, msg, session, userText)
	}
	if !opts.SkipPostPromptWork {
		s.maybeRunAutoCompact(ctx, msg, session, agent, result, err)
	}
	if sentProgress {
		reply = ""
		result.Text = ""
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", nil
		}
		s.recordACPError(session, "prompt", err)
		if strings.TrimSpace(reply) != "" {
			return reply, nil
		}
		if sentProgress {
			return "", nil
		}
		return "", err
	}
	if strings.TrimSpace(reply) == "" {
		if sentProgress {
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
	return s.runUserPromptWithOptions(ctx, msg, session, agent, text, runningTaskOptions{
		drainPendingAtAuto: isAtAutoMode(s.chatAtMode(msg)) && messageMentionsBot(msg),
	})
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
