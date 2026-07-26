package bridge

import (
	"context"
	"errors"
	"fmt"
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
	NewSession(ctx context.Context, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (acp.SessionInfo, error)
	Prompt(ctx context.Context, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (acp.PromptResult, error)
	PromptWithRuntimeKey(ctx context.Context, key runtimeKey, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (acp.PromptResult, error)
	CancelSession(ctx context.Context, key runtimeKey, session Session, agent config.AgentConfig) error
	SetConfigOption(ctx context.Context, session Session, agent config.AgentConfig, configID string, value any) ([]acp.SessionConfigOption, error)
	SubscribeUpdates(key SessionKey, handler acp.UpdateHandler) func()
	CloseRuntimeKey(key runtimeKey) error
	CloseSession(key SessionKey) error
	Shutdown(ctx context.Context) error
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
	return runtimeKey{SessionKey: key, Scope: runtimeScopeCurrent}
}

func wikiRuntimeKey(key SessionKey, generation int64, sessionID string) runtimeKey {
	return runtimeKey{
		SessionKey: key,
		Scope:      runtimeScopeWiki,
		RunID:      fmt.Sprintf("%d:%s", generation, sessionID),
	}
}

type runtimeManager struct {
	mu            sync.Mutex
	clients       map[runtimeKey]*acp.Client
	subscriptions map[SessionKey]map[int64]acp.UpdateHandler
	clientUnsub   map[runtimeKey]func()
	nextSubID     int64
}

func newRuntimeManager() *runtimeManager {
	return &runtimeManager{
		clients:       make(map[runtimeKey]*acp.Client),
		subscriptions: make(map[SessionKey]map[int64]acp.UpdateHandler),
		clientUnsub:   make(map[runtimeKey]func()),
	}
}

func (r *runtimeManager) NewSession(ctx context.Context, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (acp.SessionInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, acpRequestTimeout)
	defer cancel()

	client, err := acp.Start(ctx, agent, workspace)
	if err != nil {
		return acp.SessionInfo{}, err
	}
	if err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		return acp.SessionInfo{}, fmt.Errorf("initialize: %w", err)
	}
	sessionInfo, err := client.NewSession(ctx, cwd)
	if err != nil {
		_ = client.Close()
		return acp.SessionInfo{}, fmt.Errorf("session/new: %w", err)
	}

	r.replaceClient(currentRuntimeKey(key), client)
	return sessionInfo, nil
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

func (r *runtimeManager) SubscribeUpdates(key SessionKey, handler acp.UpdateHandler) func() {
	if handler == nil {
		return func() {}
	}
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

func (r *runtimeManager) CloseRuntimeKey(key runtimeKey) error {
	r.mu.Lock()
	client := r.clients[key]
	delete(r.clients, key)
	unsub := r.clientUnsub[key]
	delete(r.clientUnsub, key)
	r.mu.Unlock()
	if unsub != nil {
		unsub()
	}
	if client != nil {
		return client.Close()
	}
	return nil
}

func (r *runtimeManager) CloseSession(key SessionKey) error {
	r.mu.Lock()
	keys := make([]runtimeKey, 0)
	for runtimeKey := range r.clients {
		if runtimeKey.SessionKey == key {
			keys = append(keys, runtimeKey)
		}
	}
	r.mu.Unlock()
	var firstErr error
	for _, runtimeKey := range keys {
		if err := r.CloseRuntimeKey(runtimeKey); err != nil && firstErr == nil {
			firstErr = err
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
	if session.ACPSessionID == "" {
		return nil, fmt.Errorf("当前会话还没有 ACP session，请先发送 /new")
	}
	r.mu.Lock()
	client := r.clients[key]
	r.mu.Unlock()
	if client != nil {
		return client, nil
	}

	ctx, cancel := context.WithTimeout(ctx, acpRequestTimeout)
	defer cancel()
	client, err := acp.Start(ctx, agent, session.Workspace)
	if err != nil {
		return nil, err
	}
	if err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	sessionInfo, err := client.ResumeSession(ctx, session.ACPSessionID, session.Cwd)
	if err != nil {
		if loadInfo, loadErr := client.LoadSession(ctx, session.ACPSessionID, session.Cwd); loadErr != nil {
			_ = client.Close()
			return nil, fmt.Errorf("%w: session/resume: %v; session/load fallback: %v", errACPSessionUnavailable, err, loadErr)
		} else {
			sessionInfo = loadInfo
		}
	}
	r.replaceClient(key, client)
	r.dispatchSessionInfo(session.Key, session.ACPSessionID, sessionInfo)
	return client, nil
}

func (r *runtimeManager) replaceClient(key runtimeKey, client *acp.Client) {
	r.mu.Lock()
	old := r.clients[key]
	oldUnsub := r.clientUnsub[key]
	delete(r.clientUnsub, key)
	r.clients[key] = client
	r.mu.Unlock()
	if oldUnsub != nil {
		oldUnsub()
	}
	if old != nil && old != client {
		_ = old.Close()
	}
	r.attachClientSubscriptions(key, client)
}

func (r *runtimeManager) attachClientSubscriptions(key runtimeKey, client *acp.Client) {
	if client == nil {
		return
	}
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
