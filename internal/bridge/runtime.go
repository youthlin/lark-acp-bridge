package bridge

import (
	"context"
	"errors"
	"fmt"
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
)

var errACPSessionUnavailable = errors.New("acp session unavailable")

type acpRuntime interface {
	NewSession(ctx context.Context, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (acpSessionCandidate, error)
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
	runtimeScopeCurrent = "current"
	runtimeScopeWiki    = "wiki"
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
	clients       map[runtimeKey]*acp.Client
	sessionIDs    map[runtimeKey]string
	subscriptions map[SessionKey]map[int64]acp.UpdateHandler
	clientUnsub   map[runtimeKey]func()
	nextSubID     int64
	transitions   [64]sync.Mutex
}

func newRuntimeManager() *runtimeManager {
	return &runtimeManager{
		clients:       make(map[runtimeKey]*acp.Client),
		sessionIDs:    make(map[runtimeKey]string),
		subscriptions: make(map[SessionKey]map[int64]acp.UpdateHandler),
		clientUnsub:   make(map[runtimeKey]func()),
	}
}

func (r *runtimeManager) NewSession(ctx context.Context, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (acpSessionCandidate, error) {
	key = normalizeSessionKey(key)
	ctx, cancel := context.WithTimeout(ctx, acpRequestTimeout)
	defer cancel()

	client, err := acp.Start(ctx, agent, workspace)
	if err != nil {
		return nil, err
	}
	if err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	sessionInfo, err := client.NewSession(ctx, cwd)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("session/new: %w", err)
	}

	return &runtimeSessionCandidate{
		manager: r,
		key:     currentRuntimeKey(key),
		client:  client,
		info:    sessionInfo,
	}, nil
}

type runtimeSessionCandidate struct {
	mu        sync.Mutex
	manager   *runtimeManager
	key       runtimeKey
	client    *acp.Client
	info      acp.SessionInfo
	committed bool
	closed    bool
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
			_ = c.manager.closeClient(c.client, c.info.SessionID)
			return err
		}
	}
	old, oldSessionID, oldUnsub := c.manager.swapClient(c.key, c.client, c.info.SessionID)
	c.manager.attachClientSubscriptions(c.key, c.client)
	c.committed = true
	transition.Unlock()

	if oldUnsub != nil {
		oldUnsub()
	}
	if old != nil && old != c.client {
		_ = c.manager.closeClient(old, oldSessionID)
	}
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
	_ = c.manager.closeClient(c.client, c.info.SessionID)
}

