package bridge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func (s *Service) runBoundScheduledTask(ctx context.Context, task ScheduledTask, runID string, triggeredAt time.Time) (TriggerResult, error) {
	task = normalizeScheduledTask(task)
	req, err := scheduledTaskTriggerRequest(task, runID, triggeredAt, s.botWorkspace(task.BotID), nil)
	if err != nil {
		return TriggerResult{Request: req, Err: err}, err
	}
	store := s.storeForBotID(task.BotID)
	if store == nil {
		err := fmt.Errorf("未找到 bot %s 的会话存储", displayBotID(task.BotID))
		return TriggerResult{Request: req, Err: err}, err
	}
	session, ok := store.Get(task.BoundSessionKey)
	if !ok || !sameACPSession(session.ACPSessionID, task.BoundACPSessionID) {
		err := fmt.Errorf("绑定的 ACP session 已不再是当前会话：%s", task.BoundACPSessionID)
		return TriggerResult{Request: req, Session: session, Err: err}, err
	}
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		err := fmt.Errorf("未找到绑定会话的 agent 配置：%s", session.AgentName)
		return TriggerResult{Request: req, Session: session, Err: err}, err
	}

	msg := boundScheduledTaskMessage(task, runID, session)
	completion := make(chan queuedPromptResult, 1)
	s.enqueuePrompt(queuedPrompt{
		msg:                  msg,
		session:              session,
		agent:                agent,
		text:                 scheduledTaskRunPrompt(task, runID, triggeredAt),
		textWrapped:          true,
		userText:             task.Prompt,
		runCtx:               ctx,
		completion:           completion,
		requiredACPSessionID: task.BoundACPSessionID,
	})
	if !s.sessionHasRunningTask(session.Key) {
		s.drainPromptQueueAsync(context.WithoutCancel(ctx), session.Key)
	}

	select {
	case <-ctx.Done():
		err := ctx.Err()
		return TriggerResult{Request: req, Session: session, Err: err}, err
	case completed := <-completion:
		result := TriggerResult{
			Request:      req,
			Session:      session,
			Text:         completed.reply,
			TextSet:      true,
			Err:          completed.err,
			ACPSessionID: session.ACPSessionID,
		}
		return result, completed.err
	}
}

func boundScheduledTaskMessage(task ScheduledTask, runID string, session Session) feishu.Message {
	key := normalizeSessionKey(task.BoundSessionKey)
	msg := feishu.Message{
		BotID:              task.BotID,
		MessageID:          task.CreatedFromMessageID,
		ChatID:             key.MainID,
		ChatType:           firstNonEmpty(task.CreatedFromChatType, "group"),
		ThreadID:           key.SubID,
		SenderID:           task.CreatorOpenID,
		SenderType:         "user",
		MsgType:            "text",
		Text:               task.Prompt,
		Workspace:          session.Workspace,
		ForceReplyInThread: strings.TrimSpace(key.SubID) != "",
	}
	if key.SubID != "" {
		msg.GroupMessageType = "thread"
	}
	return msg
}
