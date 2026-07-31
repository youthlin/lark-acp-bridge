package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

type scheduleAddArgs struct {
	Spec      string
	Prompt    string
	AgentName string
	Cwd       string
}

func (s *Service) handleScheduleCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return scheduleCommandUsage()
	}
	switch strings.ToLower(strings.TrimSpace(fields[1])) {
	case "add":
		return s.handleScheduleAddCommand(ctx, text, msg)
	case "how":
		return s.handleScheduleHowCommand(ctx, text, msg)
	case "list":
		return s.handleScheduleListCommand(msg)
	case "status":
		if len(fields) < 3 {
			return "请使用 /schedule status <id>。"
		}
		return s.handleScheduleStatusCommand(msg, fields[2])
	case "run":
		if len(fields) < 3 {
			return "请使用 /schedule run <id>。"
		}
		return s.handleScheduleRunCommand(ctx, msg, fields[2])
	case "edit":
		if len(fields) < 3 {
			return "请使用 /schedule edit <id> [--cwd <path>] [--agent <name>] <spec> <prompt>。"
		}
		return s.handleScheduleEditCommand(ctx, text, msg, fields[2])
	case "pause":
		if len(fields) < 3 {
			return "请使用 /schedule pause <id>。"
		}
		return s.setScheduledTaskEnabled(ctx, msg, fields[2], false)
	case "resume":
		if len(fields) < 3 {
			return "请使用 /schedule resume <id>。"
		}
		return s.setScheduledTaskEnabled(ctx, msg, fields[2], true)
	case "delete":
		if len(fields) < 3 {
			return "请使用 /schedule delete <id>。"
		}
		return s.handleScheduleDeleteCommand(msg, fields[2])
	default:
		return scheduleCommandUsage()
	}
}

func scheduleCommandUsage() string {
	return strings.Join([]string{
		"可用命令：",
		"/schedule add <spec> <prompt>",
		"/schedule how <自然语言需求>",
		"/schedule list",
		"/schedule status <id>",
		"/schedule run <id>",
		"/schedule edit <id> [--cwd <path>] [--agent <name>] <spec> <prompt>",
		"/schedule pause <id>",
		"/schedule resume <id>",
		"/schedule delete <id>",
		"spec 支持 @every <duration> 或 5 段 cron，例如 /schedule add --cwd /repo --agent traex @every 1h 生成日报。",
		"/schedule how 会让当前 ACP 会话把自然语言需求转换为一条 /schedule add 命令。",
	}, "\n")
}

func (s *Service) handleScheduleAddCommand(ctx context.Context, text string, msg feishu.Message) string {
	store := s.scheduledTaskStoreForBotID(msg.BotID)
	if store == nil {
		return "当前 bot workspace 未初始化，无法创建定时任务。"
	}
	args, errText := s.parseScheduleAddArgs(commandRemainder(text, 2), msg)
	if errText != "" {
		return errText
	}
	if _, err := parseScheduleSpec(args.Spec, ""); err != nil {
		return "定时任务 spec 无效：" + err.Error()
	}
	agentName := args.AgentName
	if agentName == "" {
		agentName = s.chatAgentName(msg)
	}
	if _, ok := s.registry.Get(agentName); !ok {
		return "未知 agent：" + agentName
	}
	cwd := args.Cwd
	if cwd == "" {
		cwd, _, errText = s.defaultNewSessionCwd(msg)
		if errText != "" {
			return errText
		}
	}
	id := newScheduledTaskID()
	task, err := store.Upsert(ScheduledTask{
		ID:                   id,
		BotID:                msg.BotID,
		Enabled:              true,
		Spec:                 args.Spec,
		AgentName:            agentName,
		Cwd:                  cwd,
		Prompt:               args.Prompt,
		CreatorOpenID:        msg.SenderID,
		CreatedFromChatID:    msg.ChatID,
		CreatedFromThreadID:  msg.ThreadID,
		CreatedFromMessageID: msg.MessageID,
		ResultSink: ScheduledTaskResultSink{
			Type:      "im",
			ChatID:    msg.ChatID,
			ThreadID:  msg.ThreadID,
			MessageID: msg.MessageID,
		},
		OverlapPolicy: scheduleOverlapSkipIfRunning,
	})
	if err != nil {
		slog.ErrorContext(ctx, "保存定时任务失败", "错误", err)
		return "保存定时任务失败：" + err.Error()
	}
	if err := s.startScheduledTask(context.Background(), task, msg.Workspace); err != nil {
		slog.ErrorContext(ctx, "注册定时任务失败", "task_id", task.ID, "错误", err)
		return "定时任务已保存，但注册到当前进程失败：" + err.Error()
	}
	return formatScheduledTaskCreated(task)
}

