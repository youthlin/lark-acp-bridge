package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkvc "github.com/larksuite/oapi-sdk-go/v3/service/vc/v1"
	"github.com/youthlin/lark-acp-bridge/internal/arg"
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

const larkJoinBotPath = "/open-apis/vc/v1/bots/join"
const larkBotEventsPath = "/open-apis/vc/v1/bots/events"

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
	resp, err := c.client.Post(ctx, larkJoinBotPath, body, larkcore.AccessTokenTypeTenant)
	if err != nil {
		return MeetingJoinResult{}, fmt.Errorf("调用飞书机器人入会接口: %w", err)
	}
	if resp == nil {
		return MeetingJoinResult{}, fmt.Errorf("飞书机器人入会接口返回为空")
	}
	if resp.StatusCode != http.StatusOK {
		return MeetingJoinResult{}, fmt.Errorf("飞书机器人入会接口 HTTP 状态异常: %d", resp.StatusCode)
	}
	result, code, msg, err := parseMeetingJoinResponse(resp.RawBody)
	if err != nil {
		return MeetingJoinResult{}, fmt.Errorf("解析飞书机器人入会接口响应: %w", err)
	}
	if code != 0 {
		return MeetingJoinResult{}, fmt.Errorf("飞书机器人入会接口返回错误: code=%d msg=%s", code, msg)
	}
	return result, nil
}

func parseMeetingJoinResponse(raw []byte) (MeetingJoinResult, int, string, error) {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Meeting  *rawMeetingAgentEventMeeting `json:"meeting"`
			JoinUser *rawMeetingAgentEventUser    `json:"join_user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return MeetingJoinResult{}, 0, "", err
	}
	var result MeetingJoinResult
	if resp.Data.Meeting != nil {
		result.Meeting = meetingInfo(resp.Data.Meeting.toSDK())
	}
	if resp.Data.JoinUser != nil {
		result.BotUser = meetingUser(resp.Data.JoinUser.toSDK())
	}
	return result, resp.Code, resp.Msg, nil
}

type meetingFlexibleID struct {
	value string
}

func (id *meetingFlexibleID) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		id.value = strings.TrimSpace(value)
		return nil
	}
	if raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9') {
		var value json.Number
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		id.value = strings.TrimSpace(value.String())
		return nil
	}
	var value struct {
		ID           meetingFlexibleID `json:"id,omitempty"`
		OpenID       meetingFlexibleID `json:"open_id,omitempty"`
		OpenIDCamel  meetingFlexibleID `json:"openId,omitempty"`
		UserID       meetingFlexibleID `json:"user_id,omitempty"`
		UserIDCamel  meetingFlexibleID `json:"userId,omitempty"`
		UnionID      meetingFlexibleID `json:"union_id,omitempty"`
		UnionIDCamel meetingFlexibleID `json:"unionId,omitempty"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	id.value = strings.TrimSpace(firstNonEmpty(
		value.OpenID.String(),
		value.OpenIDCamel.String(),
		value.UserID.String(),
		value.UserIDCamel.String(),
		value.UnionID.String(),
		value.UnionIDCamel.String(),
		value.ID.String(),
	))
	return nil
}

func (id meetingFlexibleID) String() string {
	return strings.TrimSpace(id.value)
}

type rawMeetingAgentEventMeeting struct {
	ID        meetingFlexibleID         `json:"id,omitempty"`
	Topic     *string                   `json:"topic,omitempty"`
	MeetingNo *string                   `json:"meeting_no,omitempty"`
	StartTime *string                   `json:"start_time,omitempty"`
	EndTime   *string                   `json:"end_time,omitempty"`
	HostUser  *rawMeetingAgentEventUser `json:"host_user,omitempty"`
}

func (m *rawMeetingAgentEventMeeting) toSDK() *larkvc.MeetingAgentEventMeeting {
	if m == nil {
		return nil
	}
	out := &larkvc.MeetingAgentEventMeeting{
		Topic:     m.Topic,
		MeetingNo: m.MeetingNo,
		StartTime: m.StartTime,
		EndTime:   m.EndTime,
		HostUser:  m.HostUser.toSDK(),
	}
	if id := m.ID.String(); id != "" {
		out.Id = &id
	}
	return out
}

type rawMeetingAgentEventUser struct {
	ID       meetingFlexibleID `json:"id,omitempty"`
	UserType *int              `json:"user_type,omitempty"`
	UserRole *int              `json:"user_role,omitempty"`
	UserName *string           `json:"user_name,omitempty"`
}

