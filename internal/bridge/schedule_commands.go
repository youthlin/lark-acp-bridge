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

type scheduleTaskArgs struct {
	Spec      string
	Timezone  string
	Prompt    string
	AgentName string
	Cwd       string
	At        string
}

type scheduleFlagParseOptions struct {
	commandName   string
	allowTimezone bool
}

type scheduleTaskDefaults struct {
	agentName string
	cwd       string
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
		return s.handleScheduleDeleteCommand(ctx, msg, fields[2])
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
		"add 的 spec 支持 @every <duration> 或 5 段 cron；once 的时间用 YYYY-MM-DD HH:MM（按 --tz，缺省 local）或 RFC3339，例如 /schedule once 2026-08-05 09:00 生成早报。一次性任务执行后自动删除。",
		missedSchedulePolicyText(),
	}, "\n")
}

func missedSchedulePolicyText() string {
	return "错过执行策略：停机或未运行期间错过的一次性任务会删除且不补跑；循环任务跳过历史轮次，只从当前时间计算下一次；手动 /schedule run 不受该策略影响。"
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
	agentName, cwd, errText := s.resolveScheduleTaskAgentAndCwd(args, msg, scheduleTaskDefaults{
		agentName: s.chatAgentName(msg),
	})
	if errText != "" {
		return errText
	}
	if errText := s.sanitizeScheduleTaskPrompt(msg, &args); errText != "" {
		return errText
	}
	task := newScheduledTaskFromCommand(msg, scheduleTaskArgs{
		Spec:      args.Spec,
		Prompt:    args.Prompt,
		AgentName: agentName,
		Cwd:       cwd,
	})
	task, errText = s.saveAndStartScheduledTask(ctx, store, task, msg.Workspace, "定时任务")
	if errText != "" {
		return errText
	}
	return formatScheduledTaskCreated(task)
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
	parsed, err := parseScheduleSpec(args.Spec, args.Timezone)
	if err != nil {
		return "一次性任务时间无效：" + err.Error()
	}
	next, ok := parsed.Next(time.Now())
	if !ok || !next.After(time.Now()) {
		return "一次性任务的执行时间必须在将来：" + args.At
	}
	agentName, cwd, errText := s.resolveScheduleTaskAgentAndCwd(args, msg, scheduleTaskDefaults{
		agentName: s.chatAgentName(msg),
	})
	if errText != "" {
		return errText
	}
	if errText := s.sanitizeScheduleTaskPrompt(msg, &args); errText != "" {
		return errText
	}
	task := newScheduledTaskFromCommand(msg, scheduleTaskArgs{
		Spec:      args.Spec,
		Timezone:  args.Timezone,
		Prompt:    args.Prompt,
		AgentName: agentName,
		Cwd:       cwd,
	})
	task.Once = true
	task, errText = s.saveAndStartScheduledTask(ctx, store, task, msg.Workspace, "一次性任务")
	if errText != "" {
		return errText
	}
	return formatScheduledTaskCreated(task)
}

func (s *Service) parseScheduleOnceArgs(args string, msg feishu.Message) (scheduleTaskArgs, string) {
	args = strings.TrimSpace(args)
	if args == "" {
		return scheduleTaskArgs{}, "请使用 /schedule once [--cwd <path>] [--agent <name>] [--tz <zone>] <时间> <prompt>。"
	}
	parsed, rest, errText := s.parseScheduleFlags(args, msg, scheduleFlagParseOptions{commandName: "once", allowTimezone: true})
	if errText != "" {
		return scheduleTaskArgs{}, errText
	}
	if len(rest) == 0 {
		return scheduleTaskArgs{}, "请提供一次性任务的执行时间和 prompt。"
	}
	once, errText := parseOnceTimeAndPrompt(strings.Join(rest, " "))
	if errText != "" {
		return scheduleTaskArgs{}, errText
	}
	parsed.At = once.At
	parsed.Spec = once.Spec
	parsed.Prompt = once.Prompt
	return parsed, ""
}

