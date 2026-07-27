package bridge

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const workspaceContextFileMaxBytes int64 = 64 * 1024

func workspaceContextPrompt(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return ""
	}
	files := []string{
		workspaceBootstrapFile,
		"SOUL.md",
		"MEMORY.md",
		"AGENTS.md",
		"TOOLS.md",
		filepath.Join("knowledge", "AGENTS.md"),
		filepath.Join("knowledge", "core.md"),
		filepath.Join("knowledge", "index.md"),
		filepath.Join("skills", "AGENTS.md"),
		filepath.Join("skills", "core.md"),
		filepath.Join("skills", "wiki", "SKILL.md"),
	}
	var fileSections []string
	for _, name := range files {
		path := filepath.Join(workspace, name)
		content, ok := readWorkspaceContextFile(path)
		if !ok {
			continue
		}
		fileSections = append(fileSections, fmt.Sprintf("<file path=%q>\n%s\n</file>", path, content))
	}
	if len(fileSections) == 0 {
		return ""
	}
	sections := []string{
		"## Workspace Context",
		"",
		"下面是当前 bot workspace 的引导、长期记忆和工作规则。后续回复应遵循这些内容。",
	}
	sections = append(sections, fileSections...)
	return strings.Join(sections, "\n\n")
}

func readWorkspaceContextFile(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	if info.Size() <= workspaceContextFileMaxBytes {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", false
		}
		content := string(data)
		if strings.TrimSpace(content) == "" {
			return "", false
		}
		return content, true
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, workspaceContextFileMaxBytes))
	if err != nil {
		return "", false
	}
	content := strings.ToValidUTF8(string(data), "")
	if strings.TrimSpace(content) == "" {
		return "", false
	}
	content += fmt.Sprintf("\n\n[文件超过 %d 字节，已截断]", workspaceContextFileMaxBytes)
	return content, true
}

func workspaceMemoryPolicyPrompt(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return ""
	}
	return strings.Join([]string{
		"## Workspace Memory Policy",
		"",
		"当用户要求“记住”、保存长期偏好、常用路径、领域知识、项目经验或可复用流程时，必须优先更新当前 bot workspace，而不是只写入 TraeX 自己的长期记忆。",
		"",
		"写入位置按信息类型选择：",
		"- L0 根目录：用户偏好、身份、人格、工具和工作规则写入 SOUL.md、MEMORY.md、AGENTS.md 或 TOOLS.md。",
		"- L1 knowledge/：领域知识、项目经验、问题解决方案写入 knowledge/core.md 或独立主题文档。",
		"- L2 skills/：稳定、可复用的多步骤流程写入 skills/<skill-name>/SKILL.md。",
		"",
		"请先使用你可用的本地文件工具读取相关文件，常用入口包括：",
		"- " + filepath.Join(workspace, "MEMORY.md"),
		"- " + filepath.Join(workspace, "knowledge", "AGENTS.md"),
		"- " + filepath.Join(workspace, "knowledge", "core.md"),
		"- " + filepath.Join(workspace, "knowledge", "index.md"),
		"- " + filepath.Join(workspace, "knowledge", "log.md"),
		"- " + filepath.Join(workspace, "skills", "AGENTS.md"),
		"- " + filepath.Join(workspace, "skills", "core.md"),
		"",
		"再合并新信息，并使用你可用的本地文件工具写回对应文件。新增、删除或重命名知识/技能文件后必须同步 knowledge/index.md，并在 knowledge/log.md 末尾追加一行 `[YYYY-MM-DD] 操作 文件 摘要`。只记录可复用的长期信息，不要记录一次性任务结果。",
	}, "\n")
}
