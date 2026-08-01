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
	msg        feishu.Message
	session    Session
	agent      config.AgentConfig
	text       string
	userText   string
	replyIndex int
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
	prepared, err := s.preparePrompt(ctx, msg, userText)
	if err != nil {
		return "暂存队列任务失败：" + err.Error()
	}
	if prepared.errText != "" {
		return prepared.errText
	}
	session := prepared.session
	session.Key = normalizeSessionKey(session.Key)
	index := s.enqueuePrompt(queuedPrompt{
		msg:      msg,
		session:  session,
		agent:    prepared.agent,
		text:     prepared.text,
		userText: userText,
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

func (s *Service) sessionHasRunningUserTask(key SessionKey) bool {
	key = normalizeSessionKey(key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	task := s.tasks[key]
	return task != nil && task.kind == taskKindUser
}

func (s *Service) drainPromptQueueAsync(ctx context.Context, key SessionKey) {
	key = normalizeSessionKey(key)
	if !s.beginPromptQueueDrain(key) {
		return
	}
	go s.drainPromptQueue(ctx, key)
}

func (s *Service) beginPromptQueueDrain(key SessionKey) bool {
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
		reply, err := s.promptQueuedItem(ctx, item)
		if errors.Is(err, errSessionTaskBusy) {
			s.prependQueuedPrompt(key, item)
			return
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			slog.WarnContext(ctx, "执行队列 prompt 失败", "session", item.session.ACPSessionID, "queue_index", item.replyIndex, "错误", err)
			reply = "队列任务执行失败：" + err.Error()
		}
		if strings.TrimSpace(reply) == "" {
			continue
		}
		if ok, err := s.sendIntermediateReply(ctx, item.msg, reply); err != nil {
			slog.WarnContext(ctx, "发送队列 prompt 回复失败", "session", item.session.ACPSessionID, "queue_index", item.replyIndex, "错误", err)
		} else if !ok {
			slog.WarnContext(ctx, "缺少队列 prompt 回复发送器", "session", item.session.ACPSessionID, "queue_index", item.replyIndex)
		}
	}
}

func (s *Service) takeQueuedPrompt(key SessionKey) (queuedPrompt, bool) {
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
	} else if s.tasks[key] == nil {
		queue.draining = true
		restart = true
	}
	s.taskMu.Unlock()
	if restart {
		go s.drainPromptQueue(context.WithoutCancel(ctx), key)
	}
}

func (s *Service) promptQueuedItem(ctx context.Context, item queuedPrompt) (string, error) {
	result, sentProgress, err := s.runUserPromptWithOptions(ctx, item.msg, item.session, item.agent, item.text, runningTaskOptions{
		queuedContinuation: true,
	})
	if errors.Is(err, errACPSessionUnavailable) && !sentProgress {
		refreshed, refreshErr := s.refreshACPSession(ctx, item.msg, item.session, item.agent)
		if refreshErr != nil {
			return "", refreshErr
		}
		item.session = refreshed
		result, sentProgress, err = s.runUserPromptWithOptions(ctx, item.msg, item.session, item.agent, item.text, runningTaskOptions{
			queuedContinuation: true,
		})
	}
	reply := result.Text
	s.recordPromptTokenUsage(ctx, item.msg.BotID, item.session, result)
	if err == nil {
		s.scheduleWikiAfterUserPrompt(item.session, item.agent)
	}
	if s.shouldSuppressAtAutoReply(item.msg, reply) {
		return "", nil
	}
	if !errors.Is(err, context.Canceled) && (err == nil || strings.TrimSpace(reply) != "" || sentProgress) {
		item.session = s.updateAutomaticSessionTitle(ctx, item.msg, item.session, item.userText)
	}
	if sentProgress {
		return "", err
	}
	if err != nil && strings.TrimSpace(reply) == "" {
		return "", err
	}
	return reply, err
}