func parseOnceTimeAndPrompt(rest string) (scheduleTaskArgs, string) {
	rest = strings.TrimSpace(rest)
	// RFC3339：首个空白前的 token 含 'T'。
	firstEnd := strings.IndexAny(rest, " \t")
	if firstEnd > 0 {
		first := rest[:firstEnd]
		if strings.Contains(first, "T") {
			if t, err := time.Parse(time.RFC3339, first); err == nil {
				prompt := strings.TrimSpace(rest[firstEnd:])
				if prompt == "" {
					return scheduleTaskArgs{}, "一次性任务 prompt 不能为空。"
				}
				at := t.Format(time.RFC3339)
				return scheduleTaskArgs{At: at, Spec: "@at " + at, Prompt: prompt}, ""
			}
		}
	}
	// YYYY-MM-DD HH:MM：前两个空白分隔字段。
	fields := strings.Fields(rest)
	if len(fields) < 3 {
		return scheduleTaskArgs{}, "请使用 /schedule once <YYYY-MM-DD HH:MM | RFC3339> <prompt>。"
	}
	at := fields[0] + " " + fields[1]
	if _, err := time.ParseInLocation("2006-01-02 15:04", at, time.Local); err != nil {
		return scheduleTaskArgs{}, "时间需为 YYYY-MM-DD HH:MM 或 RFC3339：" + err.Error()
	}
	prompt := strings.TrimSpace(strings.TrimPrefix(rest, at))
	if prompt == "" {
		return scheduleTaskArgs{}, "一次性任务 prompt 不能为空。"
	}
	return scheduleTaskArgs{At: at, Spec: "@at " + at, Prompt: prompt}, ""
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
	now := time.Now()
	return strings.Join([]string{
		"你正在为用户生成一条 lark-acp-bridge 可直接执行的 /schedule 命令。当前 ACP 会话可能没有 bridge 项目代码上下文，因此下面先给出完整命令格式和参数语义。",
		"",
		"## 判断一次性还是循环",
		"- 用户需求只在某个将来时间点执行一次（例如“明天上午 9 点提醒我”“下周一 10 点跑一次脚本”“8 月 5 日 9 点生成一次报告”）→ 生成 /schedule once。",
		"- 用户需求会重复发生（每天、每周、每隔一段时间、cron 表达式）→ 生成 /schedule add。",
		"",
		"## /schedule once 命令格式（一次性）",
		"/schedule once [--cwd <workspace path>] [--agent <agent name>] [--tz <zone>] <时间> <prompt>",
		"- <时间> 支持两种写法：",
		"  - YYYY-MM-DD HH:MM，按 --tz 解释时区；未给 --tz 时按运行机器的 local 时区。",
		"  - RFC3339，例如 2026-08-05T09:00:00+08:00。",
		"- 相对时间（如“明天”“下周一”“一小时后”）请换算成确定的 YYYY-MM-DD HH:MM；以当前本地时间 " + now.Local().Format("2006-01-02 15:04 MST") + " 为基准计算。",
		"",
		"## /schedule add 命令格式（循环）",
		"/schedule add [--cwd <workspace path>] [--agent <agent name>] <spec> <prompt>",
		"- <spec> 支持 @every <duration>（如 @every 30m、@every 1h）或 5 段 cron（minute hour day-of-month month day-of-week）。",
		"- 每天上午 8 点应生成 0 8 * * *。",
		"- 每周一上午 9 点应生成 0 9 * * 1。",
		"- 工作日上午 10 点应生成 0 10 * * 1-5。",
		"",
		"通用说明：",
		"- --cwd <workspace path>：可选，指定任务运行工作目录；用户未明确提供时不要编造。",
		"- --agent <agent name>：可选，指定 ACP agent；用户未明确提供时不要编造。",
		"- <prompt>：必填，触发时发送给 agent 的任务说明，必须放在时间/spec 后面。",
		"- " + missedSchedulePolicyText(),
		"- 如果用户给出的时间无法可靠转换，返回一句需要用户补充的信息，不要猜测。",
		"",
		"用户需求：",
		goal,
		"",
		"请根据用户需求生成一条可直接执行的 /schedule once 或 /schedule add 命令。",
		"要求：",
		"- 一次性需求用 /schedule once，循环需求用 /schedule add；不要把一次性需求写成循环任务。",
		"- 最终只返回一条 /schedule 命令，或一条要求用户补充必要时间信息的中文句子。",
		"- 不要解释，不要使用 Markdown 代码块。",
		"- 不要真正创建任务，只生成命令。",
		"- 不要使用 /schedule how 作为最终结果。",
		"- 不要编造用户未提供的仓库、路径、agent、环境或背景。",
		"- 如果用户提到脚本、命令或文件路径，把它保留在 prompt 中。",
		"- prompt 应明确说明触发时要执行的任务，并要求需要时先检查相关文件或脚本是否存在。",
	}, "\n"), nil
}

