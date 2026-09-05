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

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

type sessionForkRequest struct {
	Force            bool
	Title            string
	ExtraUserOpenIDs []string
}

func (s *Service) handleSessionForkCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) >= 3 && strings.EqualFold(fields[2], "retry") {
		if len(fields) != 3 {
			return s.replySessionForkCommand(ctx, msg, "请在分支目标位置使用 /session fork retry。")
		}
		return s.retrySessionFork(ctx, msg)
	}
	req, errText := parseSessionForkRequest(text, msg)
	if errText != "" {
		return s.replySessionForkCommand(ctx, msg, errText)
	}
	return s.startSessionFork(ctx, msg, req)
}

func parseSessionForkRequest(text string, msg feishu.Message) (sessionForkRequest, string) {
	fields := strings.Fields(text)
	if len(fields) < 2 || !strings.EqualFold(fields[1], "fork") {
		return sessionForkRequest{}, "请使用 /session fork [--force] [标题] [mentions...]。"
	}
	args := commandRemainder(text, 2)
	req := sessionForkRequest{}
	if argFields := strings.Fields(args); len(argFields) > 0 && strings.EqualFold(argFields[0], "--force") {
		req.Force = true
		args = commandRemainder(args, 1)
	}
	for _, arg := range strings.Fields(args) {
		if strings.EqualFold(arg, "--force") {
			return sessionForkRequest{}, "--force 只能紧跟在 fork 后面。"
		}
	}
	req.Title = normalizeSessionTitle(redactSensitiveValuesForDisplay(stripNewChatMentions(args, msg.Mentions)))
	req.ExtraUserOpenIDs, _ = newChatMentionOpenIDs(msg)
	return req, ""
}