func (u *rawMeetingAgentEventUser) toSDK() *larkvc.MeetingAgentEventUser {
	if u == nil {
		return nil
	}
	out := &larkvc.MeetingAgentEventUser{
		UserType: u.UserType,
		UserRole: u.UserRole,
		UserName: u.UserName,
	}
	if id := u.ID.String(); id != "" {
		out.Id = &id
	}
	return out
}

type rawMeetingInvitedEventData struct {
	Meeting    *rawMeetingAgentEventMeeting `json:"meeting,omitempty"`
	Bot        *rawMeetingAgentEventUser    `json:"bot,omitempty"`
	Inviter    *rawMeetingAgentEventUser    `json:"inviter,omitempty"`
	InviteTime *string                      `json:"invite_time,omitempty"`
	CallId     *string                      `json:"call_id,omitempty"`
}

func (data *rawMeetingInvitedEventData) toSDK() *larkvc.P2BotMeetingInvitedV1Data {
	if data == nil {
		return nil
	}
	return &larkvc.P2BotMeetingInvitedV1Data{
		Meeting:    data.Meeting.toSDK(),
		Bot:        data.Bot.toSDK(),
		Inviter:    data.Inviter.toSDK(),
		InviteTime: data.InviteTime,
		CallId:     data.CallId,
	}
}

type rawMeetingActivityEventData struct {
	MeetingActivityItems []*rawMeetingActivityItem `json:"meeting_activity_items,omitempty"`
}

func (data *rawMeetingActivityEventData) toSDK() *larkvc.P2BotMeetingActivityV1Data {
	if data == nil {
		return nil
	}
	items := make([]*larkvc.MeetingActivityItem, 0, len(data.MeetingActivityItems))
	for _, item := range data.MeetingActivityItems {
		if converted := item.toSDK(); converted != nil {
			items = append(items, converted)
		}
	}
	return &larkvc.P2BotMeetingActivityV1Data{MeetingActivityItems: items}
}

type rawMeetingEndedEventData struct {
	Meeting *rawMeetingAgentEventMeeting `json:"meeting,omitempty"`
}

func (data *rawMeetingEndedEventData) toSDK() *larkvc.P2BotMeetingEndedV1Data {
	if data == nil {
		return nil
	}
	return &larkvc.P2BotMeetingEndedV1Data{Meeting: data.Meeting.toSDK()}
}

type rawMeetingActivityItem struct {
	Meeting                     *rawMeetingAgentEventMeeting     `json:"meeting,omitempty"`
	ActivityEventType           *string                          `json:"activity_event_type,omitempty"`
	ParticipantJoinedItems      []*rawParticipantJoinedItem      `json:"participant_joined_items,omitempty"`
	ParticipantLeftItems        []*rawParticipantLeftItem        `json:"participant_left_items,omitempty"`
	TranscriptReceivedItems     []*rawTranscriptItem             `json:"transcript_received_items,omitempty"`
	ChatReceivedItems           []*rawChatMessageItem            `json:"chat_received_items,omitempty"`
	MagicShareStartedItems      []*rawMagicShareStartedItem      `json:"magic_share_started_items,omitempty"`
	MagicShareEndedItems        []*rawMagicShareEndedItem        `json:"magic_share_ended_items,omitempty"`
	DocumentContextChangedItems []*rawDocumentContextChangedItem `json:"document_context_changed_items,omitempty"`
}

func (item *rawMeetingActivityItem) toSDK() *larkvc.MeetingActivityItem {
	if item == nil {
		return nil
	}
	return &larkvc.MeetingActivityItem{
		Meeting:                     item.Meeting.toSDK(),
		ActivityEventType:           item.ActivityEventType,
		ParticipantJoinedItems:      rawParticipantJoinedItemsToSDK(item.ParticipantJoinedItems),
		ParticipantLeftItems:        rawParticipantLeftItemsToSDK(item.ParticipantLeftItems),
		TranscriptReceivedItems:     rawTranscriptItemsToSDK(item.TranscriptReceivedItems),
		ChatReceivedItems:           rawChatMessageItemsToSDK(item.ChatReceivedItems),
		MagicShareStartedItems:      rawMagicShareStartedItemsToSDK(item.MagicShareStartedItems),
		MagicShareEndedItems:        rawMagicShareEndedItemsToSDK(item.MagicShareEndedItems),
		DocumentContextChangedItems: rawDocumentContextChangedItemsToSDK(item.DocumentContextChangedItems),
	}
}

