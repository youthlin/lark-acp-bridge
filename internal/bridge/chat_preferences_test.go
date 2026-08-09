package bridge

import (
	"context"
	"reflect"
	"strconv"
	"testing"

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

func TestTakePendingAtAutoMessagesSessionBoundaries(t *testing.T) {
	keyA := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "chat-a"})
	keyB := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "chat-b"})
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
			takeKey: SessionKey{BotID: "bot-a", ChatID: "chat-a"},
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
			takeKey: normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "chat-missing"}),
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
	key := SessionKey{BotID: "bot-a", ChatID: "chat-a"}
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
