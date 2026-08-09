package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

type rpcResponse struct {
	result json.RawMessage
	err    error
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
	// 仅当调用方未设置 deadline 时套用方法级默认超时，避免握手/会话操作永久挂起。
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		if timeout := defaultRPCTimeout(method); timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
	}
	id := c.nextID.Add(1)
	req, err := NewRequest(id, method, params)
	if err != nil {
		return nil, err
	}
	slog.DebugContext(ctx, "Call ACP", "method", method, "req", req)
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
		c.cancelRequest(ctx, req.ID)
		if cancelWait == 0 {
			c.removePending(req.ID)
			return nil, ctx.Err()
		}
		if cancelWait < 0 {
			// session/prompt must not release promptMu until the server has
			// answered the cancelled request or the read loop fails all pending
			// calls. This prevents a later prompt from being written while an ACP
			// agent is still busy finishing the old turn.
			<-ch
			return nil, ctx.Err()
		}
		// Prompt cancellation often races with the final server response. Waiting
		// briefly lets the read loop consume that response and keeps the pending
		// map from being reused while the server still refers to the old id.
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

func (c *Client) cancelRequest(ctx context.Context, id *RequestID) {
	if id == nil {
		return
	}
	msg, err := NewNotification("$/cancel_request", map[string]any{
		"id": id,
	})
	if err != nil {
		return
	}
	slog.DebugContext(ctx, "Notify ACP", "method", "$/cancel_request", "req", msg)
	_ = c.write(msg)
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
	defer withRecover(context.Background(), "readLoop", func(err error) {
		c.failPending(err)
		_ = c.Close()
	})

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			err = fmt.Errorf("ACP server stdout 输出非 JSON-RPC 消息: %w", err)
			c.failPending(err)
			c.Close()
			return
		}
		c.handleMessage(msg)
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	c.failPending(fmt.Errorf("%w: %w", ErrServerOutputClosed, err))
}

func (c *Client) handleMessage(msg Message) {
	if msg.ID != nil && msg.Method != "" {
		go func() {
			defer withRecover(context.Background(), "agent_request:"+msg.Method, func(err error) {
				c.replyResult(msg, nil, err)
			})
			c.handleAgentRequest(msg)
		}()
		return
	}
	if msg.ID != nil {
		c.handleResponse(msg)
		return
	}
	if msg.Method == "$/cancel_request" {
		c.handleCancelRequest(msg.Params)
		return
	}
	if msg.Method == "session/update" {
		c.handleSessionUpdate(msg)
	}
}

func (c *Client) handleCancelRequest(raw json.RawMessage) {
	var params struct {
		ID RequestID `json:"id"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return
	}
	c.cancelAgentRequest(&params.ID)
}

func (c *Client) handleResponse(msg Message) {
	ch := c.removePending(msg.ID)
	if ch == nil {
		return
	}
	if msg.Error != nil {
		if msg.Error.Code == -32800 {
			ch <- rpcResponse{err: context.Canceled}
			return
		}
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
	params.SessionID = strings.TrimSpace(params.SessionID)
	if params.SessionID == "" {
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
			UpdatedAt     string `json:"updatedAt"`
			StopReason    string `json:"stopReason"`
		}
		_ = json.Unmarshal(params.Update, &header)
		update = SessionUpdate{
			SessionUpdate: header.SessionUpdate,
			Message:       header.Message,
			Name:          header.Name,
			Status:        header.Status,
			Title:         header.Title,
			UpdatedAt:     header.UpdatedAt,
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
		if errors.Is(err, context.Canceled) {
			c.write(Message{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error: &RPCError{
					Code:    -32800,
					Message: "Request Cancelled",
				},
			})
			return
		}
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
