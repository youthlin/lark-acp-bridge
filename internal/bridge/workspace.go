package bridge

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const workspaceBootstrapFile = "Bootstrap.md"

type WorkspaceStatus struct {
	Path         string
	CreatedFiles []string
}

func ensureWorkspace(path string, botID string) (WorkspaceStatus, error) {
	if strings.TrimSpace(path) == "" {
		return WorkspaceStatus{}, fmt.Errorf("workspace 为空")
	}
	if err := ensureWorkspaceRoot(path); err != nil {
		return WorkspaceStatus{}, err
	}
	hadManagedFiles, err := workspaceHasManagedFiles(path)
	if err != nil {
		return WorkspaceStatus{}, err
	}
	status := WorkspaceStatus{Path: path}
	for _, file := range workspaceFiles(botID) {
		fullPath := filepath.Join(path, file.name)
		if _, err := os.Stat(fullPath); err == nil {
			continue // 文件存在 跳过
		} else if !errors.Is(err, fs.ErrNotExist) {
			// 检查失败 不是文件不存在的错误
			return WorkspaceStatus{}, fmt.Errorf("检查 workspace 文件 %s: %w", file.name, err)
		}
		// 文件不存在 创建
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return WorkspaceStatus{}, fmt.Errorf("创建 workspace 目录 %s: %w", filepath.Dir(file.name), err)
		}
		if err := os.WriteFile(fullPath, []byte(file.content), 0o644); err != nil {
			return WorkspaceStatus{}, fmt.Errorf("创建 workspace 文件 %s: %w", file.name, err)
		}
		status.CreatedFiles = append(status.CreatedFiles, file.name)
	}
	bootstrapExists, err := workspaceBootstrapExists(path)
	if err != nil {
		return WorkspaceStatus{}, err
	}
	if !hadManagedFiles && !bootstrapExists {
		if err := writeWorkspaceBootstrap(path); err != nil {
			return WorkspaceStatus{}, err
		}
		status.CreatedFiles = append(status.CreatedFiles, workspaceBootstrapFile)
	}
	return status, nil
}

func ensureWorkspaceRoot(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("workspace 路径不是目录: %s", path)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("检查 bot workspace: %w", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("创建 bot workspace: %w", err)
	}
	return nil
}

func workspaceGuide(status WorkspaceStatus) string {
	lines := []string{
		"已准备 workspace：" + status.Path,
	}
	if len(status.CreatedFiles) > 0 {
		lines = append(lines, "已创建基础文件："+strings.Join(status.CreatedFiles, "、"))
	}
	lines = append(lines,
		"发送普通文本或 /new [cwd] 会创建 ACP 会话。",
		"如果 workspace 中存在 Bootstrap.md，后续普通消息会把它作为工作区上下文注入给 ACP agent。",
	)
	return strings.Join(lines, "\n")
}

func workspaceHasManagedFiles(path string) (bool, error) {
	for _, file := range workspaceFiles("") {
		_, err := os.Stat(filepath.Join(path, file.name))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("检查 workspace 文件 %s: %w", file.name, err)
		}
	}
	return false, nil
}

func workspaceBootstrapExists(path string) (bool, error) {
	_, err := os.Stat(filepath.Join(path, workspaceBootstrapFile))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("检查 workspace 初始化引导文件: %w", err)
}

func writeWorkspaceBootstrap(path string) error {
	if err := os.WriteFile(filepath.Join(path, workspaceBootstrapFile), []byte(workspaceBootstrapContent()), 0o644); err != nil {
		return fmt.Errorf("创建 workspace 初始化引导文件: %w", err)
	}
	return nil
}

