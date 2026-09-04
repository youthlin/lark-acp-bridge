package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkvc "github.com/larksuite/oapi-sdk-go/v3/service/vc/v1"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

const (
	MeetingActivityParticipantJoined = "participant_joined"
	MeetingActivityParticipantLeft   = "participant_left"
	MeetingActivityTranscript        = "transcript_received"
	MeetingActivityChat              = "chat_received"
	MeetingActivityShareStarted      = "magic_share_started"
	MeetingActivityShareEnded        = "magic_share_ended"
	MeetingActivityDocumentChanged   = "document_context_changed"
)

type MeetingUser struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	UserType int    `json:"user_type,omitempty"`
	UserRole int    `json:"user_role,omitempty"`
}

type MeetingInfo struct {
	ID        string      `json:"id,omitempty"`
	MeetingNo string      `json:"meeting_no,omitempty"`
	Topic     string      `json:"topic,omitempty"`
	StartTime string      `json:"start_time,omitempty"`
	EndTime   string      `json:"end_time,omitempty"`
	Host      MeetingUser `json:"host,omitempty"`
}

type MeetingInvitation struct {
	BotID      string
	Workspace  string
	Meeting    MeetingInfo
	Inviter    MeetingUser
	InviteTime string
	CallID     string
}

type MeetingDocument struct {
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
}

type MeetingActivity struct {
	Meeting     MeetingInfo
	Type        string
	Actor       MeetingUser
	ID          string
	Time        string
	Text        string
	Language    string
	EndTime     string
	ShareID     string
	Document    MeetingDocument
	LeaveReason int
}

type MeetingActivities struct {
	BotID     string
	Workspace string
	Items     []MeetingActivity
}

type MeetingEnded struct {
	BotID     string
	Workspace string
	Meeting   MeetingInfo
}

type MeetingJoinRequest struct {
	MeetingNo string
	CallID    string
}

type MeetingJoinResult struct {
	Meeting MeetingInfo
	BotUser MeetingUser
}

type larkMeetingClient struct {
	client *lark.Client
}

func (c larkMeetingClient) Join(ctx context.Context, request MeetingJoinRequest) (MeetingJoinResult, error) {
	if c.client == nil {
		return MeetingJoinResult{}, fmt.Errorf("飞书客户端未初始化")
	}
	request.MeetingNo = strings.TrimSpace(request.MeetingNo)
	request.CallID = strings.TrimSpace(request.CallID)
	if request.MeetingNo == "" || request.CallID == "" {
		return MeetingJoinResult{}, fmt.Errorf("会议号和 call_id 不能为空")
	}
	body := larkvc.NewJoinBotReqBodyBuilder().
		JoinType(1).
		JoinIdentify(larkvc.NewJoinIdentifyBuilder().MeetingNo(request.MeetingNo).Build()).
		CallId(request.CallID).
		Build()
	resp, err := c.client.Vc.V1.Bot.Join(ctx, larkvc.NewJoinBotReqBuilder().Body(body).Build())
	if err != nil {
		return MeetingJoinResult{}, fmt.Errorf("调用飞书机器人入会接口: %w", err)
	}
	if resp == nil || !resp.Success() {
		code, msg := 0, ""
		if resp != nil {
			code, msg = resp.Code, resp.Msg
		}
		return MeetingJoinResult{}, fmt.Errorf("飞书机器人入会接口返回错误: code=%d msg=%s", code, msg)
	}
	var result MeetingJoinResult
	if resp.Data != nil {
		if raw := resp.Data.Meeting; raw != nil {
			result.Meeting = MeetingInfo{ID: value(raw.Id), MeetingNo: value(raw.MeetingNo), Topic: value(raw.Topic), StartTime: value(raw.StartTime)}
		}
		if raw := resp.Data.JoinUser; raw != nil {
			result.BotUser.ID = value(raw.Id)
			result.BotUser.UserType = valueInt(raw.UserType)
		}
	}
	return result, nil
}

func (c larkMeetingClient) ListActivities(ctx context.Context, meetingID string, since time.Time) ([]MeetingActivity, error) {
	if c.client == nil {
		return nil, fmt.Errorf("飞书客户端未初始化")
	}
	meetingID = strings.TrimSpace(meetingID)
	if meetingID == "" {
		return nil, fmt.Errorf("meeting_id 不能为空")
	}
	pageToken := ""
	var out []MeetingActivity
	for {
		builder := larkvc.NewEventsBotReqBuilder().MeetingId(meetingID).PageSize(100).UserIdType("open_id")
		if !since.IsZero() {
			builder.StartTime(strconv.FormatInt(since.Unix(), 10))
		}
		if pageToken != "" {
			builder.PageToken(pageToken)
		}
		resp, err := c.client.Vc.V1.Bot.Events(ctx, builder.Build())
		if err != nil {
			return nil, fmt.Errorf("调用飞书获取会议事件接口: %w", err)
		}
		if resp == nil || !resp.Success() {
			code, msg := 0, ""
			if resp != nil {
				code, msg = resp.Code, resp.Msg
			}
			return nil, fmt.Errorf("飞书获取会议事件接口返回错误: code=%d msg=%s", code, msg)
		}
		if resp.Data == nil {
			return out, nil
		}
		for _, event := range resp.Data.Events {
			if event == nil || event.Payload == nil {
				continue
			}
			items := parseMeetingActivityItem(event.Payload)
			for i := range items {
				if items[i].Meeting.ID == "" {
					items[i].Meeting.ID = meetingID
				}
				if items[i].Time == "" {
					items[i].Time = value(event.EventTime)
				}
			}
			out = append(out, items...)
		}
		if !valueBool(resp.Data.HasMore) {
			return out, nil
		}
		pageToken = value(resp.Data.PageToken)
		if pageToken == "" {
			return out, nil
		}
	}
}

