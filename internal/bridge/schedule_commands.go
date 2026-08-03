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
	case "once":
		return s.handleScheduleOnceCommand(ctx, text, msg)
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
		"/schedule once [--cwd <path>] [--agent <name>] [--tz <zone>] <时间> <prompt>",
		"/schedule how <自然语言需求>",
		"/schedule list",
		"/schedule status <id>",
		"/schedule run <id>",
		"/schedule edit <id> [--cwd <path>] [--agent <name>] <spec> <prompt>",
		"/schedule pause <id>",
		"/schedule resume <id>",
		"/schedule delete <id>",
		"add 的 spec 支持 @every <duration> 或 5 段 cron；once 的时间用 YYYY-MM-DD HH:MM（按 --tz，缺省 local）或 RFC3339，例如 /schedule once 2026-08-05 09:00 生成早报。一次性任务执行后自动删除，启动时若时间已过也会删除不补跑。",
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
			Type:   "im",
			ChatID: msg.ChatID,
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

type scheduleOnceArgs struct {
	At        string
	Timezone  string
	Prompt    string
	AgentName string
	Cwd       string
}

func (s *Service) handleScheduleOnceCommand(ctx context.Context, text string, msg feishu.Message) string {
	store := s.scheduledTaskStoreForBotID(msg.BotID)
	if store == nil {
		return "当前 bot workspace 未初始化，无法创建一次性任务。"
	}
	args, errText := s.parseScheduleOnceArgs(commandRemainder(text, 2), msg)
	if errText != "" {
		return errText
	}
	// 先按解析出的 timezone 构造 @at spec 并校验。
	spec := "@at " + args.At
	parsed, err := parseScheduleSpec(spec, args.Timezone)
	if err != nil {
		return "一次性任务时间无效：" + err.Error()
	}
	next, ok := parsed.Next(time.Now())
	if !ok || !next.After(time.Now()) {
		return "一次性任务的执行时间必须在将来：" + args.At
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
		Once:                 true,
		Spec:                 spec,
		Timezone:             args.Timezone,
		AgentName:            agentName,
		Cwd:                  cwd,
		Prompt:               args.Prompt,
		CreatorOpenID:        msg.SenderID,
		CreatedFromChatID:    msg.ChatID,
		CreatedFromThreadID:  msg.ThreadID,
		CreatedFromMessageID: msg.MessageID,
		ResultSink: ScheduledTaskResultSink{
			Type:   "im",
			ChatID: msg.ChatID,
		},
		OverlapPolicy: scheduleOverlapSkipIfRunning,
	})
	if err != nil {
		slog.ErrorContext(ctx, "保存一次性任务失败", "错误", err)
		return "保存一次性任务失败：" + err.Error()
	}
	if err := s.startScheduledTask(context.Background(), task, msg.Workspace); err != nil {
		slog.ErrorContext(ctx, "注册一次性任务失败", "task_id", task.ID, "错误", err)
		return "一次性任务已保存，但注册到当前进程失败：" + err.Error()
	}
	return formatScheduledTaskCreated(task)
}

func (s *Service) parseScheduleOnceArgs(args string, msg feishu.Message) (scheduleOnceArgs, string) {
	args = strings.TrimSpace(args)
	if args == "" {
		return scheduleOnceArgs{}, "请使用 /schedule once [--cwd <path>] [--agent <name>] [--tz <zone>] <时间> <prompt>。"
	}
	fields := strings.Fields(args)
	parsed := scheduleOnceArgs{}
	index := 0
	for index < len(fields) {
		field := strings.TrimSpace(fields[index])
		switch {
		case field == "--cwd":
			if index+1 >= len(fields) {
				return scheduleOnceArgs{}, "请为 --cwd 指定工作目录。"
			}
			cwd, isPath, errText := s.resolveNewSessionCwdArg(fields[index+1], msg)
			if errText != "" {
				return scheduleOnceArgs{}, errText
			}
			if !isPath || cwd == "" {
				return scheduleOnceArgs{}, "请为 --cwd 指定有效工作目录。"
			}
			parsed.Cwd = cwd
			index += 2
		case strings.HasPrefix(field, "--cwd="):
			value := strings.TrimSpace(strings.TrimPrefix(field, "--cwd="))
			cwd, isPath, errText := s.resolveNewSessionCwdArg(value, msg)
			if errText != "" {
				return scheduleOnceArgs{}, errText
			}
			if !isPath || cwd == "" {
				return scheduleOnceArgs{}, "请为 --cwd 指定有效工作目录。"
			}
			parsed.Cwd = cwd
			index++
		case field == "--agent":
			if index+1 >= len(fields) {
				return scheduleOnceArgs{}, "请为 --agent 指定 agent 名称。"
			}
			parsed.AgentName = strings.TrimSpace(fields[index+1])
			index += 2
		case strings.HasPrefix(field, "--agent="):
			parsed.AgentName = strings.TrimSpace(strings.TrimPrefix(field, "--agent="))
			index++
		case field == "--tz":
			if index+1 >= len(fields) {
				return scheduleOnceArgs{}, "请为 --tz 指定时区，例如 Asia/Shanghai。"
			}
			parsed.Timezone = strings.TrimSpace(fields[index+1])
			index += 2
		case strings.HasPrefix(field, "--tz="):
			parsed.Timezone = strings.TrimSpace(strings.TrimPrefix(field, "--tz="))
			index++
		case strings.HasPrefix(field, "-"):
			return scheduleOnceArgs{}, "未知 schedule once 参数：" + field
		default:
			// 剩余部分解析时间（RFC3339 一段，或 YYYY-MM-DD HH:MM 两段）+ prompt。
			rest := strings.Join(fields[index:], " ")
			return parseOnceTimeAndPrompt(rest)
		}
	}
	return scheduleOnceArgs{}, "请提供一次性任务的执行时间和 prompt。"
}

