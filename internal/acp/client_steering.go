package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type SteeringResult struct {
	Outcome string `json:"outcome,omitempty"`
}

const SteeringOutcomeInjected = "injected"

// ErrSteeringOutcomeUnknown 表示 steering 请求已经成功写入 ACP 连接，但在收到
// agent 的确认响应前调用上下文结束。此时 agent 可能已经注入补充消息，调用方
// 不应把它当成明确的“未注入”并自动重发，否则可能造成重复提交或误打断当前 turn。
var ErrSteeringOutcomeUnknown = errors.New("ACP steering 提交结果未知")

func (c *Client) SupportsSteering() bool {
	c.capMu.RLock()
	defer c.capMu.RUnlock()
	steering, ok := c.initialize.Meta["steering"].(map[string]any)
	if !ok {
		return false
	}
	return capabilityEnabled(steering["supported"])
}

func (c *Client) Steer(ctx context.Context, sessionID, text string) (SteeringResult, error) {
	if err := ctx.Err(); err != nil {
		return SteeringResult{}, err
	}
	if err := c.ensureInitialized(); err != nil {
		return SteeringResult{}, err
	}
	if !c.SupportsSteering() {
		return SteeringResult{}, fmt.Errorf("ACP agent 未声明 steering capability")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return SteeringResult{}, fmt.Errorf("ACP session id 为空")
	}
	if strings.TrimSpace(text) == "" {
		return SteeringResult{}, fmt.Errorf("ACP steering 输入为空")
	}
	requestWritten := false
	result, err := c.callWithAfterWrite(ctx, "_session/steering", map[string]any{
		"sessionId": sessionID,
		"prompt": []ContentBlock{
			{Type: "text", Text: text},
		},
	}, func() {
		requestWritten = true
	})
	if err != nil {
		if requestWritten && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return SteeringResult{}, fmt.Errorf("%w: %w", ErrSteeringOutcomeUnknown, err)
		}
		return SteeringResult{}, err
	}
	var parsed SteeringResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		return SteeringResult{}, fmt.Errorf("解析 _session/steering 响应: %w", err)
	}
	return parsed, nil
}