func workspaceBootstrapContent() string {
	return strings.Join([]string{
		"# Workspace Bootstrap",
		"",
		"当前 workspace 尚未完成初始化。你必须先完成一次性引导，再处理普通开发或问答任务。",
		"",
		"## 你的任务",
		"",
		"1. 先向用户提问，收集初始化所需信息。问题应简洁，可以一次性列出。",
		"2. 等用户回答后，使用你可用的本地文件工具更新 workspace 中的 L0/L1/L2 文件。",
		"3. 写入完成后，删除本文件 `Bootstrap.md`。本文件不存在即表示初始化完成，后续会话不会再注入本引导。",
		"",
		"## 需要询问的信息",
		"",
		"- 用户想叫你什么名字。",
		"- 你应采用什么角色、性格、语气和协作边界。",
		"- 需要长期记住的用户信息、偏好、常用路径、账号 profile 或环境约束。",
		"- 是否有需要沉淀到知识库的领域知识、项目经验或常用流程。",
		"",
		"如果用户已经在消息里给出了足够信息，可以直接整理写入；如果信息不足，先回复引导问题，不要删除 `Bootstrap.md`。",
		"如果用户明确要求跳过初始化，请写入最小可用默认内容，然后删除 `Bootstrap.md`。",
		"",
		"## 写入位置",
		"",
		"- L0 根目录记忆：`SOUL.md`、`MEMORY.md`、`AGENTS.md`、`TOOLS.md`，记录 bot 身份、用户偏好、工作规则和工具环境。",
		"- L1 `knowledge/`：领域知识、项目经验、问题解决方案；常用入口是 `knowledge/core.md`、`knowledge/index.md`、`knowledge/log.md`。",
		"- L2 `skills/`：稳定、可复用的多步骤流程；入口是 `skills/core.md`，每个技能使用 `skills/<skill-name>/SKILL.md`。",
		"",
		"新增、删除或重命名知识/技能文件后，必须同步 `knowledge/index.md`，并在 `knowledge/log.md` 末尾追加 `[YYYY-MM-DD] 操作 文件 摘要`。",
		"如果删除 `Bootstrap.md` 后 `knowledge/index.md` 仍列出了它，也要同步移除对应索引项。",
	}, "\n")
}