type rawParticipantJoinedItem struct {
	Participant *rawMeetingAgentEventUser `json:"participant,omitempty"`
	JoinTime    *string                   `json:"join_time,omitempty"`
}

func (item *rawParticipantJoinedItem) toSDK() *larkvc.ParticipantJoinedItem {
	if item == nil {
		return nil
	}
	return &larkvc.ParticipantJoinedItem{
		Participant: item.Participant.toSDK(),
		JoinTime:    item.JoinTime,
	}
}

func rawParticipantJoinedItemsToSDK(items []*rawParticipantJoinedItem) []*larkvc.ParticipantJoinedItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]*larkvc.ParticipantJoinedItem, 0, len(items))
	for _, item := range items {
		if converted := item.toSDK(); converted != nil {
			out = append(out, converted)
		}
	}
	return out
}

type rawParticipantLeftItem struct {
	Participant *rawMeetingAgentEventUser `json:"participant,omitempty"`
	LeaveReason *int                      `json:"leave_reason,omitempty"`
	LeaveTime   *string                   `json:"leave_time,omitempty"`
}

func (item *rawParticipantLeftItem) toSDK() *larkvc.ParticipantLeftItem {
	if item == nil {
		return nil
	}
	return &larkvc.ParticipantLeftItem{
		Participant: item.Participant.toSDK(),
		LeaveReason: item.LeaveReason,
		LeaveTime:   item.LeaveTime,
	}
}

func rawParticipantLeftItemsToSDK(items []*rawParticipantLeftItem) []*larkvc.ParticipantLeftItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]*larkvc.ParticipantLeftItem, 0, len(items))
	for _, item := range items {
		if converted := item.toSDK(); converted != nil {
			out = append(out, converted)
		}
	}
	return out
}

type rawTranscriptItem struct {
	Speaker     *rawMeetingAgentEventUser `json:"speaker,omitempty"`
	Text        *string                   `json:"text,omitempty"`
	Language    *string                   `json:"language,omitempty"`
	StartTimeMs *string                   `json:"start_time_ms,omitempty"`
	EndTimeMs   *string                   `json:"end_time_ms,omitempty"`
	SentenceID  *string                   `json:"sentence_id,omitempty"`
}

func (item *rawTranscriptItem) toSDK() *larkvc.TranscriptItem {
	if item == nil {
		return nil
	}
	return &larkvc.TranscriptItem{
		Speaker:     item.Speaker.toSDK(),
		Text:        item.Text,
		Language:    item.Language,
		StartTimeMs: item.StartTimeMs,
		EndTimeMs:   item.EndTimeMs,
		SentenceId:  item.SentenceID,
	}
}

func rawTranscriptItemsToSDK(items []*rawTranscriptItem) []*larkvc.TranscriptItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]*larkvc.TranscriptItem, 0, len(items))
	for _, item := range items {
		if converted := item.toSDK(); converted != nil {
			out = append(out, converted)
		}
	}
	return out
}

type rawChatMessageItem struct {
	Operator    *rawMeetingAgentEventUser `json:"operator,omitempty"`
	MessageID   *string                   `json:"message_id,omitempty"`
	MessageType *int                      `json:"message_type,omitempty"`
	Content     *string                   `json:"content,omitempty"`
	SendTime    *string                   `json:"send_time,omitempty"`
}

func (item *rawChatMessageItem) toSDK() *larkvc.ChatMessageItem {
	if item == nil {
		return nil
	}
	return &larkvc.ChatMessageItem{
		Operator:    item.Operator.toSDK(),
		MessageId:   item.MessageID,
		MessageType: item.MessageType,
		Content:     item.Content,
		SendTime:    item.SendTime,
	}
}

func rawChatMessageItemsToSDK(items []*rawChatMessageItem) []*larkvc.ChatMessageItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]*larkvc.ChatMessageItem, 0, len(items))
	for _, item := range items {
		if converted := item.toSDK(); converted != nil {
			out = append(out, converted)
		}
	}
	return out
}

type rawMagicShareStartedItem struct {
	Operator *rawMeetingAgentEventUser `json:"operator,omitempty"`
	ShareID  *string                   `json:"share_id,omitempty"`
	ShareDoc *rawShareDoc              `json:"share_doc,omitempty"`
	Time     *string                   `json:"time,omitempty"`
}

func (item *rawMagicShareStartedItem) toSDK() *larkvc.MagicShareStartedItem {
	if item == nil {
		return nil
	}
	return &larkvc.MagicShareStartedItem{
		Operator: item.Operator.toSDK(),
		ShareId:  item.ShareID,
		ShareDoc: item.ShareDoc.toSDK(),
		Time:     item.Time,
	}
}

