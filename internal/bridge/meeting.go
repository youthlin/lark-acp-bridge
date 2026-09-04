package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const sessionSourceMeeting = "meeting"

func (s *Service) HandleMeetingInvited(ctx context.Context, invitation feishu.MeetingInvitation, outbound feishu.Outbound) error {
	s.setOutbound(invitation.BotID, outbound)
	bot, ok := s.botConfig(invitation.BotID)
	if !ok || !bot.Meeting.Enabled {
		slog.InfoContext(ctx, "会议助手未开启，忽略会议邀请", "bot", invitation.BotID)
		return nil
	}
	recipient, recipientErr := s.meetingRecipient(invitation.BotID)
	if recipientErr != "" {
		return fmt.Errorf("会议助手接收人无效: %s", recipientErr)
	}
	meetingID := strings.TrimSpace(invitation.Meeting.ID)
	if meetingID == "" {
		return fmt.Errorf("会议邀请缺少 meeting_id")
	}
	store := s.meetingStore(invitation.BotID)
	if store == nil {
		return fmt.Errorf("未找到 bot %s 的会议状态存储", displayBotID(invitation.BotID))
	}

	s.meetingMu.Lock()
	if current, exists := store.Get(meetingID); exists && current.Status != meetingStatusFailed {
		s.meetingMu.Unlock()
		slog.InfoContext(ctx, "忽略重复会议邀请", "meeting_id", meetingID, "status", current.Status)
		return nil
	}
	now := time.Now()
	state := MeetingState{
		BotID:           invitation.BotID,
		MeetingID:       meetingID,
		MeetingNo:       invitation.Meeting.MeetingNo,
		CallID:          invitation.CallID,
		Topic:           invitation.Meeting.Topic,
		InviterID:       invitation.Inviter.ID,
		RecipientOpenID: recipient,
		Status:          meetingStatusJoining,
		Participants:    make(map[string]string),
		Minutes:         normalizeMeetingMinutes(MeetingMinutes{}),
		StartedAt:       parseMeetingTime(invitation.Meeting.StartTime),
		LastFlushAt:     now, Card: MeetingCardState{Dirty: true},
	}
	if state.StartedAt.IsZero() {
		state.StartedAt = now
	}
	if invitation.Meeting.Host.ID != "" {
		state.Participants[invitation.Meeting.Host.ID] = invitation.Meeting.Host.Name
	}
	if invitation.Inviter.ID != "" {
		state.Participants[invitation.Inviter.ID] = invitation.Inviter.Name
	}
	if _, err := store.Upsert(state); err != nil {
		s.meetingMu.Unlock()
		return err
	}
	s.meetingMu.Unlock()

	joiner, ok := outbound.(feishu.MeetingJoiner)
	if !ok || joiner == nil {
		return s.failMeetingJoin(store, meetingID, fmt.Errorf("飞书出站不支持机器人入会"))
	}
	joined, err := joiner.JoinMeeting(ctx, feishu.MeetingJoinRequest{MeetingNo: invitation.Meeting.MeetingNo, CallID: invitation.CallID})
	if err != nil {
		state, _, persistErr := store.Update(meetingID, func(current *MeetingState) error {
			current.RetryCount++
			current.LastError = err.Error()
			current.LastFlushAt = time.Now()
			current.Card.Dirty = true
			return nil
		})
		if persistErr != nil {
			return fmt.Errorf("机器人入会失败: %v；保存重试状态: %w", err, persistErr)
		}
		s.ensureMeetingCoordinator(ctx, state.BotID, state.MeetingID, false)
		return fmt.Errorf("机器人入会失败，已进入后台重试: %w", err)
	}
	state, _, err = store.Update(meetingID, func(current *MeetingState) error {
		current.Status = meetingStatusActive
		current.LastError = ""
		current.RetryCount = 0
		mergeMeetingInfo(current, joined.Meeting)
		if joined.BotUser.ID != "" {
			current.Participants[joined.BotUser.ID] = joined.BotUser.Name
		}
		current.Card.Dirty = true
		return nil
	})
	if err != nil {
		return err
	}
	s.ensureMeetingCoordinator(ctx, state.BotID, state.MeetingID, false)
	return nil
}

func (s *Service) failMeetingJoin(store *MeetingStore, meetingID string, cause error) error {
	_, _, persistErr := store.Update(meetingID, func(state *MeetingState) error {
		state.Status = meetingStatusFailed
		state.LastError = cause.Error()
		state.RetryCount++
		return nil
	})
	if persistErr != nil {
		return fmt.Errorf("机器人入会失败: %v；保存失败状态: %w", cause, persistErr)
	}
	return fmt.Errorf("机器人入会失败: %w", cause)
}

