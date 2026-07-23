package bridge

import (
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

func TestRuntimeDispatchSessionInfoSendsStateUpdates(t *testing.T) {
	r := newRuntimeManager()
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", ThreadID: "thread-a"}
	var updates []acp.SessionUpdate
	unsub := r.SubscribeUpdates(key, func(sessionID string, update acp.SessionUpdate) {
		if sessionID != "session-1" {
			t.Fatalf("sessionID = %q, want session-1", sessionID)
		}
		updates = append(updates, update)
	})
	defer unsub()

	r.dispatchSessionInfo(key, "session-1", acp.SessionInfo{
		AvailableCommands: []acp.AvailableCommand{{Name: "review", Description: "Review changes"}},
		ConfigOptions: []acp.SessionConfigOption{
			{ID: "model", Name: "Model", Category: "model", Type: "select", CurrentValue: "gpt-5.6"},
		},
	})

	if len(updates) != 2 {
		t.Fatalf("updates = %+v, want commands and config updates", updates)
	}
	if updates[0].SessionUpdate != "available_commands_update" || len(updates[0].AvailableCommands) != 1 || updates[0].AvailableCommands[0].Name != "review" {
		t.Fatalf("first update = %+v, want available commands", updates[0])
	}
	if updates[1].SessionUpdate != "config_option_update" || len(updates[1].ConfigOptions) != 1 || updates[1].ConfigOptions[0].ID != "model" {
		t.Fatalf("second update = %+v, want config options", updates[1])
	}
}