func (s *Service) parseScheduleAddArgs(args string, msg feishu.Message) (scheduleTaskArgs, string) {
	return s.parseScheduleTaskArgs(args, msg, "add")
}

func (s *Service) parseScheduleTaskArgs(args string, msg feishu.Message, commandName string) (scheduleTaskArgs, string) {
	args = strings.TrimSpace(args)
	commandName = strings.TrimSpace(commandName)
	if commandName == "" {
		commandName = "add"
	}
	if args == "" {
		return scheduleTaskArgs{}, "请使用 /schedule " + commandName + " <spec> <prompt>。"
	}
	parsed, fields, errText := s.parseScheduleFlags(args, msg, scheduleFlagParseOptions{commandName: commandName})
	if errText != "" {
		return scheduleTaskArgs{}, errText
	}
	if len(fields) < 2 {
		return scheduleTaskArgs{}, "请提供定时任务 spec 和 prompt。"
	}
	rest := strings.Join(fields, " ")
	if strings.EqualFold(fields[0], "@every") {
		if len(fields) < 3 {
			return scheduleTaskArgs{}, "请使用 /schedule add @every <duration> <prompt>。"
		}
		parsed.Spec = "@every " + fields[1]
		parsed.Prompt = strings.TrimSpace(strings.TrimPrefix(rest, fields[0]+" "+fields[1]))
		if parsed.Prompt == "" {
			return scheduleTaskArgs{}, "定时任务 prompt 不能为空。"
		}
		return parsed, ""
	}
	if len(fields) < 6 {
		return scheduleTaskArgs{}, "cron spec 需要 5 段，并在后面提供 prompt。"
	}
	parsed.Spec = strings.Join(fields[:5], " ")
	parsed.Prompt = strings.TrimSpace(strings.TrimPrefix(rest, parsed.Spec))
	if parsed.Prompt == "" {
		return scheduleTaskArgs{}, "定时任务 prompt 不能为空。"
	}
	return parsed, ""
}

func (s *Service) parseScheduleFlags(args string, msg feishu.Message, opts scheduleFlagParseOptions) (scheduleTaskArgs, []string, string) {
	fields := strings.Fields(strings.TrimSpace(args))
	parsed := scheduleTaskArgs{}
	commandName := strings.TrimSpace(opts.commandName)
	if commandName == "" {
		commandName = "add"
	}
	index := 0
	for index < len(fields) {
		field := strings.TrimSpace(fields[index])
		switch {
		case field == "--cwd":
			if index+1 >= len(fields) {
				return scheduleTaskArgs{}, nil, "请为 --cwd 指定工作目录。"
			}
			cwd, isPath, errText := s.resolveNewSessionCwdArg(fields[index+1], msg)
			if errText != "" {
				return scheduleTaskArgs{}, nil, errText
			}
			if !isPath || cwd == "" {
				return scheduleTaskArgs{}, nil, "请为 --cwd 指定有效工作目录。"
			}
			parsed.Cwd = cwd
			index += 2
		case strings.HasPrefix(field, "--cwd="):
			value := strings.TrimSpace(strings.TrimPrefix(field, "--cwd="))
			cwd, isPath, errText := s.resolveNewSessionCwdArg(value, msg)
			if errText != "" {
				return scheduleTaskArgs{}, nil, errText
			}
			if !isPath || cwd == "" {
				return scheduleTaskArgs{}, nil, "请为 --cwd 指定有效工作目录。"
			}
			parsed.Cwd = cwd
			index++
		case field == "--agent":
			if index+1 >= len(fields) {
				return scheduleTaskArgs{}, nil, "请为 --agent 指定 agent 名称。"
			}
			parsed.AgentName = strings.TrimSpace(fields[index+1])
			index += 2
		case strings.HasPrefix(field, "--agent="):
			parsed.AgentName = strings.TrimSpace(strings.TrimPrefix(field, "--agent="))
			index++
		case field == "--tz" && opts.allowTimezone:
			if index+1 >= len(fields) {
				return scheduleTaskArgs{}, nil, "请为 --tz 指定时区，例如 Asia/Shanghai。"
			}
			parsed.Timezone = strings.TrimSpace(fields[index+1])
			index += 2
		case strings.HasPrefix(field, "--tz=") && opts.allowTimezone:
			parsed.Timezone = strings.TrimSpace(strings.TrimPrefix(field, "--tz="))
			index++
		case strings.HasPrefix(field, "-"):
			return scheduleTaskArgs{}, nil, "未知 schedule " + commandName + " 参数：" + field
		default:
			return parsed, fields[index:], ""
		}
	}
	return parsed, nil, ""
}

