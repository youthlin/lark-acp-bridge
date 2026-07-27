package bridge

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func TestWikiTimerRunsSilentReflection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptReply: "NoReply"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	session := Session{
		Key:             SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "omt_thread"},
		AgentName:       "traex",
		ACPSessionID:    "acp-session-1",
		Cwd:             t.TempDir(),
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		WikiIntervalSec: 1,
	}
	svc.scheduleWikiAfterUserPrompt(session, config.Default().Agents["traex"])

	waitForCondition(t, 2*time.Second, func() bool { return rt.promptCallCount() == 1 })
	if got := rt.promptCalls[0].Text; !strings.Contains(got, "请对刚才的对话进行反思") || !strings.Contains(got, "NoReply") {
		t.Fatalf("wiki prompt = %q, want reflection prompt", got)
	}
	svc.taskMu.Lock()
	status := svc.wikiStatuses[session.Key]
	_, hasTimer := svc.wikiTimers[session.Key]
	svc.taskMu.Unlock()
	if hasTimer {
		t.Fatal("wiki timer should not reschedule itself after reflection")
	}
	if !status.lastSuccess || status.running {
		t.Fatalf("wiki status = %+v, want completed success", status)
	}
}
