package bridge

import (
	"testing"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestMessageMentionsBotRequiresCurrentBotOpenID(t *testing.T) {
	msg := feishu.Message{
		BotOpenID: testBotOpenID,
		Mentions: []feishu.Mention{
			{ID: "ou_other_user", Name: "其他用户", Type: "user"},
			{ID: "ou_other_bot", Name: "其他助手", Type: "bot"},
		},
	}
	if messageMentionsBot(msg) {
		t.Fatalf("messageMentionsBot(%+v) = true, want false for mentions that do not target current bot", msg.Mentions)
	}

	msg.Mentions = append(msg.Mentions, testBotMention("智能助手"))
	if !messageMentionsBot(msg) {
		t.Fatalf("messageMentionsBot(%+v) = false, want true for current bot open_id", msg.Mentions)
	}

	msg.BotOpenID = ""
	if messageMentionsBot(msg) {
		t.Fatalf("messageMentionsBot without BotOpenID = true, want false")
	}
}
