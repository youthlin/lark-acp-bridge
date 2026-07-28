package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const promptCardFinalUpdateLimit = 15 * time.Second

type preparedPrompt struct {
	session Session
	agent   config.AgentConfig
	text    string
	errText string
}

func (s *Service) prompt(ctx context.Context, msg feishu.Message, text string) (string, error) {
	prepared, err := s.preparePrompt(ctx, msg, text)
	if err != nil {
		return "", err
	}
	if prepared.errText != "" {
		return prepared.errText, nil
	}
	return s.promptSession(ctx, msg, prepared.session, prepared.agent, prepared.text, text)
}

func (s *Service) preparePrompt(ctx context.Context, msg feishu.Message, userText string) (preparedPrompt, error) {
	session, ok := s.findSession(msg)
	if !ok {
		created, agent, _, errText := s.createSession(ctx, []string{"/new"}, msg)
		if errText != "" {
			return preparedPrompt{errText: errText}, nil
		}
		text := promptTextWithWorkspaceContext(sessionWorkspace(created, msg), msg, promptTextWithReplyContext(msg, userText))
		return preparedPrompt{session: created, agent: agent, text: text}, nil
	}
	if agentName := s.chatAgentName(msg); strings.TrimSpace(agentName) != "" && session.AgentName != agentName {
		created, agent, _, errText := s.createSession(ctx, []string{"/new"}, msg)
		if errText != "" {
			return preparedPrompt{errText: errText}, nil
		}
		text := promptTextWithWorkspaceContext(sessionWorkspace(created, msg), msg, promptTextWithReplyContext(msg, userText))
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
	text = promptTextWithWorkspaceContext(sessionWorkspace(session, msg), msg, text)
	return preparedPrompt{session: session, agent: agent, text: text}, nil
}

func (s *Service) promptSession(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string, userText string) (string, error) {
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
	if !errors.Is(err, context.Canceled) && (err == nil || strings.TrimSpace(reply) != "" || sentProgress) {
		session = s.updateAutomaticSessionTitle(ctx, msg, session, userText)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", nil
		}
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
	return reply, nil
}

func (s *Service) refreshACPSession(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig) (Session, error) {
	cwd := strings.TrimSpace(session.Cwd)
	if cwd == "" {
		return Session{}, fmt.Errorf("当前会话缺少工作目录，无法重建 ACP session")
	}
	slog.WarnContext(ctx, "持久化 ACP session 不可恢复，准备重建", "session", session.ACPSessionID, "cwd", cwd)
	sessionInfo, err := s.runtime.NewSession(ctx, session.Key, session.AgentName, agent, filepath.Clean(cwd), sessionWorkspace(session, msg))
	if err != nil {
		return Session{}, fmt.Errorf("重建 ACP session 失败: %w", err)
	}
	session.ACPSessionID = sessionInfo.SessionID
	session.Cwd = filepath.Clean(cwd)
	if strings.TrimSpace(session.Workspace) == "" {
		session.Workspace = msg.Workspace
	}
	session.ACPMeta = maps.Clone(sessionInfo.Meta)
	session.AvailableCommands = sessionInfo.AvailableCommands
	session.ConfigOptions = sessionInfo.ConfigOptions
	session.Models = sessionInfo.Models
	store := s.storeForMessage(msg)
	if store == nil {
		return session, nil
	}
	if err := store.Upsert(session); err != nil {
		return Session{}, fmt.Errorf("保存重建后的 ACP session 失败: %w", err)
	}
	slog.InfoContext(ctx, "已重建 ACP session", "session", session.ACPSessionID, "cwd", session.Cwd)
	return session, nil
}

func (s *Service) runUserPrompt(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string) (acp.PromptResult, bool, error) {
	ctx, finish := s.startTask(ctx, session, agent, taskKindUser)
	defer finish()
	result, sentProgress, err := s.promptRuntimeWithProgress(ctx, msg, session, agent, text)
	if errors.Is(err, context.Canceled) {
		return result, sentProgress, err
	}
	if err == nil {
		s.scheduleWikiAfterUserPrompt(session, agent)
	}
	return result, sentProgress, err
}

func (s *Service) promptRuntimeWithProgress(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string) (acp.PromptResult, bool, error) {
	result, sentProgress, _, _, err := s.promptRuntimeWithProgressRaw(ctx, msg, session, agent, text)
	return result, sentProgress, err
}

func (s *Service) promptRuntimeWithProgressRaw(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig, text string) (acp.PromptResult, bool, acp.PromptResult, string, error) {
	slog.InfoContext(ctx, "准备发送消息给ACP后端")
	stream := newPromptCardStream(ctx, msg, session, s.chatConfigForMessage(msg))
	chunks := newPromptChunkAccumulator(stream)
	flushStreams := func() {
		chunks.finishStream()
	}
	opts := acp.PromptOptions{
		OnUpdate: func(update acp.PromptUpdate) {
			// slog.InfoContext(ctx, "ACP|OnUpdate", "update", update)
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
				flushStreams()
			}
			stream.updatePromptUpdate(update)
		},
		OnPermissionRequest: func(reqCtx context.Context, req acp.PermissionRequest) (acp.PermissionOutcome, error) {
			outcome, ok, err := feishu.RequestPermission(reqCtx, msg, req)
			if err != nil {
				return acp.PermissionOutcome{}, err
			}
			if ok {
				return outcome, nil
			}
			return defaultPermissionOutcome(req), nil
		},
	}
	result, err := s.runtime.Prompt(ctx, session, agent, text, opts)
	rawResult := result
	chunks.close()
	streamedReply := chunks.finalText()
	if stream.hasStarted() {
		finalCtx, finalCancel := context.WithTimeout(context.WithoutCancel(ctx), promptCardFinalUpdateLimit)
		defer finalCancel()
		if finalReply := strings.TrimSpace(result.Text); finalReply != "" && !chunks.hasToolBoundary() && finalReply != streamedReply {
			stream.updateTextWithContext(finalCtx, finalReply)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				stream.updateProcessMessageWithContext(finalCtx, "已取消")
				stream.finishPromptStatusWithContext(finalCtx, "cancelled")
			} else {
				stream.updateProcessMessageWithContext(finalCtx, "执行失败："+err.Error())
				stream.failPromptStatusWithContext(finalCtx)
			}
		} else {
			stream.updatePromptStatusFromResultWithContext(finalCtx, result)
			stream.updatePromptResult(result)
			stream.finishPromptStatusWithContext(finalCtx, result.StopReason)
		}
		stream.closeWithContext(finalCtx)
	}
	if stream.hasStarted() {
		result.Text = ""
	}
	return result, stream.hasStarted(), rawResult, streamedReply, err
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
