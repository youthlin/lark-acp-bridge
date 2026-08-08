package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func TestRuntimeDispatchSessionInfoSendsStateUpdates(t *testing.T) {
	r := newRuntimeManager()
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "thread-a"}
	var updates []acp.SessionUpdate
	unsub := r.SubscribeUpdates(key, func(sessionID string, update acp.SessionUpdate) {
		if sessionID != "session-1" {
			t.Fatalf("sessionID = %q, want session-1", sessionID)
		}
		updates = append(updates, update)
	})
	defer unsub()

	r.dispatchSessionInfo(key, "session-1", acp.SessionInfo{
		AvailableCommands: []acp.AvailableCommand{{Name: "review", Description: "Review changes"}},
		ConfigOptions: []acp.SessionConfigOption{
			{ID: "model", Name: "Model", Category: "model", Type: "select", CurrentValue: "gpt-5.6"},
		},
	})

	if len(updates) != 2 {
		t.Fatalf("updates = %+v, want commands and config updates", updates)
	}
	if updates[0].SessionUpdate != "available_commands_update" || len(updates[0].AvailableCommands) != 1 || updates[0].AvailableCommands[0].Name != "review" {
		t.Fatalf("first update = %+v, want available commands", updates[0])
	}
	if updates[1].SessionUpdate != "config_option_update" || len(updates[1].ConfigOptions) != 1 || updates[1].ConfigOptions[0].ID != "model" {
		t.Fatalf("second update = %+v, want config options", updates[1])
	}
}

func TestRuntimeDispatchSessionInfoSendsMetaUpdate(t *testing.T) {
	r := newRuntimeManager()
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "thread-a"}
	var updates []acp.SessionUpdate
	unsub := r.SubscribeUpdates(key, func(sessionID string, update acp.SessionUpdate) {
		if sessionID != "session-1" {
			t.Fatalf("sessionID = %q, want session-1", sessionID)
		}
		updates = append(updates, update)
	})
	defer unsub()

	r.dispatchSessionInfo(key, "session-1", acp.SessionInfo{
		Meta: map[string]any{"messageCount": 12},
	})

	if len(updates) != 1 {
		t.Fatalf("updates = %+v, want meta update", updates)
	}
	if updates[0].SessionUpdate != "session_info_update" || updates[0].Meta["messageCount"] != 12 {
		t.Fatalf("update = %+v, want session info meta", updates[0])
	}
}

func TestRuntimeTransitionCurrentSessionUpdatesMarkerWithoutClient(t *testing.T) {
	r := newRuntimeManager()
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "thread-a"}
	r.setRuntimeSessionID(currentRuntimeKey(key), "session-2")

	session, changed, err := r.TransitionCurrentSession(key, "session-2", func() (Session, bool, error) {
		return Session{Key: key, ACPSessionID: "session-1"}, true, nil
	})
	if err != nil {
		t.Fatalf("TransitionCurrentSession() error = %v", err)
	}
	if !changed || session.ACPSessionID != "session-1" {
		t.Fatalf("TransitionCurrentSession() = %+v, %v; want session-1", session, changed)
	}
	r.mu.Lock()
	slot := r.slots[currentRuntimeKey(key)]
	r.mu.Unlock()
	if slot.sessionID != "session-1" || slot.client != nil {
		t.Fatalf("runtime slot = %+v, want session-1 marker without client", slot)
	}

	if err := r.CloseSession(key); err != nil {
		t.Fatalf("CloseSession() error = %v", err)
	}
	r.mu.Lock()
	_, markerExists := r.slots[currentRuntimeKey(key)]
	r.mu.Unlock()
	if markerExists {
		t.Fatal("CloseSession() left runtime session marker")
	}
}

