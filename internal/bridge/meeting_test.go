package bridge

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

type fakeMeetingOutbound struct {
	mu           sync.Mutex
	joinRequests []feishu.MeetingJoinRequest
	joinResult   feishu.MeetingJoinResult
	joinErrors   []error
	startError   error
	card         *fakeMeetingCard
	activities   []feishu.MeetingActivity
}

func (*fakeMeetingOutbound) Outbound() {}

func (f *fakeMeetingOutbound) JoinMeeting(_ context.Context, request feishu.MeetingJoinRequest) (feishu.MeetingJoinResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joinRequests = append(f.joinRequests, request)
	if len(f.joinErrors) > 0 {
		err := f.joinErrors[0]
		f.joinErrors = f.joinErrors[1:]
		if err != nil {
			return feishu.MeetingJoinResult{}, err
		}
	}
	return f.joinResult, nil
}

func (f *fakeMeetingOutbound) StartMeetingCard(_ context.Context, _ string, _ feishu.MeetingCardView) (feishu.MeetingCard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startError != nil {
		return nil, f.startError
	}
	if f.card == nil {
		f.card = &fakeMeetingCard{snapshot: feishu.MeetingCardSnapshot{CardID: "card-1", MessageID: "msg-1", ChatID: "chat-1"}}
	}
	return f.card, nil
}

func (f *fakeMeetingOutbound) RestoreMeetingCard(snapshot feishu.MeetingCardSnapshot) feishu.MeetingCard {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.card == nil {
		f.card = &fakeMeetingCard{snapshot: snapshot}
	}
	return f.card
}

func (f *fakeMeetingOutbound) ListMeetingActivities(_ context.Context, _ string, _ time.Time) ([]feishu.MeetingActivity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]feishu.MeetingActivity(nil), f.activities...), nil
}

func (f *fakeMeetingOutbound) joinRequestsSnapshot() []feishu.MeetingJoinRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]feishu.MeetingJoinRequest(nil), f.joinRequests...)
}

type fakeMeetingCard struct {
	mu       sync.Mutex
	snapshot feishu.MeetingCardSnapshot
	updates  []feishu.MeetingCardView
	err      error
}

func (f *fakeMeetingCard) Snapshot() feishu.MeetingCardSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot
}

func (f *fakeMeetingCard) Update(_ context.Context, view feishu.MeetingCardView) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, view)
	if f.err == nil {
		f.snapshot.Sequence++
	}
	return f.err
}

func TestMeetingStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meetings.json")
	store := NewMeetingStore(path)
	want := MeetingState{
		BotID: "bot-a", MeetingID: "meeting-1", MeetingNo: "123456789", CallID: "call-original",
		RecipientOpenID: "ou_owner", Status: meetingStatusActive,
		Participants:  map[string]string{"ou_user": "用户"},
		PendingEvents: []MeetingEvent{{Key: "transcript:s1", Type: feishu.MeetingActivityTranscript, Text: "确认上线"}},
		SeenKeys:      []string{"transcript:s1"}, Card: MeetingCardState{Dirty: true, RetryCount: 2},
	}
	if _, err := store.Upsert(want); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	loaded := NewMeetingStore(path)
	if err := loaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got, ok := loaded.Get(want.MeetingID)
	if !ok {
		t.Fatal("Get() missing persisted meeting")
	}
	if got.CallID != want.CallID || got.Card.RetryCount != 2 || len(got.PendingEvents) != 1 || got.PendingEvents[0].Text != "确认上线" {
		t.Fatalf("loaded meeting = %+v, want persisted state", got)
	}
	got.Participants["ou_user"] = "changed"
	again, _ := loaded.Get(want.MeetingID)
	if again.Participants["ou_user"] != "用户" {
		t.Fatal("Get() returned mutable store state")
	}
}