func (s *Service) handleScheduleHowCommand(ctx context.Context, text string, msg feishu.Message) string {
	prompt, err := scheduleHowPrompt(strings.TrimSpace(commandRemainder(text, 2)))
	if err != nil {
		return err.Error()
	}
	reply, err := s.prompt(ctx, msg, prompt)
	if err != nil {
		return "生成 schedule 命令失败：" + err.Error()
	}
	return reply
}

func scheduleHowPrompt(goal string) (string, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return "", fmt.Errorf("请提供想创建的定时任务，例如 /schedule how 每天上午 8 点执行 scripts/report.sh。")
	}
	return strings.Join([]string{
		"你正在为用户生成一条 lark-acp-bridge 可直接执行的 /schedule add 命令。当前 ACP 会话可能没有 bridge 项目代码上下文，因此下面先给出完整命令格式和参数语义。",
		"",
		"## /schedule add 命令格式",
		"/schedule add [--cwd <workspace path>] [--agent <agent name>] <spec> <prompt>",
		"",
		"参数说明：",
		"- --cwd <workspace path>：可选，指定任务运行工作目录；用户未明确提供时不要编造。",
		"- --agent <agent name>：可选，指定 ACP agent；用户未明确提供时不要编造。",
		"- <spec>：必填，支持 @every <duration> 或 5 段 cron。",
		"- <prompt>：必填，定时触发时发送给 agent 的任务说明，必须放在 spec 后面。",
		"",
		"spec 规则：",
		"- @every <duration> 用于固定间隔，例如 @every 30m、@every 1h。",
		"- 5 段 cron 顺序是 minute hour day-of-month month day-of-week。",
		"- 每天上午 8 点应生成 0 8 * * *。",
		"- 每周一上午 9 点应生成 0 9 * * 1。",
		"- 工作日上午 10 点应生成 0 10 * * 1-5。",
		"- 如果用户给出的时间无法可靠转换，返回一句需要用户补充的信息，不要猜测。",
		"",
		"用户需求：",
		goal,
		"",
		"请根据用户需求生成一条可直接执行的 /schedule add 命令。",
		"要求：",
		"- 最终只返回一条 /schedule add 命令，或一条要求用户补充必要时间信息的中文句子。",
		"- 不要解释，不要使用 Markdown 代码块。",
		"- 不要真正创建任务，只生成命令。",
		"- 不要使用 /schedule how 作为最终结果。",
		"- 不要编造用户未提供的仓库、路径、agent、环境或背景。",
		"- 如果用户提到脚本、命令或文件路径，把它保留在 prompt 中。",
		"- prompt 应明确说明定时触发时要执行的任务，并要求需要时先检查相关文件或脚本是否存在。",
	}, "\n"), nil
}

func (s *Service) parseScheduleAddArgs(args string, msg feishu.Message) (scheduleAddArgs, string) {
	return s.parseScheduleTaskArgs(args, msg, "add")
}

