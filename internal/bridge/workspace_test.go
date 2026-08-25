package bridge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestEnsureWorkspaceCreatesBootstrapOnlyForNewWorkspace(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	status, err := ensureWorkspace(workspace, "bot-a")
	if err != nil {
		t.Fatalf("ensureWorkspace(new) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, workspaceBootstrapFile)); err != nil {
		t.Fatalf("Bootstrap.md should exist after new workspace: %v", err)
	}

	markWorkspaceBootstrapped(t, workspace)
	status, err = ensureWorkspace(workspace, "bot-a")
	if err != nil {
		t.Fatalf("ensureWorkspace(existing) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, workspaceBootstrapFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Bootstrap.md should stay deleted after bootstrapped workspace, err=%v", err)
	}
	if len(status.CreatedFiles) != 0 {
		t.Fatalf("created files = %+v, want none for existing workspace", status.CreatedFiles)
	}
	if len(status.UpgradedFiles) != 0 {
		t.Fatalf("upgraded files = %+v, want none for current workspace", status.UpgradedFiles)
	}
}

func TestEnsureWorkspaceCreatesACPTraceSkillForNewWorkspace(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if _, err := ensureWorkspace(workspace, "bot-a"); err != nil {
		t.Fatalf("ensureWorkspace(new) error = %v", err)
	}

	for _, file := range []struct {
		name string
		want string
	}{
		{workspaceACPTraceSkillFileName(), "name: acp-trace"},
		{filepath.Join("skills", "AGENTS.md"), workspaceBuiltinSkillsMarker},
		{filepath.Join("skills", "core.md"), "[[acp-trace]]"},
		{filepath.Join("knowledge", "index.md"), workspaceACPTraceSkillFileName()},
	} {
		data, err := os.ReadFile(filepath.Join(workspace, file.name))
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", file.name, err)
		}
		if !strings.Contains(string(data), file.want) {
			t.Fatalf("%s = %q, want %q", file.name, data, file.want)
		}
	}
}

func TestEnsureWorkspaceRejectsEmptyPath(t *testing.T) {
	if _, err := ensureWorkspace("", "bot-a"); err == nil || !strings.Contains(err.Error(), "workspace 为空") {
		t.Fatalf("ensureWorkspace(empty) error = %v, want workspace empty error", err)
	}
}

func TestEnsureWorkspaceRejectsFilePath(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.WriteFile(workspace, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile(workspace) error = %v", err)
	}

	if _, err := ensureWorkspace(workspace, "bot-a"); err == nil || !strings.Contains(err.Error(), "workspace 路径不是目录") {
		t.Fatalf("ensureWorkspace(file) error = %v, want not directory error", err)
	}
}