func TestHandleMeetingInvitedPassesOriginalCallID(t *testing.T) {
	workspace := t.TempDir()
	svc := newMeetingTestService(t, workspace)
	outbound := &fakeMeetingOutbound{joinResult: feishu.MeetingJoinResult{
		Meeting: feishu.MeetingInfo{ID: "meeting-1", MeetingNo: "123456789"},
		BotUser: feishu.MeetingUser{ID: "ou_bot", Name: "助手"},
	}}
	err := svc.HandleMeetingInvited(context.Background(), feishu.MeetingInvitation{
		BotID: "bot-a", CallID: "call-original",
		Meeting: feishu.MeetingInfo{ID: "meeting-1", MeetingNo: "123456789", Topic: "周会"},
	}, outbound)
	if err != nil {
		t.Fatalf("HandleMeetingInvited() error = %v", err)
	}
	calls := outbound.joinRequestsSnapshot()
	if len(calls) != 1 || calls[0].CallID != "call-original" || calls[0].MeetingNo != "123456789" {
		t.Fatalf("join requests = %+v, want original call_id and meeting_no", calls)
	}
	state, ok := svc.meetingStore("bot-a").Get("meeting-1")
	if !ok || state.Status != meetingStatusActive || state.CallID != "call-original" {
		t.Fatalf("meeting state = %+v, want active with persisted call_id", state)
	}
	stopMeetingTestService(svc)
}

func TestHandleMeetingActivitiesDeduplicatesStableIDs(t *testing.T) {
	svc := newMeetingTestService(t, t.TempDir())
	store := svc.meetingStore("bot-a")
	_, err := store.Upsert(MeetingState{
		BotID: "bot-a", MeetingID: "meeting-1", RecipientOpenID: "ou_owner", Status: meetingStatusActive,
		LastFlushAt: time.Now(), Minutes: MeetingMinutes{},
	})
	if err != nil {
		t.Fatal(err)
	}
	outbound := &fakeMeetingOutbound{}
	item := feishu.MeetingActivity{
		Meeting: feishu.MeetingInfo{ID: "meeting-1"}, Type: feishu.MeetingActivityTranscript,
		ID: "sentence-1", Time: "1712345678000", Actor: feishu.MeetingUser{ID: "ou_user", Name: "用户"}, Text: "确认周五上线",
	}
	activities := feishu.MeetingActivities{BotID: "bot-a", Items: []feishu.MeetingActivity{item, item}}
	if err := svc.HandleMeetingActivities(context.Background(), activities, outbound); err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleMeetingActivities(context.Background(), activities, outbound); err != nil {
		t.Fatal(err)
	}
	state, _ := store.Get("meeting-1")
	if len(state.PendingEvents) != 1 || len(state.SeenKeys) != 1 || state.PendingEvents[0].Key != "transcript:sentence-1" {
		t.Fatalf("deduplicated state = %+v", state)
	}
	stopMeetingTestService(svc)
}