func (s *Service) parseScheduleTaskArgs(args string, msg feishu.Message, commandName string) (scheduleAddArgs, string) {
	args = strings.TrimSpace(args)
	commandName = strings.TrimSpace(commandName)
	if commandName == "" {
		commandName = "add"
	}
	if args == "" {
		return scheduleAddArgs{}, "请使用 /schedule " + commandName + " <spec> <prompt>。"
	}
	fields := strings.Fields(args)
	if len(fields) < 2 {
		return scheduleAddArgs{}, "请提供定时任务 spec 和 prompt。"
	}
	parsed := scheduleAddArgs{}
	index := 0
	for index < len(fields) {
		field := strings.TrimSpace(fields[index])
		switch {
		case field == "--cwd":
			if index+1 >= len(fields) {
				return scheduleAddArgs{}, "请为 --cwd 指定工作目录。"
			}
			cwd, isPath, errText := s.resolveNewSessionCwdArg(fields[index+1], msg)
			if errText != "" {
				return scheduleAddArgs{}, errText
			}
			if !isPath || cwd == "" {
				return scheduleAddArgs{}, "请为 --cwd 指定有效工作目录。"
			}
			parsed.Cwd = cwd
			index += 2
		case strings.HasPrefix(field, "--cwd="):
			value := strings.TrimSpace(strings.TrimPrefix(field, "--cwd="))
			cwd, isPath, errText := s.resolveNewSessionCwdArg(value, msg)
			if errText != "" {
				return scheduleAddArgs{}, errText
			}
			if !isPath || cwd == "" {
				return scheduleAddArgs{}, "请为 --cwd 指定有效工作目录。"
			}
			parsed.Cwd = cwd
			index++
		case field == "--agent":
			if index+1 >= len(fields) {
				return scheduleAddArgs{}, "请为 --agent 指定 agent 名称。"
			}
			parsed.AgentName = strings.TrimSpace(fields[index+1])
			index += 2
		case strings.HasPrefix(field, "--agent="):
			parsed.AgentName = strings.TrimSpace(strings.TrimPrefix(field, "--agent="))
			index++
		case strings.HasPrefix(field, "-"):
			return scheduleAddArgs{}, "未知 schedule " + commandName + " 参数：" + field
		default:
			fields = fields[index:]
			args = strings.Join(fields, " ")
			index = 0
			goto parseSpec
		}
	}
	return scheduleAddArgs{}, "请提供定时任务 spec 和 prompt。"

parseSpec:
	if strings.EqualFold(fields[0], "@every") {
		if len(fields) < 3 {
			return scheduleAddArgs{}, "请使用 /schedule add @every <duration> <prompt>。"
		}
		parsed.Spec = "@every " + fields[1]
		parsed.Prompt = strings.TrimSpace(strings.TrimPrefix(args, fields[0]+" "+fields[1]))
		if parsed.Prompt == "" {
			return scheduleAddArgs{}, "定时任务 prompt 不能为空。"
		}
		return parsed, ""
	}
	if len(fields) < 6 {
		return scheduleAddArgs{}, "cron spec 需要 5 段，并在后面提供 prompt。"
	}
	parsed.Spec = strings.Join(fields[:5], " ")
	parsed.Prompt = strings.TrimSpace(strings.TrimPrefix(args, strings.Join(fields[:5], " ")))
	if parsed.Prompt == "" {
		return scheduleAddArgs{}, "定时任务 prompt 不能为空。"
	}
	return parsed, ""
}

func (s *Service) handleScheduleListCommand(msg feishu.Message) string {
	store := s.scheduledTaskStoreForBotID(msg.BotID)
	if store == nil {
		return "当前 bot workspace 未初始化，无法读取定时任务。"
	}
	tasks := store.List()
	if len(tasks) == 0 {
		return "当前 bot 还没有定时任务。"
	}
	lines := []string{"当前 bot 的定时任务："}
	for i, task := range tasks {
		state := "暂停"
		if task.Enabled {
			state = "启用"
		}
		lines = append(lines, fmt.Sprintf("%d. %s [%s]\n   spec：%s\n   agent：%s\n   cwd：%s\n   prompt：%s", i+1, task.ID, state, task.Spec, task.AgentName, task.Cwd, oneLine(task.Prompt, 80)))
	}
	return strings.Join(lines, "\n")
}