func TestRuntimeCloseIdleRuntimeSlotsClosesInactiveClient(t *testing.T) {
	r := newRuntimeManager()
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }
	r.idleTimeout = 30 * time.Minute
	key := currentRuntimeKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "thread-a"})
	r.slots[key] = runtimeClientSlot{
		client:    &acp.Client{},
		sessionID: "session-1",
		lastUsed:  base.Add(-31 * time.Minute),
	}

	if err := r.closeIdleRuntimeSlots(nil); err != nil {
		t.Fatalf("closeIdleRuntimeSlots() error = %v", err)
	}
	if _, ok := r.slots[key]; ok {
		t.Fatal("closeIdleRuntimeSlots() left inactive idle runtime slot")
	}
}

func TestRuntimeCloseIdleRuntimeSlotsKeepsActiveClient(t *testing.T) {
	r := newRuntimeManager()
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }
	r.idleTimeout = 30 * time.Minute
	key := currentRuntimeKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "thread-a"})
	client := &acp.Client{}
	r.slots[key] = runtimeClientSlot{
		client:    client,
		sessionID: "session-1",
		lastUsed:  base.Add(-31 * time.Minute),
	}

	acquired, release, ok := r.acquireCachedClientForSession(key, "session-1")
	if !ok || acquired != client {
		t.Fatalf("acquireCachedClientForSession() = %v, %v; want active client", acquired, ok)
	}
	if err := r.closeIdleRuntimeSlots(nil); err != nil {
		t.Fatalf("closeIdleRuntimeSlots(active) error = %v", err)
	}
	if _, ok := r.slots[key]; !ok {
		t.Fatal("closeIdleRuntimeSlots() closed active runtime slot")
	}

	release()
	r.now = func() time.Time { return base.Add(32 * time.Minute) }
	if err := r.closeIdleRuntimeSlots(nil); err != nil {
		t.Fatalf("closeIdleRuntimeSlots(released) error = %v", err)
	}
	if _, ok := r.slots[key]; ok {
		t.Fatal("closeIdleRuntimeSlots() kept released idle runtime slot")
	}
}

func TestRuntimeCloseIdleRuntimeSlotsKeepsBusyClient(t *testing.T) {
	r := newRuntimeManager()
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }
	r.idleTimeout = 30 * time.Minute
	key := currentRuntimeKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "thread-a"})
	r.slots[key] = runtimeClientSlot{
		client:    &acp.Client{},
		sessionID: "session-1",
		lastUsed:  base.Add(-31 * time.Minute),
	}

	if err := r.closeIdleRuntimeSlots(func(runtime runtimeKey) bool {
		return runtime == key
	}); err != nil {
		t.Fatalf("closeIdleRuntimeSlots(busy) error = %v", err)
	}
	if _, ok := r.slots[key]; !ok {
		t.Fatal("closeIdleRuntimeSlots() closed busy runtime slot")
	}
}

func TestRuntimeSnapshotReportsClientAndSlotState(t *testing.T) {
	r := newRuntimeManager()
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }
	r.idleTimeout = 30 * time.Minute
	current := currentRuntimeKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "thread-a"})
	wiki := wikiRuntimeKey(current.SessionKey, 1, "session-wiki")
	marker := currentRuntimeKey(SessionKey{BotID: "bot-b", ChatID: "oc_other"})
	r.slots[current] = runtimeClientSlot{
		client:    &acp.Client{},
		sessionID: "session-1",
		lastUsed:  base.Add(-31 * time.Minute),
	}
	r.slots[wiki] = runtimeClientSlot{
		client:    &acp.Client{},
		sessionID: "session-wiki",
		lastUsed:  base,
		active:    1,
	}
	r.slots[marker] = runtimeClientSlot{
		sessionID: "session-marker",
		lastUsed:  base,
	}

	snapshot := r.snapshot()
	if snapshot.TotalSlots != 3 || snapshot.ClientSlots != 2 || snapshot.ActiveSlots != 1 || snapshot.IdleSlots != 1 || snapshot.MarkerSlots != 1 || snapshot.MaxSlots != acpRuntimeMaxSlots {
		t.Fatalf("snapshot = %+v, want total=3 clients=2 active=1 idle=1 markers=1 max=%d", snapshot, acpRuntimeMaxSlots)
	}
	if len(snapshot.Slots) != 3 {
		t.Fatalf("snapshot slots = %+v, want 3", snapshot.Slots)
	}
	if !snapshot.Slots[0].HasClient || snapshot.Slots[0].SessionID != "session-1" || !snapshot.Slots[0].Idle {
		t.Fatalf("first slot = %+v, want idle current client sorted first", snapshot.Slots[0])
	}
}