func (a *Adapter) JoinMeeting(ctx context.Context, request MeetingJoinRequest) (MeetingJoinResult, error) {
	if a == nil || a.meetings == nil {
		return MeetingJoinResult{}, fmt.Errorf("飞书会议 client 未初始化")
	}
	return a.meetings.Join(ctx, request)
}

func (a *Adapter) ListMeetingActivities(ctx context.Context, meetingID string, since time.Time) ([]MeetingActivity, error) {
	if a == nil || a.meetings == nil {
		return nil, fmt.Errorf("飞书会议 client 未初始化")
	}
	return a.meetings.ListActivities(ctx, meetingID, since)
}

func ParseMeetingInvitation(event *larkvc.P2BotMeetingInvitedV1) (MeetingInvitation, error) {
	if event == nil || event.Event == nil {
		return MeetingInvitation{}, fmt.Errorf("飞书会议邀请事件为空")
	}
	raw := event.Event
	invitation := MeetingInvitation{
		Meeting:    meetingInfo(raw.Meeting),
		Inviter:    meetingUser(raw.Inviter),
		InviteTime: value(raw.InviteTime),
		CallID:     value(raw.CallId),
	}
	if invitation.Meeting.MeetingNo == "" || invitation.CallID == "" {
		return MeetingInvitation{}, fmt.Errorf("飞书会议邀请缺少 meeting_no 或 call_id")
	}
	return invitation, nil
}

func ParseMeetingActivities(event *larkvc.P2BotMeetingActivityV1) (MeetingActivities, error) {
	if event == nil || event.Event == nil {
		return MeetingActivities{}, fmt.Errorf("飞书会议活动事件为空")
	}
	var out MeetingActivities
	for _, raw := range event.Event.MeetingActivityItems {
		out.Items = append(out.Items, parseMeetingActivityItem(raw)...)
	}
	return out, nil
}

func parseMeetingActivityItem(raw *larkvc.MeetingActivityItem) []MeetingActivity {
	if raw == nil {
		return nil
	}
	meeting := meetingInfo(raw.Meeting)
	var items []MeetingActivity
	for _, item := range raw.ParticipantJoinedItems {
		if item != nil {
			items = append(items, MeetingActivity{Meeting: meeting, Type: MeetingActivityParticipantJoined, Actor: meetingUser(item.Participant), Time: value(item.JoinTime)})
		}
	}
	for _, item := range raw.ParticipantLeftItems {
		if item != nil {
			items = append(items, MeetingActivity{Meeting: meeting, Type: MeetingActivityParticipantLeft, Actor: meetingUser(item.Participant), Time: value(item.LeaveTime), LeaveReason: valueInt(item.LeaveReason)})
		}
	}
	for _, item := range raw.TranscriptReceivedItems {
		if item != nil {
			items = append(items, MeetingActivity{Meeting: meeting, Type: MeetingActivityTranscript, Actor: meetingUser(item.Speaker), ID: value(item.SentenceId), Time: value(item.StartTimeMs), EndTime: value(item.EndTimeMs), Text: value(item.Text), Language: value(item.Language)})
		}
	}
	for _, item := range raw.ChatReceivedItems {
		if item != nil {
			items = append(items, MeetingActivity{Meeting: meeting, Type: MeetingActivityChat, Actor: meetingUser(item.Operator), ID: value(item.MessageId), Time: value(item.SendTime), Text: value(item.Content)})
		}
	}
	for _, item := range raw.MagicShareStartedItems {
		if item != nil {
			items = append(items, MeetingActivity{Meeting: meeting, Type: MeetingActivityShareStarted, Actor: meetingUser(item.Operator), ShareID: value(item.ShareId), Time: value(item.Time), Document: meetingDocument(item.ShareDoc)})
		}
	}
	for _, item := range raw.MagicShareEndedItems {
		if item != nil {
			items = append(items, MeetingActivity{Meeting: meeting, Type: MeetingActivityShareEnded, Actor: meetingUser(item.Operator), ShareID: value(item.ShareId), Time: value(item.Time)})
		}
	}
	for _, item := range raw.DocumentContextChangedItems {
		if item != nil {
			items = append(items, MeetingActivity{Meeting: meeting, Type: MeetingActivityDocumentChanged, Actor: meetingUser(item.Operator), ShareID: value(item.ShareId), Time: value(item.Time), Document: meetingDocument(item.ShareDoc)})
		}
	}
	return items
}