func TestHandleMeetingActivitiesReopensCompletedMeetingForLateEvents(t *testing.T) {
	svc := newMeetingTestService(t, t.TempDir())
	store := svc.meetingStore("bot-a")
	completedAt := time.Now()
	_, err := store.Upsert(MeetingState{
		BotID: "bot-a", MeetingID: "meeting-1", RecipientOpenID: "ou_owner",
		Status: meetingStatusCompleted, CompletedAt: completedAt, EndedAt: completedAt, Minutes: MeetingMinutes{},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := feishu.MeetingActivity{
		Meeting: feishu.MeetingInfo{ID: "meeting-1"}, Type: feishu.MeetingActivityTranscript,
		ID: "sentence-late", Time: time.Now().Format(time.RFC3339Nano), Text: "这是迟到的最后一句字幕",
	}
	if err := svc.HandleMeetingActivities(context.Background(), feishu.MeetingActivities{BotID: "bot-a", Items: []feishu.MeetingActivity{item}}, &fakeMeetingOutbound{}); err != nil {
		t.Fatal(err)
	}
	state, _ := store.Get("meeting-1")
	if state.Status != meetingStatusEnding || !state.CompletedAt.IsZero() || len(state.PendingEvents) != 1 {
		t.Fatalf("meeting state = %+v, want reopened ending state with late event", state)
	}
	if state.PendingEvents[0].Text != item.Text || state.FinalizeAfter.Before(completedAt) {
		t.Fatalf("late event state = %+v", state)
	}
	stopMeetingTestService(svc)
}

func TestMeetingEndingWaitsForGraceAndFinalBackfill(t *testing.T) {
	svc := newMeetingTestService(t, t.TempDir())
	store := svc.meetingStore("bot-a")
	now := time.Now()
	state, err := store.Upsert(MeetingState{
		BotID: "bot-a", MeetingID: "meeting-1", RecipientOpenID: "ou_owner", Status: meetingStatusEnding,
		EndedAt: now, FinalizeAfter: now.Add(time.Minute), PendingEvents: []MeetingEvent{{Key: "transcript:s1", Text: "原内容"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &meetingCoordinator{service: svc, store: store, key: meetingKey{BotID: "bot-a", MeetingID: "meeting-1"}}
	if coordinator.shouldFlush(state, now) {
		t.Fatal("ending meeting flushed before grace period")
	}
	state.FinalizeAfter = now.Add(-time.Second)
	if coordinator.shouldFlush(state, now) {
		t.Fatal("ending meeting flushed before final backfill")
	}
	state.FinalBackfillAt = now
	if !coordinator.shouldFlush(state, now) {
		t.Fatal("ending meeting did not flush after final backfill")
	}
}

func TestMeetingCoordinatorFinalFlushCompletesAndPersistsMinutes(t *testing.T) {
	workspace := t.TempDir()
	svc := newMeetingTestService(t, workspace)
	runtime := &fakeRuntime{newSessionID: "acp-meeting", promptReply: `{"summary":["确认本周上线"],"decisions":["周五上线"],"todos":[{"id":"todo-1","content":"准备发布","assignee":"小王","due_at":"周五","status":"open","confidence":"explicit","evidence":"小王周五前准备发布"}],"risks":[],"open_questions":[],"shared_documents":[]}`}
	svc.setRuntime(runtime)
	store := svc.meetingStore("bot-a")
	state, err := store.Upsert(MeetingState{
		BotID: "bot-a", MeetingID: "meeting-1", Topic: "发布会", RecipientOpenID: "ou_owner",
		Status: meetingStatusEnding, PendingEvents: []MeetingEvent{{Key: "transcript:s1", Type: feishu.MeetingActivityTranscript, Text: "小王周五前准备发布"}},
		SeenKeys: []string{"transcript:s1"}, Minutes: MeetingMinutes{},
	})
	if err != nil {
		t.Fatal(err)
	}
	outbound := &fakeMeetingOutbound{}
	svc.setOutbound("bot-a", outbound)
	coordinator := &meetingCoordinator{service: svc, store: store, key: meetingKey{BotID: "bot-a", MeetingID: "meeting-1"}}
	coordinator.flush(context.Background(), state)
	got, _ := store.Get("meeting-1")
	if got.Status != meetingStatusCompleted || len(got.PendingEvents) != 0 || got.ACPSessionID != "acp-meeting" {
		t.Fatalf("meeting after final flush = %+v", got)
	}
	if len(got.Minutes.Todos) != 1 || got.Minutes.Todos[0].Assignee != "小王" || got.Minutes.Todos[0].Confidence != "explicit" {
		t.Fatalf("minutes = %+v, want explicit TODO", got.Minutes)
	}
	calls := runtime.promptCallsSnapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].Text, `"final": true`) {
		t.Fatalf("prompt calls = %+v, want one final meeting prompt", calls)
	}
	if !strings.Contains(calls[0].Text, `"recipient_open_id": "ou_owner"`) {
		t.Fatalf("meeting prompt = %q, want recipient identity", calls[0].Text)
	}
}

func TestMeetingTriggerDisablesTraceAndRejectsToolPermission(t *testing.T) {
	svc := newMeetingTestService(t, t.TempDir())
	runtime := &fakeRuntime{
		newSessionID: "acp-meeting",
		promptReply:  `{"summary":[],"decisions":[],"todos":[],"risks":[],"open_questions":[],"shared_documents":[]}`,
		permissionRequest: &acp.PermissionRequest{Options: []acp.PermissionOption{
			{OptionID: "allow", Kind: "allow_once"},
			{OptionID: "reject", Kind: "reject_once"},
		}},
	}
	svc.setRuntime(runtime)
	state := MeetingState{BotID: "bot-a", MeetingID: "meeting-1", RecipientOpenID: "ou_owner"}
	coordinator := &meetingCoordinator{service: svc, store: svc.meetingStore("bot-a"), key: meetingKey{BotID: "bot-a", MeetingID: "meeting-1"}}
	request, err := coordinator.triggerRequest(state, []MeetingEvent{{Key: "transcript:s1", Text: "会议内容"}}, false)
	if err != nil {
		t.Fatalf("triggerRequest() error = %v", err)
	}
	if !request.DisableToolPermissions || !request.DisableTrace {
		t.Fatalf("trigger request = %+v, want disabled tools and trace", request)
	}
	if _, err := svc.runTriggerPrompt(context.Background(), request); err != nil {
		t.Fatalf("runTriggerPrompt() error = %v", err)
	}
	if runtime.permissionOutcome.Outcome != "selected" || runtime.permissionOutcome.OptionID != "reject" {
		t.Fatalf("permission outcome = %+v, want reject option", runtime.permissionOutcome)
	}
}

func TestValidateMeetingMinutesRequiresTraceableEvidenceAndDocuments(t *testing.T) {
	events := []MeetingEvent{{
		Text:    "小王周五前准备发布，文档 https://example.com/release",
		Payload: map[string]string{"document_title": "发布文档", "document_url": "https://example.com/release"},
	}}
	valid := MeetingMinutes{
		Todos:           []MeetingTodo{{ID: "todo-1", Content: "准备发布", Confidence: "explicit", Evidence: "小王周五前准备发布"}},
		SharedDocuments: []MeetingDocument{{Title: "发布文档", URL: "https://example.com/release"}},
	}
	if err := validateMeetingMinutes(MeetingMinutes{}, events, valid); err != nil {
		t.Fatalf("validateMeetingMinutes(valid) error = %v", err)
	}
	invalidEvidence := valid
	invalidEvidence.Todos = []MeetingTodo{{ID: "todo-1", Content: "准备发布", Confidence: "explicit", Evidence: "小李下周发布"}}
	if err := validateMeetingMinutes(MeetingMinutes{}, events, invalidEvidence); err == nil {
		t.Fatal("validateMeetingMinutes() accepted fabricated evidence")
	}
	invalidDocument := valid
	invalidDocument.SharedDocuments = []MeetingDocument{{Title: "未知文档", URL: "https://invalid.example/doc"}}
	if err := validateMeetingMinutes(MeetingMinutes{}, events, invalidDocument); err == nil {
		t.Fatal("validateMeetingMinutes() accepted unsupported document")
	}
}

func TestParseMeetingMinutesRejectsUnsupportedOutput(t *testing.T) {
	tests := []string{
		`{"summary":[],"decisions":[],"todos":[],"risks":[],"open_questions":[],"shared_documents":[],"extra":true}`,
		`{"summary":[],"decisions":[],"todos":[{"id":"todo-1","content":"上线","confidence":"inferred","evidence":"猜测"}],"risks":[],"open_questions":[],"shared_documents":[]}`,
		`{"summary":[],"decisions":[],"todos":[],"risks":[],"open_questions":[]}`,
		`{"summary":null,"decisions":[],"todos":[],"risks":[],"open_questions":[],"shared_documents":[]}`,
		`not json`,
	}
	for _, input := range tests {
		if _, err := parseMeetingMinutes(input); err == nil {
			t.Fatalf("parseMeetingMinutes(%q) error = nil", input)
		}
	}
}

func TestMeetingActivitiesWithEndTimeStartFinalFlush(t *testing.T) {
	svc := newMeetingTestService(t, t.TempDir())
	store := svc.meetingStore("bot-a")
	_, err := store.Upsert(MeetingState{
		BotID: "bot-a", MeetingID: "meeting-1", RecipientOpenID: "ou_owner", Status: meetingStatusActive,
		LastFlushAt: time.Now(), Minutes: MeetingMinutes{},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := feishu.MeetingActivity{
		Meeting: feishu.MeetingInfo{ID: "meeting-1", EndTime: "1712349278"},
		Type:    feishu.MeetingActivityTranscript, ID: "sentence-1", Time: "1712345678000", Text: "最终内容",
	}
	if err := svc.HandleMeetingActivities(context.Background(), feishu.MeetingActivities{BotID: "bot-a", Items: []feishu.MeetingActivity{item}}, &fakeMeetingOutbound{}); err != nil {
		t.Fatal(err)
	}
	state, _ := store.Get("meeting-1")
	if state.Status != meetingStatusEnding || state.EndedAt.IsZero() {
		t.Fatalf("meeting state = %+v, want ending from backfilled end_time", state)
	}
	stopMeetingTestService(svc)
}

func TestMeetingRetryAndCardRetryAreIndependent(t *testing.T) {
	svc := newMeetingTestService(t, t.TempDir())
	store := svc.meetingStore("bot-a")
	now := time.Now()
	state, err := store.Upsert(MeetingState{
		BotID: "bot-a", MeetingID: "meeting-1", RecipientOpenID: "ou_owner", Status: meetingStatusActive,
		PendingEvents: []MeetingEvent{{Key: "transcript:s1", Text: "内容"}}, LastFlushAt: now, RetryCount: 2, Card: MeetingCardState{Dirty: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &meetingCoordinator{service: svc, store: store, key: meetingKey{BotID: "bot-a", MeetingID: "meeting-1"}}
	if coordinator.shouldFlush(state, now.Add(time.Second)) {
		t.Fatal("shouldFlush() = true before retry delay")
	}
	if !coordinator.shouldFlush(state, now.Add(meetingRetryDelay(2))) {
		t.Fatal("shouldFlush() = false at retry deadline")
	}
	svc.setOutbound("bot-a", &fakeMeetingOutbound{startError: errors.New("card unavailable")})
	coordinator.syncCard(context.Background(), state)
	got, _ := store.Get("meeting-1")
	if got.RetryCount != 2 || got.Card.RetryCount != 1 || !got.Card.Dirty || got.Card.LastAttemptAt.IsZero() {
		t.Fatalf("retry state = %+v, want independent card retry", got)
	}
}

func TestMeetingCoordinatorRetriesJoinWithPersistedCallID(t *testing.T) {
	svc := newMeetingTestService(t, t.TempDir())
	store := svc.meetingStore("bot-a")
	state, err := store.Upsert(MeetingState{
		BotID: "bot-a", MeetingID: "meeting-1", MeetingNo: "123456789", CallID: "call-original",
		RecipientOpenID: "ou_owner", Status: meetingStatusJoining, RetryCount: 1, LastFlushAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	outbound := &fakeMeetingOutbound{joinResult: feishu.MeetingJoinResult{Meeting: feishu.MeetingInfo{ID: "meeting-1"}}}
	svc.setOutbound("bot-a", outbound)
	coordinator := &meetingCoordinator{service: svc, store: store, key: meetingKey{BotID: "bot-a", MeetingID: "meeting-1"}}
	coordinator.retryJoin(context.Background(), state)
	calls := outbound.joinRequestsSnapshot()
	if len(calls) != 1 || calls[0].CallID != "call-original" {
		t.Fatalf("join calls = %+v, want persisted call_id", calls)
	}
	got, _ := store.Get("meeting-1")
	if got.Status != meetingStatusActive || got.RetryCount != 0 {
		t.Fatalf("meeting state = %+v, want recovered active meeting", got)
	}
}

func TestMeetingCoordinatorStopsJoinAfterRetryLimit(t *testing.T) {
	svc := newMeetingTestService(t, t.TempDir())
	store := svc.meetingStore("bot-a")
	state, err := store.Upsert(MeetingState{
		BotID: "bot-a", MeetingID: "meeting-1", MeetingNo: "123456789", CallID: "call-original",
		RecipientOpenID: "ou_owner", Status: meetingStatusJoining, RetryCount: defaultMeetingJoinMaxTries - 1, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	outbound := &fakeMeetingOutbound{joinErrors: []error{errors.New("permission denied")}}
	svc.setOutbound("bot-a", outbound)
	coordinator := &meetingCoordinator{service: svc, store: store, key: meetingKey{BotID: "bot-a", MeetingID: "meeting-1"}}
	coordinator.retryJoin(context.Background(), state)
	got, _ := store.Get(state.MeetingID)
	if got.Status != meetingStatusFailed || got.RetryCount != defaultMeetingJoinMaxTries {
		t.Fatalf("meeting state = %+v, want failed at retry limit", got)
	}
}

func TestPruneMeetingStatesCompactsAndBoundsHistory(t *testing.T) {
	now := time.Now()
	detailed := MeetingState{
		MeetingID: "detailed", Status: meetingStatusCompleted, CompletedAt: now.Add(-2 * time.Hour),
		PendingEvents: []MeetingEvent{{Key: "transcript:s1", Text: "content"}}, SeenKeys: []string{"transcript:s1"},
		Participants: map[string]string{"ou_user": "用户"}, CallID: "call", InviterID: "ou_user",
	}
	states := map[string]MeetingState{
		"active":   {MeetingID: "active", Status: meetingStatusActive, PendingEvents: []MeetingEvent{{Key: "active"}}},
		"detailed": detailed,
		"recent":   {MeetingID: "recent", Status: meetingStatusCompleted, CompletedAt: now.Add(-time.Minute)},
		"expired":  {MeetingID: "expired", Status: meetingStatusCompleted, CompletedAt: now.Add(-48 * time.Hour)},
	}
	if !pruneMeetingStates(states, now, time.Hour, 24*time.Hour, 10) {
		t.Fatal("pruneMeetingStates() reported no change")
	}
	if _, ok := states["expired"]; ok {
		t.Fatal("expired meeting was retained")
	}
	got := states["detailed"]
	if len(got.PendingEvents) != 0 || len(got.SeenKeys) != 0 || len(got.Participants) != 0 || got.CallID != "" || got.InviterID != "" {
		t.Fatalf("detailed meeting was not compacted: %+v", got)
	}
	if len(states["active"].PendingEvents) != 1 {
		t.Fatal("active meeting details were pruned")
	}
	if !pruneMeetingStates(states, now, time.Hour, 24*time.Hour, 1) {
		t.Fatal("history bound reported no change")
	}
	if _, ok := states["detailed"]; ok {
		t.Fatal("oldest terminal meeting was retained beyond max records")
	}
	if _, ok := states["recent"]; !ok {
		t.Fatal("newest terminal meeting was removed")
	}
}

func TestCompletedMeetingWithDirtyCardNeedsRestore(t *testing.T) {
	if !meetingStateIncomplete(MeetingState{Status: meetingStatusCompleted, Card: MeetingCardState{Dirty: true}}) {
		t.Fatal("completed meeting with dirty card should be restored")
	}
	if meetingStateIncomplete(MeetingState{Status: meetingStatusCompleted}) {
		t.Fatal("completed meeting with clean card should not be restored")
	}
}

func TestMeetingBackfillFinalizesStaleMeeting(t *testing.T) {
	svc := newMeetingTestService(t, t.TempDir())
	store := svc.meetingStore("bot-a")
	state, err := store.Upsert(MeetingState{
		BotID: "bot-a", MeetingID: "meeting-1", RecipientOpenID: "ou_owner", Status: meetingStatusActive,
		StartedAt: time.Now().Add(-defaultMeetingRestoreGrace - time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.setOutbound("bot-a", &fakeMeetingOutbound{})
	coordinator := &meetingCoordinator{service: svc, store: store, key: meetingKey{BotID: "bot-a", MeetingID: "meeting-1"}}
	coordinator.backfill(context.Background())
	got, _ := store.Get(state.MeetingID)
	if got.Status != meetingStatusEnding || got.EndedAt.IsZero() {
		t.Fatalf("meeting after stale backfill = %+v, want ending", got)
	}
}

func newMeetingTestService(t *testing.T, workspace string) *Service {
	t.Helper()
	cwd := t.TempDir()
	cfg := config.Config{
		Bots: []config.BotConfig{{
			ID: "bot-a", Workspace: workspace, OwnerOpenIDs: []string{"ou_owner"},
			Meeting: config.MeetingConfig{Enabled: true, RecipientOpenID: "ou_owner"},
		}},
		AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex", DefaultCwd: cwd}}},
	}
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	return NewService(cfg, store)
}

func stopMeetingTestService(svc *Service) {
	svc.stopBackgroundStarts()
	svc.cancelBackgroundRoot()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	svc.waitBackgroundShutdown(ctx)
}