func TestRuntimeReserveSlotReclaimsOldestIdleClientAtLimit(t *testing.T) {
	r := newRuntimeManager()
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }
	r.maxSlots = 2
	oldKey := currentRuntimeKey(SessionKey{BotID: "bot-a", ChatID: "old"})
	newerKey := currentRuntimeKey(SessionKey{BotID: "bot-a", ChatID: "newer"})
	targetKey := currentRuntimeKey(SessionKey{BotID: "bot-a", ChatID: "target"})
	r.slots[oldKey] = runtimeClientSlot{
		client:    &acp.Client{},
		sessionID: "old-session",
		lastUsed:  base.Add(-2 * time.Hour),
	}
	r.slots[newerKey] = runtimeClientSlot{
		client:    &acp.Client{},
		sessionID: "newer-session",
		lastUsed:  base.Add(-time.Hour),
	}

	release, err := r.reserveRuntimeSlot(targetKey)
	if err != nil {
		t.Fatalf("reserveRuntimeSlot() error = %v", err)
	}
	defer release()
	if _, ok := r.slots[oldKey]; ok {
		t.Fatal("oldest idle runtime slot still exists, want reclaimed")
	}
	if _, ok := r.slots[newerKey]; !ok {
		t.Fatal("newer runtime slot missing, want retained")
	}
	if slot := r.slots[targetKey]; slot.reserved != 1 {
		t.Fatalf("target slot = %+v, want reserved placeholder", slot)
	}
}

func TestRuntimeReserveSlotTracksMultipleReservationsForSameKey(t *testing.T) {
	r := newRuntimeManager()
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }
	r.maxSlots = 1
	key := currentRuntimeKey(SessionKey{BotID: "bot-a", ChatID: "same"})
	otherKey := currentRuntimeKey(SessionKey{BotID: "bot-a", ChatID: "other"})

	releaseOne, err := r.reserveRuntimeSlot(key)
	if err != nil {
		t.Fatalf("reserveRuntimeSlot(first) error = %v", err)
	}
	releaseTwo, err := r.reserveRuntimeSlot(key)
	if err != nil {
		t.Fatalf("reserveRuntimeSlot(second) error = %v", err)
	}
	if slot := r.slots[key]; slot.reserved != 2 {
		t.Fatalf("slot after two reservations = %+v, want reserved=2", slot)
	}

	releaseOne()
	if slot := r.slots[key]; slot.reserved != 1 {
		t.Fatalf("slot after one release = %+v, want reserved=1", slot)
	}
	if releaseOther, err := r.reserveRuntimeSlot(otherKey); err == nil {
		releaseOther()
		t.Fatal("reserveRuntimeSlot(other) succeeded while same-key reservation still holds capacity")
	} else if !errors.Is(err, errACPRuntimeLimitReached) {
		t.Fatalf("reserveRuntimeSlot(other) error = %v, want errACPRuntimeLimitReached", err)
	}

	releaseTwo()
	if _, ok := r.slots[key]; ok {
		t.Fatalf("slot still exists after all releases: %+v", r.slots[key])
	}
}