func (s *Service) startSessionFork(ctx context.Context, msg feishu.Message, req sessionForkRequest) string {
	source, ok := s.findSession(msg)
	if !ok || strings.TrimSpace(source.ACPSessionID) == "" {
		return s.replySessionForkCommand(ctx, msg, "当前位置还没有可分叉的 ACP session。")
	}
	workspace := strings.TrimSpace(firstNonEmpty(source.Workspace, msg.Workspace, s.botWorkspace(msg.BotID)))
	if workspace == "" {
		return s.replySessionForkCommand(ctx, msg, "当前会话没有 workspace，无法分叉。")
	}
	source.Workspace = workspace
	operationStore := s.forkStoreForWorkspace(workspace)
	if operationStore == nil {
		return s.replySessionForkCommand(ctx, msg, "Session fork 操作存储未初始化。")
	}
	if existing, exists := operationStore.GetByCommand(msg.MessageID); exists {
		if existing.State == forkStateFailed && strings.TrimSpace(existing.TargetChatID) == "" &&
			normalizeSessionKey(existing.Source.SourceKey) == normalizeSessionKey(sessionKeyFromMessage(msg)) {
			return s.retrySessionForkFromSource(ctx, msg, operationStore, existing)
		}
		return s.replySessionForkCommand(ctx, msg, formatExistingForkOperation(existing))
	}
	if err := s.validateForkSource(source); err != nil {
		return s.replySessionForkCommand(ctx, msg, "无法分叉当前会话："+err.Error())
	}
	status, snapshot, err := s.freezeSessionForkSource(source)
	if !req.Force && (status.Busy || status.QueueLen > 0 || status.QueueDraining) {
		if err != nil && !errors.Is(err, errNoCompletedForkTurn) {
			return s.replySessionForkCommand(ctx, msg, "冻结分支上下文失败："+err.Error())
		}
		return s.replySessionForkCommand(ctx, msg, formatForkBusyMessage(snapshot.LastUserText))
	}
	if err != nil {
		return s.replySessionForkCommand(ctx, msg, "冻结分支上下文失败："+err.Error())
	}
	title := req.Title
	if title == "" {
		title = normalizeSessionTitle(displaySessionTitle(source) + " · 分支")
	}
	origin := SessionForkOrigin{
		ForkID:               newForkID(),
		SourceKey:            source.Key,
		SourceACPSessionID:   source.ACPSessionID,
		SourceMessageID:      snapshot.CutoffMessageID,
		SourceSnapshotSeq:    snapshot.SnapshotSeq,
		SourceCutoffSeq:      snapshot.CutoffSeq,
		ForkCommandMessageID: strings.TrimSpace(msg.MessageID),
		Forced:               req.Force,
		ForkedAt:             time.Now(),
	}
	traceStore := s.traceStoreForSession(source)
	bundle, err := writeForkBundle(workspace, forkManifest{
		ForkID:                origin.ForkID,
		SourceSessionID:       source.ACPSessionID,
		SourceSessionKey:      source.Key,
		SourceTitle:           displaySessionTitle(source),
		SourceTracePath:       traceStore.sessionPath(source),
		SourceSnapshotSeq:     snapshot.SnapshotSeq,
		SourceCutoffSeq:       snapshot.CutoffSeq,
		SourceCutoffMessageID: snapshot.CutoffMessageID,
		ForkCommandMessageID:  msg.MessageID,
		Forced:                req.Force,
		CreatedBy:             msg.SenderID,
	}, snapshot.Records)
	if err != nil {
		return s.replySessionForkCommand(ctx, msg, "保存分支上下文失败："+err.Error())
	}
	operation := ForkOperation{
		ID:               origin.ForkID,
		State:            forkStatePreparing,
		Source:           origin,
		SourceTitle:      displaySessionTitle(source),
		TargetTitle:      title,
		BundlePath:       bundle.ContextPath,
		ExtraUserOpenIDs: append([]string(nil), req.ExtraUserOpenIDs...),
	}
	stored, inserted, err := operationStore.PutIfCommandAbsent(operation)
	if err != nil {
		_ = os.RemoveAll(bundle.Dir)
		return s.replySessionForkCommand(ctx, msg, "保存分叉操作失败："+err.Error())
	}
	if !inserted {
		_ = os.RemoveAll(bundle.Dir)
		return s.replySessionForkCommand(ctx, msg, formatExistingForkOperation(stored))
	}
	target, sent, inviteWarning, err := s.createSessionForkTarget(ctx, msg, source, title, req.ExtraUserOpenIDs)
	if err != nil {
		if strings.TrimSpace(target.ChatID) != "" {
			operation.State = forkStateTargetCreated
			operation.TargetKey = sessionKeyFromMessage(target)
			operation.TargetChatID = target.ChatID
			operation.TargetThreadID = target.ThreadID
			operation.TargetRootID = target.RootID
			operation.TargetMessageID = sent.MessageID
			operation.InviteWarning = inviteWarning
			_ = operationStore.Put(operation)
		}
		return s.failSessionFork(ctx, msg, operationStore, operation, feishu.Message{}, "创建分支位置失败："+err.Error())
	}
	operation = forkOperationWithTarget(operation, target, sent, inviteWarning)
	if err := operationStore.Put(operation); err != nil {
		return s.failSessionFork(ctx, msg, operationStore, operation, target, "保存分支目标失败："+err.Error())
	}
	return s.finishSessionFork(ctx, msg, target, source, operationStore, operation)
}

func forkOperationWithTarget(operation ForkOperation, target feishu.Message, sent feishu.SentMessage, inviteWarning string) ForkOperation {
	operation.State = forkStateTargetCreated
	operation.TargetKey = sessionKeyFromMessage(target)
	operation.TargetChatID = target.ChatID
	operation.TargetThreadID = target.ThreadID
	operation.TargetRootID = target.RootID
	operation.TargetMessageID = sent.MessageID
	operation.InviteWarning = inviteWarning
	return operation
}