func rawMagicShareStartedItemsToSDK(items []*rawMagicShareStartedItem) []*larkvc.MagicShareStartedItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]*larkvc.MagicShareStartedItem, 0, len(items))
	for _, item := range items {
		if converted := item.toSDK(); converted != nil {
			out = append(out, converted)
		}
	}
	return out
}

type rawMagicShareEndedItem struct {
	Operator *rawMeetingAgentEventUser `json:"operator,omitempty"`
	ShareID  *string                   `json:"share_id,omitempty"`
	Time     *string                   `json:"time,omitempty"`
}

func (item *rawMagicShareEndedItem) toSDK() *larkvc.MagicShareEndedItem {
	if item == nil {
		return nil
	}
	return &larkvc.MagicShareEndedItem{
		Operator: item.Operator.toSDK(),
		ShareId:  item.ShareID,
		Time:     item.Time,
	}
}

func rawMagicShareEndedItemsToSDK(items []*rawMagicShareEndedItem) []*larkvc.MagicShareEndedItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]*larkvc.MagicShareEndedItem, 0, len(items))
	for _, item := range items {
		if converted := item.toSDK(); converted != nil {
			out = append(out, converted)
		}
	}
	return out
}

type rawDocumentContextChangedItem struct {
	Operator *rawMeetingAgentEventUser `json:"operator,omitempty"`
	ShareID  *string                   `json:"share_id,omitempty"`
	ShareDoc *rawShareDoc              `json:"share_doc,omitempty"`
	Time     *string                   `json:"time,omitempty"`
}

func (item *rawDocumentContextChangedItem) toSDK() *larkvc.DocumentContextChangedItem {
	if item == nil {
		return nil
	}
	return &larkvc.DocumentContextChangedItem{
		Operator: item.Operator.toSDK(),
		ShareId:  item.ShareID,
		ShareDoc: item.ShareDoc.toSDK(),
		Time:     item.Time,
	}
}

func rawDocumentContextChangedItemsToSDK(items []*rawDocumentContextChangedItem) []*larkvc.DocumentContextChangedItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]*larkvc.DocumentContextChangedItem, 0, len(items))
	for _, item := range items {
		if converted := item.toSDK(); converted != nil {
			out = append(out, converted)
		}
	}
	return out
}

type rawShareDoc struct {
	URL   *string `json:"url,omitempty"`
	Title *string `json:"title,omitempty"`
}

func (doc *rawShareDoc) toSDK() *larkvc.ShareDoc {
	if doc == nil {
		return nil
	}
	return &larkvc.ShareDoc{
		Url:   doc.URL,
		Title: doc.Title,
	}
}

type rawMeetingEventRecord struct {
	EventID   *string                   `json:"event_id,omitempty"`
	EventType *string                   `json:"event_type,omitempty"`
	EventTime *string                   `json:"event_time,omitempty"`
	Payload   rawMeetingActivityPayload `json:"payload,omitempty"`
}

type rawMeetingActivityPayload struct {
	item *rawMeetingActivityItem
}

func (p *rawMeetingActivityPayload) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if raw[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return err
		}
		encoded = strings.TrimSpace(encoded)
		if encoded == "" {
			return nil
		}
		raw = []byte(encoded)
	}
	var item rawMeetingActivityItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return err
	}
	p.item = &item
	return nil
}

