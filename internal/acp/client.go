package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/arg"
	"github.com/youthlin/lark-acp-bridge/internal/config"
)

type Client struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	workspace string
	cwd       string

	nextID atomic.Int64

	writeMu  sync.Mutex
	promptMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan rpcResponse

	capMu      sync.RWMutex
	initialize InitializeResult

	toolMu    sync.RWMutex
	toolCalls map[string]map[string]ToolCallInfo

	updateMu       sync.Mutex
	updateHandlers map[int64]UpdateHandler
	nextHandlerID  atomic.Int64

	permissionMu     sync.RWMutex
	permissionScopes map[string]permissionScope
	nextPromptGen    atomic.Int64

	closeOnce sync.Once
}

const promptCancelResponseTimeout = 10 * time.Second

type permissionScope struct {
	generation     int64
	toolGeneration int64
	ctx            context.Context
	handler        PermissionRequestHandler
}

type UpdateHandler func(sessionID string, update SessionUpdate)

type PromptUpdateHandler func(update PromptUpdate)

type PromptOptions struct {
	OnUpdate            PromptUpdateHandler
	OnPermissionRequest PermissionRequestHandler
}

type PromptUpdate struct {
	SessionID string
	Update    SessionUpdate
}

type SessionUpdate struct {
	SessionUpdate     string                `json:"sessionUpdate"`
	Content           *ContentBlock         `json:"content,omitempty"`
	Message           string                `json:"message,omitempty"`
	Name              string                `json:"name,omitempty"`
	Status            string                `json:"status,omitempty"`
	Title             string                `json:"title,omitempty"`
	ToolCallID        string                `json:"toolCallId,omitempty"`
	Kind              string                `json:"kind,omitempty"`
	StopReason        string                `json:"stopReason,omitempty"`
	AvailableCommands []AvailableCommand    `json:"availableCommands,omitempty"`
	ConfigOptions     []SessionConfigOption `json:"configOptions,omitempty"`
	Models            *SessionModelState    `json:"models,omitempty"`
	Mode              *SessionModeState     `json:"mode,omitempty"`
	Raw               json.RawMessage       `json:"-"`
}

