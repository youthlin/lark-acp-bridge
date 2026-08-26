package bridge

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const workspaceBootstrapFile = "Bootstrap.md"
const workspaceWikiPolicyMarker = "<!-- lark-acp-bridge:wiki-policy:v1 -->"
const workspaceBuiltinSkillsMarker = "<!-- lark-acp-bridge:builtin-skills:v1 -->"
const workspaceLocalDir = ".local"

var workspaceLocalStateEntries = []string{
	"sessions.json",
	"scheduled_tasks.json",
	"token_usage.json",
	"restart_ack.json",
	"processed_messages.json",
	"cache",
}

func workspaceLocalPath(workspace string, elem ...string) string {
	parts := append([]string{strings.TrimSpace(workspace), workspaceLocalDir}, elem...)
	return filepath.Join(parts...)
}

func workspaceLegacyPath(workspace string, elem ...string) string {
	parts := append([]string{strings.TrimSpace(workspace)}, elem...)
	return filepath.Join(parts...)
}

func sessionStoreSiblingPath(path string, name string) string {
	return filepath.Join(filepath.Dir(path), name)
}

type WorkspaceStatus struct {
	Path          string
	CreatedFiles  []string
	UpgradedFiles []string
}

type WorkspaceUpgradeStatus struct {
	Path         string
	UpdatedFiles []string
}

type ensureWorkspaceOptions struct {
	recordBuiltinUpgradeLog bool
}

func ensureWorkspace(path string, botID string) (WorkspaceStatus, error) {
	return ensureWorkspaceWithOptions(path, botID, ensureWorkspaceOptions{recordBuiltinUpgradeLog: true})
}

