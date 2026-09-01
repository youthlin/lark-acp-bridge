package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

const (
	acpRequestTimeout    = 10 * time.Minute
	acpPromptIdleTimeout = 10 * time.Minute
	acpPromptMaxDuration = time.Hour

	acpRuntimeIdleTimeout       = 30 * time.Minute
	acpRuntimeIdleSweepInterval = 5 * time.Minute
	acpRuntimeMaxSlots          = 32
)

var errACPSessionUnavailable = errors.New("acp session unavailable")
var errACPRuntimeLimitReached = errors.New("acp runtime limit reached")

type acpRuntime interface {
	NewSession(ctx context.Context, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (acpSessionCandidate, error)
	NewSessionWithRuntimeKey(ctx context.Context, runtime runtimeKey, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (acpSessionCandidate, error)
	Prompt(ctx context.Context, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (acp.PromptResult, error)
	PromptWithRuntimeKey(ctx context.Context, key runtimeKey, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (acp.PromptResult, error)
	CancelSession(ctx context.Context, key runtimeKey, session Session, agent config.AgentConfig) error
	SetConfigOption(ctx context.Context, session Session, agent config.AgentConfig, configID string, value any) ([]acp.SessionConfigOption, error)
	SetMode(ctx context.Context, session Session, agent config.AgentConfig, modeID string) error
	SubscribeUpdates(key SessionKey, handler acp.UpdateHandler) func()
	TransitionCurrentSession(key SessionKey, expectedSessionID string, transition func() (Session, bool, error)) (Session, bool, error)
	CloseRuntimeKey(key runtimeKey) error
	CloseSession(key SessionKey) error
	Shutdown(ctx context.Context) error
}

type acpSessionCandidate interface {
	Info() acp.SessionInfo
	Commit(persist func() error) error
	Abort()
}

var _ acpRuntime = (*runtimeManager)(nil)

type runtimeKey struct {
	SessionKey
	Scope string
	RunID string
}

const (
	runtimeScopeCurrent       = "current"
	runtimeScopeWiki          = "wiki" // 兼容旧版 runtime key 与持久化测试。
	runtimeScopeWikiCompanion = "wiki-companion"
	runtimeScopeAtAuto        = "at-auto"
)

func currentRuntimeKey(key SessionKey) runtimeKey {
	return runtimeKey{SessionKey: normalizeSessionKey(key), Scope: runtimeScopeCurrent}
}

func wikiRuntimeKey(key SessionKey, generation int64, sessionID string) runtimeKey {
	return runtimeKey{
		SessionKey: normalizeSessionKey(key),
		Scope:      runtimeScopeWiki,
		RunID:      fmt.Sprintf("%d:%s", generation, sessionID),
	}
}

func normalizeRuntimeKey(key runtimeKey) runtimeKey {
	key.SessionKey = normalizeSessionKey(key.SessionKey)
	key.Scope = strings.TrimSpace(key.Scope)
	key.RunID = strings.TrimSpace(key.RunID)
	return key
}

type runtimeManager struct {
	mu            sync.Mutex
	slots         map[runtimeKey]runtimeClientSlot
	subscriptions map[SessionKey]map[int64]acp.UpdateHandler
	nextSubID     int64
	transitions   [64]sync.Mutex

	// buildMu guards building；building 中每个 key 最多有一个 goroutine 在创建 client，
	// 其余同 key 的调用方在同一把 key 锁上等待并复用结果。
	// 这样 acp.Start/Initialize/Resume（可能耗时数十秒）无需持有 transition 锁，
	// 避免哈希碰撞的无关 session 被连带阻塞。
	buildMu  sync.Mutex
	building map[runtimeKey]*clientBuild

	idleTimeout       time.Duration
	idleSweepInterval time.Duration
	maxSlots          int
	now               func() time.Time
	cleanerOnce       sync.Once
	cleanerMu         sync.Mutex
	cleanerCancel     context.CancelFunc

	startAndResumeClientFunc func(context.Context, Session, config.AgentConfig) (*acp.Client, acp.SessionInfo, error)
}

type runtimeManagerSnapshot struct {
	Slots       []runtimeSlotSnapshot
	TotalSlots  int
	ClientSlots int
	ActiveSlots int
	IdleSlots   int
	MarkerSlots int
	MaxSlots    int
}

type runtimeSlotSnapshot struct {
	Key       runtimeKey
	SessionID string
	HasClient bool
	Active    int
	LastUsed  time.Time
	Idle      bool
}

// clientBuild 协调同一 runtimeKey 上并发的 client 创建。
type clientBuild struct {
	mu       sync.Mutex
	done     chan struct{}
	doneOnce sync.Once
	client   *acp.Client
	info     acp.SessionInfo
	err      error
}

func newClientBuild() *clientBuild {
	return &clientBuild{done: make(chan struct{})}
}

// beginClientBuild 返回 build 和当前 goroutine 是否负责创建 client。
// leader 在锁外执行 Start/Initialize/Resume，follower 等待并复用 leader 结果。
func (r *runtimeManager) beginClientBuild(key runtimeKey) (*clientBuild, bool) {
	r.buildMu.Lock()
	defer r.buildMu.Unlock()
	if r.building == nil {
		r.building = make(map[runtimeKey]*clientBuild)
	}
	if b, ok := r.building[key]; ok {
		return b, false
	}
	b := newClientBuild()
	r.building[key] = b
	return b, true
}

func (r *runtimeManager) finishClientBuild(key runtimeKey, b *clientBuild) {
	r.buildMu.Lock()
	if r.building[key] == b {
		delete(r.building, key)
	}
	r.buildMu.Unlock()
	b.mu.Lock()
	b.doneOnce.Do(func() {
		close(b.done)
	})
	b.mu.Unlock()
}

func (b *clientBuild) setResult(client *acp.Client, info acp.SessionInfo, err error) {
	b.mu.Lock()
	b.client = client
	b.info = info
	b.err = err
	b.mu.Unlock()
}

func (b *clientBuild) wait(ctx context.Context) (*acp.Client, acp.SessionInfo, error) {
	select {
	case <-b.done:
	case <-ctx.Done():
		return nil, acp.SessionInfo{}, ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.client, b.info, b.err
}

type runtimeClientSlot struct {
	client    *acp.Client
	sessionID string
	unsub     func()
	lastUsed  time.Time
	active    int
	reserved  int
}

func (slot runtimeClientSlot) unsubscribe() {
	if slot.unsub != nil {
		slot.unsub()
	}
}

func (slot runtimeClientSlot) close(manager *runtimeManager) error {
	slot.unsubscribe()
	if slot.client == nil {
		return nil
	}
	return manager.closeClient(slot.client, slot.sessionID)
}

func (slot runtimeClientSlot) closeReplacedBy(manager *runtimeManager, replacement *acp.Client) error {
	if slot.client == replacement {
		slot.unsubscribe()
		return nil
	}
	return slot.close(manager)
}

func newRuntimeManager() *runtimeManager {
	return &runtimeManager{
		slots:             make(map[runtimeKey]runtimeClientSlot),
		subscriptions:     make(map[SessionKey]map[int64]acp.UpdateHandler),
		idleTimeout:       acpRuntimeIdleTimeout,
		idleSweepInterval: acpRuntimeIdleSweepInterval,
		maxSlots:          acpRuntimeMaxSlots,
		now:               time.Now,
	}
}

type runtimeBusyFunc func(runtimeKey) bool

func (r *runtimeManager) startIdleCleaner(ctx context.Context, busy runtimeBusyFunc) {
	if r == nil || r.idleTimeout <= 0 || r.idleSweepInterval <= 0 {
		return
	}
	r.cleanerOnce.Do(func() {
		cleanerCtx, cancel := context.WithCancel(ctx)
		r.cleanerMu.Lock()
		r.cleanerCancel = cancel
		r.cleanerMu.Unlock()
		go func() {
			ticker := time.NewTicker(r.idleSweepInterval)
			defer ticker.Stop()
			for {
				select {
				case <-cleanerCtx.Done():
					return
				case <-ticker.C:
					_ = r.closeIdleRuntimeSlots(busy)
				}
			}
		}()
	})
}

func (r *runtimeManager) stopIdleCleaner() {
	if r == nil {
		return
	}
	r.cleanerMu.Lock()
	cancel := r.cleanerCancel
	r.cleanerCancel = nil
	r.cleanerMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *runtimeManager) snapshot() runtimeManagerSnapshot {
	if r == nil {
		return runtimeManagerSnapshot{}
	}
	now := r.currentTime()
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := runtimeManagerSnapshot{
		Slots:    make([]runtimeSlotSnapshot, 0, len(r.slots)),
		MaxSlots: r.maxSlots,
	}
	for key, slot := range r.slots {
		item := runtimeSlotSnapshot{
			Key:       key,
			SessionID: slot.sessionID,
			HasClient: slot.client != nil,
			Active:    slot.active,
			LastUsed:  slot.lastUsed,
			Idle:      r.slotIdleLocked(slot, now),
		}
		snapshot.Slots = append(snapshot.Slots, item)
		snapshot.TotalSlots++
		if item.HasClient {
			snapshot.ClientSlots++
		} else {
			snapshot.MarkerSlots++
		}
		if item.Active > 0 {
			snapshot.ActiveSlots++
		}
		if item.Idle {
			snapshot.IdleSlots++
		}
	}
	sort.Slice(snapshot.Slots, func(i, j int) bool {
		left := snapshot.Slots[i]
		right := snapshot.Slots[j]
		for _, pair := range [][2]string{
			{left.Key.BotID, right.Key.BotID},
			{left.Key.Source, right.Key.Source},
			{left.Key.MainID, right.Key.MainID},
			{left.Key.SubID, right.Key.SubID},
			{left.Key.Scope, right.Key.Scope},
			{left.Key.RunID, right.Key.RunID},
		} {
			if pair[0] != pair[1] {
				return pair[0] < pair[1]
			}
		}
		return left.SessionID < right.SessionID
	})
	return snapshot
}

func (r *runtimeManager) NewSession(ctx context.Context, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (acpSessionCandidate, error) {
	return r.NewSessionWithRuntimeKey(ctx, currentRuntimeKey(key), key, agentName, agent, cwd, workspace)
}

func (r *runtimeManager) NewSessionWithRuntimeKey(ctx context.Context, runtime runtimeKey, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (acpSessionCandidate, error) {
	key = normalizeSessionKey(key)
	runtime = normalizeRuntimeKey(runtime)
	ctx, cancel := context.WithTimeout(ctx, acpRequestTimeout)
	defer cancel()

	releaseReservation, err := r.reserveRuntimeSlot(runtime)
	if err != nil {
		return nil, err
	}
	client, err := acp.Start(ctx, agent, workspace)
	if err != nil {
		releaseReservation()
		return nil, err
	}
	if err := client.Initialize(ctx); err != nil {
		releaseReservation()
		_ = client.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	sessionInfo, err := client.NewSession(ctx, cwd)
	if err != nil {
		releaseReservation()
		_ = client.Close()
		return nil, fmt.Errorf("session/new: %w", err)
	}

	return &runtimeSessionCandidate{
		manager:            r,
		key:                runtime,
		client:             client,
		info:               sessionInfo,
		releaseReservation: releaseReservation,
	}, nil
}

type runtimeSessionCandidate struct {
	mu                 sync.Mutex
	manager            *runtimeManager
	key                runtimeKey
	client             *acp.Client
	info               acp.SessionInfo
	releaseReservation func()
	committed          bool
	closed             bool
}

func (c *runtimeSessionCandidate) Info() acp.SessionInfo {
	if c == nil {
		return acp.SessionInfo{}
	}
	return c.info
}

func (c *runtimeSessionCandidate) Commit(persist func() error) error {
	if c == nil || c.manager == nil || c.client == nil {
		return fmt.Errorf("ACP session 候选未初始化")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.committed {
		return nil
	}
	if c.closed {
		return fmt.Errorf("ACP session 候选已关闭")
	}

	transition := c.manager.transitionLock(c.key)
	transition.Lock()
	if persist != nil {
		if err := persist(); err != nil {
			transition.Unlock()
			c.closed = true
			c.releaseRuntimeSlotReservation()
			_ = c.manager.closeClient(c.client, c.info.SessionID)
			return err
		}
	}
	old := c.manager.swapClient(c.key, c.client, c.info.SessionID)
	c.manager.attachClientSubscriptions(c.key, c.client)
	c.manager.touchRuntimeKey(c.key)
	c.committed = true
	transition.Unlock()
	c.releaseRuntimeSlotReservation()

	_ = old.closeReplacedBy(c.manager, c.client)
	return nil
}

func (c *runtimeSessionCandidate) Abort() {
	if c == nil || c.manager == nil || c.client == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.committed || c.closed {
		return
	}
	c.closed = true
	c.releaseRuntimeSlotReservation()
	_ = c.manager.closeClient(c.client, c.info.SessionID)
}

func (c *runtimeSessionCandidate) releaseRuntimeSlotReservation() {
	if c == nil || c.releaseReservation == nil {
		return
	}
	c.releaseReservation()
	c.releaseReservation = nil
}

func (r *runtimeManager) Prompt(ctx context.Context, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (acp.PromptResult, error) {
	return r.PromptWithRuntimeKey(ctx, currentRuntimeKey(session.Key), session, agent, text, opts)
}

func (r *runtimeManager) PromptWithRuntimeKey(ctx context.Context, key runtimeKey, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (acp.PromptResult, error) {
	key = normalizeRuntimeKey(key)
	client, release, err := r.clientForRuntimeSession(ctx, key, session, agent)
	if err != nil {
		return acp.PromptResult{}, err
	}
	defer release()
	result, err := promptWithClient(ctx, client, session.ACPSessionID, text, opts)
	if err == nil || !isBrokenACPClientPipeError(err) {
		return result, err
	}
	r.detachBrokenRuntimeClient(key, client)
	return result, fmt.Errorf("%w: %v", errACPSessionUnavailable, err)
}

func promptWithClient(ctx context.Context, client *acp.Client, sessionID string, text string, opts acp.PromptOptions) (acp.PromptResult, error) {
	startedAt := time.Now()
	onLifecycle := opts.OnLifecycle
	emitLifecycle := func(event acp.PromptLifecycleEvent) {
		if event.SessionID == "" {
			event.SessionID = sessionID
		}
		if event.At.IsZero() {
			event.At = time.Now()
		}
		if event.Elapsed == 0 {
			event.Elapsed = event.At.Sub(startedAt)
		}
		logPromptLifecycle(ctx, event)
		if onLifecycle != nil {
			onLifecycle(event)
		}
	}
	emitLifecycle(acp.PromptLifecycleEvent{
		Stage:     "started",
		SessionID: sessionID,
	})
	timeout := newPromptActivityTimeout(ctx, acpPromptIdleTimeout, acpPromptMaxDuration, func(event promptTimeoutEvent) {
		emitLifecycle(acp.PromptLifecycleEvent{
			Stage:     "timeout_" + event.Reason,
			SessionID: sessionID,
			Err:       event.Cause,
			Cause:     event.Cause,
			At:        event.At,
			Elapsed:   event.Elapsed,
		})
	})
	defer timeout.Stop()
	onUpdate := opts.OnUpdate
	opts.OnUpdate = func(update acp.PromptUpdate) {
		timeout.Touch()
		if onUpdate != nil {
			onUpdate(update)
		}
	}
	onPermissionRequest := opts.OnPermissionRequest
	if onPermissionRequest != nil {
		opts.OnPermissionRequest = func(ctx context.Context, req acp.PermissionRequest) (acp.PermissionOutcome, error) {
			timeout.PauseIdle()
			defer timeout.ResumeIdle()
			outcome, err := onPermissionRequest(ctx, req)
			return outcome, err
		}
	}
	opts.OnLifecycle = emitLifecycle
	result, err := client.PromptWithOptions(timeout.Context(), sessionID, text, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if cause := context.Cause(timeout.Context()); errors.Is(cause, context.DeadlineExceeded) {
				err = cause
			}
		}
		return result, fmt.Errorf("session/prompt: %w", err)
	}
	return result, nil
}

func logPromptLifecycle(ctx context.Context, event acp.PromptLifecycleEvent) {
	stage := strings.TrimSpace(event.Stage)
	if stage == "" {
		stage = "unknown"
	}
	attrs := []any{
		"session", event.SessionID,
		"stage", stage,
	}
	if event.Method != "" {
		attrs = append(attrs, "method", event.Method)
	}
	if event.RequestID != "" {
		attrs = append(attrs, "request_id", event.RequestID)
	}
	if event.Elapsed > 0 {
		attrs = append(attrs, "elapsed", event.Elapsed.String())
	}
	if event.WaitDuration > 0 {
		attrs = append(attrs, "wait", event.WaitDuration.String())
	}
	if event.Cause != nil {
		attrs = append(attrs, "cause", event.Cause)
	}
	if event.Err != nil {
		attrs = append(attrs, "错误", event.Err)
	}
	switch {
	case strings.HasPrefix(stage, "timeout_"):
		slog.WarnContext(ctx, "ACP prompt 超时触发", attrs...)
	case stage == "context_done" || stage == "cancel_wait_started" || stage == "cancel_wait_finished" || stage == "cancel_wait_timeout":
		slog.WarnContext(ctx, "ACP prompt 取消等待状态", attrs...)
	default:
		slog.InfoContext(ctx, "ACP prompt 生命周期", attrs...)
	}
}

type promptTimeoutEvent struct {
	Reason      string
	Cause       error
	At          time.Time
	Elapsed     time.Duration
	IdleTimeout time.Duration
	MaxDuration time.Duration
}

type promptActivityTimeout struct {
	ctx        context.Context
	cancel     context.CancelCauseFunc
	mu         sync.Mutex
	idle       time.Duration
	max        time.Duration
	startedAt  time.Time
	deadline   time.Time
	idleTimer  *time.Timer
	maxTimer   *time.Timer
	onExpire   func(promptTimeoutEvent)
	idlePaused bool
	stopped    bool
}

func newPromptActivityTimeout(parent context.Context, idleTimeout, maxDuration time.Duration, onExpire ...func(promptTimeoutEvent)) *promptActivityTimeout {
	ctx, cancel := context.WithCancelCause(parent)
	startedAt := time.Now()
	var expire func(promptTimeoutEvent)
	if len(onExpire) > 0 {
		expire = onExpire[0]
	}
	timeout := &promptActivityTimeout{
		ctx:       ctx,
		cancel:    cancel,
		idle:      idleTimeout,
		max:       maxDuration,
		startedAt: startedAt,
		deadline:  startedAt.Add(idleTimeout),
		onExpire:  expire,
	}
	timeout.mu.Lock()
	timeout.idleTimer = time.AfterFunc(idleTimeout, timeout.handleIdleTimeout)
	timeout.maxTimer = time.AfterFunc(maxDuration, func() {
		timeout.expire("max_duration", fmt.Errorf("ACP prompt 执行超过绝对上限 %s: %w", maxDuration, context.DeadlineExceeded))
	})
	timeout.mu.Unlock()
	return timeout
}

func (t *promptActivityTimeout) Context() context.Context {
	return t.ctx
}

func (t *promptActivityTimeout) Touch() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	t.deadline = time.Now().Add(t.idle)
	if t.idlePaused {
		return
	}
	t.idleTimer.Reset(t.idle)
}

func (t *promptActivityTimeout) PauseIdle() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.idlePaused {
		return
	}
	t.idlePaused = true
	t.idleTimer.Stop()
}

func (t *promptActivityTimeout) ResumeIdle() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || !t.idlePaused {
		return
	}
	t.idlePaused = false
	t.deadline = time.Now().Add(t.idle)
	t.idleTimer.Reset(t.idle)
}

func (t *promptActivityTimeout) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	t.stopped = true
	t.idleTimer.Stop()
	t.maxTimer.Stop()
	t.cancel(context.Canceled)
}

func (t *promptActivityTimeout) handleIdleTimeout() {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	if t.idlePaused {
		t.mu.Unlock()
		return
	}
	if remaining := time.Until(t.deadline); remaining > 0 {
		t.idleTimer.Reset(remaining)
		t.mu.Unlock()
		return
	}
	idle := t.idle
	t.mu.Unlock()
	t.expire("idle", fmt.Errorf("ACP prompt 连续 %s 未收到活动更新: %w", idle, context.DeadlineExceeded))
}

func (t *promptActivityTimeout) expire(reason string, cause error) {
	var onExpire func(promptTimeoutEvent)
	var event promptTimeoutEvent
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.stopped = true
	if t.idleTimer != nil {
		t.idleTimer.Stop()
	}
	if t.maxTimer != nil {
		t.maxTimer.Stop()
	}
	now := time.Now()
	onExpire = t.onExpire
	event = promptTimeoutEvent{
		Reason:      strings.TrimSpace(reason),
		Cause:       cause,
		At:          now,
		Elapsed:     now.Sub(t.startedAt),
		IdleTimeout: t.idle,
		MaxDuration: t.max,
	}
	t.mu.Unlock()
	if onExpire != nil {
		onExpire(event)
	}
	t.cancel(cause)
}

func (r *runtimeManager) CancelSession(ctx context.Context, key runtimeKey, session Session, agent config.AgentConfig) error {
	key = normalizeRuntimeKey(key)
	client, release, ok := r.acquireCachedClientForSession(key, session.ACPSessionID)
	if client == nil {
		return nil
	}
	if ok {
		defer release()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return client.CancelSession(ctx, session.ACPSessionID)
}

func (r *runtimeManager) SetConfigOption(ctx context.Context, session Session, agent config.AgentConfig, configID string, value any) ([]acp.SessionConfigOption, error) {
	client, release, err := r.clientForRuntimeSession(ctx, currentRuntimeKey(session.Key), session, agent)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	options, err := client.SetConfigOption(ctx, session.ACPSessionID, configID, value)
	if err != nil {
		return nil, fmt.Errorf("session/set_config_option: %w", err)
	}
	return options, nil
}

func (r *runtimeManager) SetMode(ctx context.Context, session Session, agent config.AgentConfig, modeID string) error {
	client, release, err := r.clientForRuntimeSession(ctx, currentRuntimeKey(session.Key), session, agent)
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.SetMode(ctx, session.ACPSessionID, modeID); err != nil {
		return fmt.Errorf("session/set_mode: %w", err)
	}
	return nil
}

func (r *runtimeManager) SubscribeUpdates(key SessionKey, handler acp.UpdateHandler) func() {
	if handler == nil {
		return func() {}
	}
	key = normalizeSessionKey(key)
	r.mu.Lock()
	r.nextSubID++
	id := r.nextSubID
	if r.subscriptions[key] == nil {
		r.subscriptions[key] = make(map[int64]acp.UpdateHandler)
	}
	r.subscriptions[key][id] = handler
	client := r.slots[currentRuntimeKey(key)].client
	r.mu.Unlock()
	if client != nil {
		r.attachClientSubscriptions(currentRuntimeKey(key), client)
	}
	return func() {
		r.mu.Lock()
		if handlers := r.subscriptions[key]; handlers != nil {
			delete(handlers, id)
			if len(handlers) == 0 {
				delete(r.subscriptions, key)
			}
		}
		r.mu.Unlock()
	}
}

func (r *runtimeManager) TransitionCurrentSession(key SessionKey, expectedSessionID string, transition func() (Session, bool, error)) (Session, bool, error) {
	key = normalizeSessionKey(key)
	runtime := currentRuntimeKey(key)
	lock := r.transitionLock(runtime)
	lock.Lock()

	r.mu.Lock()
	activeSessionID := r.slots[runtime].sessionID
	r.mu.Unlock()
	if activeSessionID != "" && activeSessionID != expectedSessionID {
		lock.Unlock()
		return Session{}, false, nil
	}
	session, changed, err := transition()
	if err != nil || !changed {
		lock.Unlock()
		return session, changed, err
	}
	slots := r.detachSessionClients(key)
	r.setRuntimeSessionID(runtime, session.ACPSessionID)
	lock.Unlock()
	return session, true, r.closeSlots(slots)
}

func (r *runtimeManager) CloseRuntimeKey(key runtimeKey) error {
	key = normalizeRuntimeKey(key)
	lock := r.transitionLock(key)
	lock.Lock()
	slot := r.detachClient(key)
	lock.Unlock()
	return slot.close(r)
}

func (r *runtimeManager) CloseSession(key SessionKey) error {
	key = normalizeSessionKey(key)
	lock := r.transitionLock(currentRuntimeKey(key))
	lock.Lock()
	slots := r.detachSessionClients(key)
	lock.Unlock()
	return r.closeSlots(slots)
}

func (r *runtimeManager) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	keys := make([]runtimeKey, 0, len(r.slots))
	for key := range r.slots {
		keys = append(keys, key)
	}
	r.mu.Unlock()
	var firstErr error
	for _, key := range keys {
		if err := r.CloseRuntimeKey(key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *runtimeManager) reserveRuntimeSlot(key runtimeKey) (func(), error) {
	key = normalizeRuntimeKey(key)
	if r == nil {
		return func() {}, nil
	}
	if r.maxSlots <= 0 {
		return func() {}, nil
	}
	now := r.currentTime()
	for {
		victim, ok, err := r.reserveRuntimeSlotLocked(key, now)
		if err != nil {
			return nil, err
		}
		if ok {
			break
		}
		if err := r.closeRuntimeSlotForCapacity(victim); err != nil {
			return nil, err
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			r.releaseRuntimeSlotReservation(key)
		})
	}, nil
}

func (r *runtimeManager) releaseRuntimeSlotReservation(key runtimeKey) {
	key = normalizeRuntimeKey(key)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := r.slots[key]
	if slot.reserved <= 0 {
		return
	}
	slot.reserved--
	if slot.client == nil && slot.sessionID == "" {
		if slot.reserved <= 0 {
			delete(r.slots, key)
			return
		}
		r.slots[key] = slot
		return
	}
	r.slots[key] = slot
}

func (r *runtimeManager) reserveRuntimeSlotLocked(target runtimeKey, now time.Time) (runtimeKey, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if slot := r.slots[target]; slot.client != nil || slot.reserved > 0 {
		slot.reserved++
		slot.lastUsed = now
		r.slots[target] = slot
		return runtimeKey{}, true, nil
	}
	if r.maxSlots <= 0 || r.runtimeCapacityUsedLocked() < r.maxSlots {
		r.setRuntimeSlotReservationLocked(target, now)
		return runtimeKey{}, true, nil
	}
	var victim runtimeKey
	var victimLastUsed time.Time
	victimOK := false
	for key, slot := range r.slots {
		if !r.slotCapacityReclaimableLocked(slot) {
			continue
		}
		lastUsed := slot.lastUsed
		if lastUsed.IsZero() {
			lastUsed = now
		}
		if !victimOK || lastUsed.Before(victimLastUsed) {
			victim = key
			victimLastUsed = lastUsed
			victimOK = true
		}
	}
	if victimOK {
		return victim, false, nil
	}
	return runtimeKey{}, false, fmt.Errorf("%w: 当前 ACP runtime 已达到上限 %d，且没有可回收的 idle runtime", errACPRuntimeLimitReached, r.maxSlots)
}

func (r *runtimeManager) setRuntimeSlotReservationLocked(key runtimeKey, now time.Time) {
	slot := r.slots[key]
	slot.reserved++
	slot.lastUsed = now
	r.slots[key] = slot
}

func (r *runtimeManager) runtimeCapacityUsedLocked() int {
	used := 0
	for _, slot := range r.slots {
		if slot.client != nil || slot.reserved > 0 {
			used++
		}
	}
	return used
}

func (r *runtimeManager) closeRuntimeSlotForCapacity(key runtimeKey) error {
	key = normalizeRuntimeKey(key)
	lock := r.transitionLock(key)
	lock.Lock()
	r.mu.Lock()
	slot := r.slots[key]
	if r.slotCapacityReclaimableLocked(slot) {
		delete(r.slots, key)
	} else {
		slot = runtimeClientSlot{}
	}
	r.mu.Unlock()
	lock.Unlock()
	return slot.close(r)
}

func (r *runtimeManager) slotCapacityReclaimableLocked(slot runtimeClientSlot) bool {
	return slot.client != nil && slot.active == 0 && slot.reserved <= 0
}

func (r *runtimeManager) clientForRuntimeSession(ctx context.Context, key runtimeKey, session Session, agent config.AgentConfig) (*acp.Client, func(), error) {
	key = normalizeRuntimeKey(key)
	session.Key = normalizeSessionKey(session.Key)
	if session.ACPSessionID == "" {
		return nil, nil, fmt.Errorf("当前会话还没有 ACP session，请先发送 /new")
	}

	// 快速路径：已有匹配的 client，直接返回（短暂持锁）。
	if client, release, ok := r.acquireCachedClientForSession(key, session.ACPSessionID); ok {
		return client, release, nil
	}

	// 同 key 上若已有创建在进行，等待其结果；Start/Initialize/Resume 在锁外执行，
	// 不会长时间占用 transition 锁。
	build, leader := r.beginClientBuild(key)
	if !leader {
		client, info, err := build.wait(ctx)
		if err != nil {
			return nil, nil, err
		}
		// 复用 leader 结果前，确认它确实对应当前 session。
		if client != nil {
			if cached, release, ok := r.acquireCachedClientForSession(key, session.ACPSessionID); ok {
				return cached, release, nil
			}
		}
		_ = info
		return nil, nil, fmt.Errorf("%w: ACP runtime 正在重建", errACPSessionUnavailable)
	}

	// leader：在锁外启动并握手 client。
	releaseReservation, err := r.reserveRuntimeSlot(key)
	if err != nil {
		r.finishClientBuild(key, build)
		return nil, nil, err
	}
	client, info, err := r.startAndResumeRuntimeClient(ctx, session, agent)
	build.setResult(client, info, err)

	if err != nil {
		releaseReservation()
		r.finishClientBuild(key, build)
		if client != nil {
			_ = client.Close()
		}
		return nil, nil, err
	}

	// 短暂持 transition 锁做最终交换，避免与并发 close/swap 竞争。
	lock := r.transitionLock(key)
	lock.Lock()
	// 交换前再确认一次：可能有更新的 client 已被插入（例如被 close 后重建）。
	if existing, release, ok := r.acquireCachedClientForSession(key, session.ACPSessionID); ok {
		lock.Unlock()
		releaseReservation()
		r.finishClientBuild(key, build)
		_ = client.Close()
		return existing, release, nil
	}
	old := r.swapClient(key, client, session.ACPSessionID)
	r.attachClientSubscriptions(key, client)
	client, release, _ := r.acquireCachedClientForSession(key, session.ACPSessionID)
	lock.Unlock()
	releaseReservation()
	r.finishClientBuild(key, build)

	_ = old.closeReplacedBy(r, client)
	r.dispatchSessionInfo(session.Key, session.ACPSessionID, info)
	if client == nil {
		return nil, nil, fmt.Errorf("%w: ACP runtime 已关闭", errACPSessionUnavailable)
	}
	return client, release, nil
}

func (r *runtimeManager) startAndResumeRuntimeClient(ctx context.Context, session Session, agent config.AgentConfig) (*acp.Client, acp.SessionInfo, error) {
	if r.startAndResumeClientFunc != nil {
		return r.startAndResumeClientFunc(ctx, session, agent)
	}
	return r.startAndResumeClient(ctx, session, agent)
}

// acquireCachedClientForSession 在短临界区内返回当前缓存且 sessionID 匹配的 client，
// 并增加 active 计数，避免 idle cleaner 关闭正在使用的 ACP 子进程。
func (r *runtimeManager) acquireCachedClientForSession(key runtimeKey, sessionID string) (*acp.Client, func(), bool) {
	key = normalizeRuntimeKey(key)
	r.mu.Lock()
	slot := r.slots[key]
	if slot.client == nil || slot.sessionID != sessionID {
		r.mu.Unlock()
		return nil, func() {}, false
	}
	slot.active++
	slot.lastUsed = r.currentTime()
	r.slots[key] = slot
	client := slot.client
	r.mu.Unlock()
	return client, func() {
		r.releaseRuntimeClient(key, client)
	}, true
}

func (r *runtimeManager) releaseRuntimeClient(key runtimeKey, client *acp.Client) {
	key = normalizeRuntimeKey(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := r.slots[key]
	if slot.client != client {
		return
	}
	if slot.active > 0 {
		slot.active--
	}
	slot.lastUsed = r.currentTime()
	r.slots[key] = slot
}

func (r *runtimeManager) touchRuntimeKey(key runtimeKey) {
	key = normalizeRuntimeKey(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := r.slots[key]
	if slot.client == nil && slot.sessionID == "" {
		return
	}
	slot.lastUsed = r.currentTime()
	r.slots[key] = slot
}

func (r *runtimeManager) currentTime() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// startAndResumeClient 在不持有 transition 锁的情况下启动 ACP 子进程、initialize 并 resume/load。
func (r *runtimeManager) startAndResumeClient(ctx context.Context, session Session, agent config.AgentConfig) (*acp.Client, acp.SessionInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, acpRequestTimeout)
	defer cancel()
	client, err := acp.Start(ctx, agent, session.Workspace)
	if err != nil {
		return nil, acp.SessionInfo{}, err
	}
	if err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		return nil, acp.SessionInfo{}, fmt.Errorf("initialize: %w", err)
	}
	sessionInfo, err := client.ResumeSession(ctx, session.ACPSessionID, session.Cwd)
	if err != nil {
		loadInfo, loadErr := client.LoadSession(ctx, session.ACPSessionID, session.Cwd)
		if loadErr != nil {
			_ = client.Close()
			return nil, acp.SessionInfo{}, fmt.Errorf("%w: session/resume: %v; session/load fallback: %v", errACPSessionUnavailable, err, loadErr)
		}
		return client, loadInfo, nil
	}
	return client, sessionInfo, nil
}

func (r *runtimeManager) swapClient(key runtimeKey, client *acp.Client, sessionID string) runtimeClientSlot {
	key = normalizeRuntimeKey(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.slots[key]
	r.slots[key] = runtimeClientSlot{client: client, sessionID: sessionID, lastUsed: r.currentTime(), reserved: old.reserved}
	return old
}
func (r *runtimeManager) detachClient(key runtimeKey) runtimeClientSlot {
	key = normalizeRuntimeKey(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := r.slots[key]
	delete(r.slots, key)
	return slot
}

func (r *runtimeManager) detachBrokenRuntimeClient(key runtimeKey, broken *acp.Client) {
	key = normalizeRuntimeKey(key)
	lock := r.transitionLock(key)
	lock.Lock()
	r.mu.Lock()
	slot := r.slots[key]
	if slot.client == broken {
		delete(r.slots, key)
	} else {
		slot = runtimeClientSlot{}
	}
	r.mu.Unlock()
	lock.Unlock()
	_ = slot.close(r)
}

func isBrokenACPClientPipeError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, acp.ErrServerOutputClosed) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, os.ErrClosed) ||
		isPlatformBrokenPipeError(err) ||
		strings.Contains(strings.ToLower(err.Error()), "broken pipe")
}

func (r *runtimeManager) detachSessionClients(key SessionKey) []runtimeClientSlot {
	key = normalizeSessionKey(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	slots := make([]runtimeClientSlot, 0)
	for runtime, slot := range r.slots {
		if runtime.SessionKey != key {
			continue
		}
		slots = append(slots, slot)
		delete(r.slots, runtime)
	}
	return slots
}

func (r *runtimeManager) closeSlots(slots []runtimeClientSlot) error {
	var firstErr error
	for _, slot := range slots {
		if err := slot.close(r); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *runtimeManager) closeIdleRuntimeSlots(busy runtimeBusyFunc) error {
	if r == nil || r.idleTimeout <= 0 {
		return nil
	}
	now := r.currentTime()
	keys := r.idleRuntimeKeys(now, busy)
	var firstErr error
	for _, key := range keys {
		if err := r.closeIdleRuntimeKey(key, now, busy); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *runtimeManager) idleRuntimeKeys(now time.Time, busy runtimeBusyFunc) []runtimeKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]runtimeKey, 0)
	for key, slot := range r.slots {
		if !r.slotIdleLocked(slot, now) {
			continue
		}
		keys = append(keys, key)
	}
	return keys
}

func (r *runtimeManager) closeIdleRuntimeKey(key runtimeKey, now time.Time, busy runtimeBusyFunc) error {
	key = normalizeRuntimeKey(key)
	lock := r.transitionLock(key)
	lock.Lock()
	r.mu.Lock()
	slot := r.slots[key]
	idle := r.slotIdleLocked(slot, now)
	r.mu.Unlock()
	if idle && busy != nil && busy(key) {
		idle = false
	}
	r.mu.Lock()
	slot = r.slots[key]
	if idle && r.slotIdleLocked(slot, now) {
		delete(r.slots, key)
	} else {
		slot = runtimeClientSlot{}
	}
	r.mu.Unlock()
	lock.Unlock()
	return slot.close(r)
}

func (r *runtimeManager) slotIdleLocked(slot runtimeClientSlot, now time.Time) bool {
	if slot.client == nil || slot.active > 0 {
		return false
	}
	lastUsed := slot.lastUsed
	if lastUsed.IsZero() {
		lastUsed = now
	}
	return now.Sub(lastUsed) >= r.idleTimeout
}

func (r *runtimeManager) setRuntimeSessionID(key runtimeKey, sessionID string) {
	key = normalizeRuntimeKey(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := r.slots[key]
	slot.sessionID = sessionID
	slot.lastUsed = r.currentTime()
	r.slots[key] = slot
}

func (r *runtimeManager) transitionLock(key runtimeKey) *sync.Mutex {
	key = normalizeRuntimeKey(key)
	hash := uint64(1469598103934665603)
	for _, value := range []string{key.BotID, sessionKeySource(key.SessionKey), sessionKeyMainID(key.SessionKey), key.SubID, key.Scope, key.RunID} {
		for i := 0; i < len(value); i++ {
			hash ^= uint64(value[i])
			hash *= 1099511628211
		}
		hash ^= 0xff
		hash *= 1099511628211
	}
	return &r.transitions[hash%uint64(len(r.transitions))]
}

func (r *runtimeManager) closeClient(client *acp.Client, sessionID string) error {
	if client == nil {
		return nil
	}
	var firstErr error
	if sessionID != "" && client.SupportsCloseSession() {
		// Runtime shutdown may be triggered by idle cleanup or session
		// transitions rather than an inbound request; bound the close RPC.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := client.CloseSession(ctx, sessionID); err != nil && firstErr == nil {
			firstErr = err
		}
		cancel()
	}
	if err := client.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (r *runtimeManager) attachClientSubscriptions(key runtimeKey, client *acp.Client) {
	if client == nil {
		return
	}
	key = normalizeRuntimeKey(key)
	r.mu.Lock()
	slot := r.slots[key]
	if slot.client != client {
		r.mu.Unlock()
		return
	}
	if slot.unsub != nil {
		slot.unsub()
	}
	unsub := client.SubscribeUpdates(func(sessionID string, update acp.SessionUpdate) {
		r.mu.Lock()
		handlers := make([]acp.UpdateHandler, 0, len(r.subscriptions[key.SessionKey]))
		for _, handler := range r.subscriptions[key.SessionKey] {
			handlers = append(handlers, handler)
		}
		r.mu.Unlock()
		for _, handler := range handlers {
			handler(sessionID, update)
		}
	})
	slot.unsub = unsub
	r.slots[key] = slot
	r.mu.Unlock()
}

func (r *runtimeManager) dispatchSessionInfo(key SessionKey, sessionID string, info acp.SessionInfo) {
	key = normalizeSessionKey(key)
	if info.Meta != nil {
		r.dispatchUpdate(key, sessionID, acp.SessionUpdate{
			SessionUpdate: "session_info_update",
			Meta:          info.Meta,
		})
	}
	if len(info.AvailableCommands) > 0 {
		r.dispatchUpdate(key, sessionID, acp.SessionUpdate{
			SessionUpdate:     "available_commands_update",
			AvailableCommands: info.AvailableCommands,
		})
	}
	if len(info.ConfigOptions) > 0 {
		r.dispatchUpdate(key, sessionID, acp.SessionUpdate{
			SessionUpdate: "config_option_update",
			ConfigOptions: info.ConfigOptions,
		})
	}
	if info.Models != nil || info.Mode != nil {
		r.dispatchUpdate(key, sessionID, acp.SessionUpdate{
			SessionUpdate: "session_state_update",
			Models:        info.Models,
			Mode:          info.Mode,
		})
	}
}

func (r *runtimeManager) dispatchUpdate(key SessionKey, sessionID string, update acp.SessionUpdate) {
	key = normalizeSessionKey(key)
	r.mu.Lock()
	handlers := make([]acp.UpdateHandler, 0, len(r.subscriptions[key]))
	for _, handler := range r.subscriptions[key] {
		handlers = append(handlers, handler)
	}
	r.mu.Unlock()
	for _, handler := range handlers {
		handler(sessionID, update)
	}
}
