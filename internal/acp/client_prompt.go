package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

const promptCancelResponseTimeout = 10 * time.Second

func (c *Client) Prompt(ctx context.Context, sessionID, text string) (string, error) {
	result, err := c.PromptWithOptions(ctx, sessionID, text, PromptOptions{})
	return result.Text, err
}

func (c *Client) PromptWithOptions(ctx context.Context, sessionID, text string, opts PromptOptions) (PromptResult, error) {
	// One ACP session prompt is treated as a single active turn. Serializing here
	// keeps streamed chunks, permission handlers, and tool-call generations from
	// crossing between overlapping prompts on the same Client.
	c.promptMu.Lock()
	defer c.promptMu.Unlock()
	if err := ctx.Err(); err != nil {
		return PromptResult{}, err
	}
	if err := c.ensureInitialized(); err != nil {
		return PromptResult{}, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return PromptResult{}, fmt.Errorf("ACP session id 为空")
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

	rawResult, err := c.callWithAfterWriteAndCancelWait(ctx, "session/prompt", map[string]any{
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
		return PromptResult{Text: output.String()}, err
	}
	outputMu.Lock()
	textOutput := output.String()
	outputMu.Unlock()
	result := PromptResult{Text: textOutput, Raw: append(json.RawMessage(nil), rawResult...)}
	if len(rawResult) > 0 && string(rawResult) != "null" {
		if err := json.Unmarshal(rawResult, &result); err != nil {
			result.Text = textOutput
			return result, fmt.Errorf("解析 session/prompt 响应: %w", err)
		}
		result.Text = textOutput
		result.Raw = append(json.RawMessage(nil), rawResult...)
	}
	return result, nil
}

func (c *Client) SubscribeUpdates(handler UpdateHandler) func() {
	if handler == nil {
		return func() {}
	}
	id := c.nextHandlerID.Add(1)
	c.updateMu.Lock()
	if c.updateHandlers == nil {
		c.updateHandlers = make(map[int64]UpdateHandler)
	}
	c.updateHandlers[id] = handler
	c.updateMu.Unlock()
	return func() {
		c.updateMu.Lock()
		delete(c.updateHandlers, id)
		c.updateMu.Unlock()
	}
}