func ensureWorkspaceWithOptions(path string, botID string, opts ensureWorkspaceOptions) (WorkspaceStatus, error) {
	if strings.TrimSpace(path) == "" {
		return WorkspaceStatus{}, fmt.Errorf("workspace 为空")
	}
	gitignoreUpdated, err := prepareWorkspaceLocalState(path)
	if err != nil {
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
	if gitignoreUpdated {
		status.CreatedFiles = append(status.CreatedFiles, ".gitignore")
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
	if hadManagedFiles {
		builtinStatus, err := ensureWorkspaceBuiltinSkills(path)
		if err != nil {
			return WorkspaceStatus{}, err
		}
		for _, file := range status.CreatedFiles {
			if isWorkspaceBuiltinSkillFile(file) {
				builtinStatus.UpdatedFiles = appendUniqueString(builtinStatus.UpdatedFiles, file)
			}
		}
		if opts.recordBuiltinUpgradeLog {
			if err := appendWorkspaceUpgradeLog(path, builtinStatus); err != nil {
				return WorkspaceStatus{}, err
			}
		}
		status.UpgradedFiles = append(status.UpgradedFiles, builtinStatus.UpdatedFiles...)
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

func prepareWorkspaceLocalState(path string) (bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, fmt.Errorf("workspace 为空")
	}
	if err := migrateWorkspaceLocalState(path); err != nil {
		return false, err
	}
	gitignoreUpdated, err := ensureWorkspaceLocalGitignore(path)
	if err != nil {
		return false, err
	}
	return gitignoreUpdated, nil
}

func migrateWorkspaceLocalState(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("workspace 为空")
	}
	if err := ensureWorkspaceRoot(path); err != nil {
		return err
	}
	for _, name := range workspaceLocalStateEntries {
		if err := migrateWorkspaceLocalStateEntry(path, name); err != nil {
			return err
		}
	}
	return nil
}

func migrateWorkspaceLocalStateEntry(workspace string, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	legacyPath := workspaceLegacyPath(workspace, name)
	localPath := workspaceLocalPath(workspace, name)
	info, err := os.Stat(legacyPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("检查 workspace 本地状态 %s: %w", name, err)
	}
	if _, err := os.Stat(localPath); err == nil {
		slog.Warn("workspace 本地状态已存在于 .local，跳过旧路径迁移", "旧路径", legacyPath, "新路径", localPath)
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("检查 workspace .local 状态 %s: %w", name, err)
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return fmt.Errorf("创建 workspace .local 目录: %w", err)
	}
	if err := os.Rename(legacyPath, localPath); err != nil {
		return fmt.Errorf("迁移 workspace 本地状态 %s 到 .local: %w", name, err)
	}
	slog.Info("已迁移 workspace 本地状态到 .local", "旧路径", legacyPath, "新路径", localPath, "目录", info.IsDir())
	return nil
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

func ensureWorkspaceLocalGitignore(path string) (bool, error) {
	gitignorePath := filepath.Join(path, ".gitignore")
	data, err := os.ReadFile(gitignorePath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("读取 workspace .gitignore: %w", err)
	}
	text := string(data)
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == ".local/" {
			return false, nil
		}
	}
	if strings.TrimSpace(text) != "" {
		text = strings.TrimRight(text, " \t\r\n") + "\n"
	}
	text += ".local/\n"
	if err := os.WriteFile(gitignorePath, []byte(text), 0o644); err != nil {
		return false, fmt.Errorf("写入 workspace .gitignore: %w", err)
	}
	return true, nil
}

func upgradeWorkspaceWikiPolicy(path string) (WorkspaceUpgradeStatus, error) {
	if strings.TrimSpace(path) == "" {
		return WorkspaceUpgradeStatus{}, fmt.Errorf("workspace 为空")
	}
	if err := ensureWorkspaceRoot(path); err != nil {
		return WorkspaceUpgradeStatus{}, err
	}
	status := WorkspaceUpgradeStatus{Path: path}
	files := map[string]string{
		filepath.Join("knowledge", "AGENTS.md"):     workspaceKnowledgePolicyBlock(),
		filepath.Join("knowledge", "lint.md"):       workspaceKnowledgeLintPolicyBlock(),
		filepath.Join("skills", "wiki", "SKILL.md"): workspaceWikiSkillPolicyBlock(),
	}
	for _, name := range []string{
		filepath.Join("knowledge", "AGENTS.md"),
		filepath.Join("knowledge", "lint.md"),
		filepath.Join("skills", "wiki", "SKILL.md"),
	} {
		updated, err := appendWorkspacePolicyBlock(filepath.Join(path, name), files[name])
		if err != nil {
			return WorkspaceUpgradeStatus{}, fmt.Errorf("更新 workspace 文件 %s: %w", name, err)
		}
		if updated {
			status.UpdatedFiles = append(status.UpdatedFiles, name)
		}
	}
	builtinStatus, err := ensureWorkspaceBuiltinSkills(path)
	if err != nil {
		return WorkspaceUpgradeStatus{}, err
	}
	for _, name := range builtinStatus.UpdatedFiles {
		status.UpdatedFiles = appendUniqueString(status.UpdatedFiles, name)
	}
	return status, nil
}

func appendWorkspacePolicyBlock(path string, block string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, err
		}
		data = nil
	}
	if strings.Contains(string(data), workspaceWikiPolicyMarker) {
		return false, nil
	}
	next := strings.TrimRight(string(data), " \t\r\n")
	if next != "" {
		next += "\n\n"
	}
	next += strings.TrimSpace(block) + "\n"
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func ensureWorkspaceBuiltinSkills(path string) (WorkspaceUpgradeStatus, error) {
	if strings.TrimSpace(path) == "" {
		return WorkspaceUpgradeStatus{}, fmt.Errorf("workspace 为空")
	}
	if err := ensureWorkspaceRoot(path); err != nil {
		return WorkspaceUpgradeStatus{}, err
	}
	status := WorkspaceUpgradeStatus{Path: path}
	for _, file := range workspaceBuiltinSkillFiles() {
		created, err := ensureWorkspaceFile(filepath.Join(path, file.name), file.content)
		if err != nil {
			return WorkspaceUpgradeStatus{}, fmt.Errorf("更新 workspace 文件 %s: %w", file.name, err)
		}
		if created {
			status.UpdatedFiles = appendUniqueString(status.UpdatedFiles, file.name)
		}
	}
	updated, err := appendWorkspaceLineIfMissing(
		filepath.Join(path, "skills", "AGENTS.md"),
		workspaceBuiltinSkillsMarker,
		workspaceSkillsUsagePolicyBlock(),
	)
	if err != nil {
		return WorkspaceUpgradeStatus{}, fmt.Errorf("更新 workspace 文件 %s: %w", filepath.Join("skills", "AGENTS.md"), err)
	}
	if updated {
		status.UpdatedFiles = appendUniqueString(status.UpdatedFiles, filepath.Join("skills", "AGENTS.md"))
	}
	updated, err = insertWorkspaceLineBeforeIfMissing(
		filepath.Join(path, "skills", "core.md"),
		"[[acp-trace]]",
		"- [[acp-trace]]：通过 sid 读取 ACP JSONL trace 并整理执行过程。",
		"- [[wiki]]",
	)
	if err != nil {
		return WorkspaceUpgradeStatus{}, fmt.Errorf("更新 workspace 文件 %s: %w", filepath.Join("skills", "core.md"), err)
	}
	if updated {
		status.UpdatedFiles = appendUniqueString(status.UpdatedFiles, filepath.Join("skills", "core.md"))
	}
	updated, err = insertWorkspaceLineBeforeIfMissing(
		filepath.Join(path, "knowledge", "index.md"),
		workspaceACPTraceSkillFileName(),
		"| `"+workspaceACPTraceSkillFileName()+"` | ACP trace 执行轨迹读取流程 |",
		"| `skills/wiki/SKILL.md`",
	)
	if err != nil {
		return WorkspaceUpgradeStatus{}, fmt.Errorf("更新 workspace 文件 %s: %w", filepath.Join("knowledge", "index.md"), err)
	}
	if updated {
		status.UpdatedFiles = appendUniqueString(status.UpdatedFiles, filepath.Join("knowledge", "index.md"))
	}
	return status, nil
}

func ensureWorkspaceFile(path string, content string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func appendWorkspaceLineIfMissing(path string, present string, line string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, err
		}
		data = nil
	}
	text := string(data)
	if strings.Contains(text, present) {
		return false, nil
	}
	next := strings.TrimRight(text, " \t\r\n")
	if next != "" {
		next += "\n\n"
	}
	next += strings.TrimSpace(line) + "\n"
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func insertWorkspaceLineBeforeIfMissing(path string, present string, line string, before string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, err
		}
		data = nil
	}
	text := string(data)
	if strings.Contains(text, present) {
		return false, nil
	}
	line = strings.TrimSpace(line)
	if idx := strings.Index(text, before); idx >= 0 {
		prefix := strings.TrimRight(text[:idx], " \t\r\n")
		if prefix != "" {
			prefix += "\n"
		}
		text = prefix + line + "\n" + text[idx:]
	} else {
		text = strings.TrimRight(text, " \t\r\n")
		if text != "" {
			text += "\n"
		}
		text += line + "\n"
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func appendUniqueString(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func workspaceKnowledgePolicyBlock() string {
	return strings.Join([]string{
		workspaceWikiPolicyMarker,
		"",
		"## Bridge Wiki Policy",
		"",
		"以下规则由 lark-acp-bridge 写入，用于让已有 workspace 跟随当前知识库维护约束：",
		"",
		"1. 同级文件或目录名称必须能区分用途；结构应服务检索，而不是为了分类本身增加层级。",
		"2. 写入前先判断信息属于 L0 偏好、L1 项目知识还是 L2 可复用流程；不要把一次性任务结果写成长期知识。",
		"3. 遇到矛盾或过时信息时，保留当前结论、依据和更新时间，不要静默覆盖重要背景。",
	}, "\n")
}

func workspaceKnowledgeLintPolicyBlock() string {
	return strings.Join([]string{
		workspaceWikiPolicyMarker,
		"",
		"## Bridge Wiki Lint Policy",
		"",
		"执行知识库一致性检查时，除原有检查项外，还应检查：",
		"",
		"1. 同级文件或目录名称是否能区分用途，目录层级是否服务检索。",
		"2. 是否有一次性任务结果、临时状态或可由源码直接读取的信息被写成长期知识。",
	}, "\n")
}

func workspaceWikiSkillPolicyBlock() string {
	return strings.Join([]string{
		workspaceWikiPolicyMarker,
		"",
		"## Bridge Wiki Maintenance Policy",
		"",
		"维护 workspace 知识库时还应遵守：",
		"",
		"1. 保持 taxonomy 清晰：同级名称可区分、父级覆盖子级、相关内容位置相近，结构应服务检索。",
		"2. 写入矛盾信息时保留当前结论、依据和更新时间；不确定时标注待确认。",
	}, "\n")
}

func workspaceSkillsUsagePolicyBlock() string {
	return strings.Join([]string{
		workspaceBuiltinSkillsMarker,
		"",
		"## Bridge Built-in Skills Policy",
		"",
		"使用 workspace 技能时还应遵守：",
		"",
		"1. 当用户请求命中 `skills/core.md` 中的技能名或说明时，先读取对应的 `skills/<skill-name>/SKILL.md`，再按其中步骤执行。",
		"2. 内置技能文件由 bridge 幂等补齐；如果用户已经自定义同名文件，不要覆盖已有内容。",
	}, "\n")
}

func workspaceKnowledgeIndexContent() string {
	return strings.Join([]string{
		"---",
		"title: knowledge index",
		"type: knowledge",
		"tags:",
		"- index",
		"---",
		"",
		"# Index",
		"",
		"## L0 Memory",
		"",
		"| 文件 | 用途 |",
		"| --- | --- |",
		"| `SOUL.md` | bot 名字、角色、语气、边界和默认工作方式 |",
		"| `MEMORY.md` | 用户信息、长期偏好和常用上下文 |",
		"| `AGENTS.md` | 工作流程、协作约定和仓库操作规则 |",
		"| `TOOLS.md` | 可用工具、关键路径、账号 profile 和环境约束 |",
		"",
		"## L1 Knowledge",
		"",
		"| 文件 | 用途 |",
		"| --- | --- |",
		"| `knowledge/AGENTS.md` | L1 写入规范 |",
		"| `knowledge/core.md` | 知识概要入口 |",
		"| `knowledge/index.md` | 三层文件索引 |",
		"| `knowledge/log.md` | 知识变更日志 |",
		"| `knowledge/lint.md` | 一致性检查提示词 |",
		"",
		"## L2 Skills",
		"",
		"| 文件 | 用途 |",
		"| --- | --- |",
		"| `skills/AGENTS.md` | L2 写入规范 |",
		"| `skills/core.md` | 技能索引入口 |",
		"| `skills/acp-trace/SKILL.md` | ACP trace 执行轨迹读取流程 |",
		"| `skills/wiki/SKILL.md` | 知识库维护流程 |",
		"",
	}, "\n")
}

func workspaceSkillsAgentsContent() string {
	return strings.Join([]string{
		"# Skills 层规范 (L2)",
		"",
		"本目录存放稳定、可复用的操作流程。不要把一次性任务总结成 skill。",
		"",
		"## 目录结构",
		"",
		"- `core.md`：技能索引入口。",
		"- `<skill-name>/SKILL.md`：单个技能定义。",
		"",
		"## SKILL.md 格式",
		"",
		"每个 skill 必须包含 YAML frontmatter：",
		"",
		"```markdown",
		"---",
		"name: <skill-name>",
		"description: <一句话描述该 skill 的用途>",
		"trigger: <描述什么场景下应该使用该 skill>",
		"---",
		"```",
		"",
		"正文应包含 Purpose、When to Use、Steps / Commands / Usage 等可执行说明。",
		"",
		workspaceSkillsUsagePolicyBlock(),
		"",
	}, "\n")
}

func workspaceSkillsCoreContent() string {
	return strings.Join([]string{
		"---",
		"title: skills core",
		"type: skill",
		"tags:",
		"- core",
		"---",
		"",
		"# Skills",
		"",
		"## 可用技能",
		"",
		"- [[acp-trace]]：通过 sid 读取 ACP JSONL trace 并整理执行过程。",
		"- [[wiki]]：维护 workspace 知识库和技能库。",
		"",
	}, "\n")
}

func workspaceBuiltinSkillFiles() []struct {
	name    string
	content string
} {
	return []struct {
		name    string
		content string
	}{
		{name: workspaceACPTraceSkillFileName(), content: workspaceACPTraceSkillContent()},
	}
}

func isWorkspaceBuiltinSkillFile(name string) bool {
	for _, file := range workspaceBuiltinSkillFiles() {
		if file.name == name {
			return true
		}
	}
	return false
}

func workspaceACPTraceSkillFileName() string {
	return filepath.Join("skills", "acp-trace", "SKILL.md")
}

func workspaceACPTraceSkillContent() string {
	return strings.Join([]string{
		"---",
		"name: acp-trace",
		"description: 通过 ACP session id 读取本地 JSONL trace，并还原执行过程、最终回复和关键工具调用。",
		"trigger: 当用户提到 sid、session id、会话 id、执行轨迹、trace、查看另一个会话过程、继续某次会话结果时使用。",
		"---",
		"",
		"# ACP Trace",
		"",
		"## Purpose",
		"",
		"通过用户提供的 `sid` 定位 bot workspace 本地 ACP JSONL trace，整理对应会话的执行过程、最终 assistant 回复、计划、工具调用和错误信息。",
		"",
		"## When to Use",
		"",
		"- 用户提供 `sid: xxx`，要求查看或总结这次执行。",
		"- 用户要求查看另一个会话、某次会话、执行轨迹、trace、最终回复或工具调用。",
		"- 用户要求基于某次执行结果继续分析，但当前会话没有那次上下文。",
		"",
		"## Steps",
		"",
		"1. 从用户消息中提取 `sid`，去掉可能的 `sid:` 前缀和首尾空白。",
		"2. 确认目标 bot。用户明确指定 bot 时使用该 bot；未指定时先查当前 bot workspace，再按需搜索 `$HOME/.lark-acp-bridge/bots/*/.local/traces/`。",
		"3. 优先读取 `<bot-workspace>/.local/traces/<sid>.jsonl`。如果找不到，搜索同级 trace 目录中名称包含该 sid 的 `.jsonl` 文件。",
		"4. 按 JSONL 逐行解析；不要用纯文本拼接猜测结构。读取失败、JSON 行损坏或文件不存在时说明具体路径和错误。",
		"5. 解释 trace 类型时遵守：",
		"   - `user` 是 bridge 发给 ACP agent 的完整 prompt。",
		"   - `assistant` 是最终回复前、已进入执行过程区的 ACP assistant 文本。",
		"   - `assistant` 且 `is_final=true` 是本轮最终回复区文本，通常是最终回复来源。",
		"   - `thought`、`plan`、`status`、`tool` 是执行过程。",
		"   - `turn_result` 是 `session/prompt` 收尾元信息，不是最终回复。",
		"   - `error` 是执行错误。",
		"6. 每条记录的 `ts` 是固定宽度时间字符串；如果 trace 记录包含顶层 `message_id`，可用它把同一个 ACP session 内的多轮聊天区分开。",
		"7. 用户只指定 `sid` 时汇总整个 session；用户同时指定 `msg` 或某条飞书消息时，优先按 `message_id` 过滤，只查看这一条消息触发的执行轨迹。",
		"8. 如果用户只想看对话的一问一答、不需要中间过程，可过滤 `type=user` 和 `type=assistant && is_final=true`。",
		"9. 默认输出高信号摘要：会话路径、用户请求、最终 assistant 回复、关键计划/工具/错误。不要整段粘贴超长 trace；用户要求细节时再展开。",
		"10. 涉及隐私、密钥、token、cookie、app_secret 等敏感内容时只概述，不原样输出。",
		"",
		"## Useful Commands",
		"",
		"```bash",
		"ls -lt $HOME/.lark-acp-bridge/bots/*/.local/traces/*.jsonl",
		"rg -l '<sid>' $HOME/.lark-acp-bridge/bots/*/.local/traces",
		"```",
		"",
	}, "\n")
}

func appendWorkspaceUpgradeLog(path string, status WorkspaceUpgradeStatus) error {
	if len(status.UpdatedFiles) == 0 {
		return nil
	}
	logPath := filepath.Join(path, "knowledge", "log.md")
	data, err := os.ReadFile(logPath)
	if err != nil {
		return err
	}
	files := append([]string(nil), status.UpdatedFiles...)
	slices.Sort(files)
	line := fmt.Sprintf("[%s] 更新 workspace 同步 bridge 当前知识库维护约束和内置技能，涉及 %s", time.Now().Format("2006-01-02"), strings.Join(files, "、"))
	text := strings.TrimRight(string(data), " \t\r\n") + "\n" + line + "\n"
	return os.WriteFile(logPath, []byte(text), 0o644)
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
	files := []struct {
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
- 如需用消息表情表达轻量态度，可给当前用户消息添加 reaction；这只是可选表达，不应替代必要回复。目标消息 ID 来自每轮 ` + "`Message Metadata`" + ` 的 ` + "`message_id`" + `，可用 emoji_type：` + strings.Join(feishuMessageReactionEmojiTypes, ", ") + `。
- 添加 reaction 可用 ` + "`lark-cli im reactions create --message-id <message_id> --data '{\"reaction_type\":{\"emoji_type\":\"THUMBSUP\"}}' --as bot --profile " + profileName + "`" + `；如无把握或上下文不适合，就不要添加。
- 如果不存在对应 profile，运行 ` + "`lark-acp-bridge bots create-lark-cli-profile " + botID + "`" + ` 创建；如需自定义 profile 名称，使用 ` + "`--profile <name>`" + `。不要手动读取或解密 app_secret。
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
6. 同级文件或目录名称必须能区分用途；结构应服务检索，而不是为了分类本身增加层级。
7. 写入前先判断信息属于 L0 偏好、L1 项目知识还是 L2 可复用流程；不要把一次性任务结果写成长期知识。
8. 遇到矛盾或过时信息时，保留当前结论、依据和更新时间，不要静默覆盖重要背景。
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
			name:    filepath.Join("knowledge", "index.md"),
			content: workspaceKnowledgeIndexContent(),
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
5. 同级文件或目录名称是否能区分用途，目录层级是否服务检索。
6. 是否有一次性任务结果、临时状态或可由源码直接读取的信息被写成长期知识。

发现问题后直接修复，并在 ` + "`knowledge/log.md`" + ` 追加变更记录。
`,
		},
		{
			name:    filepath.Join("skills", "AGENTS.md"),
			content: workspaceSkillsAgentsContent(),
		},
		{
			name:    filepath.Join("skills", "core.md"),
			content: workspaceSkillsCoreContent(),
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
6. 保持 taxonomy 清晰：同级名称可区分、父级覆盖子级、相关内容位置相近，结构应服务检索。
7. 写入矛盾信息时保留当前结论、依据和更新时间；不确定时标注待确认。
`,
		},
	}
	files = append(files, workspaceBuiltinSkillFiles()...)
	return files
}