func (s *Service) validateForkSource(source Session) error {
	if _, ok := s.registry.Get(source.AgentName); !ok {
		return fmt.Errorf("未找到源会话 agent 配置: %s", source.AgentName)
	}
	cwd := filepath.Clean(strings.TrimSpace(source.Cwd))
	if cwd == "" || cwd == "." {
		return fmt.Errorf("源会话缺少工作目录")
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return fmt.Errorf("源会话工作目录不可访问: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("源会话工作目录不是目录: %s", cwd)
	}
	return nil
}

func (s *Service) freezeSessionForkSource(source Session) (sessionRuntimeStatus, forkTraceSnapshot, error) {
	trace := s.traceStoreForSession(source)
	if trace == nil {
		return sessionRuntimeStatus{}, forkTraceSnapshot{}, fmt.Errorf("当前 workspace 未启用 ACP trace，无法分叉会话")
	}
	key := normalizeSessionKey(source.Key)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	status := s.sessionRuntimeStatusLocked(key)
	trace.mu.Lock()
	defer trace.mu.Unlock()
	snapshotSeq, err := trace.snapshotSeqLocked(source)
	if err != nil {
		return status, forkTraceSnapshot{}, err
	}
	snapshot, err := readForkTraceSnapshot(trace.sessionPath(source), snapshotSeq)
	return status, snapshot, err
}

func (s *Service) sessionRuntimeStatusLocked(key SessionKey) sessionRuntimeStatus {
	key = normalizeSessionKey(key)
	var status sessionRuntimeStatus
	if task := s.tasks[key]; task != nil {
		status.Busy = true
		status.RunningKind = task.kind
	}
	if !status.Busy {
		for runtime := range s.wikiTasks {
			if normalizeSessionKey(runtime.SessionKey) == key {
				status.Busy = true
				status.RunningKind = taskKindWiki
				break
			}
		}
	}
	if queue := s.promptQueues[key]; queue != nil {
		status.QueueLen = len(queue.items)
		status.QueueDraining = queue.draining
	}
	return status
}

func formatForkBusyMessage(lastUserText string) string {
	lines := []string{"当前会话仍有任务正在运行或排队，暂时不能分叉。"}
	if summary := forkUserSummary(lastUserText); summary != "" {
		lines = append(lines, "上一轮正常结束的消息是：“"+summary+"”")
	} else {
		lines = append(lines, "当前没有已正常结束、可供分叉的消息。")
	}
	lines = append(lines, "如确认跳过运行中和排队消息，请使用 /session fork --force。")
	return strings.Join(lines, "\n")
}

func (s *Service) createSessionForkTarget(ctx context.Context, sourceMsg feishu.Message, source Session, title string, extraUserOpenIDs []string) (feishu.Message, feishu.SentMessage, string, error) {
	display := fmt.Sprintf("已从「%s」分叉，正在接续上下文……", displaySessionTitle(source))
	if sourceMsg.IsTopicGroup() {
		rootTarget := feishu.Message{
			BotID:            sourceMsg.BotID,
			BotOpenID:        sourceMsg.BotOpenID,
			Workspace:        source.Workspace,
			ChatID:           sourceMsg.ChatID,
			ChatType:         "topic_group",
			ChatMode:         "topic",
			GroupMessageType: "thread",
		}
		sent, supported, err := s.sendTextMessageOutbound(ctx, rootTarget, display)
		if err != nil {
			return feishu.Message{}, feishu.SentMessage{}, "", err
		}
		if !supported {
			return feishu.Message{}, feishu.SentMessage{}, "", fmt.Errorf("当前上下文不支持发送新话题")
		}
		if strings.TrimSpace(sent.MessageID) == "" {
			return feishu.Message{}, feishu.SentMessage{}, "", fmt.Errorf("发送新话题后未返回 message_id")
		}
		threadID := firstNonEmpty(sent.ThreadID, sent.RootID, sent.MessageID)
		target := rootTarget
		target.MessageID = sent.MessageID
		target.ThreadID = threadID
		target.RootID = firstNonEmpty(sent.RootID, sent.MessageID)
		target.ForceReplyInThread = true
		return target, sent, "", nil
	}

	ownerID := strings.TrimSpace(sourceMsg.SenderID)
	if ownerID == "" {
		return feishu.Message{}, feishu.SentMessage{}, "", fmt.Errorf("当前消息缺少发送者 open_id")
	}
	chat, supported, err := s.createChat(ctx, sourceMsg, feishu.CreateChatRequest{
		Name:             title,
		Mode:             newChatModeGroup,
		ChatType:         "private",
		GroupMessageType: "chat",
		OwnerOpenID:      ownerID,
		UserOpenIDs:      []string{ownerID},
		SetBotManager:    true,
	})
	if err != nil {
		return feishu.Message{}, feishu.SentMessage{}, "", err
	}
	if !supported {
		return feishu.Message{}, feishu.SentMessage{}, "", fmt.Errorf("当前上下文不支持创建群聊")
	}
	if strings.TrimSpace(chat.ChatID) == "" {
		return feishu.Message{}, feishu.SentMessage{}, "", fmt.Errorf("创建群聊后未返回 chat_id")
	}
	warning := ""
	if len(extraUserOpenIDs) > 0 {
		result, added, addErr := s.addChatMembers(ctx, sourceMsg, feishu.AddChatMembersRequest{ChatID: chat.ChatID, UserOpenIDs: extraUserOpenIDs})
		switch {
		case addErr != nil:
			warning = "部分成员邀请失败：" + addErr.Error()
		case !added:
			warning = "当前上下文不支持邀请额外成员。"
		case len(failedNewChatMemberOpenIDs(&result)) > 0:
			warning = "部分成员未加入：" + strings.Join(failedNewChatMemberOpenIDs(&result), ", ")
		}
	}
	target := feishu.Message{
		BotID:            sourceMsg.BotID,
		BotOpenID:        sourceMsg.BotOpenID,
		Workspace:        source.Workspace,
		ChatID:           chat.ChatID,
		ChatType:         "group",
		ChatMode:         "group",
		GroupMessageType: "chat",
	}
	sent, supported, err := s.sendTextMessageOutbound(ctx, target, display)
	if err != nil {
		return target, feishu.SentMessage{}, warning, err
	}
	if !supported {
		return target, feishu.SentMessage{}, warning, fmt.Errorf("当前上下文不支持向新群发送消息")
	}
	if strings.TrimSpace(sent.MessageID) == "" {
		return target, feishu.SentMessage{}, warning, fmt.Errorf("向新群发送消息后未返回 message_id")
	}
	return target, sent, warning, nil
}

func (s *Service) finishSessionFork(ctx context.Context, sourceMsg, targetMsg feishu.Message, source Session, store *forkOperationStore, operation ForkOperation) string {
	session, agent, err := s.createForkSession(ctx, targetMsg, source, operation.Source, operation.TargetTitle)
	if err != nil {
		return s.failSessionFork(ctx, sourceMsg, store, operation, targetMsg, "创建分支 ACP session 失败："+err.Error())
	}
	operation.State = forkStateSessionCreated
	operation.TargetSession = session.ACPSessionID
	if err := store.Put(operation); err != nil {
		return s.failSessionFork(ctx, sourceMsg, store, operation, targetMsg, "保存分支会话状态失败："+err.Error())
	}
	if strings.TrimSpace(operation.TargetMessageID) != "" {
		if sessionStore := s.storeForMessage(targetMsg); sessionStore != nil {
			if _, err := sessionStore.BindMessageToSession(MessageSessionBinding{
				BotID:      targetMsg.BotID,
				ChatID:     targetMsg.ChatID,
				MessageID:  operation.TargetMessageID,
				SessionKey: session.Key,
			}); err != nil {
				slog.WarnContext(ctx, "绑定 session fork 展示消息失败", "fork_id", operation.ID, "错误", err)
			}
		}
	}
	return s.bootstrapSessionFork(ctx, sourceMsg, targetMsg, session, agent, store, operation)
}

func (s *Service) bootstrapSessionFork(ctx context.Context, sourceMsg, targetMsg feishu.Message, session Session, agent config.AgentConfig, store *forkOperationStore, operation ForkOperation) string {
	if operation.State != forkStateBootstrapping {
		operation.State = forkStateBootstrapping
		operation.Error = ""
		if err := store.Put(operation); err != nil {
			return s.failSessionFork(ctx, sourceMsg, store, operation, targetMsg, "保存分支 bootstrap 状态失败："+err.Error())
		}
	}
	prompt := sessionForkBootstrapPrompt(operation)
	run := s.runUserPromptWithOptionsDetailed(ctx, targetMsg, session, agent, prompt, forkBootstrapTaskOptions())
	if run.err != nil {
		return s.failSessionFork(ctx, sourceMsg, store, operation, targetMsg, "分支上下文接续失败："+run.err.Error())
	}
	if !run.sentProgress && strings.TrimSpace(run.reply) != "" {
		if sent, err := s.sendIntermediateReply(ctx, targetMsg, run.reply); err != nil || !sent {
			if err == nil {
				err = fmt.Errorf("当前上下文不支持发送 bootstrap 回复")
			}
			return s.failSessionFork(ctx, sourceMsg, store, operation, targetMsg, "发送分支接续结果失败："+err.Error())
		}
	}
	operation.State = forkStateReady
	operation.Error = ""
	if err := store.Put(operation); err != nil {
		return s.failSessionFork(ctx, sourceMsg, store, operation, targetMsg, "保存分支完成状态失败："+err.Error())
	}
	s.updateSessionForkDisplay(ctx, targetMsg, operation, fmt.Sprintf("已从「%s」分叉，上下文已接续。", operation.SourceTitle))
	return s.notifySessionForkReady(ctx, sourceMsg, store, operation)
}

func sessionForkBootstrapPrompt(operation ForkOperation) string {
	return fmt.Sprintf(`# Session Fork Bootstrap

这是 Bridge 创建的新会话分支，不是用户提出的新开发任务。

来源：
- source session: %s
- source title: %s
- fork bundle: %s
- source cutoff seq: %d
- source cutoff message id: %s

请完成以下操作：

1. 按 JSONL 解析 fork bundle。
2. 仅使用其中完整的 user/assistant turns 重建已有上下文。
3. 提取当前目标、已确认事实、技术决策、约束、修改文件、已完成验证、失败尝试和未完成事项。
4. 把这些内容视为当前新分支的已有上下文。
5. 本轮不要继续执行未完成任务，不要修改文件，不要执行安装、提交或部署。
6. 不要展示原始 trace、thought、密钥或大段工具输出；任何工具权限请求都应视为不可用。
7. 最终只输出简短的“分支上下文已接续”说明，包括来源会话、当前目标、已完成事项和待处理事项。`,
		operation.Source.SourceACPSessionID, operation.SourceTitle, operation.BundlePath, operation.Source.SourceCutoffSeq, operation.Source.SourceMessageID)
}

func (s *Service) retrySessionFork(ctx context.Context, msg feishu.Message) string {
	store := s.forkStoreForWorkspace(firstNonEmpty(msg.Workspace, s.botWorkspace(msg.BotID)))
	if store == nil {
		return "Session fork 操作存储未初始化。"
	}
	operation, ok := s.findSessionForkOperationForTarget(store, msg)
	if !ok {
		if sourceOperation, sourceOK := store.GetFailedWithoutTargetBySource(sessionKeyFromMessage(msg)); sourceOK {
			return s.retrySessionForkFromSource(ctx, msg, store, sourceOperation)
		}
		return "当前位置不是 Session fork 的目标位置，也没有等待恢复的分叉操作。"
	}
	if operation.State == forkStateReady {
		return "分支上下文已经接续完成，无需重试。"
	}
	if operation.State != forkStateFailed {
		return "分支正在初始化，请稍候。"
	}
	if _, err := os.Stat(operation.BundlePath); err != nil {
		return "无法重试：分支上下文文件不可访问：" + err.Error()
	}
	sessionStore := s.storeForMessage(msg)
	if sessionStore == nil {
		return "会话持久化未初始化。"
	}
	target := forkTargetMessage(msg, operation)
	var (
		targetSession Session
		targetAgent   config.AgentConfig
		sourceSession Session
		reuseTarget   bool
	)
	if operation.TargetSession == "" {
		if existing, exists := sessionStore.Get(operation.TargetKey); exists {
			if existing.ForkOrigin == nil || existing.ForkOrigin.ForkID != operation.ID || strings.TrimSpace(existing.ACPSessionID) == "" {
				return "无法重试：目标位置已经存在其他 ACP session。"
			}
			agent, ok := s.registry.Get(existing.AgentName)
			if !ok {
				return "无法重试：找不到目标 agent 配置。"
			}
			targetSession = existing
			targetAgent = agent
			reuseTarget = true
		} else {
			source, ok := sessionStore.SessionByACPSessionID(msg.BotID, operation.Source.SourceACPSessionID)
			if !ok {
				return "无法重试：找不到源 ACP session。"
			}
			sourceSession = source
		}
	} else {
		session, ok := sessionStore.Get(operation.TargetKey)
		if !ok || session.ACPSessionID != operation.TargetSession {
			return "无法重试：目标 ACP session 已变化。"
		}
		agent, ok := s.registry.Get(session.AgentName)
		if !ok {
			return "无法重试：找不到目标 agent 配置。"
		}
		targetSession = session
		targetAgent = agent
		reuseTarget = true
	}

	claimed, acquired, err := store.ClaimRetry(operation.ID, operation.Revision)
	if err != nil {
		return "无法重试：抢占分支初始化失败：" + err.Error()
	}
	if !acquired {
		if claimed.State == forkStateReady {
			return "分支上下文已经接续完成，无需重试。"
		}
		return "分支正在初始化，请稍候。"
	}
	operation = claimed
	if !reuseTarget {
		return s.finishSessionFork(ctx, feishu.Message{}, target, sourceSession, store, operation)
	}
	if operation.TargetSession == "" {
		operation.TargetSession = targetSession.ACPSessionID
		if err := store.Put(operation); err != nil {
			return s.failSessionFork(ctx, feishu.Message{}, store, operation, target, "保存分支会话状态失败："+err.Error())
		}
	}
	return s.bootstrapSessionFork(ctx, feishu.Message{}, target, targetSession, targetAgent, store, operation)
}

func (s *Service) retrySessionForkFromSource(ctx context.Context, msg feishu.Message, store *forkOperationStore, operation ForkOperation) string {
	if operation.State != forkStateFailed || strings.TrimSpace(operation.TargetChatID) != "" {
		return formatExistingForkOperation(operation)
	}
	if _, err := os.Stat(operation.BundlePath); err != nil {
		return s.replySessionForkCommand(ctx, msg, "无法重试：分支上下文文件不可访问："+err.Error())
	}
	sessionStore := s.storeForMessage(msg)
	if sessionStore == nil {
		return s.replySessionForkCommand(ctx, msg, "会话持久化未初始化。")
	}
	source, ok := sessionStore.SessionByACPSessionID(msg.BotID, operation.Source.SourceACPSessionID)
	if !ok || normalizeSessionKey(source.Key) != normalizeSessionKey(operation.Source.SourceKey) {
		return s.replySessionForkCommand(ctx, msg, "无法重试：找不到源 ACP session。")
	}
	claimed, acquired, err := store.ClaimSourceRetry(operation.ID, operation.Revision)
	if err != nil {
		return s.replySessionForkCommand(ctx, msg, "无法重试：抢占分支目标创建失败："+err.Error())
	}
	if !acquired {
		return s.replySessionForkCommand(ctx, msg, formatExistingForkOperation(claimed))
	}
	operation = claimed
	target, sent, inviteWarning, err := s.createSessionForkTarget(ctx, msg, source, operation.TargetTitle, operation.ExtraUserOpenIDs)
	if strings.TrimSpace(target.ChatID) != "" {
		operation = forkOperationWithTarget(operation, target, sent, inviteWarning)
		if persistErr := store.Put(operation); persistErr != nil {
			return s.failSessionFork(ctx, msg, store, operation, target, "保存分支目标失败："+persistErr.Error())
		}
	}
	if err != nil {
		return s.failSessionFork(ctx, msg, store, operation, target, "创建分支位置失败："+err.Error())
	}
	return s.finishSessionFork(ctx, msg, target, source, store, operation)
}

func forkTargetMessage(msg feishu.Message, operation ForkOperation) feishu.Message {
	target := feishu.Message{
		BotID:            firstNonEmpty(msg.BotID, operation.TargetKey.BotID),
		BotOpenID:        msg.BotOpenID,
		Workspace:        msg.Workspace,
		ChatID:           operation.TargetChatID,
		ChatType:         msg.ChatType,
		GroupMessageType: msg.GroupMessageType,
	}
	if operation.TargetThreadID != "" {
		target.ChatType = "topic_group"
		target.ChatMode = "topic"
		target.GroupMessageType = "thread"
		target.MessageID = firstNonEmpty(operation.TargetMessageID, operation.TargetRootID)
		target.ThreadID = operation.TargetThreadID
		target.RootID = operation.TargetRootID
		target.ForceReplyInThread = true
	} else {
		target.ChatType = "group"
		target.ChatMode = "group"
		target.GroupMessageType = "chat"
	}
	return target
}

func (s *Service) failSessionFork(ctx context.Context, sourceMsg feishu.Message, store *forkOperationStore, operation ForkOperation, targetMsg feishu.Message, message string) string {
	operation.State = forkStateFailed
	operation.Error = redactSensitiveValuesForDisplay(message)
	if err := store.Put(operation); err != nil {
		slog.ErrorContext(ctx, "保存 session fork 失败状态失败", "fork_id", operation.ID, "错误", err)
	}
	if operation.TargetMessageID != "" {
		s.updateSessionForkDisplay(ctx, targetMsg, operation, "分支初始化失败。请在此处执行 /session fork retry。")
	}
	if strings.TrimSpace(sourceMsg.MessageID) == "" {
		return operation.Error
	}
	return s.replySessionForkCommand(ctx, sourceMsg, operation.Error)
}

func (s *Service) updateSessionForkDisplay(ctx context.Context, targetMsg feishu.Message, operation ForkOperation, text string) {
	if operation.TargetMessageID == "" {
		return
	}
	if ok, err := s.updateTextMessageOutbound(ctx, targetMsg, operation.TargetMessageID, text); err != nil {
		slog.WarnContext(ctx, "更新 session fork 展示消息失败", "fork_id", operation.ID, "错误", err)
	} else if !ok {
		slog.WarnContext(ctx, "当前上下文不支持更新 session fork 展示消息", "fork_id", operation.ID)
	}
}

func (s *Service) notifySessionForkReady(ctx context.Context, sourceMsg feishu.Message, store *forkOperationStore, operation ForkOperation) string {
	if strings.TrimSpace(sourceMsg.MessageID) == "" {
		return "分支上下文已接续。"
	}
	notice := "已创建新群聊并分叉当前会话。"
	if operation.TargetThreadID != "" {
		notice = fmt.Sprintf("已创建分支话题「%s」。", operation.TargetTitle)
	}
	if operation.InviteWarning != "" {
		notice += "\n" + operation.InviteWarning
	}
	replyMsg := sourceMsg
	replyMsg.ForceReplyInThread = true
	if _, supported, err := s.sendTextMessageOutbound(ctx, replyMsg, notice); err != nil {
		slog.WarnContext(ctx, "发送 session fork 原位置通知失败", "fork_id", operation.ID, "错误", err)
		return notice
	} else if !supported {
		return notice
	}
	operation.OriginalNoticeSent = true
	if operation.TargetThreadID == "" {
		if _, supported, err := s.sendShareChatOutbound(ctx, replyMsg, operation.TargetChatID); err != nil {
			slog.WarnContext(ctx, "发送 session fork 群名片失败", "fork_id", operation.ID, "chat_id", operation.TargetChatID, "错误", err)
			s.sendSessionForkChatIDFallback(ctx, replyMsg, operation)
		} else if supported {
			operation.OriginalShareChatSent = true
		} else {
			s.sendSessionForkChatIDFallback(ctx, replyMsg, operation)
		}
	}
	if err := store.Put(operation); err != nil {
		slog.WarnContext(ctx, "保存 session fork 通知状态失败", "fork_id", operation.ID, "错误", err)
	}
	return ""
}

func (s *Service) sendSessionForkChatIDFallback(ctx context.Context, replyMsg feishu.Message, operation ForkOperation) {
	text := "群名片发送失败，新群 chat_id：" + operation.TargetChatID
	if _, supported, err := s.sendTextMessageOutbound(ctx, replyMsg, text); err != nil {
		slog.WarnContext(ctx, "发送 session fork chat_id 兜底消息失败", "fork_id", operation.ID, "chat_id", operation.TargetChatID, "错误", err)
	} else if !supported {
		slog.WarnContext(ctx, "当前上下文不支持发送 session fork chat_id 兜底消息", "fork_id", operation.ID, "chat_id", operation.TargetChatID)
	}
}

func (s *Service) replySessionForkCommand(ctx context.Context, msg feishu.Message, text string) string {
	if strings.TrimSpace(msg.MessageID) == "" {
		return text
	}
	reply := msg
	reply.ForceReplyInThread = true
	if _, supported, err := s.sendTextMessageOutbound(ctx, reply, text); err != nil {
		slog.WarnContext(ctx, "发送 session fork 命令回复失败", "错误", err)
		return text
	} else if !supported {
		return text
	}
	return ""
}

func formatExistingForkOperation(operation ForkOperation) string {
	switch operation.State {
	case forkStateReady:
		return "这条命令已经创建过分支。目标 chat_id：" + operation.TargetChatID
	case forkStateFailed:
		if strings.TrimSpace(operation.TargetChatID) == "" {
			return "这条分叉命令此前在创建目标位置前中断，请在源位置执行 /session fork retry。"
		}
		return "这条分叉命令此前执行失败：" + operation.Error
	default:
		return "这条分叉命令正在处理中，请稍候。"
	}
}

func (s *Service) forkStoreForWorkspace(workspace string) *forkOperationStore {
	workspace = normalizeWorkspaceLockPath(workspace)
	if workspace == "" {
		return nil
	}
	s.forkStoreMu.Lock()
	defer s.forkStoreMu.Unlock()
	if store := s.forkStores[workspace]; store != nil {
		return store
	}
	store := newForkOperationStore(workspace)
	if err := store.Load(); err != nil {
		slog.Warn("加载 session fork 操作记录失败", "workspace", workspace, "错误", err)
	}
	if err := store.RecoverInterrupted(); err != nil {
		slog.Warn("恢复中断的 session fork 操作失败", "workspace", workspace, "错误", err)
	}
	s.forkStores[workspace] = store
	return store
}

func (s *Service) forkTargetGuard(msg feishu.Message, text string) string {
	if isSessionForkRetryCommand(text) {
		return ""
	}
	store := s.forkStoreForWorkspace(firstNonEmpty(msg.Workspace, s.botWorkspace(msg.BotID)))
	if store == nil {
		return ""
	}
	operation, ok := s.findSessionForkOperationForTarget(store, msg)
	if !ok || operation.State == forkStateReady {
		return ""
	}
	if operation.State == forkStateFailed {
		return "分支初始化失败，请使用 /session fork retry 重试。"
	}
	return "分支正在初始化，请稍候。"
}

func (s *Service) findSessionForkOperationForTarget(store *forkOperationStore, msg feishu.Message) (ForkOperation, bool) {
	if operation, ok := store.GetByTarget(sessionKeyFromMessage(msg)); ok {
		return operation, true
	}
	messageIDs := append(messageSessionBindingLookupIDs(msg), msg.ThreadID)
	if operation, ok := store.GetByTargetMessageIDs(msg.BotID, msg.ChatID, messageIDs...); ok {
		return operation, true
	}
	if session, ok := s.findSession(msg); ok && session.ForkOrigin != nil {
		return store.Get(session.ForkOrigin.ForkID)
	}
	return ForkOperation{}, false
}

func isSessionForkRetryCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) == 3 && strings.EqualFold(fields[0], "/session") &&
		strings.EqualFold(fields[1], "fork") && strings.EqualFold(fields[2], "retry")
}
