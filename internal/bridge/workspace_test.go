package bridge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