func TestEnsureWorkspaceDefaultToolsIncludesLarkCLIProfileGuidance(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if _, err := ensureWorkspace(workspace, "default"); err != nil {
		t.Fatalf("ensureWorkspace() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "TOOLS.md"))
	if err != nil {
		t.Fatalf("ReadFile(TOOLS.md) error = %v", err)
	}
	tools := string(data)
	for _, want := range []string{
		"飞书 CLI",
		"lark-cli",
		"https://open.feishu.cn/document/no_class/mcp-archive/feishu-cli-installation-guide.md",
		"lark-acp-default",
		"lark-acp-bridge bots create-lark-cli-profile default",
		"app_secret",
		"不要手动读取或解密",
		"不要写入提示词、回复、日志或命令行参数",
	} {
		if !strings.Contains(tools, want) {
			t.Fatalf("TOOLS.md = %q, want %q", tools, want)
		}
	}
}

func TestEnsureWorkspaceMigratesLocalStateFiles(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(filepath.Join(workspace, "cache"), 0o755); err != nil {
		t.Fatalf("MkdirAll(cache) error = %v", err)
	}
	for _, name := range []string{
		"sessions.json",
		"scheduled_tasks.json",
		"token_usage.json",
		"restart_ack.json",
		"processed_messages.json",
	} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(name), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "cache", "img.png"), []byte("image"), 0o600); err != nil {
		t.Fatalf("WriteFile(cache/img.png) error = %v", err)
	}

	if _, err := ensureWorkspace(workspace, "bot-a"); err != nil {
		t.Fatalf("ensureWorkspace() error = %v", err)
	}

	for _, name := range workspaceLocalStateEntries {
		if _, err := os.Stat(filepath.Join(workspace, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("legacy %s err = %v, want removed", name, err)
		}
		if _, err := os.Stat(filepath.Join(workspace, workspaceLocalDir, name)); err != nil {
			t.Fatalf("local %s err = %v, want migrated", name, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(workspace, ".local", "cache", "img.png")); err != nil || string(data) != "image" {
		t.Fatalf("ReadFile(.local/cache/img.png) = %q, %v; want image", data, err)
	}
}

func TestMigrateWorkspaceLocalStateDoesNotOverwriteExistingLocalFile(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(filepath.Join(workspace, ".local"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.local) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "sessions.json"), []byte("legacy"), 0o600); err != nil {
		t.Fatalf("WriteFile(legacy sessions) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".local", "sessions.json"), []byte("local"), 0o600); err != nil {
		t.Fatalf("WriteFile(local sessions) error = %v", err)
	}

	if err := migrateWorkspaceLocalState(workspace); err != nil {
		t.Fatalf("migrateWorkspaceLocalState() error = %v", err)
	}

	if data, err := os.ReadFile(filepath.Join(workspace, ".local", "sessions.json")); err != nil || string(data) != "local" {
		t.Fatalf("local sessions = %q, %v; want unchanged local", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "sessions.json")); err != nil || string(data) != "legacy" {
		t.Fatalf("legacy sessions = %q, %v; want retained legacy", data, err)
	}
}

func TestUpgradeWorkspaceWikiPolicyAppendsRulesAndIsIdempotent(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if _, err := ensureWorkspace(workspace, "bot-a"); err != nil {
		t.Fatalf("ensureWorkspace() error = %v", err)
	}
	markWorkspaceBootstrapped(t, workspace)
	knowledgeAgents := filepath.Join(workspace, "knowledge", "AGENTS.md")
	if err := os.WriteFile(knowledgeAgents, []byte("# Custom Knowledge Rules\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(knowledge/AGENTS.md) error = %v", err)
	}

	status, err := upgradeWorkspaceWikiPolicy(workspace)
	if err != nil {
		t.Fatalf("upgradeWorkspaceWikiPolicy() error = %v", err)
	}
	if len(status.UpdatedFiles) != 3 {
		t.Fatalf("updated files = %+v, want three policy files", status.UpdatedFiles)
	}
	if err := appendWorkspaceUpgradeLog(workspace, status); err != nil {
		t.Fatalf("appendWorkspaceUpgradeLog() error = %v", err)
	}
	for _, file := range []string{
		filepath.Join("knowledge", "AGENTS.md"),
		filepath.Join("knowledge", "lint.md"),
		filepath.Join("skills", "wiki", "SKILL.md"),
	} {
		data, err := os.ReadFile(filepath.Join(workspace, file))
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", file, err)
		}
		text := string(data)
		if strings.Count(text, workspaceWikiPolicyMarker) != 1 {
			t.Fatalf("%s marker count = %d, want one in:\n%s", file, strings.Count(text, workspaceWikiPolicyMarker), text)
		}
		if file == filepath.Join("knowledge", "AGENTS.md") && !strings.Contains(text, "# Custom Knowledge Rules") {
			t.Fatalf("knowledge/AGENTS.md lost existing content:\n%s", text)
		}
	}
	logData, err := os.ReadFile(filepath.Join(workspace, "knowledge", "log.md"))
	if err != nil {
		t.Fatalf("ReadFile(knowledge/log.md) error = %v", err)
	}
	if !strings.Contains(string(logData), "同步 bridge 当前知识库维护约束") {
		t.Fatalf("knowledge/log.md = %q, want upgrade log", logData)
	}

	status, err = upgradeWorkspaceWikiPolicy(workspace)
	if err != nil {
		t.Fatalf("second upgradeWorkspaceWikiPolicy() error = %v", err)
	}
	if len(status.UpdatedFiles) != 0 {
		t.Fatalf("second updated files = %+v, want none", status.UpdatedFiles)
	}
}

func TestEnsureWorkspaceUpgradesExistingWorkspaceWithACPTraceSkill(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	for _, file := range []struct {
		name    string
		content string
	}{
		{name: filepath.Join("skills", "AGENTS.md"), content: "# Custom Skills Rules\n"},
		{name: filepath.Join("skills", "core.md"), content: "# Custom Skills\n\n- [[wiki]]：维护 workspace 知识库和技能库。\n"},
		{name: filepath.Join("knowledge", "index.md"), content: "# Custom Index\n\n## L2 Skills\n\n| 文件 | 用途 |\n| --- | --- |\n"},
		{name: filepath.Join("knowledge", "log.md"), content: "# Log\n"},
	} {
		path := filepath.Join(workspace, file.name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", filepath.Dir(file.name), err)
		}
		if err := os.WriteFile(path, []byte(file.content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", file.name, err)
		}
	}

	status, err := ensureWorkspace(workspace, "bot-a")
	if err != nil {
		t.Fatalf("ensureWorkspace(existing) error = %v", err)
	}
	if !slicesContains(status.CreatedFiles, workspaceACPTraceSkillFileName()) {
		t.Fatalf("created files = %+v, want acp-trace skill file", status.CreatedFiles)
	}
	for _, want := range []string{
		workspaceACPTraceSkillFileName(),
		filepath.Join("skills", "AGENTS.md"),
		filepath.Join("skills", "core.md"),
		filepath.Join("knowledge", "index.md"),
	} {
		if !slicesContains(status.UpgradedFiles, want) {
			t.Fatalf("upgraded files = %+v, want %s", status.UpgradedFiles, want)
		}
	}
	skillsAgents, err := os.ReadFile(filepath.Join(workspace, "skills", "AGENTS.md"))
	if err != nil {
		t.Fatalf("ReadFile(skills/AGENTS.md) error = %v", err)
	}
	if !strings.Contains(string(skillsAgents), "# Custom Skills Rules") || !strings.Contains(string(skillsAgents), workspaceBuiltinSkillsMarker) {
		t.Fatalf("skills/AGENTS.md = %q, want custom content plus builtin marker", skillsAgents)
	}
	skillData, err := os.ReadFile(filepath.Join(workspace, workspaceACPTraceSkillFileName()))
	if err != nil {
		t.Fatalf("ReadFile(acp-trace skill) error = %v", err)
	}
	if !strings.Contains(string(skillData), "最终 assistant 回复") || !strings.Contains(string(skillData), "turn_result") {
		t.Fatalf("acp-trace skill = %q, want trace guidance", skillData)
	}
	logData, err := os.ReadFile(filepath.Join(workspace, "knowledge", "log.md"))
	if err != nil {
		t.Fatalf("ReadFile(knowledge/log.md) error = %v", err)
	}
	if !strings.Contains(string(logData), "内置技能") || !strings.Contains(string(logData), workspaceACPTraceSkillFileName()) {
		t.Fatalf("knowledge/log.md = %q, want builtin skill upgrade log", logData)
	}

	status, err = ensureWorkspace(workspace, "bot-a")
	if err != nil {
		t.Fatalf("second ensureWorkspace(existing) error = %v", err)
	}
	if len(status.UpgradedFiles) != 0 {
		t.Fatalf("second upgraded files = %+v, want none", status.UpgradedFiles)
	}
}

func TestWorkspaceContextPromptIgnoresEmptyWorkspace(t *testing.T) {
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, workspaceBootstrapFile), []byte("# Should Not Leak\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(Bootstrap.md) error = %v", err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir(tmp) error = %v", err)
	}
	defer func() {
		if err := os.Chdir(oldWd); err != nil {
			t.Fatalf("Chdir(oldWd) error = %v", err)
		}
	}()

	if got := workspaceContextPrompt(""); got != "" {
		t.Fatalf("workspaceContextPrompt(empty) = %q, want empty", got)
	}
	if got := promptTextWithWorkspaceContext("", feishu.Message{}, "hello"); got != "hello" {
		t.Fatalf("promptTextWithWorkspaceContext(empty) = %q, want user text only", got)
	}
}

func TestWorkspaceContextPromptTruncatesLargeFiles(t *testing.T) {
	workspace := t.TempDir()
	largeContent := strings.Repeat("a", int(workspaceContextFileMaxBytes)+32) + "TAIL_SHOULD_NOT_APPEAR"
	if err := os.WriteFile(filepath.Join(workspace, "SOUL.md"), []byte(largeContent), 0o644); err != nil {
		t.Fatalf("WriteFile(SOUL.md) error = %v", err)
	}

	prompt := workspaceContextPrompt(workspace)
	if !strings.Contains(prompt, "Workspace Context") {
		t.Fatalf("prompt = %q, want workspace context", prompt)
	}
	if !strings.Contains(prompt, "已截断") {
		t.Fatalf("prompt = %q, want truncation notice", prompt)
	}
	if strings.Contains(prompt, "TAIL_SHOULD_NOT_APPEAR") {
		t.Fatalf("prompt contains truncated tail")
	}
}

func TestWorkspacePromptForSessionInjectsWorkspaceContextAndMemoryPolicyTogether(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "MEMORY.md"), []byte("# MEMORY\n\n偏好：中文回复\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(MEMORY.md) error = %v", err)
	}
	svc := newTestService(config.Default(), nil)
	session := Session{Workspace: workspace}

	first := svc.promptTextWithWorkspaceContextForSession(session, feishu.Message{}, "你好")
	if !strings.Contains(first, "Workspace Context") || !strings.Contains(first, "Workspace Memory Policy") {
		t.Fatalf("first prompt = %q, want workspace context and memory policy", first)
	}

	session.WorkspacePrompted = true
	normal := svc.promptTextWithWorkspaceContextForSession(session, feishu.Message{}, "继续")
	if strings.Contains(normal, "Workspace Context") || strings.Contains(normal, "Workspace Memory Policy") {
		t.Fatalf("normal prompt = %q, want no repeated workspace context or memory policy", normal)
	}
}

func slicesContains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
