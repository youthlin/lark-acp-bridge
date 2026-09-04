package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	meetingStatusJoining     = "joining"
	meetingStatusActive      = "active"
	meetingStatusEnding      = "ending"
	meetingStatusCompleted   = "completed"
	meetingStatusFailed      = "failed"
	meetingStatusFinalFailed = "final_failed"

	meetingLateEventWindow   = 10 * time.Minute
	meetingDetailRetention   = meetingLateEventWindow
	meetingHistoryRetention  = 90 * 24 * time.Hour
	meetingHistoryMaxRecords = 100
)

type meetingKey struct {
	BotID     string
	MeetingID string
}

type MeetingEvent struct {
	Key       string            `json:"key"`
	Type      string            `json:"type"`
	Time      time.Time         `json:"time,omitempty"`
	ActorID   string            `json:"actor_id,omitempty"`
	ActorName string            `json:"actor_name,omitempty"`
	Text      string            `json:"text,omitempty"`
	Payload   map[string]string `json:"payload,omitempty"`
}

type MeetingTodo struct {
	ID         string `json:"id"`
	Content    string `json:"content"`
	Assignee   string `json:"assignee,omitempty"`
	DueAt      string `json:"due_at,omitempty"`
	Status     string `json:"status,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	Evidence   string `json:"evidence,omitempty"`
}

type MeetingDocument struct {
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
}

type MeetingMinutes struct {
	Summary         []string          `json:"summary"`
	Decisions       []string          `json:"decisions"`
	Todos           []MeetingTodo     `json:"todos"`
	Risks           []string          `json:"risks"`
	OpenQuestions   []string          `json:"open_questions"`
	SharedDocuments []MeetingDocument `json:"shared_documents"`
}

type MeetingCardState struct {
	CardID        string    `json:"card_id,omitempty"`
	MessageID     string    `json:"message_id,omitempty"`
	ChatID        string    `json:"chat_id,omitempty"`
	Sequence      int       `json:"sequence,omitempty"`
	Dirty         bool      `json:"dirty,omitempty"`
	RetryCount    int       `json:"retry_count,omitempty"`
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
}

type MeetingState struct {
	BotID            string            `json:"bot_id"`
	MeetingID        string            `json:"meeting_id"`
	MeetingNo        string            `json:"meeting_no,omitempty"`
	CallID           string            `json:"call_id,omitempty"`
	Topic            string            `json:"topic,omitempty"`
	InviterID        string            `json:"inviter_id,omitempty"`
	RecipientOpenID  string            `json:"recipient_open_id"`
	Status           string            `json:"status"`
	Participants     map[string]string `json:"participants,omitempty"`
	PendingEvents    []MeetingEvent    `json:"pending_events,omitempty"`
	SeenKeys         []string          `json:"seen_keys,omitempty"`
	Minutes          MeetingMinutes    `json:"minutes"`
	ACPSessionID     string            `json:"acp_session_id,omitempty"`
	Card             MeetingCardState  `json:"card,omitempty"`
	LastFlushAt      time.Time         `json:"last_flush_at,omitempty"`
	LastCardUpdateAt time.Time         `json:"last_card_update_at,omitempty"`
	RetryCount       int               `json:"retry_count,omitempty"`
	LastError        string            `json:"last_error,omitempty"`
	StartedAt        time.Time         `json:"started_at,omitempty"`
	EndedAt          time.Time         `json:"ended_at,omitempty"`
	FinalizeAfter    time.Time         `json:"finalize_after,omitempty"`
	FinalBackfillAt  time.Time         `json:"final_backfill_at,omitempty"`
	BackfillAttempts int               `json:"backfill_attempts,omitempty"`
	CompletedAt      time.Time         `json:"completed_at,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

type MeetingStore struct {
	path     string
	mu       sync.Mutex
	meetings map[string]MeetingState
}

type meetingStoreFile struct {
	Version  int            `json:"version"`
	Meetings []MeetingState `json:"meetings"`
}

func NewMeetingStore(path string) *MeetingStore {
	return &MeetingStore{path: strings.TrimSpace(path), meetings: make(map[string]MeetingState)}
}

func (s *MeetingStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.TrimSpace(s.path) == "" {
			s.meetings = make(map[string]MeetingState)
			return nil
		}
		return fmt.Errorf("读取会议状态文件: %w", err)
	}
	var file meetingStoreFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("解析会议状态文件: %w", err)
	}
	s.meetings = make(map[string]MeetingState, len(file.Meetings))
	for _, meeting := range file.Meetings {
		meeting = normalizeMeetingState(meeting)
		if meeting.MeetingID != "" {
			s.meetings[meeting.MeetingID] = meeting
		}
	}
	if pruneMeetingStates(
		s.meetings,
		time.Now(),
		meetingDetailRetention,
		meetingHistoryRetention,
		meetingHistoryMaxRecords,
	) {
		if err := s.writeLocked(); err != nil {
			return fmt.Errorf("保存清理后的会议状态文件: %w", err)
		}
	}
	return nil
}

