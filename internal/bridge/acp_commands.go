package bridge

import (
	"context"
	"errors"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func (s *Service) handleCommandsCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) >= 2 {
		command := commandRemainder(text, 1)
		return s.forwardACPCommand(ctx, command, msg)
	}
	session, ok := s.findSession(msg)
	if !ok || strings.TrimSpace(session.ACPSessionID) == "" {
		return "当前会话还没有 ACP session，发送普通文本或 /new 后再查看 ACP commands。"
	}
	if len(session.AvailableCommands) == 0 {
		return "当前 ACP server 还没有上报可用命令。可以先发送一条普通消息，等 server 返回 available_commands_update 后再查看。"
	}
	lines := []string{"当前 ACP server 支持的命令："}
	for _, cmd := range session.AvailableCommands {
		name := strings.TrimSpace(cmd.Name)
		if name == "" {
			continue
		}
		line := "/" + name
		if desc := strings.TrimSpace(cmd.Description); desc != "" {
			line += " - " + desc
		}
		if cmd.Input != nil && strings.TrimSpace(cmd.Input.Hint) != "" {
			line += "（参数：" + strings.TrimSpace(cmd.Input.Hint) + "）"
		}
		lines = append(lines, line)
	}
	lines = append(lines, "", "执行命令：/cmds /review ...，或简写为 //review ...")
	return strings.Join(lines, "\n")
}

func (s *Service) forwardACPCommand(ctx context.Context, command string, msg feishu.Message) string {
	command = strings.TrimSpace(command)
	if !strings.HasPrefix(command, "/") || strings.HasPrefix(command, "//") {
		return "请使用 /cmds /command [args]，或简写为 //command [args]。"
	}
	name := strings.TrimPrefix(strings.Fields(command)[0], "/")
	if name == "" {
		return "ACP command 名称不能为空。请使用 /cmds /command [args]，或简写为 //command [args]。"
	}
	session, ok := s.findSession(msg)
	if !ok || strings.TrimSpace(session.ACPSessionID) == "" {
		return "当前会话还没有 ACP session，发送普通文本或 /new 后再执行 ACP command。"
	}
	if len(session.AvailableCommands) > 0 && !sessionHasCommand(session, name) {
		return "当前 ACP server 未上报该命令：" + "/" + name + "。发送 /cmds 查看可用命令。"
	}
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		return "未找到 agent 配置：" + session.AgentName
	}
	result, _, err := s.runUserPrompt(ctx, msg, session, agent, command)
	reply := result.Text
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return ""
		}
		if strings.TrimSpace(reply) != "" {
			return reply
		}
		return "执行 ACP command 失败：" + err.Error()
	}
	if name == "compact" {
		s.resetWorkspacePrompted(ctx, msg, session)
	}
	if strings.TrimSpace(reply) == "" {
		return "ACP command 已执行完成。"
	}
	return reply
}

func sessionHasCommand(session Session, name string) bool {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	for _, cmd := range session.AvailableCommands {
		if cmd.Name == name {
			return true
		}
	}
	return false
}
