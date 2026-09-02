package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestSanitizePromptSecretsForModelRedactsAndWritesFiles(t *testing.T) {
	workspace := t.TempDir()
	markWorkspaceBootstrapped(t, workspace)
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	session := testReadySession(t, store)
	session.Workspace = workspace
	session.Cwd = t.TempDir()
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	cfg := config.Config{
		Bots: []config.BotConfig{{
			ID:        "bot-a",
			Workspace: workspace,
		}},
		AgentList: []config.NamedAgentConfig{{
			Name: "traex",
			AgentConfig: config.AgentConfig{
				Command:    "traex",
				DefaultCwd: session.Cwd,
			},
		}},
	}
	rt := &fakeRuntime{promptReply: "done"}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    sessionKeyMainID(session.Key),
		ChatType:  "topic_group",
		ThreadID:  session.Key.SubID,
		MessageID: "om_secret_prompt",
		Workspace: workspace,
		Text:      "BASE_URL=x\nAPI_KEY=sk-live-secret",
		Mentions:  testBotMentions(),
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage() error = %v", err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q, want done", reply)
	}
	calls := rt.promptCallsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("promptCalls = %+v, want one call", calls)
	}
	prompt := calls[0].Text
	for _, unexpected := range []string{"sk-live-secret", "API_KEY=sk-live-secret"} {
		if strings.Contains(prompt, unexpected) {
			t.Fatalf("prompt = %q, should not include %q", prompt, unexpected)
		}
	}
	for _, want := range []string{
		secretInputNoticeTemplate,
		"BASE_URL=x",
		"API_KEY=[已隐藏: ",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want %q", prompt, want)
		}
	}
	secretPath := filepath.Join(workspace, ".local", secretInputsDirName, "om_secret_prompt", "secret-1.txt")
	data, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("ReadFile(secret) error = %v", err)
	}
	if string(data) != "sk-live-secret" {
		t.Fatalf("secret file content = %q, want raw secret", string(data))
	}
	if info, err := os.Stat(secretPath); err != nil {
		t.Fatalf("Stat(secret) error = %v", err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("secret file mode = %o, want 600", got)
	}
	updated, ok := store.Get(session.Key)
	if !ok {
		t.Fatal("session missing after prompt")
	}
	if strings.Contains(updated.Title, "sk-live-secret") {
		t.Fatalf("session title = %q, should not include raw secret", updated.Title)
	}
	if !strings.Contains(updated.Title, "API_KEY="+secretInputPlaceholder) {
		t.Fatalf("session title = %q, want display placeholder", updated.Title)
	}
}

func TestSanitizePromptSecretsKeepsBaseURLAndPlainKeyText(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	text := "BASE_URL=https://example.com\nuse the key concept\napi_key=sk-value"
	sanitized, err := svc.sanitizePromptSecretsForModel(feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_keep",
		Workspace: t.TempDir(),
	}, Session{Workspace: t.TempDir()}, text, text)
	if err != nil {
		t.Fatalf("sanitizePromptSecretsForModel() error = %v", err)
	}
	if strings.Contains(sanitized.Text, "sk-value") {
		t.Fatalf("sanitized prompt = %q, should not include secret", sanitized.Text)
	}
	for _, want := range []string{"BASE_URL=https://example.com", "use the key concept"} {
		if !strings.Contains(sanitized.Text, want) {
			t.Fatalf("sanitized prompt = %q, want %q", sanitized.Text, want)
		}
	}
}