func parseOnceTimeAndPrompt(rest string) (scheduleOnceArgs, string) {
	rest = strings.TrimSpace(rest)
	// RFC3339：首个空白前的 token 含 'T'。
	firstEnd := strings.IndexAny(rest, " \t")
	if firstEnd > 0 {
		first := rest[:firstEnd]
		if strings.Contains(first, "T") {
			if t, err := time.Parse(time.RFC3339, first); err == nil {
				prompt := strings.TrimSpace(rest[firstEnd:])
				if prompt == "" {
					return scheduleOnceArgs{}, "一次性任务 prompt 不能为空。"
				}
				return scheduleOnceArgs{At: t.Format(time.RFC3339), Prompt: prompt}, ""
			}
		}
	}
	// YYYY-MM-DD HH:MM：前两个空白分隔字段。
	fields := strings.Fields(rest)
	if len(fields) < 3 {
		return scheduleOnceArgs{}, "请使用 /schedule once <YYYY-MM-DD HH:MM | RFC3339> <prompt>。"
	}
	at := fields[0] + " " + fields[1]
	if _, err := time.ParseInLocation("2006-01-02 15:04", at, time.Local); err != nil {
		return scheduleOnceArgs{}, "时间需为 YYYY-MM-DD HH:MM 或 RFC3339：" + err.Error()
	}
	prompt := strings.TrimSpace(strings.TrimPrefix(rest, at))
	if prompt == "" {
		return scheduleOnceArgs{}, "一次性任务 prompt 不能为空。"
	}
	return scheduleOnceArgs{At: at, Prompt: prompt}, ""
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
		if task.Once {
			state = "一次性"
		}
		lines = append(lines, fmt.Sprintf("%d. %s [%s]\n   触发：%s\n   agent：%s\n   cwd：%s\n   prompt：%s", i+1, task.ID, state, scheduledTaskTriggerText(task), task.AgentName, task.Cwd, oneLine(task.Prompt, 80)))
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
		"触发：" + scheduledTaskTriggerText(task),
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
	isOnceSpec := strings.HasPrefix(strings.TrimSpace(args.Spec), "@at ")
	if existing.Once && !isOnceSpec {
		return "一次性任务只能用 @at <时间> 作为 spec；请删除后用 /schedule add 创建循环任务。"
	}
	if !existing.Once && isOnceSpec {
		return "@at <时间> 仅用于 /schedule once 创建的一次性任务；请用 /schedule add 创建循环任务。"
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
	kind := "定时"
	if task.Once {
		kind = "一次性"
	}
	return strings.Join([]string{
		"已创建" + kind + "任务：" + task.ID,
		"触发：" + scheduledTaskTriggerText(task),
		"agent：" + task.AgentName,
		"cwd：" + task.Cwd,
		"回传：" + scheduledTaskSinkText(task.ResultSink),
	}, "\n")
}

func formatScheduledTaskUpdated(task ScheduledTask) string {
	return strings.Join([]string{
		"已更新定时任务：" + task.ID,
		"状态：" + scheduledTaskEnabledText(task.Enabled),
		"触发：" + scheduledTaskTriggerText(task),
		"agent：" + task.AgentName,
		"cwd：" + task.Cwd,
		"prompt：" + task.Prompt,
		"回传：" + scheduledTaskSinkText(task.ResultSink),
	}, "\n")
}

// scheduledTaskTriggerText 返回任务触发时间的可读描述。
// 一次性任务额外解析并展示具体执行时刻（按任务时区）。
func scheduledTaskTriggerText(task ScheduledTask) string {
	if !task.Once {
		return task.Spec
	}
	parsed, err := parseScheduleSpec(task.Spec, task.Timezone)
	if err != nil {
		return task.Spec
	}
	next, ok := parsed.Next(time.Now())
	if !ok {
		return task.Spec + "（时间已过）"
	}
	loc := time.Local
	if strings.TrimSpace(task.Timezone) != "" {
		if loaded, err := time.LoadLocation(task.Timezone); err == nil {
			loc = loaded
		}
	}
	return task.Spec + "（" + next.In(loc).Format("2006-01-02 15:04:05 MST") + "）"
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