func parseMeetingRawEventData[T any](event *larkevent.EventReq) (*T, error) {
	var body []byte
	if event != nil {
		body = event.Body
	}
	var payload struct {
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if len(payload.Event) == 0 {
		return nil, fmt.Errorf("缺少 event")
	}
	var data T
	if err := json.Unmarshal(payload.Event, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func parseMeetingEventsResponse(raw []byte, fallbackMeetingID string) ([]MeetingActivity, bool, string, int, string, error) {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data *struct {
			HasMore   bool                     `json:"has_more"`
			PageToken string                   `json:"page_token"`
			Events    []*rawMeetingEventRecord `json:"events"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, false, "", 0, "", err
	}
	if resp.Data == nil {
		return nil, false, "", resp.Code, resp.Msg, nil
	}
	var out []MeetingActivity
	for _, event := range resp.Data.Events {
		if event == nil || event.Payload.item == nil {
			continue
		}
		items := parseMeetingActivityItem(event.Payload.item.toSDK())
		for i := range items {
			if items[i].Meeting.ID == "" {
				items[i].Meeting.ID = fallbackMeetingID
			}
			if items[i].Time == "" {
				items[i].Time = value(event.EventTime)
			}
		}
		out = append(out, items...)
	}
	return out, resp.Data.HasMore, strings.TrimSpace(resp.Data.PageToken), resp.Code, resp.Msg, nil
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
		query := larkcore.QueryParams{}
		query.Set("meeting_id", meetingID)
		query.Set("page_size", "100")
		query.Set("user_id_type", "open_id")
		if !since.IsZero() {
			query.Set("start_time", strconv.FormatInt(since.Unix(), 10))
		}
		if pageToken != "" {
			query.Set("page_token", pageToken)
		}
		resp, err := c.client.Do(ctx, &larkcore.ApiReq{
			HttpMethod:                http.MethodGet,
			ApiPath:                   larkBotEventsPath,
			QueryParams:               query,
			SupportedAccessTokenTypes: []larkcore.AccessTokenType{larkcore.AccessTokenTypeTenant},
		})
		if err != nil {
			return nil, fmt.Errorf("调用飞书获取会议事件接口: %w", err)
		}
		if resp == nil {
			return nil, fmt.Errorf("飞书获取会议事件接口返回为空")
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("飞书获取会议事件接口 HTTP 状态异常: %d", resp.StatusCode)
		}
		items, hasMore, nextPageToken, code, msg, err := parseMeetingEventsResponse(resp.RawBody, meetingID)
		if err != nil {
			return nil, fmt.Errorf("解析飞书获取会议事件接口响应: %w", err)
		}
		if code != 0 {
			return nil, fmt.Errorf("飞书获取会议事件接口返回错误: code=%d msg=%s", code, msg)
		}
		out = append(out, items...)
		if !hasMore {
			return out, nil
		}
		pageToken = nextPageToken
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

func (a *Adapter) handleMeetingInvitedRaw(ctx context.Context, event *larkevent.EventReq) error {
	var body []byte
	if event != nil {
		body = event.Body
	}
	slog.InfoContext(ctx, "收到飞书会议邀请原始事件",
		"bot", a.cfg.ID,
		"event_type", "vc.bot.meeting_invited_v1",
		"body", eventLogBody(body, event),
	)
	data, err := parseMeetingRawEventData[rawMeetingInvitedEventData](event)
	if err != nil {
		return logMeetingParseError(ctx, "会议邀请", event, err)
	}
	event2 := &larkvc.P2BotMeetingInvitedV1{
		EventReq: event,
		Event:    data.toSDK(),
	}
	return a.handleMeetingInvited(ctx, event2)
}

func (a *Adapter) handleMeetingInvited(ctx context.Context, event *larkvc.P2BotMeetingInvitedV1) (err error) {
	slog.InfoContext(ctx, "收到入会邀请", "事件", arg.JSON(event.Event))
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

func (a *Adapter) handleMeetingActivityRaw(ctx context.Context, event *larkevent.EventReq) error {
	var body []byte
	if event != nil {
		body = event.Body
	}
	slog.DebugContext(ctx, "收到飞书会议活动原始事件",
		"bot", a.cfg.ID,
		"event_type", "vc.bot.meeting_activity_v1",
		"body", eventLogBody(body, event),
	)
	data, err := parseMeetingRawEventData[rawMeetingActivityEventData](event)
	if err != nil {
		return logMeetingParseError(ctx, "会议活动", event, err)
	}
	event2 := &larkvc.P2BotMeetingActivityV1{
		EventReq: event,
		Event:    data.toSDK(),
	}
	return a.handleMeetingActivity(ctx, event2)
}

func (a *Adapter) handleMeetingActivity(ctx context.Context, event *larkvc.P2BotMeetingActivityV1) (err error) {
	slog.InfoContext(ctx, "收到飞书会议Activity")
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

func (a *Adapter) handleMeetingEndedRaw(ctx context.Context, event *larkevent.EventReq) error {
	var body []byte
	if event != nil {
		body = event.Body
	}
	slog.InfoContext(ctx, "收到飞书会议结束原始事件",
		"bot", a.cfg.ID,
		"event_type", "vc.bot.meeting_ended_v1",
		"body", eventLogBody(body, event),
	)
	data, err := parseMeetingRawEventData[rawMeetingEndedEventData](event)
	if err != nil {
		return logMeetingParseError(ctx, "会议结束", event, err)
	}
	event2 := &larkvc.P2BotMeetingEndedV1{
		EventReq: event,
		Event:    data.toSDK(),
	}
	return a.handleMeetingEnded(ctx, event2)
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
