package feishu

import (
	"strings"
	"testing"

	larkvc "github.com/larksuite/oapi-sdk-go/v3/service/vc/v1"
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
		Todos: []MeetingCardTodo{{Content: "准备发布", Assignee: "小王", DueAt: "周五"}},
	})
	for _, want := range []string{"发布会", "会议已结束，可转发", "确认上线", "准备发布", "小王", "周五"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card JSON = %s, want %q", card, want)
		}
	}
}

func stringPointer(value string) *string { return &value }