func (s *Service) resolveScheduleTaskAgentAndCwd(args scheduleTaskArgs, msg feishu.Message, defaults scheduleTaskDefaults) (string, string, string) {
	agentName := strings.TrimSpace(args.AgentName)
	if agentName == "" {
		agentName = strings.TrimSpace(defaults.agentName)
	}
	if _, ok := s.registry.Get(agentName); !ok {
		return "", "", "未知 agent：" + agentName
	}
	cwd := strings.TrimSpace(args.Cwd)
	if cwd == "" {
		cwd = strings.TrimSpace(defaults.cwd)
	}
	if cwd == "" {
		var errText string
		cwd, _, errText = s.defaultNewSessionCwd(msg)
		if errText != "" {
			return "", "", errText
		}
	}
	return agentName, cwd, ""
}

func newScheduledTaskFromCommand(msg feishu.Message, args scheduleTaskArgs) ScheduledTask {
	return ScheduledTask{
		ID:                   newScheduledTaskID(),
		BotID:                msg.BotID,
		Enabled:              true,
		Spec:                 args.Spec,
		Timezone:             args.Timezone,
		AgentName:            args.AgentName,
		Cwd:                  args.Cwd,
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
	}
}

func (s *Service) saveAndStartScheduledTask(ctx context.Context, store *ScheduledTaskStore, task ScheduledTask, workspace string, kind string) (ScheduledTask, string) {
	task, err := store.Upsert(task)
	if err != nil {
		slog.ErrorContext(ctx, "保存"+kind+"失败", "错误", err)
		return ScheduledTask{}, "保存" + kind + "失败：" + err.Error()
	}
	if err := s.startScheduledTask(ctx, task, workspace); err != nil {
		slog.ErrorContext(ctx, "注册"+kind+"失败", "task_id", task.ID, "错误", err)
		return ScheduledTask{}, kind + "已保存，但注册到当前进程失败：" + err.Error()
	}
	return task, ""
}

func (s *Service) scheduleTaskWorkspace(task ScheduledTask, msg feishu.Message) string {
	workspace := strings.TrimSpace(s.botWorkspace(task.BotID))
	if workspace == "" {
		workspace = strings.TrimSpace(msg.Workspace)
	}
	return workspace
}

func (s *Service) startImmediateScheduleRun(ctx context.Context, task ScheduledTask, workspace string) string {
	triggeredAt := time.Now()
	runID := scheduledTaskRunID(task, triggeredAt)
	runKey := scheduledTaskRunKey(task, runID)
	s.markScheduleRunPending(task, runID, runKey, triggeredAt)
	runCtx := context.WithoutCancel(ctx)
	if !s.goBackground(
		runCtx,
		"schedule-once:"+task.ID,
		func(ctx context.Context) {
			if _, err := s.runScheduledTaskOnce(ctx, task, runID, triggeredAt, workspace, nil); err != nil {
				slog.ErrorContext(ctx, "立即执行定时任务失败", "task_id", task.ID, "run_id", runID, "错误", err)
			}
		},
	) {
		s.markScheduleRunFinished(task.ID, runID, time.Now(), context.Canceled)
	}
	return runID
}

