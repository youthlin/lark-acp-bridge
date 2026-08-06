package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
)

func (c *Client) NewSession(ctx context.Context, cwd string) (SessionInfo, error) {
	if err := c.ensureInitialized(); err != nil {
		return SessionInfo{}, err
	}
	cleanCwd, err := cleanSessionCwd(cwd)
	if err != nil {
		return SessionInfo{}, err
	}
	result, err := c.call(ctx, "session/new", c.lifecycleParams("", cleanCwd))
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
	if err := c.ensureInitialized(); err != nil {
		return SessionInfo{}, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return SessionInfo{}, fmt.Errorf("ACP session id 为空")
	}
	cleanCwd, err := cleanSessionCwd(cwd)
	if err != nil {
		return SessionInfo{}, err
	}
	c.capMu.RLock()
	supportsLoad := c.initialize.AgentCapabilities.LoadSession
	c.capMu.RUnlock()
	if !supportsLoad {
		return SessionInfo{}, fmt.Errorf("ACP agent 未声明 loadSession capability")
	}
	result, err := c.call(ctx, "session/load", c.lifecycleParams(sessionID, cleanCwd))
	if err != nil {
		return SessionInfo{}, err
	}
	return parseSessionInfoResult(result, "session/load")
}

func (c *Client) ResumeSession(ctx context.Context, sessionID, cwd string) (SessionInfo, error) {
	if err := c.ensureInitialized(); err != nil {
		return SessionInfo{}, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return SessionInfo{}, fmt.Errorf("ACP session id 为空")
	}
	cleanCwd, err := cleanSessionCwd(cwd)
	if err != nil {
		return SessionInfo{}, err
	}
	c.capMu.RLock()
	supportsResume := c.initialize.AgentCapabilities.SessionCapabilities.SupportsResume()
	c.capMu.RUnlock()
	if !supportsResume {
		return SessionInfo{}, fmt.Errorf("ACP agent 未声明 sessionCapabilities.resume")
	}
	result, err := c.call(ctx, "session/resume", c.lifecycleParams(sessionID, cleanCwd))
	if err != nil {
		return SessionInfo{}, err
	}
	return parseSessionInfoResult(result, "session/resume")
}

func (c *Client) CancelSession(ctx context.Context, sessionID string) error {
	if err := c.ensureInitialized(); err != nil {
		return err
	}
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("ACP session id 为空")
	}
	msg, err := NewNotification("session/cancel", map[string]any{
		"sessionId": sessionID,
	})
	if err != nil {
		return err
	}
	slog.DebugContext(ctx, "Notify ACP", "method", "session/cancel", "req", msg)
	return c.write(msg)
}

func (c *Client) SupportsCloseSession() bool {
	c.capMu.RLock()
	defer c.capMu.RUnlock()
	return c.initialize.AgentCapabilities.SessionCapabilities.SupportsClose()
}

func (c *Client) CloseSession(ctx context.Context, sessionID string) error {
	if err := c.ensureInitialized(); err != nil {
		return err
	}
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("ACP session id 为空")
	}
	if !c.SupportsCloseSession() {
		return fmt.Errorf("ACP agent 未声明 sessionCapabilities.close")
	}
	_, err := c.call(ctx, "session/close", map[string]any{
		"sessionId": sessionID,
	})
	if err != nil {
		return err
	}
	c.clearSessionState(sessionID)
	return nil
}

func (c *Client) SupportsDeleteSession() bool {
	c.capMu.RLock()
	defer c.capMu.RUnlock()
	return c.initialize.AgentCapabilities.SessionCapabilities.SupportsDelete()
}

func (c *Client) DeleteSession(ctx context.Context, sessionID string) error {
	if err := c.ensureInitialized(); err != nil {
		return err
	}
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("ACP session id 为空")
	}
	if !c.SupportsDeleteSession() {
		return fmt.Errorf("ACP agent 未声明 sessionCapabilities.delete")
	}
	_, err := c.call(ctx, "session/delete", map[string]any{
		"sessionId": sessionID,
	})
	if err != nil {
		return err
	}
	c.clearSessionState(sessionID)
	return nil
}

func (c *Client) SupportsListSessions() bool {
	c.capMu.RLock()
	defer c.capMu.RUnlock()
	return c.initialize.AgentCapabilities.SessionCapabilities.SupportsList()
}

func (c *Client) ListSessions(ctx context.Context, opts SessionListOptions) (SessionListResult, error) {
	if err := c.ensureInitialized(); err != nil {
		return SessionListResult{}, err
	}
	if !c.SupportsListSessions() {
		return SessionListResult{}, fmt.Errorf("ACP agent 未声明 sessionCapabilities.list")
	}
	params := map[string]any{}
	if strings.TrimSpace(opts.Cwd) != "" {
		cwd, err := cleanSessionCwd(opts.Cwd)
		if err != nil {
			return SessionListResult{}, err
		}
		params["cwd"] = cwd
	}
	if opts.Cursor != "" {
		params["cursor"] = opts.Cursor
	}
	result, err := c.call(ctx, "session/list", params)
	if err != nil {
		return SessionListResult{}, err
	}
	var parsed SessionListResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		return SessionListResult{}, fmt.Errorf("解析 session/list 响应: %w", err)
	}
	if parsed.Sessions == nil {
		parsed.Sessions = []SessionInfo{}
	}
	return parsed, nil
}

func (c *Client) SetConfigOption(ctx context.Context, sessionID, configID string, value any) ([]SessionConfigOption, error) {
	if err := c.ensureInitialized(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("ACP session id 为空")
	}
	if strings.TrimSpace(configID) == "" {
		return nil, fmt.Errorf("ACP config id 为空")
	}
	params := map[string]any{
		"sessionId": sessionID,
		"configId":  configID,
		"value":     value,
	}
	if _, ok := value.(bool); ok {
		params["type"] = "boolean"
	}
	result, err := c.call(ctx, "session/set_config_option", params)
	if err != nil {
		return nil, err
	}
	var parsed SetConfigOptionResult
	if err := json.Unmarshal(result, &parsed); err != nil {
		return nil, fmt.Errorf("解析 session/set_config_option 响应: %w", err)
	}
	return parsed.ConfigOptions, nil
}

func (c *Client) SetMode(ctx context.Context, sessionID, modeID string) error {
	if err := c.ensureInitialized(); err != nil {
		return err
	}
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("ACP session id 为空")
	}
	if strings.TrimSpace(modeID) == "" {
		return fmt.Errorf("ACP mode id 为空")
	}
	_, err := c.call(ctx, "session/set_mode", map[string]any{
		"sessionId": sessionID,
		"modeId":    modeID,
	})
	return err
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
		// Session cwd may point at a subdirectory. When the agent supports it,
		// expose the bridge workspace root as an extra directory so local tools
		// can still see sibling files without changing the session cwd.
		if workspace, err := filepath.Abs(c.workspace); err == nil && filepath.Clean(workspace) != filepath.Clean(cwd) {
			params["additionalDirectories"] = []string{filepath.Clean(workspace)}
		}
	}
	return params
}

func cleanSessionCwd(cwd string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(cwd))
	if clean == "." || !filepath.IsAbs(clean) {
		return "", fmt.Errorf("ACP session cwd 必须是绝对路径: %s", cwd)
	}
	return clean, nil
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