func workspaceFiles(botID string) []struct {
	name    string
	content string
} {
	profileName := "lark-acp-<bot-id>"
	if botID = strings.TrimSpace(botID); botID != "" {
		profileName = "lark-acp-" + botID
	}
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

## 知识沉淀

- 根目录文件是 L0 记忆层，记录身份、用户偏好、行为准则和工具环境。
- ` + "`knowledge/`" + ` 是 L1 知识层，记录领域知识、项目经验和问题解决方案。
- ` + "`skills/`" + ` 是 L2 技能层，记录稳定可复用的操作流程。
`,
		},
		{
			name: "TOOLS.md",
			content: `# TOOLS

## 工具与环境

请在这里记录这个 bot 可用的工具、关键路径、账号 profile 和环境约束。

## 飞书 CLI

- 如果当前环境没有安装 lark-cli，按飞书 CLI 安装指南安装：https://open.feishu.cn/document/no_class/mcp-archive/feishu-cli-installation-guide.md
- 需要调用 lark-cli 时，优先使用当前对话智能体对应的 profile；当前 bot 建议使用 profile：` + profileName + `。
- 如果不存在对应 profile，先查看 lark-cli 的帮助命令确认创建方式，再使用 config.json 中当前 bot 的 app_id 和 app_secret 创建该 profile，后续固定使用这个 profile。
- app_secret 属于敏感信息，不要写入提示词、回复、日志或命令行参数；需要传给 lark-cli 时，应使用 stdin 等不回显到命令文本的方式。
`,
		},
		{
			name: filepath.Join("knowledge", "AGENTS.md"),
			content: `# Knowledge 层规范 (L1)

本目录存放领域知识和经验：具体的技术知识、项目经验、问题解决方案。

## 目录结构

- ` + "`core.md`" + `：知识内容入口，按主题组织概要级条目。
- ` + "`index.md`" + `：全量文件索引。新增、删除、重命名文件后同步更新。
- ` + "`log.md`" + `：活动日志。追加式记录每次知识变更。
- ` + "`lint.md`" + `：一致性检查提示词。
- 其余 ` + "`*.md`" + `：独立主题文档，记录完整背景、细节、步骤和结论。

## 文件格式要求

- Frontmatter：` + "`title`" + ` 必须，` + "`type`" + ` 为 ` + "`knowledge`" + `，` + "`tags`" + ` 可选数组。
- 跨文件引用：使用 wikilink 格式 ` + "`[[title]]`" + ` 或 ` + "`[[文件名]]`" + `。

## 写入原则

1. 知识内容写入 ` + "`core.md`" + ` 或独立主题文档；` + "`core.md`" + ` 放概要，详细内容拆分到独立文件。
2. 新增、删除、重命名文件后，同步更新 ` + "`index.md`" + `。
3. 在 ` + "`log.md`" + ` 末尾追加变更记录，格式：` + "`[YYYY-MM-DD] 操作 文件 摘要`" + `。
4. 文件名用小写短横线命名，体现主题。
5. 保持简洁，避免冗余；过时知识及时修订或删除，并同步索引。
`,
		},
		{
			name: filepath.Join("knowledge", "core.md"),
			content: `---
title: core knowledge
type: knowledge
tags:
- core
---

# Core Knowledge

## 核心知识概要

- 暂无。沉淀新知识时，在这里添加概要条目，并用 wikilink 指向详细文档。

## 引用清单

- 暂无。
`,
		},
		{
			name: filepath.Join("knowledge", "index.md"),
			content: `---
title: knowledge index
type: knowledge
tags:
- index
---

# Index

## L0 Memory

| 文件 | 用途 |
| --- | --- |
| ` + "`SOUL.md`" + ` | bot 名字、角色、语气、边界和默认工作方式 |
| ` + "`MEMORY.md`" + ` | 用户信息、长期偏好和常用上下文 |
| ` + "`AGENTS.md`" + ` | 工作流程、协作约定和仓库操作规则 |
| ` + "`TOOLS.md`" + ` | 可用工具、关键路径、账号 profile 和环境约束 |

## L1 Knowledge

| 文件 | 用途 |
| --- | --- |
| ` + "`knowledge/AGENTS.md`" + ` | L1 写入规范 |
| ` + "`knowledge/core.md`" + ` | 知识概要入口 |
| ` + "`knowledge/index.md`" + ` | 三层文件索引 |
| ` + "`knowledge/log.md`" + ` | 知识变更日志 |
| ` + "`knowledge/lint.md`" + ` | 一致性检查提示词 |

## L2 Skills

| 文件 | 用途 |
| --- | --- |
| ` + "`skills/AGENTS.md`" + ` | L2 写入规范 |
| ` + "`skills/core.md`" + ` | 技能索引入口 |
| ` + "`skills/wiki/SKILL.md`" + ` | 知识库维护流程 |
`,
		},
		{
			name: filepath.Join("knowledge", "log.md"),
			content: `---
title: knowledge log
type: knowledge
tags:
- log
---

# Log

[初始化] 创建 workspace 知识层级模板
`,
		},
		{
			name: filepath.Join("knowledge", "lint.md"),
			content: `---
title: knowledge lint
type: knowledge
tags:
- lint
---

# Lint Prompt

读取 ` + "`knowledge/index.md`" + ` 获取全量文件清单，并检查：

1. index 中列出但实际不存在的文件，或存在但未列入 index 的文件。
2. ` + "`knowledge/core.md`" + ` 引用清单与实际知识文件是否同步。
3. 不同文件中是否存在互相矛盾或明显过时的信息。
4. 新增、删除、重命名文件后是否已在 ` + "`knowledge/log.md`" + ` 追加记录。

发现问题后直接修复，并在 ` + "`knowledge/log.md`" + ` 追加变更记录。
`,
		},
		{
			name: filepath.Join("skills", "AGENTS.md"),
			content: `# Skills 层规范 (L2)

本目录存放稳定、可复用的操作流程。不要把一次性任务总结成 skill。

## 目录结构

- ` + "`core.md`" + `：技能索引入口。
- ` + "`<skill-name>/SKILL.md`" + `：单个技能定义。

## SKILL.md 格式

每个 skill 必须包含 YAML frontmatter：

` + "```markdown" + `
---
name: <skill-name>
description: <一句话描述该 skill 的用途>
trigger: <描述什么场景下应该使用该 skill>
---
` + "```" + `

正文应包含 Purpose、When to Use、Steps / Commands / Usage 等可执行说明。
`,
		},
		{
			name: filepath.Join("skills", "core.md"),
			content: `---
title: skills core
type: skill
tags:
- core
---

# Skills

## 可用技能

- [[wiki]]：维护 workspace 知识库和技能库。
`,
		},
		{
			name: filepath.Join("skills", "wiki", "SKILL.md"),
			content: `---
name: wiki
description: 维护 workspace 知识库和技能库，包括索引同步和日志记录。
trigger: 当用户要求记住经验、沉淀知识、整理知识库、创建可复用流程或执行一致性检查时使用。
---

# Wiki

## Purpose

维护当前 bot workspace 的 L0/L1/L2 知识体系。

## When to Use

- 用户要求“记住”长期偏好、常用路径或工作规则。
- 用户要求沉淀领域知识、项目经验或问题解决方案。
- 用户要求把稳定流程总结为可复用技能。
- 用户要求检查知识库一致性。

## Steps

1. 先读取 ` + "`knowledge/index.md`" + `，确认当前文件全貌。
2. 按信息类型选择写入位置：L0 根目录、L1 ` + "`knowledge/`" + ` 或 L2 ` + "`skills/`" + `。
3. 新增、删除、重命名知识或技能文件后，同步更新 ` + "`knowledge/index.md`" + `。
4. 在 ` + "`knowledge/log.md`" + ` 末尾追加 [YYYY-MM-DD] 操作 文件 摘要。
5. 保持内容简洁，避免记录一次性任务结果。
`,
		},
	}
}
