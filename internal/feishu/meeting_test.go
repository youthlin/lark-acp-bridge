package feishu

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	larkvc "github.com/larksuite/oapi-sdk-go/v3/service/vc/v1"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func TestParseMeetingInvitationPreservesCallID(t *testing.T) {
	event := &larkvc.P2BotMeetingInvitedV1{Event: &larkvc.P2BotMeetingInvitedV1Data{
		Meeting: larkvc.NewMeetingAgentEventMeetingBuilder().Id("meeting-1").MeetingNo("123456789").Topic("周会").Build(),
		Inviter: larkvc.NewMeetingAgentEventUserBuilder().Id("ou_user").UserName("用户").Build(),
		CallId:  stringPointer("call-original"),
	}}
	got, err := ParseMeetingInvitation(event)
	if err != nil {
		t.Fatalf("ParseMeetingInvitation() error = %v", err)
	}
	if got.Meeting.ID != "meeting-1" || got.Meeting.MeetingNo != "123456789" || got.CallID != "call-original" || got.Inviter.ID != "ou_user" {
		t.Fatalf("invitation = %+v", got)
	}
}

func TestEventDispatcherLogsRawMeetingInvitationWithObjectUserID(t *testing.T) {
	var logs bytes.Buffer
	oldDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(oldDefault) })

	handler := &rawMeetingHandler{}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	payload := []byte(`{
		"schema": "2.0",
		"header": {
			"event_id": "evt-meeting-invited",
			"event_type": "vc.bot.meeting_invited_v1",
			"app_id": "cli_app",
			"tenant_key": "tenant",
			"create_time": "1700000000000"
		},
		"event": {
			"meeting": {"id": "meeting-1", "meeting_no": "123456789", "topic": "周会"},
			"bot": {"id": {"open_id": "ou_bot"}, "user_type": 1, "user_name": "助手"},
			"inviter": {"id": {"open_id": "ou_user"}, "user_type": 1, "user_name": "用户"},
			"invite_time": "1700000000",
			"call_id": "call-original"
		}
	}`)

	if _, err := adapter.newEventDispatcher().Do(context.Background(), payload); err != nil {
		t.Fatalf("dispatcher.Do() error = %v", err)
	}
	if handler.invited != 1 {
		t.Fatalf("meeting handler called %d times; raw bridge should call handler once", handler.invited)
	}
	logText := logs.String()
	for _, want := range []string{"收到飞书会议邀请原始事件", "vc.bot.meeting_invited_v1", "call-original", "open_id"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("logs = %s, want %q", logText, want)
		}
	}
}

func TestParseMeetingJoinResponseAcceptsStringJoinUserID(t *testing.T) {
	raw := []byte(`{
		"code": 0,
		"msg": "success",
		"data": {
			"meeting": {
				"id": "meeting-1",
				"meeting_no": "123456789",
				"topic": "周会",
				"start_time": "1700000000"
			},
			"join_user": {
				"id": "ou_bot",
				"user_type": 1
			}
		}
	}`)
	got, code, msg, err := parseMeetingJoinResponse(raw)
	if err != nil {
		t.Fatalf("parseMeetingJoinResponse() error = %v", err)
	}
	if code != 0 || msg != "success" {
		t.Fatalf("code=%d msg=%q", code, msg)
	}
	if got.Meeting.ID != "meeting-1" || got.Meeting.MeetingNo != "123456789" || got.Meeting.Topic != "周会" || got.BotUser.ID != "ou_bot" || got.BotUser.UserType != 1 {
		t.Fatalf("result = %+v", got)
	}
}

