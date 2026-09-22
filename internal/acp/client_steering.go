package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type SteeringResult struct {
	Outcome string `json:"outcome,omitempty"`
}

const SteeringOutcomeInjected = "injected"

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
	result, err := c.call(ctx, "_session/steering", map[string]any{
		"sessionId": sessionID,
		"prompt": []ContentBlock{
			{Type: "text", Text: text},
		},
	})
	if err != nil {
		return SteeringResult{}, err
	}
	var parsed SteeringResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		return SteeringResult{}, fmt.Errorf("解析 _session/steering 响应: %w", err)
	}
	return parsed, nil
}