func (s *Service) handleScheduleStatusCommand(msg feishu.Message, id string) string {
	store := s.scheduledTaskStoreForBotID(msg.BotID)
	if store == nil {
		return "当前 bot workspace 未初始化，无法读取定时任务。"
	}
	task, ok := store.Get(id)
	if !ok {
		return "定时任务不存在：" + strings.TrimSpace(id)
	}
	lines := []string{
		"定时任务：" + task.ID,
		"状态：" + scheduledTaskEnabledText(task.Enabled),
		"spec：" + task.Spec,
		"agent：" + task.AgentName,
		"cwd：" + task.Cwd,
		"prompt：" + task.Prompt,
		"创建者：" + task.CreatorOpenID,
		"回传：" + scheduledTaskSinkText(task.ResultSink),
	}
	last, ok := s.lastScheduleRunStatus(task.ID)
	if ok {
		lines = append(lines,
			"最近 run："+last.RunID,
			"最近状态："+string(last.State),
		)
		if last.LastError != "" {
			lines = append(lines, "最近错误："+last.LastError)
		}
		if last.SkipReason != "" {
			lines = append(lines, "跳过原因："+last.SkipReason)
		}
	}
	return strings.Join(lines, "\n")
}

func (s *Service) handleScheduleEditCommand(ctx context.Context, text string, msg feishu.Message, id string) string {
	store := s.scheduledTaskStoreForBotID(msg.BotID)
	if store == nil {
		return "当前 bot workspace 未初始化，无法编辑定时任务。"
	}
	id = strings.TrimSpace(id)
	existing, ok := store.Get(id)
	if !ok {
		return "定时任务不存在：" + id
	}
	args, errText := s.parseScheduleTaskArgs(commandRemainder(text, 3), msg, "edit")
	if errText != "" {
		return errText
	}
	if _, err := parseScheduleSpec(args.Spec, ""); err != nil {
		return "定时任务 spec 无效：" + err.Error()
	}
	agentName := args.AgentName
	if agentName == "" {
		agentName = existing.AgentName
	}
	if _, ok := s.registry.Get(agentName); !ok {
		return "未知 agent：" + agentName
	}
	cwd := args.Cwd
	if cwd == "" {
		cwd = existing.Cwd
	}
	task, ok, err := store.Update(id, func(task *ScheduledTask) {
		task.Spec = args.Spec
		task.AgentName = agentName
		task.Cwd = cwd
		task.Prompt = args.Prompt
	})
	if err != nil {
		slog.ErrorContext(ctx, "编辑定时任务失败", "task_id", id, "错误", err)
		return "编辑定时任务失败：" + err.Error()
	}
	if !ok {
		return "定时任务不存在：" + id
	}
	if task.Enabled {
		workspace := strings.TrimSpace(s.botWorkspace(task.BotID))
		if workspace == "" {
			workspace = strings.TrimSpace(msg.Workspace)
		}
		if err := s.startScheduledTask(context.Background(), task, workspace); err != nil {
			slog.ErrorContext(ctx, "重新注册定时任务失败", "task_id", task.ID, "错误", err)
			return "定时任务已保存，但重新注册到当前进程失败：" + err.Error()
		}
	} else {
		s.stopScheduledTask(task)
	}
	return formatScheduledTaskUpdated(task)
}

func (s *Service) handleScheduleRunCommand(ctx context.Context, msg feishu.Message, id string) string {
	store := s.scheduledTaskStoreForBotID(msg.BotID)
	if store == nil {
		return "当前 bot workspace 未初始化，无法执行定时任务。"
	}
	task, ok := store.Get(id)
	if !ok {
		return "定时任务不存在：" + strings.TrimSpace(id)
	}
	workspace := strings.TrimSpace(s.botWorkspace(task.BotID))
	if workspace == "" {
		workspace = strings.TrimSpace(msg.Workspace)
	}
	if workspace == "" {
		return "定时任务 workspace 为空，无法执行：" + task.ID
	}
	triggeredAt := time.Now()
	runID := scheduledTaskRunID(task, triggeredAt)
	runKey := scheduledTaskRunKey(task, runID)
	s.markScheduleRunPending(task, runID, runKey, triggeredAt)
	runCtx := context.WithoutCancel(ctx)
	go func() {
		if _, err := s.runScheduledTaskOnce(runCtx, task, runID, triggeredAt, workspace, nil); err != nil {
			slog.ErrorContext(runCtx, "立即执行定时任务失败", "task_id", task.ID, "run_id", runID, "错误", err)
		}
	}()
	return strings.Join([]string{
		"已开始立即执行定时任务：" + task.ID,
		"run：" + runID,
		"执行结果会发送到该任务配置的回传目标。",
	}, "\n")
}

