package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func (c *Client) Initialize(ctx context.Context) error {
	result, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		// lark-acp-bridge starts TraeX as a local child process, so TraeX can use
		// its own local file and command tools under the session cwd. Do not
		// advertise ACP client-side fs/terminal capabilities unless the bridge
		// later owns a remote, virtual, or permission-gated workspace surface.
		"clientCapabilities": map[string]any{
			"session": map[string]any{
				"configOptions": map[string]any{
					"boolean": map[string]any{},
				},
			},
		},
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
		c.Close()
		return fmt.Errorf("不支持的 ACP protocolVersion: %d", parsed.ProtocolVersion)
	}
	c.capMu.Lock()
	c.initialize = parsed
	c.capMu.Unlock()
	return nil
}

func (c *Client) Authenticate(ctx context.Context, methodID string) error {
	methodID = strings.TrimSpace(methodID)
	if methodID == "" {
		return fmt.Errorf("ACP auth method id 为空")
	}
	if err := c.ensureInitialized(); err != nil {
		return err
	}
	c.capMu.RLock()
	methods := append([]AuthMethod(nil), c.initialize.AuthMethods...)
	c.capMu.RUnlock()
	found := false
	for _, method := range methods {
		if method.ID == methodID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("ACP agent 未声明 auth method: %s", methodID)
	}
	_, err := c.call(ctx, "authenticate", map[string]any{
		"methodId": methodID,
	})
	return err
}

func (c *Client) SupportsLogout() bool {
	c.capMu.RLock()
	defer c.capMu.RUnlock()
	return c.initialize.AgentCapabilities.Auth.SupportsLogout()
}

func (c *Client) Logout(ctx context.Context) error {
	if err := c.ensureInitialized(); err != nil {
		return err
	}
	if !c.SupportsLogout() {
		return fmt.Errorf("ACP agent 未声明 auth.logout capability")
	}
	_, err := c.call(ctx, "logout", map[string]any{})
	return err
}
