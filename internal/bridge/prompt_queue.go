package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

type queuedPrompt struct {
	msg                  feishu.Message
	session              Session
	agent                config.AgentConfig
	text                 string
	textWrapped          bool
	userText             string
	replyIndex           int
	runCtx               context.Context
	completion           chan queuedPromptResult
	requiredACPSessionID string
}

type queuedPromptResult struct {
	reply string
	err   error
}

type promptQueue struct {
	items    []queuedPrompt
	draining bool
	nextSeq  int
}

func (s *Service) handleQueueCommand(ctx context.Context, text string, msg feishu.Message) string {
	userText := strings.TrimSpace(commandRemainder(text, 1))
	if userText == "" {
		return queueCommandUsage()
	}
	prepared, err := s.preparePrompt(ctx, msg, userText, false)
	if err != nil {
		return "暂存队列任务失败：" + err.Error()
	}
	if prepared.errText != "" {
		return prepared.errText
	}
	session := prepared.session
	session.Key = normalizeSessionKey(session.Key)
	index := s.enqueuePrompt(queuedPrompt{
		msg:         msg,
		session:     session,
		agent:       prepared.agent,
		text:        prepared.text,
		textWrapped: prepared.textWrapped,
		userText:    prepared.titleText,
	})
	if !s.sessionHasRunningUserTask(session.Key) {
		s.drainPromptQueueAsync(context.WithoutCancel(ctx), session.Key)
	}
	return fmt.Sprintf("已暂存到当前会话队列：第 %d 条。当前任务结束后会按顺序执行。", index)
}

func queueCommandUsage() string {
	return "请使用 /queue <要暂存的提示词>。"
}

func (s *Service) enqueuePrompt(item queuedPrompt) int {
	key := normalizeSessionKey(item.session.Key)
	item.session.Key = key
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.promptQueues == nil {
		s.promptQueues = make(map[SessionKey]*promptQueue)
	}
	queue := s.promptQueues[key]
	if queue == nil {
		queue = &promptQueue{}
		s.promptQueues[key] = queue
	}
	queue.nextSeq++
	item.replyIndex = queue.nextSeq
	queue.items = append(queue.items, item)
	return item.replyIndex
}

func (s *Service) drainPromptQueueAsync(ctx context.Context, key SessionKey) {
	key = normalizeSessionKey(key)
	if !s.beginPromptQueueDrain(key) {
		return
	}
	if !s.goBackground(
		ctx,
		"prompt-queue-drain",
		func(ctx context.Context) {
			s.drainPromptQueue(ctx, key)
		},
	) {
		s.resetPromptQueueDrain(key)
	}
}

func (s *Service) resetPromptQueueDrain(key SessionKey) {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if queue := s.promptQueues[key]; queue != nil {
		queue.draining = false
	}
}

func (s *Service) beginPromptQueueDrain(key SessionKey) bool {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	queue := s.promptQueues[key]
	if queue == nil || queue.draining || len(queue.items) == 0 {
		return false
	}
	if task := s.tasks[key]; task != nil {
		return false
	}
	queue.draining = true
	return true
}

func (s *Service) drainPromptQueue(ctx context.Context, key SessionKey) {
	defer s.finishPromptQueueDrain(ctx, key)
	for {
		item, ok := s.takeQueuedPrompt(key)
		if !ok {
			return
		}
		itemCtx := ctx
		if item.runCtx != nil {
			itemCtx = item.runCtx
		}
		if err := itemCtx.Err(); err != nil {
			completeQueuedPrompt(item, "", err)
			continue
		}
		reply, err := s.promptQueuedItem(itemCtx, item)
		if errors.Is(err, errSessionTaskBusy) {
			s.prependQueuedPrompt(key, item)
			return
		}
		if errors.Is(err, context.Canceled) {
			completeQueuedPrompt(item, "", err)
			continue
		}
		if err != nil {
			slog.WarnContext(ctx, "执行队列 prompt 失败", "session", item.session.ACPSessionID, "queue_index", item.replyIndex, "错误", err)
			reply = "队列任务执行失败：" + err.Error()
		}
		if strings.TrimSpace(reply) == "" {
			completeQueuedPrompt(item, "", err)
			continue
		}
		completionErr := err
		if ok, sendErr := s.sendIntermediateReply(ctx, item.msg, reply); sendErr != nil {
			slog.WarnContext(ctx, "发送队列 prompt 回复失败", "session", item.session.ACPSessionID, "queue_index", item.replyIndex, "错误", sendErr)
			completionErr = errors.Join(completionErr, sendErr)
		} else if !ok {
			slog.WarnContext(ctx, "缺少队列 prompt 回复发送器", "session", item.session.ACPSessionID, "queue_index", item.replyIndex)
		}
		completeQueuedPrompt(item, reply, completionErr)
	}
}

