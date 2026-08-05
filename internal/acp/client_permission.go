package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type permissionScope struct {
	generation     int64
	toolGeneration int64
	ctx            context.Context
	handler        PermissionRequestHandler
}

type PermissionRequestHandler func(context.Context, PermissionRequest) (PermissionOutcome, error)

type PermissionRequest struct {
	RequestID     string                `json:"-"`
	SessionID     string                `json:"sessionId"`
	ToolCall      PermissionToolCallRef `json:"toolCall"`
	Options       []PermissionOption    `json:"options"`
	ToolCallState *ToolCallInfo         `json:"-"`
	Raw           json.RawMessage       `json:"-"`
}

type PermissionToolCallRef struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Status     string          `json:"status,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"`
	Locations  json.RawMessage `json:"locations,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
}

type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name,omitempty"`
	Kind     string `json:"kind"`
}

type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

type PermissionResult struct {
	Outcome PermissionOutcome `json:"outcome"`
}

func (c *Client) handleRequestPermission(id *RequestID, raw json.RawMessage) (any, error) {
	var req PermissionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("解析 session/request_permission 参数: %w", err)
	}
	req = normalizePermissionRequest(req)
	if id != nil {
		req.RequestID = id.Key()
	}
	req.Raw = append(json.RawMessage(nil), raw...)
	if tool := c.toolCallSnapshot(req.SessionID, req.ToolCall.ToolCallID); tool != nil {
		req.ToolCallState = tool
	}
	ctx, handler := c.permissionRequestHandler(req)
	if ctx != nil && ctx.Err() != nil {
		return PermissionResult{Outcome: PermissionOutcome{Outcome: "cancelled"}}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cleanup := c.trackAgentRequest(ctx, id)
	defer cleanup()
	if handler != nil {
		outcome, err := handler(ctx, req)
		if err != nil {
			if errors.Is(err, context.Canceled) || (ctx != nil && ctx.Err() != nil) {
				return PermissionResult{Outcome: PermissionOutcome{Outcome: "cancelled"}}, nil
			}
			return nil, err
		}
		switch outcome.Outcome {
		case "selected":
		case "cancelled":
			outcome.OptionID = ""
		default:
			outcome = PermissionOutcome{Outcome: "cancelled"}
		}
		if outcome.Outcome == "selected" && !permissionOptionExists(req.Options, outcome.OptionID) {
			outcome = PermissionOutcome{Outcome: "cancelled"}
		}
		return PermissionResult{Outcome: outcome}, nil
	}
	for _, option := range req.Options {
		switch option.Kind {
		case "reject_once", "reject_always":
			if strings.TrimSpace(option.OptionID) != "" {
				return PermissionResult{Outcome: PermissionOutcome{Outcome: "selected", OptionID: option.OptionID}}, nil
			}
		}
	}
	return PermissionResult{Outcome: PermissionOutcome{Outcome: "cancelled"}}, nil
}

func permissionOptionExists(options []PermissionOption, optionID string) bool {
	optionID = strings.TrimSpace(optionID)
	if optionID == "" {
		return false
	}
	for _, option := range options {
		if strings.TrimSpace(option.OptionID) == optionID {
			return true
		}
	}
	return false
}

func normalizePermissionRequest(req PermissionRequest) PermissionRequest {
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.ToolCall.ToolCallID = strings.TrimSpace(req.ToolCall.ToolCallID)
	req.ToolCall.Title = strings.TrimSpace(req.ToolCall.Title)
	req.ToolCall.Kind = strings.TrimSpace(req.ToolCall.Kind)
	req.ToolCall.Status = strings.TrimSpace(req.ToolCall.Status)
	for i := range req.Options {
		req.Options[i].OptionID = strings.TrimSpace(req.Options[i].OptionID)
		req.Options[i].Name = strings.TrimSpace(req.Options[i].Name)
		req.Options[i].Kind = strings.TrimSpace(req.Options[i].Kind)
	}
	return req
}

