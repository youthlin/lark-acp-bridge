package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	workspaceSetupFile = ".setup.json"
)

type WorkspaceStatus struct {
	Path         string
	CreatedFiles []string
	Ready        bool
}

type workspaceSetup struct {
	Version int       `json:"version"`
	Ready   bool      `json:"ready"`
	Updated time.Time `json:"updated_at"`
}

func ensureWorkspace(path string) (WorkspaceStatus, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return WorkspaceStatus{}, fmt.Errorf("创建 bot workspace: %w", err)
	}
	status := WorkspaceStatus{Path: path}
	for _, file := range workspaceFiles() {
		fullPath := filepath.Join(path, file.name)
		if _, err := os.Stat(fullPath); err == nil {
			continue // 文件存在 跳过
		} else if !errors.Is(err, fs.ErrNotExist) {
			// 检查失败 不是文件不存在的错误
			return WorkspaceStatus{}, fmt.Errorf("检查 workspace 文件 %s: %w", file.name, err)
		}
		// 文件不存在 创建
		if err := os.WriteFile(fullPath, []byte(file.content), 0o644); err != nil {
			return WorkspaceStatus{}, fmt.Errorf("创建 workspace 文件 %s: %w", file.name, err)
		}
		status.CreatedFiles = append(status.CreatedFiles, file.name)
	}
	setup, _, err := loadWorkspaceSetup(path)
	if err != nil {
		return WorkspaceStatus{}, err
	}
	status.Ready = setup.Ready
	return status, nil
}

func workspaceReady(path string) (bool, error) {
	setup, _, err := loadWorkspaceSetup(path)
	if err != nil {
		return false, err
	}
	return setup.Ready, nil
}

func workspaceGuide(status WorkspaceStatus) string {
	lines := []string{
		"当前 bot workspace 尚未 ready。",
		"已准备 workspace：" + status.Path,
	}
	if len(status.CreatedFiles) > 0 {
		lines = append(lines, "已创建基础文件："+strings.Join(status.CreatedFiles, "、"))
	}
	lines = append(lines,
		"发送普通文本或 /new [cwd] 会创建 ACP 会话，并让 ACP agent 完成一次性初始化引导。",
		"初始化完成后由 ACP agent 写入 .setup.json 的 ready=true。",
	)
	return strings.Join(lines, "\n")
}

func markWorkspaceReady(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("workspace 为空")
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("创建 bot workspace: %w", err)
	}
	setup, _, err := loadWorkspaceSetup(path)
	if err != nil {
		return err
	}
	setup.Version = 1
	setup.Ready = true
	setup.Updated = time.Now()
	return saveWorkspaceSetup(path, setup)
}

func loadWorkspaceSetup(path string) (workspaceSetup, bool, error) {
	data, err := os.ReadFile(filepath.Join(path, workspaceSetupFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return workspaceSetup{Version: 1}, false, nil
		}
		return workspaceSetup{}, false, fmt.Errorf("读取 workspace 设置状态: %w", err)
	}
	var setup workspaceSetup
	if err := json.Unmarshal(data, &setup); err != nil {
		return workspaceSetup{}, false, fmt.Errorf("解析 workspace 设置状态: %w", err)
	}
	if setup.Version == 0 {
		setup.Version = 1
	}
	return setup, true, nil
}

func saveWorkspaceSetup(path string, setup workspaceSetup) error {
	if setup.Version == 0 {
		setup.Version = 1
	}
	if setup.Updated.IsZero() {
		setup.Updated = time.Now()
	}
	data, err := json.MarshalIndent(setup, "", "  ")
	if err != nil {
		return fmt.Errorf("编码 workspace 设置状态: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(path, workspaceSetupFile), data, 0o644); err != nil {
		return fmt.Errorf("写入 workspace 设置状态: %w", err)
	}
	return nil
}

func workspaceSetupPrompt(workspace string) string {
	return strings.Join([]string{
		"## Workspace Setup Required",
		"",
		"当前 bot workspace 还没有 ready。请你完成一次性初始化引导。",
		"",
		"请一次性询问用户以下信息，允许用户在下一条消息里一次回答：",
		"1. 用户想叫你什么名字。",
		"2. 你是什么性格、语气和边界。",
		"3. 需要长期记住的用户信息、偏好或常用上下文。",
		"",
		"当用户回答后，请使用 fs/write_text_file 写入以下文件：",
		"- " + filepath.Join(workspace, "SOUL.md"),
		"- " + filepath.Join(workspace, "MEMORY.md"),
		"- " + filepath.Join(workspace, "AGENTS.md"),
		"- " + filepath.Join(workspace, "TOOLS.md"),
		"- " + filepath.Join(workspace, workspaceSetupFile),
		"",
		".setup.json 必须写成 JSON：",
		"{\"version\":1,\"ready\":true,\"updated_at\":\"<RFC3339 time>\"}",
		"",
		"在用户回答前，不要写 ready=true。先直接回复引导问题。",
	}, "\n")
}

func workspaceReadyPrompt(workspace string) string {
	files := []string{"SOUL.md", "MEMORY.md", "AGENTS.md", "TOOLS.md"}
	var fileSections []string
	for _, name := range files {
		path := filepath.Join(workspace, name)
		data, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(data)) == "" {
			continue
		}
		fileSections = append(fileSections, fmt.Sprintf("<file path=%q>\n%s\n</file>", path, string(data)))
	}
	if len(fileSections) == 0 {
		return ""
	}
	sections := []string{
		"## Workspace Knowledge",
		"",
		"下面是当前 bot workspace 的长期记忆和工作规则。后续回复应遵循这些内容。",
	}
	sections = append(sections, fileSections...)
	return strings.Join(sections, "\n\n")
}

func workspaceMemoryPolicyPrompt(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return ""
	}
	return strings.Join([]string{
		"## Workspace Memory Policy",
		"",
		"当用户要求“记住”、保存长期偏好、常用路径或工作规则时，必须优先更新当前 bot workspace 的 MEMORY.md，而不是只写入 TraeX 自己的长期记忆。",
		"",
		"请先使用 fs/read_text_file 读取：",
		"- " + filepath.Join(workspace, "MEMORY.md"),
		"",
		"再合并新信息，并使用 fs/write_text_file 写回同一个文件。只记录可复用的长期信息，不要记录一次性任务结果。",
	}, "\n")
}

func workspaceFiles() []struct {
	name    string
	content string
} {
	return []struct {
		name    string
		content string
	}{
		{
			name: "SOUL.md",
			content: `# SOUL

## 人格

首次对话引导完成后，会在这里记录这个 bot 的名字、角色、语气、边界和默认工作方式。

## 行为准则

- 不确定时先说明不确定性。
- 需要用户决策时提出明确选项。
`,
		},
		{
			name: "MEMORY.md",
			content: `# MEMORY

## 用户信息

首次对话引导完成后，会在这里记录这个 bot 需要长期记住的用户信息、偏好和常用上下文。

## 长期偏好

- 
`,
		},
		{
			name: "AGENTS.md",
			content: `# AGENTS

## 工作规则

请在这里记录这个 bot 的工作流程、协作约定和仓库操作规则。
`,
		},
		{
			name: "TOOLS.md",
			content: `# TOOLS

## 工具与环境

请在这里记录这个 bot 可用的工具、关键路径、账号 profile 和环境约束。
`,
		},
	}
}
