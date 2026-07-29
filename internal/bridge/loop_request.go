package bridge

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const defaultLoopInterval = 10 * time.Second

type loopRequest struct {
	MaxDuration time.Duration
	MaxRounds   int
	Interval    time.Duration
	Prompt      string
}

func parseLoopRequest(text string) (loopRequest, error) {
	argsText := strings.TrimSpace(strings.TrimPrefix(text, "/loop"))
	req := loopRequest{Interval: defaultLoopInterval}
	for strings.TrimSpace(argsText) != "" {
		var arg string
		arg, argsText = nextLoopToken(argsText)
		switch arg {
		case "-t":
			var raw string
			raw, argsText = nextLoopToken(argsText)
			if raw == "" {
				return loopRequest{}, fmt.Errorf("请为 -t 指定 duration，例如 /loop -t 30m 提示词；-t 0 表示不限。")
			}
			d, err := parseLoopDuration(raw, "time")
			if err != nil {
				return loopRequest{}, err
			}
			req.MaxDuration = d
		case "-n":
			var raw string
			raw, argsText = nextLoopToken(argsText)
			if raw == "" {
				return loopRequest{}, fmt.Errorf("请为 -n 指定最大轮次；-n 0 表示不限。")
			}
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				return loopRequest{}, fmt.Errorf("-n 必须是非负整数；-n 0 表示不限。")
			}
			req.MaxRounds = n
		case "-i":
			var raw string
			raw, argsText = nextLoopToken(argsText)
			if raw == "" {
				return loopRequest{}, fmt.Errorf("请为 -i 指定每轮间隔，例如 /loop -i 10s 提示词。")
			}
			d, err := parseLoopDuration(raw, "interval")
			if err != nil {
				return loopRequest{}, err
			}
			if d <= 0 {
				return loopRequest{}, fmt.Errorf("-i 必须大于 0，例如 10s。")
			}
			req.Interval = d
		default:
			if strings.HasPrefix(arg, "-") {
				return loopRequest{}, fmt.Errorf("未知 loop 参数：%s。用法：/loop [-t 0] [-n 0] [-i 10s] 提示词", arg)
			}
			req.Prompt = strings.TrimSpace(arg + argsText)
			argsText = ""
		}
	}
	if req.Prompt == "" {
		return loopRequest{}, fmt.Errorf("提示词必填。用法：/loop [-t 0] [-n 0] [-i 10s] 提示词")
	}
	return req, nil
}

func nextLoopToken(text string) (string, string) {
	text = strings.TrimLeftFunc(text, unicode.IsSpace)
	if text == "" {
		return "", ""
	}
	for i, r := range text {
		if unicode.IsSpace(r) {
			return text[:i], text[i:]
		}
	}
	return text, ""
}

func parseLoopDuration(raw string, name string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("%s duration 不能为空", name)
	}
	if raw == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s duration 格式无效，可用 10s、30m、1h 或 0", name)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s duration 不能小于 0", name)
	}
	return d, nil
}

func formatLoopRequest(req loopRequest) string {
	maxDuration := "不限"
	if req.MaxDuration > 0 {
		maxDuration = formatDuration(req.MaxDuration)
	}
	maxRounds := "不限"
	if req.MaxRounds > 0 {
		maxRounds = strconv.Itoa(req.MaxRounds)
	}
	return strings.Join([]string{
		"最长运行：" + maxDuration,
		"最大轮次：" + maxRounds,
		"每轮间隔：" + formatDuration(req.Interval),
		"停止条件：agent 最终回复 DONE、达到最长运行、达到最大轮次，或发送新消息 / /loop stop。",
	}, "\n")
}

func loopHowPrompt(goal string) (string, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return "", fmt.Errorf("请提供想在循环中完成的目标，例如 /loop how 持续修复 todo.md 中的优化项。")
	}
	return strings.Join([]string{
		goal,
		"",
		"请根据上面的目标生成一条可直接执行的 /loop 命令。",
		"要求：",
		"- 命令使用 /loop -t 0 -n 0 -i 30s 开头。",
		"- 把目标改写成适合自动循环执行的中文任务说明。",
		"- 任务说明必须要求每轮只推进一个最小、可独立验证的步骤。",
		"- 任务说明必须要求定位相关代码和测试、实现最小改动、补充或更新测试、运行相关验证。",
		"- 任务说明必须要求不要执行 git commit，不要重启服务，不要修改与目标无关的代码。",
		"- 任务说明必须要求完成目标、遇到需要用户决策的问题、测试环境阻塞，或继续执行没有新增价值时只回复 DONE。",
		"- 最终只返回一条 /loop 命令，不要解释，不要使用 Markdown 代码块。",
	}, "\n"), nil
}
