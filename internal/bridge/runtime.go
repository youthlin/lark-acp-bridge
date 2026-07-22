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
	NewSession(ctx context.Context, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (string, error)
	Prompt(ctx context.Context, session Session, agent config.AgentConfig, text string, opts acp.PromptOptions) (string, error)
	CloseSession(key SessionKey) error
	Shutdown(ctx context.Context) error
}

var _ acpRuntime = (*runtimeManager)(nil)

type runtimeManager struct {
	mu      sync.Mutex
	clients map[SessionKey]*acp.Client
}

func newRuntimeManager() *runtimeManager {
	return &runtimeManager{
		clients: make(map[SessionKey]*acp.Client),
	}
}

func (r *runtimeManager) NewSession(ctx context.Context, key SessionKey, agentName string, agent config.AgentConfig, cwd string, workspace string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, acpRequestTimeout)
	defer cancel()

	client, err := acp.Start(ctx, agent, workspace)
	if err != nil {
		return "", err
	}
	if err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		return "", fmt.Errorf("initialize: %w", err)
	}
	sessionID, err := client.NewSession(ctx, cwd)
	if err != nil {
		_ = client.Close()
		return "", fmt.Errorf("session/new: %w", err)
	}

	r.mu.Lock()
	old := r.clients[key]
	r.clients[key] = client
	r.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return sessionID, nil
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

func (r *runtimeManager) CloseSession(key SessionKey) error {
	r.mu.Lock()
	client := r.clients[key]
	delete(r.clients, key)
	r.mu.Unlock()
	if client != nil {
		return client.Close()
	}
	return nil
}

func (r *runtimeManager) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	clients := r.clients
	r.clients = make(map[SessionKey]*acp.Client)
	r.mu.Unlock()
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
	if err := client.LoadSession(ctx, session.ACPSessionID, session.Cwd); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("session/load: %w", err)
	}
	r.mu.Lock()
	r.clients[session.Key] = client
	r.mu.Unlock()
	return client, nil
}