func (s *Service) HandleMeetingActivities(ctx context.Context, activities feishu.MeetingActivities, outbound feishu.Outbound) error {
	s.setOutbound(activities.BotID, outbound)
	store := s.meetingStore(activities.BotID)
	if store == nil {
		return nil
	}
	grouped := make(map[string][]MeetingEvent)
	info := make(map[string]feishu.MeetingInfo)
	for _, item := range activities.Items {
		meetingID := strings.TrimSpace(item.Meeting.ID)
		if meetingID == "" {
			continue
		}
		event, ok := normalizeMeetingActivity(item)
		if !ok {
			continue
		}
		grouped[meetingID] = append(grouped[meetingID], event)
		info[meetingID] = item.Meeting
	}
	for meetingID, events := range grouped {
		sortMeetingEvents(events)
		state, exists, err := store.Update(meetingID, func(state *MeetingState) error {
			if state.Status == meetingStatusFailed {
				return nil
			}
			if state.Status == meetingStatusCompleted {
				if !meetingLateEventDeadline(*state).After(time.Now()) {
					return nil
				}
				state.Status = meetingStatusEnding
				state.CompletedAt = time.Time{}
				state.FinalizeAfter = time.Now().Add(defaultMeetingFinalizeGrace)
				state.FinalBackfillAt = time.Time{}
				state.BackfillAttempts = 0
			}
			mergeMeetingInfo(state, info[meetingID])
			if state.Status == meetingStatusJoining {
				state.Status = meetingStatusActive
				state.RetryCount = 0
				state.LastError = ""
			}
			seen := make(map[string]struct{}, len(state.SeenKeys)+len(events))
			for _, key := range state.SeenKeys {
				seen[key] = struct{}{}
			}
			for _, event := range events {
				if _, duplicate := seen[event.Key]; duplicate {
					continue
				}
				seen[event.Key] = struct{}{}
				state.SeenKeys = append(state.SeenKeys, event.Key)
				state.PendingEvents = append(state.PendingEvents, event)
				updateMeetingParticipants(state, event)
			}
			sortMeetingEvents(state.PendingEvents)
			return nil
		})
		if err != nil {
			return err
		}
		if !exists || state.Status == meetingStatusCompleted || state.Status == meetingStatusFailed {
			continue
		}
		s.ensureMeetingCoordinator(ctx, activities.BotID, meetingID, false)
		s.wakeMeetingCoordinator(meetingKey{BotID: activities.BotID, MeetingID: meetingID})
	}
	return nil
}

func (s *Service) HandleMeetingEnded(ctx context.Context, ended feishu.MeetingEnded, outbound feishu.Outbound) error {
	s.setOutbound(ended.BotID, outbound)
	store := s.meetingStore(ended.BotID)
	if store == nil {
		return nil
	}
	meetingID := strings.TrimSpace(ended.Meeting.ID)
	state, exists, err := store.Update(meetingID, func(state *MeetingState) error {
		if state.Status == meetingStatusCompleted || state.Status == meetingStatusFailed {
			return nil
		}
		state.Status = meetingStatusEnding
		mergeMeetingInfo(state, ended.Meeting)
		state.EndedAt = parseMeetingTime(ended.Meeting.EndTime)
		if state.EndedAt.IsZero() {
			state.EndedAt = time.Now()
		}
		state.FinalizeAfter = time.Now().Add(defaultMeetingFinalizeGrace)
		state.FinalBackfillAt = time.Time{}
		state.BackfillAttempts = 0
		state.Card.Dirty = true
		return nil
	})
	if err != nil {
		return err
	}
	if !exists || state.Status == meetingStatusCompleted || state.Status == meetingStatusFailed {
		return nil
	}
	s.ensureMeetingCoordinator(ctx, ended.BotID, meetingID, false)
	s.wakeMeetingCoordinator(meetingKey{BotID: ended.BotID, MeetingID: meetingID})
	return nil
}

func (s *Service) meetingStore(botID string) *MeetingStore {
	s.meetingMu.Lock()
	defer s.meetingMu.Unlock()
	return s.meetingStores[strings.TrimSpace(botID)]
}