func TestParseMeetingJoinResponseAcceptsObjectJoinUserID(t *testing.T) {
	raw := []byte(`{
		"code": 0,
		"msg": "success",
		"data": {
			"meeting": {
				"id": "meeting-1",
				"meeting_no": "123456789",
				"topic": "周会",
				"start_time": "1700000000"
			},
			"join_user": {
				"id": {"open_id": "ou_bot", "user_id": "user_bot"},
				"user_type": 1
			}
		}
	}`)
	got, code, msg, err := parseMeetingJoinResponse(raw)
	if err != nil {
		t.Fatalf("parseMeetingJoinResponse() error = %v", err)
	}
	if code != 0 || msg != "success" {
		t.Fatalf("code=%d msg=%q", code, msg)
	}
	if got.BotUser.ID != "ou_bot" || got.BotUser.UserType != 1 {
		t.Fatalf("bot user = %+v", got.BotUser)
	}
}

func TestParseMeetingJoinResponseAcceptsNumberJoinUserID(t *testing.T) {
	raw := []byte(`{
		"code": 0,
		"msg": "success",
		"data": {
			"meeting": {
				"id": "meeting-1",
				"meeting_no": "123456789",
				"topic": "周会"
			},
			"join_user": {
				"id": 123456789,
				"user_type": 10
			}
		}
	}`)
	got, code, msg, err := parseMeetingJoinResponse(raw)
	if err != nil {
		t.Fatalf("parseMeetingJoinResponse() error = %v", err)
	}
	if code != 0 || msg != "success" {
		t.Fatalf("code=%d msg=%q", code, msg)
	}
	if got.BotUser.ID != "123456789" || got.BotUser.UserType != 10 {
		t.Fatalf("bot user = %+v", got.BotUser)
	}
}

func TestEventDispatcherHandlesRawMeetingActivityWithObjectUserID(t *testing.T) {
	handler := &rawMeetingHandler{}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	payload := []byte(`{
		"schema": "2.0",
		"header": {
			"event_id": "evt-meeting-activity",
			"event_type": "vc.bot.meeting_activity_v1",
			"app_id": "cli_app",
			"tenant_key": "tenant",
			"create_time": "1700000000000"
		},
		"event": {
			"meeting_activity_items": [{
				"meeting": {
					"id": "meeting-1",
					"meeting_no": "123456789",
					"topic": "周会",
					"host_user": {"id": {"open_id": "ou_host"}, "user_type": 1, "user_role": 2, "user_name": "主持人"}
				},
				"transcript_received_items": [{
					"speaker": {"id": {"open_id": "ou_speaker"}, "user_type": 1, "user_name": "发言人"},
					"sentence_id": "sentence-1",
					"text": "确认上线",
					"language": "zh",
					"start_time_ms": "1712345678000",
					"end_time_ms": "1712345682000"
				}]
			}]
		}
	}`)

	if _, err := adapter.newEventDispatcher().Do(context.Background(), payload); err != nil {
		t.Fatalf("dispatcher.Do() error = %v", err)
	}
	if len(handler.activities) != 1 || len(handler.activities[0].Items) != 1 {
		t.Fatalf("activities = %+v", handler.activities)
	}
	got := handler.activities[0].Items[0]
	if got.Meeting.Host.ID != "ou_host" || got.Actor.ID != "ou_speaker" || got.Text != "确认上线" {
		t.Fatalf("activity = %+v", got)
	}
}

func TestEventDispatcherHandlesRawMeetingEndedWithObjectHostID(t *testing.T) {
	handler := &rawMeetingHandler{}
	adapter := NewAdapter(config.BotConfig{ID: "bot-a"}, handler)
	payload := []byte(`{
		"schema": "2.0",
		"header": {
			"event_id": "evt-meeting-ended",
			"event_type": "vc.bot.meeting_ended_v1",
			"app_id": "cli_app",
			"tenant_key": "tenant",
			"create_time": "1700000000000"
		},
		"event": {
			"meeting": {
				"id": "meeting-1",
				"meeting_no": "123456789",
				"topic": "周会",
				"end_time": "1712349278000",
				"host_user": {"id": {"open_id": "ou_host"}, "user_type": 1, "user_role": 2, "user_name": "主持人"}
			}
		}
	}`)

	if _, err := adapter.newEventDispatcher().Do(context.Background(), payload); err != nil {
		t.Fatalf("dispatcher.Do() error = %v", err)
	}
	if len(handler.ended) != 1 {
		t.Fatalf("ended = %+v", handler.ended)
	}
	got := handler.ended[0].Meeting
	if got.ID != "meeting-1" || got.Host.ID != "ou_host" || got.EndTime != "1712349278000" {
		t.Fatalf("meeting ended = %+v", got)
	}
}