func (s *MeetingStore) Get(meetingID string) (MeetingState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.meetings[strings.TrimSpace(meetingID)]
	return cloneMeetingState(state), ok
}

func (s *MeetingStore) List() []MeetingState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MeetingState, 0, len(s.meetings))
	for _, state := range s.meetings {
		out = append(out, cloneMeetingState(state))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (s *MeetingStore) Upsert(state MeetingState) (MeetingState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state = normalizeMeetingState(state)
	if state.BotID == "" || state.MeetingID == "" {
		return MeetingState{}, fmt.Errorf("会议状态 bot_id 和 meeting_id 不能为空")
	}
	previous, existed := s.meetings[state.MeetingID]
	now := time.Now()
	if state.CreatedAt.IsZero() {
		if existed {
			state.CreatedAt = previous.CreatedAt
		}
		if state.CreatedAt.IsZero() {
			state.CreatedAt = now
		}
	}
	state.UpdatedAt = now
	var previousMeetings map[string]MeetingState
	if _, terminal := meetingTerminalTime(state); terminal {
		previousMeetings = cloneMeetingStates(s.meetings)
	}
	s.meetings[state.MeetingID] = cloneMeetingState(state)
	if previousMeetings != nil {
		pruneMeetingStates(
			s.meetings,
			now,
			meetingDetailRetention,
			meetingHistoryRetention,
			meetingHistoryMaxRecords,
		)
	}
	if err := s.writeLocked(); err != nil {
		if previousMeetings != nil {
			s.meetings = previousMeetings
		} else if existed {
			s.meetings[state.MeetingID] = previous
		} else {
			delete(s.meetings, state.MeetingID)
		}
		return MeetingState{}, err
	}
	stored, ok := s.meetings[state.MeetingID]
	if !ok {
		return cloneMeetingState(state), nil
	}
	return cloneMeetingState(stored), nil
}

func (s *MeetingStore) Update(meetingID string, update func(*MeetingState) error) (MeetingState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	meetingID = strings.TrimSpace(meetingID)
	state, ok := s.meetings[meetingID]
	if !ok {
		return MeetingState{}, false, nil
	}
	previous := cloneMeetingState(state)
	state = cloneMeetingState(state)
	if update != nil {
		if err := update(&state); err != nil {
			return MeetingState{}, true, err
		}
	}
	state = normalizeMeetingState(state)
	state.UpdatedAt = time.Now()
	var previousMeetings map[string]MeetingState
	if _, terminal := meetingTerminalTime(state); terminal {
		previousMeetings = cloneMeetingStates(s.meetings)
	}
	s.meetings[meetingID] = cloneMeetingState(state)
	if previousMeetings != nil {
		pruneMeetingStates(
			s.meetings,
			state.UpdatedAt,
			meetingDetailRetention,
			meetingHistoryRetention,
			meetingHistoryMaxRecords,
		)
	}
	if err := s.writeLocked(); err != nil {
		if previousMeetings != nil {
			s.meetings = previousMeetings
		} else {
			s.meetings[meetingID] = previous
		}
		return MeetingState{}, true, err
	}
	stored, stillExists := s.meetings[meetingID]
	if !stillExists {
		return cloneMeetingState(state), true, nil
	}
	return cloneMeetingState(stored), true, nil
}

func (s *MeetingStore) writeLocked() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	file := meetingStoreFile{Version: 1, Meetings: make([]MeetingState, 0, len(s.meetings))}
	for _, meeting := range s.meetings {
		file.Meetings = append(file.Meetings, meeting)
	}
	sort.Slice(file.Meetings, func(i, j int) bool { return file.Meetings[i].MeetingID < file.Meetings[j].MeetingID })
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("编码会议状态文件: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("创建会议状态目录: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "."+filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建会议状态临时文件: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("写入会议状态临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("替换会议状态文件: %w", err)
	}
	return nil
}

func normalizeMeetingState(state MeetingState) MeetingState {
	state.BotID = strings.TrimSpace(state.BotID)
	state.MeetingID = strings.TrimSpace(state.MeetingID)
	state.MeetingNo = strings.TrimSpace(state.MeetingNo)
	state.CallID = strings.TrimSpace(state.CallID)
	state.Topic = strings.TrimSpace(state.Topic)
	state.InviterID = strings.TrimSpace(state.InviterID)
	state.RecipientOpenID = strings.TrimSpace(state.RecipientOpenID)
	state.Status = strings.TrimSpace(state.Status)
	state.ACPSessionID = strings.TrimSpace(state.ACPSessionID)
	state.LastError = strings.TrimSpace(state.LastError)
	if state.Participants == nil {
		state.Participants = make(map[string]string)
	}
	state.Minutes = normalizeMeetingMinutes(state.Minutes)
	return state
}

func normalizeMeetingMinutes(minutes MeetingMinutes) MeetingMinutes {
	if minutes.Summary == nil {
		minutes.Summary = []string{}
	}
	if minutes.Decisions == nil {
		minutes.Decisions = []string{}
	}
	if minutes.Todos == nil {
		minutes.Todos = []MeetingTodo{}
	}
	if minutes.Risks == nil {
		minutes.Risks = []string{}
	}
	if minutes.OpenQuestions == nil {
		minutes.OpenQuestions = []string{}
	}
	if minutes.SharedDocuments == nil {
		minutes.SharedDocuments = []MeetingDocument{}
	}
	return minutes
}

func cloneMeetingState(state MeetingState) MeetingState {
	state.Participants = cloneStringMap(state.Participants)
	state.PendingEvents = append([]MeetingEvent(nil), state.PendingEvents...)
	for i := range state.PendingEvents {
		state.PendingEvents[i].Payload = cloneStringMap(state.PendingEvents[i].Payload)
	}
	state.SeenKeys = append([]string(nil), state.SeenKeys...)
	state.Minutes.Summary = append([]string(nil), state.Minutes.Summary...)
	state.Minutes.Decisions = append([]string(nil), state.Minutes.Decisions...)
	state.Minutes.Todos = append([]MeetingTodo(nil), state.Minutes.Todos...)
	state.Minutes.Risks = append([]string(nil), state.Minutes.Risks...)
	state.Minutes.OpenQuestions = append([]string(nil), state.Minutes.OpenQuestions...)
	state.Minutes.SharedDocuments = append([]MeetingDocument(nil), state.Minutes.SharedDocuments...)
	return state
}

func cloneMeetingStates(states map[string]MeetingState) map[string]MeetingState {
	out := make(map[string]MeetingState, len(states))
	for meetingID, state := range states {
		out[meetingID] = cloneMeetingState(state)
	}
	return out
}

func pruneMeetingStates(states map[string]MeetingState, now time.Time, detailRetention, historyRetention time.Duration, maxRecords int) bool {
	type terminalMeeting struct {
		id string
		at time.Time
	}
	terminal := make([]terminalMeeting, 0, len(states))
	changed := false
	for meetingID, state := range states {
		terminalAt, ok := meetingTerminalTime(state)
		if !ok {
			continue
		}
		if historyRetention > 0 && !terminalAt.IsZero() && terminalAt.Before(now.Add(-historyRetention)) {
			delete(states, meetingID)
			changed = true
			continue
		}
		if detailRetention > 0 && !terminalAt.IsZero() && !terminalAt.After(now.Add(-detailRetention)) {
			hadDetails := len(state.PendingEvents) > 0 ||
				len(state.SeenKeys) > 0 ||
				len(state.Participants) > 0 ||
				state.CallID != "" ||
				state.InviterID != ""
			state.PendingEvents = nil
			state.SeenKeys = nil
			state.Participants = nil
			state.CallID = ""
			state.InviterID = ""
			states[meetingID] = state
			changed = changed || hadDetails
		}
		terminal = append(terminal, terminalMeeting{id: meetingID, at: terminalAt})
	}
	if maxRecords <= 0 || len(terminal) <= maxRecords {
		return changed
	}
	sort.Slice(terminal, func(i, j int) bool {
		if terminal[i].at.Equal(terminal[j].at) {
			return terminal[i].id < terminal[j].id
		}
		return terminal[i].at.Before(terminal[j].at)
	})
	for _, meeting := range terminal[:len(terminal)-maxRecords] {
		delete(states, meeting.id)
		changed = true
	}
	return changed
}

func meetingTerminalTime(state MeetingState) (time.Time, bool) {
	switch state.Status {
	case meetingStatusCompleted:
		if !state.CompletedAt.IsZero() {
			return state.CompletedAt, true
		}
		return state.UpdatedAt, true
	case meetingStatusFailed:
		return state.UpdatedAt, true
	default:
		return time.Time{}, false
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return make(map[string]string)
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func meetingStateIncomplete(state MeetingState) bool {
	switch state.Status {
	case meetingStatusJoining, meetingStatusActive, meetingStatusEnding, meetingStatusFinalFailed:
		return true
	case meetingStatusCompleted:
		return state.Card.Dirty
	default:
		return false
	}
}