func (r *runtimeManager) Prompt(ctx context.Context, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (acp.PromptResult, error) {
	return r.PromptWithRuntimeKey(ctx, currentRuntimeKey(session.Key), session, agent, text, opts)
}

func (r *runtimeManager) PromptWithRuntimeKey(ctx context.Context, key runtimeKey, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (acp.PromptResult, error) {
	client, err := r.clientForRuntimeSession(ctx, key, session, agent)
	if err != nil {
		return acp.PromptResult{}, err
	}
	return promptWithClient(ctx, client, session.ACPSessionID, text, opts)
}

func promptWithClient(ctx context.Context, client *acp.Client, sessionID string, text string, opts acp.PromptOptions) (acp.PromptResult, error) {
	timeout := newPromptActivityTimeout(ctx, acpPromptIdleTimeout, acpPromptMaxDuration)
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

type promptActivityTimeout struct {
	ctx        context.Context
	cancel     context.CancelCauseFunc
	mu         sync.Mutex
	idle       time.Duration
	deadline   time.Time
	idleTimer  *time.Timer
	maxTimer   *time.Timer
	idlePaused bool
	stopped    bool
}

func newPromptActivityTimeout(parent context.Context, idleTimeout, maxDuration time.Duration) *promptActivityTimeout {
	ctx, cancel := context.WithCancelCause(parent)
	timeout := &promptActivityTimeout{
		ctx:      ctx,
		cancel:   cancel,
		idle:     idleTimeout,
		deadline: time.Now().Add(idleTimeout),
	}
	timeout.mu.Lock()
	timeout.idleTimer = time.AfterFunc(idleTimeout, timeout.handleIdleTimeout)
	timeout.maxTimer = time.AfterFunc(maxDuration, func() {
		timeout.expire(fmt.Errorf("ACP prompt 执行超过绝对上限 %s: %w", maxDuration, context.DeadlineExceeded))
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
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	if t.idlePaused {
		return
	}
	if remaining := time.Until(t.deadline); remaining > 0 {
		t.idleTimer.Reset(remaining)
		return
	}
	t.stopped = true
	t.maxTimer.Stop()
	t.cancel(fmt.Errorf("ACP prompt 连续 %s 未收到活动更新: %w", t.idle, context.DeadlineExceeded))
}

func (t *promptActivityTimeout) expire(cause error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	t.stopped = true
	t.idleTimer.Stop()
	t.cancel(cause)
}

func (r *runtimeManager) CancelSession(ctx context.Context, key runtimeKey, session Session, agent config.AgentConfig) error {
	key = normalizeRuntimeKey(key)
	r.mu.Lock()
	client := r.clients[key]
	r.mu.Unlock()
	if client == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return client.CancelSession(ctx, session.ACPSessionID)
}

func (r *runtimeManager) SetConfigOption(ctx context.Context, session Session, agent config.AgentConfig, configID string, value any) ([]acp.SessionConfigOption, error) {
	client, err := r.clientForRuntimeSession(ctx, currentRuntimeKey(session.Key), session, agent)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	options, err := client.SetConfigOption(ctx, session.ACPSessionID, configID, value)
	if err != nil {
		return nil, fmt.Errorf("session/set_config_option: %w", err)
	}
	return options, nil
}

func (r *runtimeManager) SetMode(ctx context.Context, session Session, agent config.AgentConfig, modeID string) error {
	client, err := r.clientForRuntimeSession(ctx, currentRuntimeKey(session.Key), session, agent)
	if err != nil {
		return err
	}
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
	client := r.clients[currentRuntimeKey(key)]
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
	activeSessionID := r.sessionIDs[runtime]
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
	clients := r.detachSessionClients(key)
	r.setRuntimeSessionID(runtime, session.ACPSessionID)
	lock.Unlock()
	var firstErr error
	for _, client := range clients {
		if client.unsub != nil {
			client.unsub()
		}
		if client.client != nil {
			if err := r.closeClient(client.client, client.sessionID); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return session, true, firstErr
}

func (r *runtimeManager) CloseRuntimeKey(key runtimeKey) error {
	key = normalizeRuntimeKey(key)
	lock := r.transitionLock(key)
	lock.Lock()
	client, sessionID, unsub := r.detachClient(key)
	lock.Unlock()
	if unsub != nil {
		unsub()
	}
	if client != nil {
		return r.closeClient(client, sessionID)
	}
	return nil
}

func (r *runtimeManager) CloseSession(key SessionKey) error {
	key = normalizeSessionKey(key)
	lock := r.transitionLock(currentRuntimeKey(key))
	lock.Lock()
	clients := r.detachSessionClients(key)
	lock.Unlock()
	var firstErr error
	for _, client := range clients {
		if client.unsub != nil {
			client.unsub()
		}
		if client.client != nil {
			if err := r.closeClient(client.client, client.sessionID); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (r *runtimeManager) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	unsubs := r.clientUnsub
	r.clientUnsub = make(map[runtimeKey]func())
	keys := make([]runtimeKey, 0, len(r.clients))
	for key := range r.clients {
		keys = append(keys, key)
	}
	r.mu.Unlock()
	for _, unsub := range unsubs {
		if unsub != nil {
			unsub()
		}
	}
	var firstErr error
	for _, key := range keys {
		if err := r.CloseRuntimeKey(key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *runtimeManager) clientForRuntimeSession(ctx context.Context, key runtimeKey, session Session, agent config.AgentConfig) (*acp.Client, error) {
	key = normalizeRuntimeKey(key)
	session.Key = normalizeSessionKey(session.Key)
	if session.ACPSessionID == "" {
		return nil, fmt.Errorf("当前会话还没有 ACP session，请先发送 /new")
	}
	lock := r.transitionLock(key)
	lock.Lock()
	r.mu.Lock()
	client := r.clients[key]
	activeSessionID := r.sessionIDs[key]
	r.mu.Unlock()
	if client != nil && activeSessionID == session.ACPSessionID {
		lock.Unlock()
		return client, nil
	}
	if activeSessionID != "" && activeSessionID != session.ACPSessionID {
		lock.Unlock()
		return nil, fmt.Errorf("%w: runtime session %s 与当前映射 %s 不一致", errACPSessionUnavailable, activeSessionID, session.ACPSessionID)
	}

	ctx, cancel := context.WithTimeout(ctx, acpRequestTimeout)
	defer cancel()
	client, err := acp.Start(ctx, agent, session.Workspace)
	if err != nil {
		lock.Unlock()
		return nil, err
	}
	if err := client.Initialize(ctx); err != nil {
		lock.Unlock()
		_ = client.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	sessionInfo, err := client.ResumeSession(ctx, session.ACPSessionID, session.Cwd)
	if err != nil {
		if loadInfo, loadErr := client.LoadSession(ctx, session.ACPSessionID, session.Cwd); loadErr != nil {
			lock.Unlock()
			_ = client.Close()
			return nil, fmt.Errorf("%w: session/resume: %v; session/load fallback: %v", errACPSessionUnavailable, err, loadErr)
		} else {
			sessionInfo = loadInfo
		}
	}
	old, oldSessionID, oldUnsub := r.swapClient(key, client, session.ACPSessionID)
	r.attachClientSubscriptions(key, client)
	lock.Unlock()
	if oldUnsub != nil {
		oldUnsub()
	}
	if old != nil && old != client {
		_ = r.closeClient(old, oldSessionID)
	}
	r.dispatchSessionInfo(session.Key, session.ACPSessionID, sessionInfo)
	return client, nil
}

func (r *runtimeManager) swapClient(key runtimeKey, client *acp.Client, sessionID string) (*acp.Client, string, func()) {
	key = normalizeRuntimeKey(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.clients[key]
	oldSessionID := r.sessionIDs[key]
	oldUnsub := r.clientUnsub[key]
	delete(r.clientUnsub, key)
	r.clients[key] = client
	r.sessionIDs[key] = sessionID
	return old, oldSessionID, oldUnsub
}

func (r *runtimeManager) detachClient(key runtimeKey) (*acp.Client, string, func()) {
	key = normalizeRuntimeKey(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	client := r.clients[key]
	sessionID := r.sessionIDs[key]
	unsub := r.clientUnsub[key]
	delete(r.clients, key)
	delete(r.sessionIDs, key)
	delete(r.clientUnsub, key)
	return client, sessionID, unsub
}

type detachedRuntimeClient struct {
	client    *acp.Client
	sessionID string
	unsub     func()
}

func (r *runtimeManager) detachSessionClients(key SessionKey) []detachedRuntimeClient {
	key = normalizeSessionKey(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make(map[runtimeKey]struct{})
	for runtime := range r.clients {
		if runtime.SessionKey == key {
			keys[runtime] = struct{}{}
		}
	}
	for runtime := range r.sessionIDs {
		if runtime.SessionKey == key {
			keys[runtime] = struct{}{}
		}
	}
	for runtime := range r.clientUnsub {
		if runtime.SessionKey != key {
			continue
		}
		keys[runtime] = struct{}{}
	}
	clients := make([]detachedRuntimeClient, 0, len(keys))
	for runtime := range keys {
		clients = append(clients, detachedRuntimeClient{
			client:    r.clients[runtime],
			sessionID: r.sessionIDs[runtime],
			unsub:     r.clientUnsub[runtime],
		})
		delete(r.clients, runtime)
		delete(r.sessionIDs, runtime)
		delete(r.clientUnsub, runtime)
	}
	return clients
}

func (r *runtimeManager) setRuntimeSessionID(key runtimeKey, sessionID string) {
	key = normalizeRuntimeKey(key)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessionIDs[key] = sessionID
}

func (r *runtimeManager) transitionLock(key runtimeKey) *sync.Mutex {
	key = normalizeRuntimeKey(key)
	hash := uint64(1469598103934665603)
	for _, value := range []string{key.BotID, sessionKeySource(key.SessionKey), sessionKeyMainID(key.SessionKey), key.ChatID, key.SubID, key.Scope, key.RunID} {
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
	if old := r.clientUnsub[key]; old != nil {
		old()
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
	r.clientUnsub[key] = unsub
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