func (s *Service) setScheduledTaskEnabled(ctx context.Context, msg feishu.Message, id string, enabled bool) string {
	store := s.scheduledTaskStoreForBotID(msg.BotID)
	if store == nil {
		return "当前 bot workspace 未初始化，无法更新定时任务。"
	}
	task, ok, err := store.Update(id, func(task *ScheduledTask) {
		task.Enabled = enabled
	})
	if err != nil {
		slog.ErrorContext(ctx, "更新定时任务失败", "task_id", id, "错误", err)
		return "更新定时任务失败：" + err.Error()
	}
	if !ok {
		return "定时任务不存在：" + strings.TrimSpace(id)
	}
	if enabled {
		if err := s.startScheduledTask(context.Background(), task, msg.Workspace); err != nil {
			slog.ErrorContext(ctx, "注册定时任务失败", "task_id", task.ID, "错误", err)
			return "定时任务已恢复，但注册到当前进程失败：" + err.Error()
		}
		return "已恢复定时任务：" + task.ID
	}
	s.stopScheduledTask(task)
	return "已暂停定时任务：" + task.ID
}

func (s *Service) handleScheduleDeleteCommand(msg feishu.Message, id string) string {
	store := s.scheduledTaskStoreForBotID(msg.BotID)
	if store == nil {
		return "当前 bot workspace 未初始化，无法删除定时任务。"
	}
	task, ok := store.Get(id)
	if !ok {
		return "定时任务不存在：" + strings.TrimSpace(id)
	}
	deleted, err := store.Delete(id)
	if err != nil {
		return "删除定时任务失败：" + err.Error()
	}
	if !deleted {
		return "定时任务不存在：" + strings.TrimSpace(id)
	}
	s.stopScheduledTask(task)
	return "已删除定时任务：" + task.ID
}

func (s *Service) lastScheduleRunStatus(taskID string) (scheduleRunStatus, bool) {
	taskID = strings.TrimSpace(taskID)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	var last scheduleRunStatus
	var ok bool
	for _, status := range s.scheduleRuns {
		if status.TaskID != taskID {
			continue
		}
		if !ok || scheduleRunStatusTime(status).After(scheduleRunStatusTime(last)) {
			last = status
			ok = true
		}
	}
	return last, ok
}

func scheduleRunStatusTime(status scheduleRunStatus) time.Time {
	for _, t := range []time.Time{status.EndedAt, status.SkippedAt, status.StartedAt} {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func formatScheduledTaskCreated(task ScheduledTask) string {
	return strings.Join([]string{
		"已创建定时任务：" + task.ID,
		"spec：" + task.Spec,
		"agent：" + task.AgentName,
		"cwd：" + task.Cwd,
		"回传：" + scheduledTaskSinkText(task.ResultSink),
	}, "\n")
}

func formatScheduledTaskUpdated(task ScheduledTask) string {
	return strings.Join([]string{
		"已更新定时任务：" + task.ID,
		"状态：" + scheduledTaskEnabledText(task.Enabled),
		"spec：" + task.Spec,
		"agent：" + task.AgentName,
		"cwd：" + task.Cwd,
		"prompt：" + task.Prompt,
		"回传：" + scheduledTaskSinkText(task.ResultSink),
	}, "\n")
}

func scheduledTaskEnabledText(enabled bool) string {
	if enabled {
		return "启用"
	}
	return "暂停"
}

func scheduledTaskSinkText(sink ScheduledTaskResultSink) string {
	if !strings.EqualFold(sink.Type, "im") || strings.TrimSpace(sink.ChatID) == "" {
		return "仅日志"
	}
	if strings.TrimSpace(sink.ThreadID) != "" {
		return "IM " + sink.ChatID + " / " + sink.ThreadID
	}
	return "IM " + sink.ChatID
}

func newScheduledTaskID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "task-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	}
	return "task-" + hex.EncodeToString(b[:])
}

func oneLine(text string, maxRunes int) string {
	text = strings.Join(strings.Fields(text), " ")
	if maxRunes <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "..."
}