func (u *SessionUpdate) UnmarshalJSON(data []byte) error {
	type sessionUpdateAlias SessionUpdate
	aux := struct {
		*sessionUpdateAlias
		LegacyModes *SessionModeState `json:"modes,omitempty"`
	}{
		sessionUpdateAlias: (*sessionUpdateAlias)(u),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if u.Mode == nil {
		u.Mode = aux.LegacyModes
	}
	return nil
}

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type ToolCallInfo struct {
	ToolCallID string          `json:"toolCallId,omitempty"`
	Title      string          `json:"title,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Status     string          `json:"status,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"`
	Locations  json.RawMessage `json:"locations,omitempty"`
	RawInput   json.RawMessage `json:"rawInput,omitempty"`
	RawOutput  json.RawMessage `json:"rawOutput,omitempty"`
	generation int64
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
	ToolCallID string `json:"toolCallId"`
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

type SetConfigOptionResult struct {
	ConfigOptions []SessionConfigOption `json:"configOptions"`
}

type InitializeResult struct {
	ProtocolVersion   int                `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities  `json:"agentCapabilities"`
	AgentInfo         ImplementationInfo `json:"agentInfo"`
	AuthMethods       []AuthMethod       `json:"authMethods,omitempty"`
}

type AgentCapabilities struct {
	LoadSession         bool                `json:"loadSession,omitempty"`
	PromptCapabilities  PromptCapabilities  `json:"promptCapabilities,omitempty"`
	MCPCapabilities     MCPCapabilities     `json:"mcpCapabilities,omitempty"`
	SessionCapabilities SessionCapabilities `json:"sessionCapabilities,omitempty"`
	Auth                AuthCapabilities    `json:"auth,omitempty"`
}

type PromptCapabilities struct {
	Image           bool `json:"image,omitempty"`
	Audio           bool `json:"audio,omitempty"`
	EmbeddedContext bool `json:"embeddedContext,omitempty"`
}

type MCPCapabilities struct {
	HTTP bool `json:"http,omitempty"`
	SSE  bool `json:"sse,omitempty"`
}

type SessionCapabilities struct {
	Resume                any `json:"resume,omitempty"`
	Close                 any `json:"close,omitempty"`
	Delete                any `json:"delete,omitempty"`
	List                  any `json:"list,omitempty"`
	AdditionalDirectories any `json:"additionalDirectories,omitempty"`
}

func (c SessionCapabilities) SupportsResume() bool {
	return c.Resume != nil
}

func (c SessionCapabilities) SupportsAdditionalDirectories() bool {
	return c.AdditionalDirectories != nil
}

type AuthCapabilities struct {
	Logout any `json:"logout,omitempty"`
}

type ImplementationInfo struct {
	Name    string `json:"name,omitempty"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

type AuthMethod struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

type rpcResponse struct {
	result json.RawMessage
	err    error
}

func Start(ctx context.Context, agent config.AgentConfig, workspace string) (*Client, error) {
	if agent.Command == "" {
		return nil, fmt.Errorf("agent 启动命令为空")
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(agent.Command, agent.Args...)
	cmd.Env = os.Environ()
	for key, value := range agent.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("打开 stdin 管道: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("打开 stdout 管道: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 ACP server: %w", err)
	}

	client := &Client{
		cmd:              cmd,
		stdin:            stdin,
		workspace:        workspace,
		pending:          make(map[string]chan rpcResponse),
		toolCalls:        make(map[string]map[string]ToolCallInfo),
		permissionScopes: make(map[string]permissionScope),
		updateHandlers:   make(map[int64]UpdateHandler),
	}
	client.nextID.Store(1)
	go client.readLoop(stdout)
	return client, nil
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
			_, _ = c.cmd.Process.Wait()
		}
	})
	return nil
}

func (c *Client) Initialize(ctx context.Context) error {
	result, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		// lark-acp-bridge starts TraeX as a local child process, so TraeX can use
		// its own local file and command tools under the session cwd. Do not
		// advertise ACP client-side fs/terminal capabilities unless the bridge
		// later owns a remote, virtual, or permission-gated workspace surface.
		"clientCapabilities": map[string]any{},
		"clientInfo": map[string]any{
			"name":    "lark-acp-bridge",
			"title":   "Lark ACP Bridge",
			"version": "dev",
		},
	})
	if err != nil {
		return err
	}
	var parsed InitializeResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		return fmt.Errorf("解析 initialize 响应: %w", err)
	}
	if parsed.ProtocolVersion != 1 {
		return fmt.Errorf("不支持的 ACP protocolVersion: %d", parsed.ProtocolVersion)
	}
	c.capMu.Lock()
	c.initialize = parsed
	c.capMu.Unlock()
	return nil
}

func (c *Client) NewSession(ctx context.Context, cwd string) (SessionInfo, error) {
	c.cwd = filepath.Clean(cwd)
	result, err := c.call(ctx, "session/new", c.lifecycleParams("", cwd))
	if err != nil {
		return SessionInfo{}, err
	}
	var parsed SessionInfo
	if err := json.Unmarshal(result, &parsed); err != nil {
		return SessionInfo{}, fmt.Errorf("解析 session/new 响应: %w", err)
	}
	if parsed.SessionID == "" {
		return SessionInfo{}, fmt.Errorf("session/new 未返回 sessionId")
	}
	return parsed, nil
}

func (c *Client) LoadSession(ctx context.Context, sessionID, cwd string) (SessionInfo, error) {
	c.capMu.RLock()
	supportsLoad := c.initialize.AgentCapabilities.LoadSession
	c.capMu.RUnlock()
	if !supportsLoad {
		return SessionInfo{}, fmt.Errorf("ACP agent 未声明 loadSession capability")
	}
	c.cwd = filepath.Clean(cwd)
	result, err := c.call(ctx, "session/load", c.lifecycleParams(sessionID, cwd))
	if err != nil {
		return SessionInfo{}, err
	}
	return parseSessionInfoResult(result, "session/load")
}

func (c *Client) ResumeSession(ctx context.Context, sessionID, cwd string) (SessionInfo, error) {
	c.capMu.RLock()
	supportsResume := c.initialize.AgentCapabilities.SessionCapabilities.SupportsResume()
	c.capMu.RUnlock()
	if !supportsResume {
		return SessionInfo{}, fmt.Errorf("ACP agent 未声明 sessionCapabilities.resume")
	}
	c.cwd = filepath.Clean(cwd)
	result, err := c.call(ctx, "session/resume", c.lifecycleParams(sessionID, cwd))
	if err != nil {
		return SessionInfo{}, err
	}
	return parseSessionInfoResult(result, "session/resume")
}

func (c *Client) CancelSession(ctx context.Context, sessionID string) error {
	msg, err := NewNotification("session/cancel", map[string]any{
		"sessionId": sessionID,
	})
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "Notify ACP", "method", "session/cancel", "req", msg)
	return c.write(msg)
}

func (c *Client) SetConfigOption(ctx context.Context, sessionID, configID string, value any) ([]SessionConfigOption, error) {
	result, err := c.call(ctx, "session/set_config_option", map[string]any{
		"sessionId": sessionID,
		"configId":  configID,
		"value":     value,
	})
	if err != nil {
		return nil, err
	}
	var parsed SetConfigOptionResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		return nil, fmt.Errorf("解析 session/set_config_option 响应: %w", err)
	}
	return parsed.ConfigOptions, nil
}

func (c *Client) Prompt(ctx context.Context, sessionID, text string) (string, error) {
	return c.PromptWithOptions(ctx, sessionID, text, PromptOptions{})
}

func (c *Client) PromptWithOptions(ctx context.Context, sessionID, text string, opts PromptOptions) (string, error) {
	c.promptMu.Lock()
	defer c.promptMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var output strings.Builder
	var outputMu sync.Mutex
	generation := c.setPermissionHandlerPending(sessionID, ctx, opts.OnPermissionRequest)
	defer c.clearPermissionHandler(sessionID, generation)
	unsubscribe := c.SubscribeUpdates(func(id string, update SessionUpdate) {
		if id != sessionID {
			return
		}
		if opts.OnUpdate != nil {
			opts.OnUpdate(PromptUpdate{
				SessionID: id,
				Update:    update,
			})
		}
		if update.SessionUpdate == "agent_message_chunk" && update.Content != nil && update.Content.Text != "" && update.Title == "" {
			outputMu.Lock()
			output.WriteString(update.Content.Text)
			outputMu.Unlock()
		}
	})
	defer unsubscribe()

	_, err := c.callWithAfterWriteAndCancelWait(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt": []ContentBlock{
			{Type: "text", Text: text},
		},
	}, func() {
		c.activatePermissionHandler(sessionID, generation)
	}, promptCancelResponseTimeout)
	if err != nil {
		outputMu.Lock()
		defer outputMu.Unlock()
		return output.String(), err
	}
	outputMu.Lock()
	defer outputMu.Unlock()
	return output.String(), nil
}

func (c *Client) SubscribeUpdates(handler UpdateHandler) func() {
	if handler == nil {
		return func() {}
	}
	id := c.nextHandlerID.Add(1)
	c.updateMu.Lock()
	c.updateHandlers[id] = handler
	c.updateMu.Unlock()
	return func() {
		c.updateMu.Lock()
		delete(c.updateHandlers, id)
		c.updateMu.Unlock()
	}
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.callWithAfterWrite(ctx, method, params, nil)
}

func (c *Client) callWithAfterWrite(ctx context.Context, method string, params any, afterWrite func()) (json.RawMessage, error) {
	return c.callWithAfterWriteAndCancelWait(ctx, method, params, afterWrite, 0)
}

func (c *Client) callWithAfterWriteAndCancelWait(
	ctx context.Context,
	method string,
	params any,
	afterWrite func(),
	cancelWait time.Duration,
) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	req, err := NewRequest(id, method, params)
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "Call ACP", "method", method, "req", req)
	ch := make(chan rpcResponse, 1)
	c.pendingMu.Lock()
	c.pending[req.ID.Key()] = ch
	c.pendingMu.Unlock()
	if err := c.write(req); err != nil {
		c.removePending(req.ID)
		return nil, err
	}
	if afterWrite != nil {
		afterWrite()
	}
	select {
	case <-ctx.Done():
		if cancelWait <= 0 {
			c.removePending(req.ID)
			return nil, ctx.Err()
		}
		timer := time.NewTimer(cancelWait)
		defer timer.Stop()
		select {
		case <-ch:
		case <-timer.C:
			c.removePending(req.ID)
		}
		return nil, ctx.Err()
	case res := <-ch:
		return res.result, res.err
	}
}

func (c *Client) write(msg Message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

func (c *Client) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		slog.Info("read acp line", "line", arg.RawJSON(line), "comp", "acp-loop")
		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		c.handleMessage(msg)
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	c.failPending(fmt.Errorf("ACP server 输出已关闭: %w", err))
}

func (c *Client) handleMessage(msg Message) {
	if msg.ID != nil && msg.Method != "" {
		c.handleAgentRequest(msg)
		return
	}
	if msg.ID != nil {
		c.handleResponse(msg)
		return
	}
	if msg.Method == "session/update" {
		c.handleSessionUpdate(msg)
	}
}

func (c *Client) handleResponse(msg Message) {
	ch := c.removePending(msg.ID)
	if ch == nil {
		return
	}
	if msg.Error != nil {
		detail := strings.TrimSpace(msg.Error.Detail())
		if detail != "" && detail != msg.Error.Message {
			ch <- rpcResponse{err: fmt.Errorf("ACP JSON-RPC 错误: code=%d message=%s detail=%s", msg.Error.Code, msg.Error.Message, detail)}
			return
		}
		ch <- rpcResponse{err: fmt.Errorf("ACP JSON-RPC 错误: code=%d message=%s", msg.Error.Code, msg.Error.Message)}
		return
	}
	ch <- rpcResponse{result: msg.Result}
}

func (c *Client) handleSessionUpdate(msg Message) {
	var params struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return
	}
	if len(params.Update) == 0 {
		return
	}
	var update SessionUpdate
	if err := json.Unmarshal(params.Update, &update); err != nil {
		var header struct {
			SessionUpdate string `json:"sessionUpdate"`
			Message       string `json:"message"`
			Name          string `json:"name"`
			Status        string `json:"status"`
			Title         string `json:"title"`
			StopReason    string `json:"stopReason"`
		}
		_ = json.Unmarshal(params.Update, &header)
		update = SessionUpdate{
			SessionUpdate: header.SessionUpdate,
			Message:       header.Message,
			Name:          header.Name,
			Status:        header.Status,
			Title:         header.Title,
			StopReason:    header.StopReason,
		}
	}
	update.Raw = append(json.RawMessage(nil), params.Update...)
	c.rememberToolCallUpdate(params.SessionID, params.Update)

	c.updateMu.Lock()
	handlers := make([]UpdateHandler, 0, len(c.updateHandlers))
	for _, handler := range c.updateHandlers {
		handlers = append(handlers, handler)
	}
	c.updateMu.Unlock()
	for _, handler := range handlers {
		handler(params.SessionID, update)
	}
}

func (c *Client) handleAgentRequest(msg Message) {
	switch msg.Method {
	case "session/request_permission":
		result, err := c.handleRequestPermission(msg.ID, msg.Params)
		c.replyResult(msg, result, err)
	default:
		c.replyUnsupported(msg)
	}
}

func (c *Client) replyResult(msg Message, result any, err error) {
	if err != nil {
		c.write(Message{
			JSONRPC: "2.0",
			ID:      msg.ID,
			Error: &RPCError{
				Code:    -32603,
				Message: err.Error(),
			},
		})
		return
	}
	raw, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		c.write(Message{
			JSONRPC: "2.0",
			ID:      msg.ID,
			Error: &RPCError{
				Code:    -32603,
				Message: marshalErr.Error(),
			},
		})
		return
	}
	c.write(Message{JSONRPC: "2.0", ID: msg.ID, Result: raw})
}

func (c *Client) lifecycleParams(sessionID, cwd string) map[string]any {
	params := map[string]any{
		"cwd":        filepath.Clean(cwd),
		"mcpServers": []any{},
	}
	if strings.TrimSpace(sessionID) != "" {
		params["sessionId"] = sessionID
	}
	c.capMu.RLock()
	supportsAdditionalDirectories := c.initialize.AgentCapabilities.SessionCapabilities.SupportsAdditionalDirectories()
	c.capMu.RUnlock()
	if supportsAdditionalDirectories && strings.TrimSpace(c.workspace) != "" {
		if workspace, err := filepath.Abs(c.workspace); err == nil && filepath.Clean(workspace) != filepath.Clean(cwd) {
			params["additionalDirectories"] = []string{filepath.Clean(workspace)}
		}
	}
	return params
}

func (c *Client) handleRequestPermission(id *RequestID, raw json.RawMessage) (any, error) {
	var req PermissionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("解析 session/request_permission 参数: %w", err)
	}
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
	if handler != nil {
		outcome, err := handler(ctx, req)
		if err != nil {
			if errors.Is(err, context.Canceled) || (ctx != nil && ctx.Err() != nil) {
				return PermissionResult{Outcome: PermissionOutcome{Outcome: "cancelled"}}, nil
			}
			return nil, err
		}
		if strings.TrimSpace(outcome.Outcome) == "" {
			outcome.Outcome = "cancelled"
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
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(update.ToolCallID) == "" {
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

func (c *Client) replyUnsupported(msg Message) {
	c.write(Message{
		JSONRPC: "2.0",
		ID:      msg.ID,
		Error: &RPCError{
			Code:    -32601,
			Message: "method not supported by lark-acp-bridge",
		},
	})
}

func (c *Client) removePending(id *RequestID) chan rpcResponse {
	if id == nil {
		return nil
	}
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	key := id.Key()
	ch := c.pending[key]
	delete(c.pending, key)
	return ch
}

func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[string]chan rpcResponse)
	c.pendingMu.Unlock()
	for _, ch := range pending {
		ch <- rpcResponse{err: err}
	}
}

func parseSessionInfoResult(result json.RawMessage, method string) (SessionInfo, error) {
	if len(result) == 0 || string(result) == "null" {
		return SessionInfo{}, nil
	}
	var parsed SessionInfo
	if err := json.Unmarshal(result, &parsed); err != nil {
		return SessionInfo{}, fmt.Errorf("解析 %s 响应: %w", method, err)
	}
	return parsed, nil
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