func (c *Client) trackAgentRequest(parent context.Context, id *RequestID) (context.Context, func()) {
	if parent == nil {
		parent = context.Background()
	}
	if id == nil {
		return parent, func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	key := id.Key()
	c.agentRequestMu.Lock()
	if c.agentRequestCancels == nil {
		c.agentRequestCancels = make(map[string]context.CancelFunc)
	}
	c.agentRequestCancels[key] = cancel
	c.agentRequestMu.Unlock()
	return ctx, func() {
		c.agentRequestMu.Lock()
		delete(c.agentRequestCancels, key)
		c.agentRequestMu.Unlock()
		cancel()
	}
}

func (c *Client) cancelAgentRequest(id *RequestID) {
	if id == nil {
		return
	}
	key := id.Key()
	c.agentRequestMu.Lock()
	cancel := c.agentRequestCancels[key]
	c.agentRequestMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *Client) setPermissionHandler(sessionID string, ctx context.Context, handler PermissionRequestHandler) int64 {
	return c.setPermissionHandlerWithToolGeneration(sessionID, ctx, handler, true)
}

func (c *Client) setPermissionHandlerPending(sessionID string, ctx context.Context, handler PermissionRequestHandler) int64 {
	return c.setPermissionHandlerWithToolGeneration(sessionID, ctx, handler, false)
}

func (c *Client) setPermissionHandlerWithToolGeneration(sessionID string, ctx context.Context, handler PermissionRequestHandler, active bool) int64 {
	if strings.TrimSpace(sessionID) == "" {
		return 0
	}
	generation := c.nextPromptGen.Add(1)
	toolGeneration := int64(0)
	if active {
		toolGeneration = generation
	}
	c.permissionMu.Lock()
	defer c.permissionMu.Unlock()
	if c.permissionScopes == nil {
		c.permissionScopes = make(map[string]permissionScope)
	}
	c.permissionScopes[sessionID] = permissionScope{
		generation:     generation,
		toolGeneration: toolGeneration,
		ctx:            ctx,
		handler:        handler,
	}
	return generation
}

func (c *Client) activatePermissionHandler(sessionID string, generation int64) {
	if strings.TrimSpace(sessionID) == "" || generation == 0 {
		return
	}
	c.permissionMu.Lock()
	defer c.permissionMu.Unlock()
	scope, ok := c.permissionScopes[sessionID]
	if ok && scope.generation == generation {
		scope.toolGeneration = generation
		c.permissionScopes[sessionID] = scope
	}
}

func (c *Client) clearPermissionHandler(sessionID string, generation int64) {
	if strings.TrimSpace(sessionID) == "" || generation == 0 {
		return
	}
	c.permissionMu.Lock()
	defer c.permissionMu.Unlock()
	scope, ok := c.permissionScopes[sessionID]
	if ok && scope.generation == generation {
		delete(c.permissionScopes, sessionID)
	}
}

func (c *Client) permissionRequestHandler(req PermissionRequest) (context.Context, PermissionRequestHandler) {
	c.permissionMu.RLock()
	defer c.permissionMu.RUnlock()
	scope := c.permissionScopes[req.SessionID]
	// Tool-call updates are tagged with the prompt generation active when they
	// arrive. Permission requests tied to an older generation are cancelled so
	// late server messages cannot consume the next prompt's approval handler.
	if req.ToolCallState != nil && req.ToolCallState.generation != 0 && scope.generation != 0 && req.ToolCallState.generation != scope.generation {
		return canceledContext(), nil
	}
	if strings.TrimSpace(req.ToolCall.ToolCallID) != "" && req.ToolCallState != nil && req.ToolCallState.generation == 0 && scope.generation != 0 {
		return canceledContext(), nil
	}
	if scope.ctx == nil || scope.handler == nil {
		return scope.ctx, nil
	}
	return scope.ctx, scope.handler
}

func (c *Client) rememberToolCallUpdate(sessionID string, raw json.RawMessage) {
	var update struct {
		SessionUpdate string          `json:"sessionUpdate"`
		ToolCallID    string          `json:"toolCallId"`
		Title         *string         `json:"title,omitempty"`
		Kind          *string         `json:"kind,omitempty"`
		Status        *string         `json:"status,omitempty"`
		Content       json.RawMessage `json:"content,omitempty"`
		Locations     json.RawMessage `json:"locations,omitempty"`
		RawInput      json.RawMessage `json:"rawInput,omitempty"`
		RawOutput     json.RawMessage `json:"rawOutput,omitempty"`
	}
	if err := json.Unmarshal(raw, &update); err != nil {
		return
	}
	if update.SessionUpdate != "tool_call" && update.SessionUpdate != "tool_call_update" {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	update.ToolCallID = strings.TrimSpace(update.ToolCallID)
	if sessionID == "" || update.ToolCallID == "" {
		return
	}
	generation := c.currentPermissionGeneration(sessionID)
	c.toolMu.Lock()
	defer c.toolMu.Unlock()
	if c.toolCalls == nil {
		c.toolCalls = make(map[string]map[string]ToolCallInfo)
	}
	if c.toolCalls[sessionID] == nil {
		c.toolCalls[sessionID] = make(map[string]ToolCallInfo)
	}
	info := c.toolCalls[sessionID][update.ToolCallID]
	info.ToolCallID = update.ToolCallID
	if update.Title != nil {
		info.Title = *update.Title
	}
	if update.Kind != nil {
		info.Kind = *update.Kind
	}
	if update.Status != nil {
		info.Status = *update.Status
	}
	if len(update.Content) > 0 {
		info.Content = append(json.RawMessage(nil), update.Content...)
	}
	if len(update.Locations) > 0 {
		info.Locations = append(json.RawMessage(nil), update.Locations...)
	}
	if len(update.RawInput) > 0 {
		info.RawInput = append(json.RawMessage(nil), update.RawInput...)
	}
	if len(update.RawOutput) > 0 {
		info.RawOutput = append(json.RawMessage(nil), update.RawOutput...)
	}
	if generation != 0 {
		info.generation = generation
	}
	c.toolCalls[sessionID][update.ToolCallID] = info
}

func (c *Client) currentPermissionGeneration(sessionID string) int64 {
	c.permissionMu.RLock()
	defer c.permissionMu.RUnlock()
	return c.permissionScopes[sessionID].toolGeneration
}

func (c *Client) toolCallSnapshot(sessionID string, toolCallID string) *ToolCallInfo {
	c.toolMu.RLock()
	defer c.toolMu.RUnlock()
	if c.toolCalls == nil || c.toolCalls[sessionID] == nil {
		return nil
	}
	info, ok := c.toolCalls[sessionID][toolCallID]
	if !ok {
		return nil
	}
	info.Content = append(json.RawMessage(nil), info.Content...)
	info.Locations = append(json.RawMessage(nil), info.Locations...)
	info.RawInput = append(json.RawMessage(nil), info.RawInput...)
	info.RawOutput = append(json.RawMessage(nil), info.RawOutput...)
	return &info
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