func normalizeMeetingActivity(item feishu.MeetingActivity) (MeetingEvent, bool) {
	event := MeetingEvent{
		Type:      strings.TrimSpace(item.Type),
		Time:      parseMeetingTime(item.Time),
		ActorID:   strings.TrimSpace(item.Actor.ID),
		ActorName: strings.TrimSpace(item.Actor.Name),
		Text:      strings.TrimSpace(item.Text),
		Payload:   make(map[string]string),
	}
	switch event.Type {
	case feishu.MeetingActivityTranscript:
		if id := strings.TrimSpace(item.ID); id != "" {
			event.Key = "transcript:" + id
		}
		event.Payload["language"] = strings.TrimSpace(item.Language)
		event.Payload["end_time"] = strings.TrimSpace(item.EndTime)
	case feishu.MeetingActivityChat:
		if id := strings.TrimSpace(item.ID); id != "" {
			event.Key = "chat:" + id
		}
	case feishu.MeetingActivityParticipantJoined, feishu.MeetingActivityParticipantLeft:
		event.Key = strings.Join([]string{"participant", event.Type, event.ActorID, strings.TrimSpace(item.Time)}, ":")
		if item.LeaveReason != 0 {
			event.Payload["leave_reason"] = strconv.Itoa(item.LeaveReason)
		}
	case feishu.MeetingActivityShareStarted, feishu.MeetingActivityShareEnded, feishu.MeetingActivityDocumentChanged:
		event.Payload["share_id"] = strings.TrimSpace(item.ShareID)
		event.Payload["document_title"] = strings.TrimSpace(item.Document.Title)
		event.Payload["document_url"] = strings.TrimSpace(item.Document.URL)
		event.Key = strings.Join([]string{"share", event.Type, strings.TrimSpace(item.ShareID), strings.TrimSpace(item.Time)}, ":")
	default:
		return MeetingEvent{}, false
	}
	if event.Key == "" || meetingEventKeyIncomplete(event) {
		event.Key = meetingEventHash(event)
	}
	for key, value := range event.Payload {
		if value == "" {
			delete(event.Payload, key)
		}
	}
	return event, true
}

func meetingEventKeyIncomplete(event MeetingEvent) bool {
	switch event.Type {
	case feishu.MeetingActivityParticipantJoined, feishu.MeetingActivityParticipantLeft:
		return event.ActorID == "" || event.Time.IsZero()
	case feishu.MeetingActivityShareStarted, feishu.MeetingActivityShareEnded, feishu.MeetingActivityDocumentChanged:
		return event.Payload["share_id"] == "" || event.Time.IsZero()
	default:
		return false
	}
}

func (s *Service) loadMeetingStores() error {
	s.meetingMu.Lock()
	stores := make(map[string]*MeetingStore, len(s.meetingStores))
	for botID, store := range s.meetingStores {
		stores[botID] = store
	}
	s.meetingMu.Unlock()
	for botID, store := range stores {
		if store == nil {
			continue
		}
		if err := store.Load(); err != nil {
			return fmt.Errorf("加载 bot %s 的会议状态: %w", displayBotID(botID), err)
		}
	}
	return nil
}

func (s *Service) restoreMeetings(ctx context.Context) {
	s.meetingMu.Lock()
	stores := make(map[string]*MeetingStore, len(s.meetingStores))
	for botID, store := range s.meetingStores {
		stores[botID] = store
	}
	s.meetingMu.Unlock()
	for botID, store := range stores {
		if store == nil {
			continue
		}
		for _, state := range store.List() {
			if meetingStateIncomplete(state) {
				s.ensureMeetingCoordinator(ctx, botID, state.MeetingID, true)
			}
		}
	}
}

func meetingEventHash(event MeetingEvent) string {
	event.Key = ""
	raw, _ := json.Marshal(event)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sortMeetingEvents(events []MeetingEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Time.Equal(events[j].Time) {
			return events[i].Key < events[j].Key
		}
		if events[i].Time.IsZero() {
			return false
		}
		if events[j].Time.IsZero() {
			return true
		}
		return events[i].Time.Before(events[j].Time)
	})
}

func parseMeetingTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if value > 100000000000 {
			return time.UnixMilli(value)
		}
		return time.Unix(value, 0)
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if value, err := time.ParseInLocation(layout, raw, time.Local); err == nil {
			return value
		}
	}
	return time.Time{}
}

func mergeMeetingInfo(state *MeetingState, info feishu.MeetingInfo) {
	if state == nil {
		return
	}
	if value := strings.TrimSpace(info.MeetingNo); value != "" {
		state.MeetingNo = value
	}
	if value := strings.TrimSpace(info.Topic); value != "" {
		state.Topic = value
	}
	if value := parseMeetingTime(info.StartTime); !value.IsZero() {
		state.StartedAt = value
	}
	if state.Participants == nil {
		state.Participants = make(map[string]string)
	}
	if info.Host.ID != "" {
		state.Participants[info.Host.ID] = info.Host.Name
	}
}

func updateMeetingParticipants(state *MeetingState, event MeetingEvent) {
	if state == nil || event.ActorID == "" {
		return
	}
	if state.Participants == nil {
		state.Participants = make(map[string]string)
	}
	if event.Type == feishu.MeetingActivityParticipantJoined ||
		event.Type == feishu.MeetingActivityTranscript ||
		event.Type == feishu.MeetingActivityChat {
		state.Participants[event.ActorID] = event.ActorName
	}
}

func meetingSessionKey(botID, meetingID string) SessionKey {
	return SessionKey{
		BotID:  strings.TrimSpace(botID),
		Source: sessionSourceMeeting,
		MainID: "meeting:" + strings.TrimSpace(meetingID),
		SubID:  "live",
	}
}
