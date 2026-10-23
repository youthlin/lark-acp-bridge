package bridge

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/config"
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

func TestStripLeadingBotMentionNames(t *testing.T) {
	mentions := []feishu.Mention{
		testBotMention("智能助手"),
		{ID: "ou_other_bot", Name: "其他助手", Type: "bot"},
		{ID: "ou_user", Name: "用户甲", Type: "user"},
	}
	tests := []struct {
		name     string
		text     string
		mentions []feishu.Mention
		want     string
	}{
		{name: "multiple bots", text: "@智能助手 @其他助手 /rules set xxx", mentions: mentions, want: "/rules set xxx"},
		{name: "reverse order", text: "@其他助手 @智能助手 /status", mentions: []feishu.Mention{mentions[1], mentions[0]}, want: "/status"},
		{name: "keep user mention", text: "@智能助手 @用户甲 /status", mentions: []feishu.Mention{mentions[0], mentions[2]}, want: "@用户甲 /status"},
		{name: "keep command argument mention", text: "@智能助手 /rules set @其他助手", mentions: mentions, want: "/rules set @其他助手"},
		{name: "keep non-leading bot mention", text: "说明 @智能助手 /status", mentions: mentions, want: "说明 @智能助手 /status"},
		{name: "keep command for other leading bot", text: "@其他助手 /status @智能助手", mentions: []feishu.Mention{mentions[1], mentions[0]}, want: "@其他助手 /status @智能助手"},
		{name: "do not handle command for other bot", text: "@其他助手 /status", mentions: mentions[1:], want: "@其他助手 /status"},
		{name: "require mention boundary", text: "@智能助手扩展 /status", mentions: mentions, want: "@智能助手扩展 /status"},
		{
			name: "same display name bot and user",
			text: "@助手 @助手 /restart",
			mentions: []feishu.Mention{
				{ID: testBotOpenID, Name: "助手", Type: "bot"},
				{ID: "ou_user", Name: "助手", Type: "user"},
			},
			want: "@助手 /restart",
		},
		{
			name: "same display name bots are consumed once each",
			text: "@助手 @助手 /status",
			mentions: []feishu.Mention{
				{ID: testBotOpenID, Name: "助手", Type: "bot"},
				{ID: "ou_other_bot", Name: "助手", Type: "bot"},
			},
			want: "/status",
		},
		{
			name: "overlapping bot names follow metadata order",
			text: "@A B @A /status",
			mentions: []feishu.Mention{
				{ID: testBotOpenID, Name: "A B", Type: "bot"},
				{ID: "ou_other_bot", Name: "A", Type: "bot"},
			},
			want: "/status",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := feishu.Message{BotOpenID: testBotOpenID, Mentions: tt.mentions}
			if got := stripLeadingBotMentionNames(tt.text, msg); got != tt.want {
				t.Fatalf("stripLeadingBotMentionNames(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestNormalizeIncomingMessageDoesNotTurnSameNameUserMentionIntoCommand(t *testing.T) {
	svc := newTestService(config.Default(), nil)
	incoming := svc.normalizeIncomingMessage(feishu.Message{
		BotOpenID: testBotOpenID,
		Text:      "@助手 @助手 /restart",
		Mentions: []feishu.Mention{
			{ID: testBotOpenID, Name: "助手", Type: "bot"},
			{ID: "ou_user", Name: "助手", Type: "user"},
		},
	})
	if incoming.text != "@助手 /restart" {
		t.Fatalf("normalized text = %q, want user mention preserved before command", incoming.text)
	}
	if strings.HasPrefix(incoming.text, "/") {
		t.Fatalf("normalized text = %q, must not be dispatched as a slash command", incoming.text)
	}
}

func TestSelectPendingAtHistoryDropsExpiredAndTruncatesLongMessages(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)
	longText := strings.Repeat("长", maxPendingAtMessageLen+100)
	selected, remaining := selectPendingAtHistory([]pendingAtMessage{
		{MessageID: "old", Text: "过期消息", CachedAt: now.Add(-pendingAtHistoryTTL - time.Second)},
		{MessageID: "fresh", Text: longText, CachedAt: now.Add(-time.Minute)},
	}, feishu.Message{}, now)
	if len(remaining) != 0 {
		t.Fatalf("remaining = %+v, want none", remaining)
	}
	if len(selected) != 1 || selected[0].MessageID != "fresh" {
		t.Fatalf("selected = %+v, want only fresh message", selected)
	}
	if got := len([]rune(selected[0].Text)); got != maxPendingAtMessageLen {
		t.Fatalf("selected text runes = %d, want %d", got, maxPendingAtMessageLen)
	}
	if !strings.HasSuffix(selected[0].Text, pendingAtTruncatedText) {
		t.Fatalf("selected text = %q, want truncation marker", selected[0].Text)
	}
}

func TestClearPendingAtTextsForChatClearsAllThreadsOnlyInTargetChat(t *testing.T) {
	svc := NewService(config.Config{}, NewSessionStore(""))
	targetGroup := normalizeSessionKey(imSessionKey("bot-a", "chat-a", ""))
	targetThread := normalizeSessionKey(imSessionKey("bot-a", "chat-a", "thread-a"))
	otherChat := normalizeSessionKey(imSessionKey("bot-a", "chat-b", ""))
	otherBot := normalizeSessionKey(imSessionKey("bot-b", "chat-a", ""))
	for _, key := range []SessionKey{targetGroup, targetThread, otherChat, otherBot} {
		svc.pendingAtTexts[key] = []pendingAtMessage{{Text: key.MainID}}
	}

	svc.clearPendingAtTextsForChat("bot-a", "chat-a")
	if _, ok := svc.pendingAtTexts[targetGroup]; ok {
		t.Fatal("target group cache still exists")
	}
	if _, ok := svc.pendingAtTexts[targetThread]; ok {
		t.Fatal("target thread cache still exists")
	}
	if _, ok := svc.pendingAtTexts[otherChat]; !ok {
		t.Fatal("other chat cache was removed")
	}
	if _, ok := svc.pendingAtTexts[otherBot]; !ok {
		t.Fatal("other bot cache was removed")
	}
}

func TestTakePendingAtAutoMessagesSessionBoundaries(t *testing.T) {
	keyA := normalizeSessionKey(imSessionKey("bot-a", "chat-a", ""))
	keyB := normalizeSessionKey(imSessionKey("bot-a", "chat-b", ""))
	cases := []struct {
		name      string
		takeKey   SessionKey
		want      []pendingAtMessage
		remaining map[SessionKey][]pendingAtMessage
	}{
		{
			name:    "取出同 session 的 pending auto 消息并清空",
			takeKey: keyA,
			want: []pendingAtMessage{
				{SenderID: "ou_a", Text: "补充 1"},
				{SenderID: "ou_b", Text: "补充 2"},
			},
			remaining: map[SessionKey][]pendingAtMessage{
				keyB: {{SenderID: "ou_c", Text: "其他会话"}},
			},
		},
		{
			name:    "取出时先规范化 session key",
			takeKey: imSessionKey("bot-a", "chat-a", ""),
			want: []pendingAtMessage{
				{SenderID: "ou_a", Text: "补充 1"},
				{SenderID: "ou_b", Text: "补充 2"},
			},
			remaining: map[SessionKey][]pendingAtMessage{
				keyB: {{SenderID: "ou_c", Text: "其他会话"}},
			},
		},
		{
			name:    "取出不存在的 session 返回空且不影响其他 session",
			takeKey: normalizeSessionKey(imSessionKey("bot-a", "chat-missing", "")),
			want:    nil,
			remaining: map[SessionKey][]pendingAtMessage{
				keyA: {
					{SenderID: "ou_a", Text: "补充 1"},
					{SenderID: "ou_b", Text: "补充 2"},
				},
				keyB: {{SenderID: "ou_c", Text: "其他会话"}},
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			svc.pendingAtAuto[keyA] = []pendingAtMessage{
				{SenderID: "ou_a", Text: "补充 1"},
				{SenderID: "ou_b", Text: "补充 2"},
			}
			svc.pendingAtAuto[keyB] = []pendingAtMessage{{SenderID: "ou_c", Text: "其他会话"}}

			got := svc.takePendingAtAutoMessages(tt.takeKey)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("takePendingAtAutoMessages() = %+v, want %+v", got, tt.want)
			}
			if len(got) > 0 {
				got[0].Text = "调用方修改不应写回"
			}
			if !reflect.DeepEqual(svc.pendingAtAuto, tt.remaining) {
				t.Fatalf("pendingAtAuto = %+v, want %+v", svc.pendingAtAuto, tt.remaining)
			}
		})
	}
}

func TestAppendPendingAtAutoMessageSessionWorkBoundaries(t *testing.T) {
	key := imSessionKey("bot-a", "chat-a", "")
	normalizedKey := normalizeSessionKey(key)
	agent := config.AgentConfig{Command: "traex"}
	cases := []struct {
		name       string
		setup      func(t *testing.T, svc *Service)
		wantQueued bool
		want       []pendingAtMessage
	}{
		{
			name:       "没有运行任务时不入队",
			setup:      func(t *testing.T, svc *Service) {},
			wantQueued: false,
			want:       nil,
		},
		{
			name: "运行任务不允许 pending 入队时不入队",
			setup: func(t *testing.T, svc *Service) {
				_, finish := svc.startTask(context.Background(), Session{Key: normalizedKey, AgentName: "traex"}, agent, taskKindUser)
				t.Cleanup(finish)
			},
			wantQueued: false,
			want:       nil,
		},
		{
			name: "运行任务允许 pending 入队时按规范化 key 入队",
			setup: func(t *testing.T, svc *Service) {
				_, finish, err := svc.startTaskWithOptions(context.Background(), Session{Key: normalizedKey, AgentName: "traex"}, agent, taskKindUser, runningTaskOptions{queuePendingAtAuto: true})
				if err != nil {
					t.Fatalf("startTaskWithOptions() error = %v", err)
				}
				t.Cleanup(finish)
			},
			wantQueued: true,
			want:       []pendingAtMessage{{SenderID: "ou_a", Text: "补充消息"}},
		},
		{
			name: "超过上限时保留最新消息",
			setup: func(t *testing.T, svc *Service) {
				_, finish, err := svc.startTaskWithOptions(context.Background(), Session{Key: normalizedKey, AgentName: "traex"}, agent, taskKindUser, runningTaskOptions{queuePendingAtAuto: true})
				if err != nil {
					t.Fatalf("startTaskWithOptions() error = %v", err)
				}
				t.Cleanup(finish)
				for i := 0; i < maxPendingAtAuto; i++ {
					svc.pendingAtAuto[normalizedKey] = append(svc.pendingAtAuto[normalizedKey], pendingAtMessage{
						SenderID: "ou_old",
						Text:     "旧消息 " + strconv.Itoa(i),
					})
				}
			},
			wantQueued: true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			tt.setup(t, svc)

			got := svc.appendPendingAtAutoMessage(key, pendingAtMessage{SenderID: "ou_a", Text: "补充消息"})
			if got != tt.wantQueued {
				t.Fatalf("appendPendingAtAutoMessage() = %v, want %v", got, tt.wantQueued)
			}
			pending := svc.pendingAtAuto[normalizedKey]
			if tt.name == "超过上限时保留最新消息" {
				if len(pending) != maxPendingAtAuto {
					t.Fatalf("pending len = %d, want %d", len(pending), maxPendingAtAuto)
				}
				if pending[0].Text != "旧消息 1" || pending[len(pending)-1].Text != "补充消息" {
					t.Fatalf("pending boundary = first %q last %q, want latest window", pending[0].Text, pending[len(pending)-1].Text)
				}
				return
			}
			if !reflect.DeepEqual(pending, tt.want) {
				t.Fatalf("pendingAtAuto[%+v] = %+v, want %+v", normalizedKey, pending, tt.want)
			}
		})
	}
}