func TestParseMeetingActivitiesExtractsTranscriptAndChatIDs(t *testing.T) {
	meeting := larkvc.NewMeetingAgentEventMeetingBuilder().Id("meeting-1").Build()
	user := larkvc.NewMeetingAgentEventUserBuilder().Id("ou_user").UserName("用户").Build()
	event := &larkvc.P2BotMeetingActivityV1{Event: &larkvc.P2BotMeetingActivityV1Data{MeetingActivityItems: []*larkvc.MeetingActivityItem{
		larkvc.NewMeetingActivityItemBuilder().Meeting(meeting).TranscriptReceivedItems([]*larkvc.TranscriptItem{
			larkvc.NewTranscriptItemBuilder().Speaker(user).SentenceId("sentence-1").Text("确认上线").StartTimeMs("1712345678000").Build(),
		}).Build(),
		larkvc.NewMeetingActivityItemBuilder().Meeting(meeting).ChatReceivedItems([]*larkvc.ChatMessageItem{
			larkvc.NewChatMessageItemBuilder().Operator(user).MessageId("message-1").Content("文档链接").SendTime("1712345679000").Build(),
		}).Build(),
	}}}
	got, err := ParseMeetingActivities(event)
	if err != nil {
		t.Fatalf("ParseMeetingActivities() error = %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %+v, want transcript and chat", got.Items)
	}
	if got.Items[0].Type != MeetingActivityTranscript || got.Items[0].ID != "sentence-1" || got.Items[0].Text != "确认上线" {
		t.Fatalf("transcript = %+v", got.Items[0])
	}
	if got.Items[1].Type != MeetingActivityChat || got.Items[1].ID != "message-1" || got.Items[1].Text != "文档链接" {
		t.Fatalf("chat = %+v", got.Items[1])
	}
}

func TestMeetingCardJSONShowsFinalMinutes(t *testing.T) {
	card := newMeetingCardJSON(MeetingCardView{
		Topic: "发布会", MeetingNo: "123456789", Status: "completed", Summary: []string{"确认上线"},
		Todos: []MeetingCardTodo{{Content: "准备发布", Assignee: "小王", DueAt: "周五"}}, UpdatedAt: "15:24",
	})
	for _, want := range []string{"发布会", "会议已结束，可转发", "确认上线", "准备发布", "小王", "周五", "最近更新：15:24"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card JSON = %s, want %q", card, want)
		}
	}
	if strings.Contains(card, `"tag":"note"`) {
		t.Fatalf("card JSON = %s, should not contain unsupported note tag", card)
	}
}

type rawMeetingHandler struct {
	invited    int
	activities []MeetingActivities
	ended      []MeetingEnded
}

func (h *rawMeetingHandler) HandleFeishuMessage(context.Context, Message) (string, error) {
	return "", nil
}

func (h *rawMeetingHandler) HandleMeetingInvited(context.Context, MeetingInvitation, Outbound) error {
	h.invited++
	return nil
}

func (h *rawMeetingHandler) HandleMeetingActivities(_ context.Context, activities MeetingActivities, _ Outbound) error {
	h.activities = append(h.activities, activities)
	return nil
}

func (h *rawMeetingHandler) HandleMeetingEnded(_ context.Context, ended MeetingEnded, _ Outbound) error {
	h.ended = append(h.ended, ended)
	return nil
}

func stringPointer(value string) *string { return &value }