func TestRuntimeReserveSlotRejectsWhenLimitIsAllBusy(t *testing.T) {
	r := newRuntimeManager()
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }
	r.maxSlots = 1
	busyKey := currentRuntimeKey(SessionKey{BotID: "bot-a", ChatID: "busy"})
	targetKey := currentRuntimeKey(SessionKey{BotID: "bot-a", ChatID: "target"})
	r.slots[busyKey] = runtimeClientSlot{
		client:    &acp.Client{},
		sessionID: "busy-session",
		lastUsed:  base.Add(-2 * time.Hour),
		active:    1,
	}

	release, err := r.reserveRuntimeSlot(targetKey)
	if err == nil || !errors.Is(err, errACPRuntimeLimitReached) {
		t.Fatalf("reserveRuntimeSlot() err = %v, want errACPRuntimeLimitReached", err)
	}
	if release != nil {
		t.Fatal("reserveRuntimeSlot() returned release function on failure")
	}
	if _, ok := r.slots[busyKey]; !ok {
		t.Fatal("busy runtime slot missing, want retained")
	}
	if _, ok := r.slots[targetKey]; ok {
		t.Fatal("target runtime slot exists after failed reserve")
	}
}

func TestRuntimeClientForRuntimeSessionSingleflightsConcurrentBuilds(t *testing.T) {
	r := newRuntimeManager()
	key := currentRuntimeKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "thread-a"})
	session := Session{Key: key.SessionKey, ACPSessionID: "session-1", Cwd: "/repo"}
	var starts atomic.Int32
	var startedOnce sync.Once
	started := make(chan struct{})
	releaseStart := make(chan struct{})
	r.startAndResumeClientFunc = func(ctx context.Context, got Session, agent config.AgentConfig) (*acp.Client, acp.SessionInfo, error) {
		starts.Add(1)
		startedOnce.Do(func() { close(started) })
		select {
		case <-releaseStart:
		case <-ctx.Done():
			return nil, acp.SessionInfo{}, ctx.Err()
		}
		if got.ACPSessionID != session.ACPSessionID {
			t.Errorf("startAndResumeClientFunc session = %+v, want %s", got, session.ACPSessionID)
		}
		return &acp.Client{}, acp.SessionInfo{SessionID: got.ACPSessionID}, nil
	}

	const callers = 8
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, release, err := r.clientForRuntimeSession(context.Background(), key, session, config.AgentConfig{})
			if err != nil {
				errs <- err
				return
			}
			if client == nil {
				errs <- errors.New("clientForRuntimeSession returned nil client")
				return
			}
			release()
		}()
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("client build did not start")
	}
	deadline := time.After(100 * time.Millisecond)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			close(releaseStart)
			wg.Wait()
			close(errs)
			if starts.Load() != 1 {
				t.Fatalf("startAndResumeClientFunc called %d times, want 1", starts.Load())
			}
			for err := range errs {
				if err != nil {
					t.Fatalf("clientForRuntimeSession() error = %v", err)
				}
			}
			return
		case <-ticker.C:
			if starts.Load() > 1 {
				close(releaseStart)
				wg.Wait()
				t.Fatalf("startAndResumeClientFunc called %d times while first build was still running, want 1", starts.Load())
			}
		}
	}
}

func TestRuntimeTreatsBrokenPipeAsUnavailableSession(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("session/prompt: write |1: broken pipe"),
		fmt.Errorf("session/prompt: %w", io.ErrClosedPipe),
		fmt.Errorf("session/prompt: %w: %w", acp.ErrServerOutputClosed, io.EOF),
	} {
		if !isBrokenACPClientPipeError(err) {
			t.Fatalf("isBrokenACPClientPipeError(%v) = false, want true", err)
		}
	}
	if isBrokenACPClientPipeError(fmt.Errorf("session/prompt: read response: %w", io.EOF)) {
		t.Fatal("isBrokenACPClientPipeError(naked EOF) = true, want false")
	}
	if isBrokenACPClientPipeError(errors.New("session/prompt: model overloaded")) {
		t.Fatal("isBrokenACPClientPipeError(non pipe error) = true, want false")
	}
}