func completeQueuedPrompt(item queuedPrompt, reply string, err error) {
	if item.completion == nil {
		return
	}
	select {
	case item.completion <- queuedPromptResult{reply: reply, err: err}:
	default:
	}
}

func (s *Service) takeQueuedPrompt(key SessionKey) (queuedPrompt, bool) {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	queue := s.promptQueues[key]
	if queue == nil || len(queue.items) == 0 {
		return queuedPrompt{}, false
	}
	item := queue.items[0]
	copy(queue.items, queue.items[1:])
	queue.items[len(queue.items)-1] = queuedPrompt{}
	queue.items = queue.items[:len(queue.items)-1]
	return item, true
}

func (s *Service) prependQueuedPrompt(key SessionKey, item queuedPrompt) {
	key = normalizeSessionKey(key)
	item.session.Key = key
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.promptQueues == nil {
		s.promptQueues = make(map[SessionKey]*promptQueue)
	}
	queue := s.promptQueues[key]
	if queue == nil {
		queue = &promptQueue{}
		s.promptQueues[key] = queue
	}
	queue.items = append([]queuedPrompt{item}, queue.items...)
}

func (s *Service) finishPromptQueueDrain(ctx context.Context, key SessionKey) {
	key = normalizeSessionKey(key)
	restart := false
	s.taskMu.Lock()
	queue := s.promptQueues[key]
	if queue == nil {
		s.taskMu.Unlock()
		return
	}
	queue.draining = false
	if len(queue.items) == 0 {
		delete(s.promptQueues, key)
	} else if s.canRestartPromptQueueDrainLocked(key) {
		queue.draining = true
		restart = true
	}
	s.taskMu.Unlock()
	if restart {
		if !s.goBackground(
			context.WithoutCancel(ctx),
			"prompt-queue-drain",
			func(ctx context.Context) {
				s.drainPromptQueue(ctx, key)
			},
		) {
			s.resetPromptQueueDrain(key)
		}
	}
}

func (s *Service) canRestartPromptQueueDrainLocked(key SessionKey) bool {
	key = normalizeSessionKey(key)
	return s.tasks[key] == nil
}

func (s *Service) refreshQueuedPromptSessionState(item queuedPrompt) queuedPrompt {
	store := s.storeForMessage(item.msg)
	if store == nil {
		return item
	}
	current, ok := store.Get(item.session.Key)
	if !ok || current.ACPSessionID != item.session.ACPSessionID {
		return item
	}
	item.session.WorkspacePrompted = current.WorkspacePrompted
	return item
}

func (s *Service) promptQueuedItem(ctx context.Context, item queuedPrompt) (string, error) {
	if strings.TrimSpace(item.requiredACPSessionID) != "" {
		store := s.storeForBotID(item.session.Key.BotID)
		if store == nil {
			return "", fmt.Errorf("绑定会话存储不可用")
		}
		current, ok := store.Get(item.session.Key)
		if !ok || !sameACPSession(current.ACPSessionID, item.requiredACPSessionID) {
			return "", fmt.Errorf("绑定的 ACP session 已不再是当前会话：%s", item.requiredACPSessionID)
		}
		item.session = current
	} else {
		item = s.refreshQueuedPromptSessionState(item)
	}
	var refresh func(context.Context, Session) (Session, error)
	if strings.TrimSpace(item.requiredACPSessionID) == "" {
		refresh = func(refreshCtx context.Context, refreshSession Session) (Session, error) {
			return s.refreshACPSession(refreshCtx, item.msg, refreshSession, item.agent)
		}
	}
	out := s.executePromptWithRecovery(ctx, item.session, func(runCtx context.Context, runSession Session) promptRunOutcome {
		return s.runUserPromptWithWorkspaceContext(runCtx, item.msg, runSession, item.agent, item.text, item.textWrapped, queuedPromptTaskOptions())
	}, refresh)
	post := s.finishPromptPostWork(ctx, out, promptPostWorkOptions{
		botID:                 item.msg.BotID,
		msg:                   item.msg,
		agent:                 item.agent,
		operation:             "queue prompt",
		allowReplySuppression: true,
		updateTitle: func(titleCtx context.Context, current Session) Session {
			return s.updateAutomaticSessionTitle(titleCtx, item.msg, current, item.userText)
		},
		recordACPError: true,
	})
	if post.suppressed {
		return "", nil
	}
	reply := post.reply
	if out.sentProgress {
		return "", out.err
	}
	if out.err != nil && strings.TrimSpace(reply) == "" {
		return "", out.err
	}
	return reply, out.err
}