func TestACPCommandSecretsAreRedactedWithoutBreakingSlashCommand(t *testing.T) {
	workspace := t.TempDir()
	markWorkspaceBootstrapped(t, workspace)
	store := NewSessionStore(filepath.Join(workspace, "sessions.json"))
	session := testReadySession(t, store)
	session.Workspace = workspace
	session.AvailableCommands = []acp.AvailableCommand{{Name: "review"}}
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert(session) error = %v", err)
	}
	rt := &fakeRuntime{promptReply: "review done"}
	cfg := config.Default()
	cfg.Bots[0].ID = "bot-a"
	cfg.Bots[0].Workspace = workspace
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	reply, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    sessionKeyMainID(session.Key),
		ChatType:  "topic_group",
		ThreadID:  session.Key.SubID,
		MessageID: "om_secret_command",
		Workspace: workspace,
		Text:      "/cmds /review api_key=sk-command-secret",
		Mentions:  testBotMentions(),
	})
	if err != nil {
		t.Fatalf("HandleFeishuMessage(/cmds) error = %v", err)
	}
	if reply != "review done" {
		t.Fatalf("reply = %q, want review done", reply)
	}
	calls := rt.promptCallsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("promptCalls = %+v, want one call", calls)
	}
	prompt := calls[0].Text
	if !strings.HasPrefix(prompt, "/review ") {
		t.Fatalf("prompt = %q, want slash command preserved", prompt)
	}
	if strings.Contains(prompt, "sk-command-secret") {
		t.Fatalf("prompt = %q, should not include raw secret", prompt)
	}
	if !strings.Contains(prompt, "api_key=[已隐藏: ") || !strings.Contains(prompt, secretInputNoticeTemplate) {
		t.Fatalf("prompt = %q, want placeholder and notice", prompt)
	}
}

func TestPendingAtAndAtAutoDisplayRedactsSecrets(t *testing.T) {
	workspace := t.TempDir()
	svc := NewService(config.Config{Bots: []config.BotConfig{{ID: "bot-a", Workspace: workspace}}}, NewSessionStore(""))
	msg := feishu.Message{
		BotID:     "bot-a",
		MessageID: "om_pending_secret",
		SenderID:  "ou_sender",
		Workspace: workspace,
		Text:      "API_KEY=sk-pending-secret\n请后续处理",
	}
	pending := svc.pendingAtMessageFromMessage(msg)
	if strings.Contains(pending.Text, "sk-pending-secret") {
		t.Fatalf("pending text = %q, should not include raw secret", pending.Text)
	}
	if !strings.Contains(pending.Text, "API_KEY=[已隐藏: ") {
		t.Fatalf("pending text = %q, want secret file placeholder", pending.Text)
	}
	data, err := os.ReadFile(filepath.Join(workspace, ".local", secretInputsDirName, "om_pending_secret", "secret-1.txt"))
	if err != nil {
		t.Fatalf("ReadFile(pending secret) error = %v", err)
	}
	if string(data) != "sk-pending-secret" {
		t.Fatalf("pending secret file content = %q, want raw secret", string(data))
	}
	formatted := formatPendingAtMessageBlock("## Pending", []pendingAtMessage{{
		MessageID: "om_1",
		SenderID:  "ou_a",
		Text:      "token=[已隐藏: /tmp/secret.txt]",
	}})
	if strings.Contains(formatted, "/tmp/secret.txt") {
		t.Fatalf("formatted pending = %q, should not include secret file path", formatted)
	}
	responsePrompt := formatAtAutoPendingResponsePrompt([]pendingAtMessage{pending})
	if strings.Contains(responsePrompt, "sk-pending-secret") {
		t.Fatalf("response prompt = %q, should not include raw secret", responsePrompt)
	}
	if !strings.Contains(responsePrompt, "API_KEY=[已隐藏: ") {
		t.Fatalf("response prompt = %q, want secret file placeholder", responsePrompt)
	}

	decision := svc.formatAtAutoDecisionPromptWithMessages(Session{
		Key: normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "")),
	}, []string{"API_KEY=sk-auto-secret"}, false)
	if strings.Contains(decision, "sk-auto-secret") {
		t.Fatalf("decision prompt = %q, should not include raw secret", decision)
	}
	if !strings.Contains(decision, "API_KEY="+secretInputPlaceholder) {
		t.Fatalf("decision prompt = %q, want placeholder", decision)
	}
}