func ParseMeetingEnded(event *larkvc.P2BotMeetingEndedV1) (MeetingEnded, error) {
	if event == nil || event.Event == nil || event.Event.Meeting == nil {
		return MeetingEnded{}, fmt.Errorf("飞书会议结束事件为空")
	}
	meeting := meetingInfo(event.Event.Meeting)
	if meeting.ID == "" {
		return MeetingEnded{}, fmt.Errorf("飞书会议结束事件缺少 meeting_id")
	}
	return MeetingEnded{Meeting: meeting}, nil
}

func (a *Adapter) handleMeetingInvited(ctx context.Context, event *larkvc.P2BotMeetingInvitedV1) (err error) {
	defer recoverEventHandler(ctx, "meeting_invited", &err)
	invitation, err := ParseMeetingInvitation(event)
	if err != nil {
		return logMeetingParseError(ctx, "会议邀请", event, err)
	}
	invitation.BotID, invitation.Workspace = a.cfg.ID, a.cfg.Workspace
	ctx = meetingLogContext(ctx, invitation.BotID, invitation.Meeting)
	if handler, ok := a.handler.(MeetingHandler); ok {
		if err := handler.HandleMeetingInvited(ctx, invitation, a); err != nil {
			slog.ErrorContext(ctx, "处理飞书会议邀请失败", "错误", err)
		}
	}
	return nil
}

func (a *Adapter) handleMeetingActivity(ctx context.Context, event *larkvc.P2BotMeetingActivityV1) (err error) {
	defer recoverEventHandler(ctx, "meeting_activity", &err)
	activities, err := ParseMeetingActivities(event)
	if err != nil {
		return logMeetingParseError(ctx, "会议活动", event, err)
	}
	activities.BotID, activities.Workspace = a.cfg.ID, a.cfg.Workspace
	if len(activities.Items) == 0 {
		return nil
	}
	ctx = meetingLogContext(ctx, activities.BotID, activities.Items[0].Meeting)
	if handler, ok := a.handler.(MeetingHandler); ok {
		if err := handler.HandleMeetingActivities(ctx, activities, a); err != nil {
			slog.ErrorContext(ctx, "处理飞书会议活动失败", "错误", err)
		}
	}
	return nil
}

func (a *Adapter) handleMeetingEnded(ctx context.Context, event *larkvc.P2BotMeetingEndedV1) (err error) {
	defer recoverEventHandler(ctx, "meeting_ended", &err)
	ended, err := ParseMeetingEnded(event)
	if err != nil {
		return logMeetingParseError(ctx, "会议结束", event, err)
	}
	ended.BotID, ended.Workspace = a.cfg.ID, a.cfg.Workspace
	ctx = meetingLogContext(ctx, ended.BotID, ended.Meeting)
	if handler, ok := a.handler.(MeetingHandler); ok {
		if err := handler.HandleMeetingEnded(ctx, ended, a); err != nil {
			slog.ErrorContext(ctx, "处理飞书会议结束失败", "错误", err)
		}
	}
	return nil
}

func meetingInfo(raw *larkvc.MeetingAgentEventMeeting) MeetingInfo {
	if raw == nil {
		return MeetingInfo{}
	}
	return MeetingInfo{ID: value(raw.Id), MeetingNo: value(raw.MeetingNo), Topic: value(raw.Topic), StartTime: value(raw.StartTime), EndTime: value(raw.EndTime), Host: meetingUser(raw.HostUser)}
}

func meetingUser(raw *larkvc.MeetingAgentEventUser) MeetingUser {
	if raw == nil {
		return MeetingUser{}
	}
	return MeetingUser{ID: value(raw.Id), Name: value(raw.UserName), UserType: valueInt(raw.UserType), UserRole: valueInt(raw.UserRole)}
}

func meetingDocument(raw *larkvc.ShareDoc) MeetingDocument {
	if raw == nil {
		return MeetingDocument{}
	}
	return MeetingDocument{Title: value(raw.Title), URL: value(raw.Url)}
}

func meetingLogContext(ctx context.Context, botID string, meeting MeetingInfo) context.Context {
	return logging.CtxAddAttr(ctx, slog.String("bot", strings.TrimSpace(botID)), slog.String("source", "meeting"), slog.String("meeting_id", strings.TrimSpace(meeting.ID)), slog.String("meeting_no", strings.TrimSpace(meeting.MeetingNo)))
}

func logMeetingParseError(ctx context.Context, label string, event any, err error) error {
	slog.WarnContext(ctx, "解析飞书"+label+"事件失败", "错误", err)
	slog.DebugContext(ctx, "解析飞书"+label+"事件失败详情", "错误", err, "事件", larkcore.Prettify(event))
	return nil
}