func TestPromptActivityTimeoutExpiresAfterIdlePeriod(t *testing.T) {
	timeout := newPromptActivityTimeout(context.Background(), 30*time.Millisecond, time.Second)
	defer timeout.Stop()

	select {
	case <-timeout.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("prompt activity timeout did not expire")
	}

	cause := context.Cause(timeout.Context())
	if !errors.Is(cause, context.DeadlineExceeded) {
		t.Fatalf("cause = %v, want context.DeadlineExceeded", cause)
	}
	if !strings.Contains(cause.Error(), "未收到活动更新") {
		t.Fatalf("cause = %v, want idle timeout detail", cause)
	}
}

func TestPromptActivityTimeoutTouchExtendsIdlePeriod(t *testing.T) {
	timeout := newPromptActivityTimeout(context.Background(), 60*time.Millisecond, time.Second)
	defer timeout.Stop()

	time.Sleep(40 * time.Millisecond)
	timeout.Touch()

	select {
	case <-timeout.Context().Done():
		t.Fatalf("prompt activity timeout expired before extended idle deadline: %v", context.Cause(timeout.Context()))
	case <-time.After(40 * time.Millisecond):
	}

	select {
	case <-timeout.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("prompt activity timeout did not expire after extended idle deadline")
	}
}

func TestPromptActivityTimeoutPauseIdleKeepsPermissionWaitAlive(t *testing.T) {
	timeout := newPromptActivityTimeout(context.Background(), 30*time.Millisecond, time.Second)
	defer timeout.Stop()

	timeout.PauseIdle()
	time.Sleep(70 * time.Millisecond)

	select {
	case <-timeout.Context().Done():
		t.Fatalf("prompt activity timeout expired while idle timer was paused: %v", context.Cause(timeout.Context()))
	default:
	}

	timeout.ResumeIdle()
	select {
	case <-timeout.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("prompt activity timeout did not expire after idle timer resumed")
	}

	cause := context.Cause(timeout.Context())
	if !errors.Is(cause, context.DeadlineExceeded) {
		t.Fatalf("cause = %v, want context.DeadlineExceeded", cause)
	}
	if !strings.Contains(cause.Error(), "未收到活动更新") {
		t.Fatalf("cause = %v, want idle timeout detail", cause)
	}
}

func TestPromptActivityTimeoutPauseIdleKeepsAbsoluteLimit(t *testing.T) {
	timeout := newPromptActivityTimeout(context.Background(), time.Second, 50*time.Millisecond)
	defer timeout.Stop()

	timeout.PauseIdle()
	select {
	case <-timeout.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("prompt activity timeout did not reach absolute limit while idle timer was paused")
	}

	cause := context.Cause(timeout.Context())
	if !errors.Is(cause, context.DeadlineExceeded) {
		t.Fatalf("cause = %v, want context.DeadlineExceeded", cause)
	}
	if !strings.Contains(cause.Error(), "绝对上限") {
		t.Fatalf("cause = %v, want absolute timeout detail", cause)
	}
}

func TestPromptActivityTimeoutKeepsAbsoluteLimit(t *testing.T) {
	timeout := newPromptActivityTimeout(context.Background(), 80*time.Millisecond, 120*time.Millisecond)
	defer timeout.Stop()

	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			timeout.Touch()
		case <-timeout.Context().Done():
			cause := context.Cause(timeout.Context())
			if !errors.Is(cause, context.DeadlineExceeded) {
				t.Fatalf("cause = %v, want context.DeadlineExceeded", cause)
			}
			if !strings.Contains(cause.Error(), "绝对上限") {
				t.Fatalf("cause = %v, want absolute timeout detail", cause)
			}
			return
		case <-time.After(time.Second):
			t.Fatal("prompt activity timeout did not reach absolute limit")
		}
	}
}