func (s *Service) handleScheduleListCommand(msg feishu.Message) string {
	store := s.scheduledTaskStoreForBotID(msg.BotID)
	if store == nil {
		return "当前 bot workspace 未初始化，无法读取定时任务。"
	}
	tasks := store.List()
	if len(tasks) == 0 {
		return strings.Join([]string{
			"当前 bot 还没有定时任务。",
			missedSchedulePolicyText(),
		}, "\n")
	}
	lines := []string{
		"当前 bot 的定时任务：",
		missedSchedulePolicyText(),
	}
	for i, task := range tasks {
		state := "暂停"
		if task.Enabled {
			state = "启用"
		}
		if task.Once {
			state = "一次性"
		}
		lines = append(lines, fmt.Sprintf("%d. %s [%s]\n   触发：%s\n   agent：%s\n   cwd：%s\n   prompt：%s", i+1, task.ID, state, scheduledTaskTriggerText(task), task.AgentName, task.Cwd, oneLine(redactSensitiveValuesForDisplay(task.Prompt), 80)))
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
		"prompt：" + redactSensitiveValuesForDisplay(task.Prompt),
		"创建者：" + task.CreatorOpenID,
		"回传：" + scheduledTaskSinkText(task.ResultSink),
		missedSchedulePolicyText(),
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
	agentName, cwd, errText := s.resolveScheduleTaskAgentAndCwd(args, msg, scheduleTaskDefaults{
		agentName: existing.AgentName,
		cwd:       existing.Cwd,
	})
	if errText != "" {
		return errText
	}
	if errText := s.sanitizeScheduleTaskPrompt(msg, &args); errText != "" {
		return errText
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
		workspace := s.scheduleTaskWorkspace(task, msg)
		if err := s.startScheduledTask(ctx, task, workspace); err != nil {
			slog.ErrorContext(ctx, "重新注册定时任务失败", "task_id", task.ID, "错误", err)
			return "定时任务已保存，但重新注册到当前进程失败：" + err.Error()
		}
	} else {
		s.stopScheduledTask(ctx, task)
	}
	return formatScheduledTaskUpdated(task)
}

func (s *Service) sanitizeScheduleTaskPrompt(msg feishu.Message, args *scheduleTaskArgs) string {
	if args == nil {
		return ""
	}
	session := Session{
		Key:       imSessionKey(msg.BotID, msg.ChatID, firstNonEmpty(msg.ThreadID, msg.RootID, msg.MessageID)),
		Workspace: firstNonEmpty(msg.Workspace, s.botWorkspace(msg.BotID)),
	}
	sanitized, err := s.sanitizePromptSecretsToFiles(msg, session, args.Prompt)
	if err != nil {
		return "处理敏感输入失败：" + err.Error()
	}
	args.Prompt = sanitized
	return ""
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
	workspace := s.scheduleTaskWorkspace(task, msg)
	if workspace == "" {
		return "定时任务 workspace 为空，无法执行：" + task.ID
	}
	runID := s.startImmediateScheduleRun(ctx, task, workspace)
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
		if err := s.startScheduledTask(ctx, task, msg.Workspace); err != nil {
			slog.ErrorContext(ctx, "注册定时任务失败", "task_id", task.ID, "错误", err)
			return "定时任务已恢复，但注册到当前进程失败：" + err.Error()
		}
		return "已恢复定时任务：" + task.ID
	}
	s.stopScheduledTask(ctx, task)
	return "已暂停定时任务：" + task.ID
}

func (s *Service) handleScheduleDeleteCommand(ctx context.Context, msg feishu.Message, id string) string {
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
	s.stopScheduledTask(ctx, task)
	return "已删除定时任务：" + task.ID
}

func (s *Service) lastScheduleRunStatus(taskID string) (scheduleRunStatus, bool) {
	taskID = strings.TrimSpace(taskID)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	var last scheduleRunStatus
	var ok bool
	for id := range s.scheduleRunsByTask[taskID] {
		status, exists := s.scheduleRuns[id]
		if !exists || status.TaskID != taskID {
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
		"prompt：" + redactSensitiveValuesForDisplay(task.Prompt),
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
