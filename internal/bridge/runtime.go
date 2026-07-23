package bridge

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

const acpRequestTimeout = 10 * time.Minute

type acpRuntime interface {
	NewSession(ctx context.Context, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (acp.SessionInfo, error)
	Prompt(ctx context.Context, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (string, error)
	CancelSession(ctx context.Context, session Session, agent config.AgentConfig) error
	SetConfigOption(ctx context.Context, session Session, agent config.AgentConfig, configID string, value any) ([]acp.SessionConfigOption, error)
	SubscribeUpdates(key SessionKey, handler acp.UpdateHandler) func()
	CloseSession(key SessionKey) error
	Shutdown(ctx context.Context) error
}

var _ acpRuntime = (*runtimeManager)(nil)

type runtimeManager struct {
	mu            sync.Mutex
	clients       map[SessionKey]*acp.Client
	subscriptions map[SessionKey]map[int64]acp.UpdateHandler
	clientUnsub   map[SessionKey]func()
	nextSubID     int64
}

func newRuntimeManager() *runtimeManager {
	return &runtimeManager{
		clients:       make(map[SessionKey]*acp.Client),
		subscriptions: make(map[SessionKey]map[int64]acp.UpdateHandler),
		clientUnsub:   make(map[SessionKey]func()),
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

	r.replaceClient(key, client)
	return sessionInfo, nil
}

func (r *runtimeManager) Prompt(ctx context.Context, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (string, error) {
	client, err := r.clientForSession(ctx, session, agent)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, acpRequestTimeout)
	defer cancel()
	output, err := client.PromptWithOptions(ctx, session.ACPSessionID, text, opts)
	if err != nil {
		return output, fmt.Errorf("session/prompt: %w", err)
	}
	return output, nil
}

func (r *runtimeManager) CancelSession(ctx context.Context, session Session, agent config.AgentConfig) error {
	client, err := r.clientForSession(ctx, session, agent)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return client.CancelSession(ctx, session.ACPSessionID)
}

func (r *runtimeManager) SetConfigOption(ctx context.Context, session Session, agent config.AgentConfig, configID string, value any) ([]acp.SessionConfigOption, error) {
	client, err := r.clientForSession(ctx, session, agent)
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
	client := r.clients[key]
	r.mu.Unlock()
	if client != nil {
		r.attachClientSubscriptions(key, client)
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

func (r *runtimeManager) CloseSession(key SessionKey) error {
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

func (r *runtimeManager) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	clients := r.clients
	r.clients = make(map[SessionKey]*acp.Client)
	unsubs := r.clientUnsub
	r.clientUnsub = make(map[SessionKey]func())
	r.mu.Unlock()
	for _, unsub := range unsubs {
		if unsub != nil {
			unsub()
		}
	}
	for _, client := range clients {
		_ = client.Close()
	}
	return nil
}

func (r *runtimeManager) clientForSession(ctx context.Context, session Session, agent config.AgentConfig) (*acp.Client, error) {
	if session.ACPSessionID == "" {
		return nil, fmt.Errorf("当前会话还没有 ACP session，请先发送 /new")
	}
	r.mu.Lock()
	client := r.clients[session.Key]
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
			return nil, fmt.Errorf("session/resume: %w; session/load fallback: %w", err, loadErr)
		} else {
			sessionInfo = loadInfo
		}
	}
	r.replaceClient(session.Key, client)
	r.dispatchSessionInfo(session.Key, session.ACPSessionID, sessionInfo)
	return client, nil
}

func (r *runtimeManager) replaceClient(key SessionKey, client *acp.Client) {
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

func (r *runtimeManager) attachClientSubscriptions(key SessionKey, client *acp.Client) {
	if client == nil {
		return
	}
	r.mu.Lock()
	if old := r.clientUnsub[key]; old != nil {
		old()
	}
	unsub := client.SubscribeUpdates(func(sessionID string, update acp.SessionUpdate) {
		r.mu.Lock()
		handlers := make([]acp.UpdateHandler, 0, len(r.subscriptions[key]))
		for _, handler := range r.subscriptions[key] {
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
