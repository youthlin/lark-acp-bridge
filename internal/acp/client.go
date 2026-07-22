package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/youthlin/lark-acp-bridge/internal/config"
)

type Client struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	workspace string

	nextID atomic.Int64

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[int64]chan rpcResponse

	updateMu       sync.Mutex
	updateHandlers map[int64]UpdateHandler
	nextHandlerID  atomic.Int64

	closeOnce sync.Once
}

type UpdateHandler func(sessionID string, update SessionUpdate)

type PromptUpdateHandler func(update PromptUpdate)

type PromptOptions struct {
	OnUpdate PromptUpdateHandler
}

type PromptUpdate struct {
	SessionID string
	Update    SessionUpdate
}

type SessionUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       *ContentBlock   `json:"content,omitempty"`
	Message       string          `json:"message,omitempty"`
	Name          string          `json:"name,omitempty"`
	Status        string          `json:"status,omitempty"`
	Title         string          `json:"title,omitempty"`
	StopReason    string          `json:"stopReason,omitempty"`
	Raw           json.RawMessage `json:"-"`
}

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type newSessionResult struct {
	SessionID string `json:"sessionId"`
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
		cmd:            cmd,
		stdin:          stdin,
		workspace:      workspace,
		pending:        make(map[int64]chan rpcResponse),
		updateHandlers: make(map[int64]UpdateHandler),
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
	_, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{
				"readTextFile":  strings.TrimSpace(c.workspace) != "",
				"writeTextFile": strings.TrimSpace(c.workspace) != "",
			},
			"terminal": false,
		},
		"clientInfo": map[string]any{
			"name":    "lark-acp-bridge",
			"title":   "Lark ACP Bridge",
			"version": "dev",
		},
	})
	return err
}

func (c *Client) NewSession(ctx context.Context, cwd string) (string, error) {
	result, err := c.call(ctx, "session/new", map[string]any{
		"cwd":        cwd,
		"mcpServers": []any{},
	})
	if err != nil {
		return "", err
	}
	var parsed newSessionResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		return "", fmt.Errorf("解析 session/new 响应: %w", err)
	}
	if parsed.SessionID == "" {
		return "", fmt.Errorf("session/new 未返回 sessionId")
	}
	return parsed.SessionID, nil
}

func (c *Client) LoadSession(ctx context.Context, sessionID, cwd string) error {
	_, err := c.call(ctx, "session/load", map[string]any{
		"sessionId":  sessionID,
		"cwd":        cwd,
		"mcpServers": []any{},
	})
	return err
}

func (c *Client) CancelSession(ctx context.Context, sessionID string) error {
	_, err := c.call(ctx, "session/cancel", map[string]any{
		"sessionId": sessionID,
	})
	return err
}

func (c *Client) Prompt(ctx context.Context, sessionID, text string) (string, error) {
	return c.PromptWithOptions(ctx, sessionID, text, PromptOptions{})
}

func (c *Client) PromptWithOptions(ctx context.Context, sessionID, text string, opts PromptOptions) (string, error) {
	var output strings.Builder
	var outputMu sync.Mutex
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

	_, err := c.call(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt": []ContentBlock{
			{Type: "text", Text: text},
		},
	})
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
	id := c.nextID.Add(1)
	req, err := NewRequest(id, method, params)
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "Call ACP", "method", method, "req", req)
	ch := make(chan rpcResponse, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	if err := c.write(req); err != nil {
		c.removePending(id)
		return nil, err
	}
	select {
	case <-ctx.Done():
		c.removePending(id)
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
	ch := c.removePending(*msg.ID)
	if ch == nil {
		return
	}
	if msg.Error != nil {
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
	case "fs/read_text_file":
		result, err := c.handleReadTextFile(msg.Params)
		c.replyResult(msg, result, err)
	case "fs/write_text_file":
		result, err := c.handleWriteTextFile(msg.Params)
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

func (c *Client) handleReadTextFile(raw json.RawMessage) (any, error) {
	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, fmt.Errorf("解析 fs/read_text_file 参数: %w", err)
	}
	path, err := c.workspacePath(params.Path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取文件 %s: %w", params.Path, err)
	}
	return map[string]string{"content": string(data)}, nil
}

func (c *Client) handleWriteTextFile(raw json.RawMessage) (any, error) {
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, fmt.Errorf("解析 fs/write_text_file 参数: %w", err)
	}
	path, err := c.workspacePath(params.Path)
	if err != nil {
		return nil, err
	}
	_, statErr := os.Stat(path)
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("检查文件 %s: %w", params.Path, statErr)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建目录: %w", err)
	}
	if err := os.WriteFile(path, []byte(params.Content), 0o644); err != nil {
		return nil, fmt.Errorf("写入文件 %s: %w", params.Path, err)
	}
	return map[string]bool{"created": os.IsNotExist(statErr)}, nil
}

func (c *Client) workspacePath(path string) (string, error) {
	if strings.TrimSpace(c.workspace) == "" {
		return "", fmt.Errorf("未配置 workspace，不能执行文件操作")
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("文件路径为空")
	}
	base, err := filepath.Abs(c.workspace)
	if err != nil {
		return "", fmt.Errorf("解析 workspace: %w", err)
	}
	var target string
	if filepath.IsAbs(path) {
		target = filepath.Clean(path)
	} else {
		target = filepath.Join(base, path)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("解析文件路径: %w", err)
	}
	if target != base && !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("文件路径超出 workspace: %s", path)
	}
	return target, nil
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

func (c *Client) removePending(id int64) chan rpcResponse {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	ch := c.pending[id]
	delete(c.pending, id)
	return ch
}

func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan rpcResponse)
	c.pendingMu.Unlock()
	for _, ch := range pending {
		ch <- rpcResponse{err: err}
	}
}
