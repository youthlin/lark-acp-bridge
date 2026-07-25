package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestClientInitializeDoesNotAdvertiseLocalFSOrTerminal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace string
	}{
		{name: "workspace configured", workspace: t.TempDir()},
		{name: "workspace empty", workspace: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := newPipeClient(t, tc.workspace)
			defer server.close()

			done := make(chan struct{})
			go func() {
				defer close(done)
				req := server.readRequest(t)
				if req.Method != "initialize" {
					t.Errorf("method = %q, want initialize", req.Method)
				}
				var params struct {
					ClientCapabilities map[string]json.RawMessage `json:"clientCapabilities"`
				}
				if err := json.Unmarshal(req.Params, &params); err != nil {
					t.Errorf("Unmarshal params error = %v", err)
					return
				}
				if _, ok := params.ClientCapabilities["fs"]; ok {
					t.Errorf("clientCapabilities declares fs, want omitted")
				}
				if _, ok := params.ClientCapabilities["terminal"]; ok {
					t.Errorf("clientCapabilities declares terminal, want omitted")
				}
				server.writeResponse(t, req.ID, map[string]any{
					"protocolVersion":   1,
					"agentCapabilities": map[string]any{},
					"agentInfo":         map[string]any{"name": "test-agent"},
				})
			}()

			if err := client.Initialize(context.Background()); err != nil {
				t.Fatalf("Initialize() error = %v", err)
			}
			<-done
		})
	}
}

func TestClientInitializeAllowsAndStoresAuthMethods(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		server.writeResponse(t, req.ID, map[string]any{
			"protocolVersion":   1,
			"agentCapabilities": map[string]any{},
			"agentInfo":         map[string]any{"name": "test-agent"},
			"authMethods": []map[string]any{
				{"id": "agent-login", "name": "Agent Login"},
			},
		})
	}()

	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	<-done
	client.capMu.RLock()
	methods := client.initialize.AuthMethods
	client.capMu.RUnlock()
	if len(methods) != 1 || methods[0].ID != "agent-login" {
		t.Fatalf("AuthMethods = %+v, want stored agent-login", methods)
	}
}

func TestClientNewSessionSendsMCPServers(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/new" {
			t.Errorf("method = %q, want session/new", req.Method)
		}
		var params struct {
			Cwd        string `json:"cwd"`
			MCPServers []any  `json:"mcpServers"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("Unmarshal params error = %v", err)
			return
		}
		if params.Cwd != "/repo" {
			t.Errorf("cwd = %q, want /repo", params.Cwd)
		}
		if params.MCPServers == nil || len(params.MCPServers) != 0 {
			t.Errorf("mcpServers = %#v, want empty array", params.MCPServers)
		}
		server.writeResponse(t, req.ID, map[string]any{
			"sessionId": "session-1",
			"availableCommands": []map[string]any{
				{"name": "review", "description": "Review current changes"},
			},
			"configOptions": []map[string]any{
				{
					"id":           "model",
					"name":         "Model",
					"category":     "model",
					"type":         "select",
					"currentValue": "gpt-5.5",
					"options": []map[string]any{
						{"value": "gpt-5.5", "name": "GPT-5.5"},
					},
				},
			},
			"mode": map[string]any{
				"currentModeId": "default",
				"availableModes": []map[string]any{
					{"modeId": "default", "name": "Default"},
					{"modeId": "plan", "name": "Plan"},
				},
			},
		})
	}()

	sessionInfo, err := client.NewSession(context.Background(), "/repo")
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	if sessionInfo.SessionID != "session-1" {
		t.Fatalf("sessionID = %q, want session-1", sessionInfo.SessionID)
	}
	if len(sessionInfo.AvailableCommands) != 1 || sessionInfo.AvailableCommands[0].Name != "review" {
		t.Fatalf("AvailableCommands = %+v, want review command", sessionInfo.AvailableCommands)
	}
	if len(sessionInfo.ConfigOptions) != 1 || sessionInfo.ConfigOptions[0].ID != "model" {
		t.Fatalf("ConfigOptions = %+v, want model option", sessionInfo.ConfigOptions)
	}
	if sessionInfo.Mode == nil || sessionInfo.Mode.CurrentModeID != "default" || len(sessionInfo.Mode.AvailableModes) != 2 {
		t.Fatalf("Mode = %+v, want default mode state", sessionInfo.Mode)
	}
	<-done
}

func TestSessionInfoParsesModeStringAndLegacyModes(t *testing.T) {
	var stringMode SessionInfo
	if err := json.Unmarshal([]byte(`{"sessionId":"session-1","mode":"plan"}`), &stringMode); err != nil {
		t.Fatalf("Unmarshal string mode error = %v", err)
	}
	if stringMode.Mode == nil || stringMode.Mode.CurrentModeID != "plan" {
		t.Fatalf("Mode = %+v, want plan from string mode", stringMode.Mode)
	}

	var legacyModes SessionInfo
	if err := json.Unmarshal([]byte(`{"sessionId":"session-1","modes":{"currentModeId":"default","availableModes":[{"modeId":"default","name":"Default"}]}}`), &legacyModes); err != nil {
		t.Fatalf("Unmarshal legacy modes error = %v", err)
	}
	if legacyModes.Mode == nil || legacyModes.Mode.CurrentModeID != "default" || len(legacyModes.Mode.AvailableModes) != 1 {
		t.Fatalf("Mode = %+v, want legacy modes fallback", legacyModes.Mode)
	}
}

func TestClientNewSessionSendsAdditionalDirectoriesWhenSupported(t *testing.T) {
	workspace := t.TempDir()
	client, server := newPipeClient(t, workspace)
	defer server.close()
	client.initialize = InitializeResult{
		ProtocolVersion: 1,
		AgentCapabilities: AgentCapabilities{
			SessionCapabilities: SessionCapabilities{
				AdditionalDirectories: map[string]any{},
			},
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/new" {
			t.Errorf("method = %q, want session/new", req.Method)
		}
		var params struct {
			Cwd                   string   `json:"cwd"`
			AdditionalDirectories []string `json:"additionalDirectories"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("Unmarshal params error = %v", err)
			return
		}
		if params.Cwd != "/repo" {
			t.Errorf("cwd = %q, want /repo", params.Cwd)
		}
		if len(params.AdditionalDirectories) != 1 || params.AdditionalDirectories[0] != workspace {
			t.Errorf("additionalDirectories = %#v, want workspace", params.AdditionalDirectories)
		}
		server.writeResponse(t, req.ID, map[string]any{"sessionId": "session-1"})
	}()

	if _, err := client.NewSession(context.Background(), "/repo"); err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	<-done
}

func TestClientSetConfigOption(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/set_config_option" {
			t.Errorf("method = %q, want session/set_config_option", req.Method)
		}
		var params struct {
			SessionID string `json:"sessionId"`
			ConfigID  string `json:"configId"`
			Value     string `json:"value"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("Unmarshal params error = %v", err)
			return
		}
		if params.SessionID != "session-1" || params.ConfigID != "model" || params.Value != "gpt-5.6" {
			t.Errorf("params = %+v, want model update", params)
		}
		server.writeResponse(t, req.ID, map[string]any{
			"configOptions": []map[string]any{
				{
					"id":           "model",
					"name":         "Model",
					"category":     "model",
					"type":         "select",
					"currentValue": "gpt-5.6",
					"options": []map[string]any{
						{"value": "gpt-5.6", "name": "GPT-5.6"},
					},
				},
			},
		})
	}()

	options, err := client.SetConfigOption(context.Background(), "session-1", "model", "gpt-5.6")
	if err != nil {
		t.Fatalf("SetConfigOption() error = %v", err)
	}
	if len(options) != 1 || options[0].ID != "model" || modelValueStringForTest(options[0].CurrentValue) != "gpt-5.6" {
		t.Fatalf("options = %+v, want updated model option", options)
	}
	<-done
}

func TestClientResumeSessionParsesReturnedState(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{
		ProtocolVersion: 1,
		AgentCapabilities: AgentCapabilities{
			SessionCapabilities: SessionCapabilities{
				Resume: map[string]any{},
			},
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/resume" {
			t.Errorf("method = %q, want session/resume", req.Method)
		}
		server.writeResponse(t, req.ID, map[string]any{
			"sessionId": "session-1",
			"configOptions": []map[string]any{
				{
					"id":           "model",
					"name":         "Model",
					"category":     "model",
					"type":         "select",
					"currentValue": "gpt-5.6",
				},
			},
		})
	}()

	info, err := client.ResumeSession(context.Background(), "session-1", "/repo")
	if err != nil {
		t.Fatalf("ResumeSession() error = %v", err)
	}
	if len(info.ConfigOptions) != 1 || info.ConfigOptions[0].ID != "model" || modelValueStringForTest(info.ConfigOptions[0].CurrentValue) != "gpt-5.6" {
		t.Fatalf("ConfigOptions = %+v, want resume state", info.ConfigOptions)
	}
	<-done
}

func TestClientCancelSession(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/cancel" {
			t.Errorf("method = %q, want session/cancel", req.Method)
		}
		var params struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("Unmarshal params error = %v", err)
			return
		}
		if params.SessionID != "session-1" {
			t.Errorf("sessionId = %q, want session-1", params.SessionID)
		}
		if req.ID != nil {
			t.Errorf("session/cancel id = %v, want notification without id", req.ID.Key())
		}
	}()

	if err := client.CancelSession(context.Background(), "session-1"); err != nil {
		t.Fatalf("CancelSession() error = %v", err)
	}
	<-done
}

func TestClientRejectsWorkspaceFileRequests(t *testing.T) {
	client, server := newPipeClient(t, t.TempDir())
	defer server.close()
	client.cwd = t.TempDir()
	_ = client

	for _, tc := range []struct {
		method string
		params map[string]any
	}{
		{
			method: "fs/write_text_file",
			params: map[string]any{
				"sessionId": "session-1",
				"path":      "/tmp/SOUL.md",
				"content":   "line1\nline2\nline3\n",
			},
		},
		{
			method: "fs/read_text_file",
			params: map[string]any{
				"sessionId": "session-1",
				"path":      "/tmp/SOUL.md",
				"line":      2,
				"limit":     1,
			},
		},
	} {
		t.Run(tc.method, func(t *testing.T) {
			server.writeRequest(t, 100, tc.method, tc.params)
			resp := server.readRequest(t)
			if resp.Error == nil {
				t.Fatalf("%s response error = nil, want unsupported method error", tc.method)
			}
			if resp.Error.Code != -32601 || !strings.Contains(resp.Error.Message, "method not supported") {
				t.Fatalf("%s response error = %+v, want unsupported method", tc.method, resp.Error)
			}
		})
	}
}

func TestClientHandlesStringIDAndPermissionRequests(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	server.writeRaw(t, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"session-1","options":[{"optionId":"reject","kind":"reject_once"}]}}`)
	resp := server.readRequest(t)
	if resp.ID == nil || resp.ID.Key() != `"perm-1"` {
		t.Fatalf("response id = %v, want string id", resp.ID)
	}
	if resp.Error != nil {
		t.Fatalf("permission response error = %+v", resp.Error)
	}
	var result struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Unmarshal permission result error = %v", err)
	}
	if result.Outcome.Outcome != "selected" || result.Outcome.OptionID != "reject" {
		t.Fatalf("permission outcome = %+v, want selected reject", result.Outcome)
	}

	_ = client
}

func TestClientPermissionRequestUsesHandlerAndToolCallState(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	gotReq := make(chan PermissionRequest, 1)
	generation := client.setPermissionHandler("session-1", context.Background(), func(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
		gotReq <- req
		return PermissionOutcome{Outcome: "selected", OptionID: "allow-once"}, nil
	})
	defer client.clearPermissionHandler("session-1", generation)

	server.writeNotification(t, "session/update", map[string]any{
		"sessionId": "session-1",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "call-1",
			"title":         "Run tests",
			"kind":          "execute",
			"status":        "pending",
			"rawInput": map[string]any{
				"command": "go test ./...",
			},
		},
	})
	server.writeRaw(t, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"session-1","toolCall":{"toolCallId":"call-1","title":"Run tests from request","kind":"execute","status":"pending"},"options":[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"},{"optionId":"reject","name":"Reject","kind":"reject_once"}]}}`)

	resp := server.readRequest(t)
	if resp.Error != nil {
		t.Fatalf("permission response error = %+v", resp.Error)
	}
	var result PermissionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Unmarshal permission result error = %v", err)
	}
	if result.Outcome.Outcome != "selected" || result.Outcome.OptionID != "allow-once" {
		t.Fatalf("permission outcome = %+v, want allow-once", result.Outcome)
	}
	select {
	case req := <-gotReq:
		if req.RequestID != `"perm-1"` {
			t.Fatalf("requestID = %q, want string JSON-RPC id", req.RequestID)
		}
		if req.ToolCall.ToolCallID != "call-1" {
			t.Fatalf("toolCallID = %q, want call-1", req.ToolCall.ToolCallID)
		}
		if req.ToolCall.Title != "Run tests from request" || req.ToolCall.Kind != "execute" || req.ToolCall.Status != "pending" {
			t.Fatalf("toolCall = %+v, want request tool title/kind/status", req.ToolCall)
		}
		if req.ToolCallState == nil || req.ToolCallState.Title != "Run tests" || req.ToolCallState.Kind != "execute" {
			t.Fatalf("toolCallState = %+v, want Run tests execute", req.ToolCallState)
		}
		if !strings.Contains(string(req.ToolCallState.RawInput), "go test ./...") {
			t.Fatalf("rawInput = %s, want command", req.ToolCallState.RawInput)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for permission request")
	}
}

func TestClientPermissionRequestReturnsCancelledForCanceledContext(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	generation := client.setPermissionHandler("session-1", ctx, func(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
		t.Fatal("handler should not be called when prompt context is canceled")
		return PermissionOutcome{}, nil
	})
	defer client.clearPermissionHandler("session-1", generation)

	server.writeRaw(t, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"session-1","toolCall":{"toolCallId":"call-1"},"options":[{"optionId":"reject","kind":"reject_once"}]}}`)

	resp := server.readRequest(t)
	if resp.Error != nil {
		t.Fatalf("permission response error = %+v", resp.Error)
	}
	var result PermissionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Unmarshal permission result error = %v", err)
	}
	if result.Outcome.Outcome != "cancelled" || result.Outcome.OptionID != "" {
		t.Fatalf("permission outcome = %+v, want cancelled", result.Outcome)
	}
}

func TestClientPermissionRequestReturnsCancelledForStaleToolCall(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	oldGeneration := client.setPermissionHandler("session-1", context.Background(), func(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
		return PermissionOutcome{Outcome: "selected", OptionID: "old"}, nil
	})
	server.writeNotification(t, "session/update", map[string]any{
		"sessionId": "session-1",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "call-old",
			"title":         "Old call",
		},
	})
	waitForToolCallSnapshot(t, client, "session-1", "call-old")
	newGeneration := client.setPermissionHandler("session-1", context.Background(), func(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
		t.Fatal("new handler should not receive stale permission request")
		return PermissionOutcome{}, nil
	})
	defer client.clearPermissionHandler("session-1", newGeneration)
	defer client.clearPermissionHandler("session-1", oldGeneration)

	server.writeRaw(t, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"session-1","toolCall":{"toolCallId":"call-old"},"options":[{"optionId":"reject","kind":"reject_once"}]}}`)

	resp := server.readRequest(t)
	if resp.Error != nil {
		t.Fatalf("permission response error = %+v", resp.Error)
	}
	var result PermissionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Unmarshal permission result error = %v", err)
	}
	if result.Outcome.Outcome != "cancelled" {
		t.Fatalf("permission outcome = %+v, want stale request cancelled", result.Outcome)
	}
}

func TestClientPermissionRequestReturnsCancelledForLateOldToolCallBeforeNewPromptWrite(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	oldGeneration := client.setPermissionHandler("session-1", context.Background(), func(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
		return PermissionOutcome{Outcome: "selected", OptionID: "old"}, nil
	})
	newGeneration := client.setPermissionHandlerPending("session-1", context.Background(), func(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
		t.Fatal("new handler should not receive late old-turn permission request")
		return PermissionOutcome{}, nil
	})
	defer client.clearPermissionHandler("session-1", newGeneration)
	defer client.clearPermissionHandler("session-1", oldGeneration)

	server.writeNotification(t, "session/update", map[string]any{
		"sessionId": "session-1",
		"update": map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    "call-late-old",
			"title":         "Late old call",
		},
	})
	waitForToolCallSnapshot(t, client, "session-1", "call-late-old")

	server.writeRaw(t, `{"jsonrpc":"2.0","id":"perm-late","method":"session/request_permission","params":{"sessionId":"session-1","toolCall":{"toolCallId":"call-late-old"},"options":[{"optionId":"reject","kind":"reject_once"}]}}`)
	resp := server.readRequest(t)
	if resp.Error != nil {
		t.Fatalf("permission response error = %+v", resp.Error)
	}
	var result PermissionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Unmarshal permission result error = %v", err)
	}
	if result.Outcome.Outcome != "cancelled" {
		t.Fatalf("permission outcome = %+v, want late old-turn request cancelled", result.Outcome)
	}
}

func TestClientPromptCollectsAgentMessageChunks(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/prompt" {
			t.Errorf("method = %q, want session/prompt", req.Method)
		}
		var params struct {
			SessionID string         `json:"sessionId"`
			Prompt    []ContentBlock `json:"prompt"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("Unmarshal params error = %v", err)
			return
		}
		if params.SessionID != "session-1" {
			t.Errorf("sessionId = %q, want session-1", params.SessionID)
		}
		if len(params.Prompt) != 1 || params.Prompt[0].Type != "text" || params.Prompt[0].Text != "你好" {
			t.Errorf("prompt = %+v, want one text block", params.Prompt)
		}
		server.writeNotification(t, "session/update", map[string]any{
			"sessionId": "session-1",
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content": map[string]any{
					"type": "text",
					"text": "第一段",
				},
			},
		})
		server.writeNotification(t, "session/update", map[string]any{
			"sessionId": "other-session",
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content": map[string]any{
					"type": "text",
					"text": "忽略",
				},
			},
		})
		server.writeNotification(t, "session/update", map[string]any{
			"sessionId": "session-1",
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content": map[string]any{
					"type": "text",
					"text": "第二段",
				},
			},
		})
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	reply, err := client.Prompt(context.Background(), "session-1", "你好")
	if err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
	if reply != "第一段第二段" {
		t.Fatalf("reply = %q, want collected chunks", reply)
	}
	<-done
}

func TestClientPromptWaitsForServerResponseAfterContextCancellation(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	ctx, cancel := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, err := client.Prompt(ctx, "session-1", "开始 review")
		firstResult <- err
	}()

	firstReq := server.readRequest(t)
	if firstReq.Method != "session/prompt" {
		t.Fatalf("method = %q, want session/prompt", firstReq.Method)
	}
	cancel()

	secondResult := make(chan error, 1)
	go func() {
		_, err := client.Prompt(context.Background(), "session-1", "新的普通消息")
		secondResult <- err
	}()
	secondRequest := make(chan Message, 1)
	go func() {
		secondRequest <- server.readRequest(t)
	}()

	select {
	case err := <-firstResult:
		t.Fatalf("Prompt() returned before server cancellation response: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case req := <-secondRequest:
		t.Fatalf("second prompt was written before first turn ended: %+v", req)
	case <-time.After(50 * time.Millisecond):
	}

	server.writeResponse(t, firstReq.ID, map[string]any{"stopReason": "cancelled"})
	select {
	case err := <-firstResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Prompt() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Prompt() did not return after server cancellation response")
	}

	var secondReq Message
	select {
	case secondReq = <-secondRequest:
		if secondReq.Method != "session/prompt" {
			t.Fatalf("second method = %q, want session/prompt", secondReq.Method)
		}
	case <-time.After(time.Second):
		t.Fatal("second prompt was not written after first turn ended")
	}
	server.writeResponse(t, secondReq.ID, map[string]any{"stopReason": "end_turn"})
	select {
	case err := <-secondResult:
		if err != nil {
			t.Fatalf("second Prompt() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second Prompt() did not return")
	}
}

func TestClientPromptIncludesJSONRPCErrorDetail(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	go func() {
		req := server.readRequest(t)
		server.writeRaw(t, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"Internal error","data":{"message":"cannot steer a review turn"}}}`,
			req.ID.Key(),
		))
	}()

	_, err := client.Prompt(context.Background(), "session-1", "新的普通消息")
	if err == nil || !strings.Contains(err.Error(), "detail=cannot steer a review turn") {
		t.Fatalf("Prompt() error = %v, want JSON-RPC data.message detail", err)
	}
}

func TestClientPromptWithOptionsReportsSessionUpdates(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/prompt" {
			t.Errorf("method = %q, want session/prompt", req.Method)
		}
		server.writeNotification(t, "session/update", map[string]any{
			"sessionId": "session-1",
			"update": map[string]any{
				"sessionUpdate": "agent_message",
				"message":       "开始处理。",
			},
		})
		server.writeNotification(t, "session/update", map[string]any{
			"sessionId": "session-1",
			"update": map[string]any{
				"sessionUpdate": "function_call",
				"name":          "exec_command",
			},
		})
		server.writeNotification(t, "session/update", map[string]any{
			"sessionId": "other-session",
			"update": map[string]any{
				"sessionUpdate": "agent_message",
				"message":       "忽略",
			},
		})
		server.writeNotification(t, "session/update", map[string]any{
			"sessionId": "session-1",
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content": map[string]any{
					"type": "text",
					"text": "最终回复",
				},
			},
		})
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	var updates []PromptUpdate
	reply, err := client.PromptWithOptions(context.Background(), "session-1", "你好", PromptOptions{
		OnUpdate: func(update PromptUpdate) {
			updates = append(updates, update)
		},
	})
	if err != nil {
		t.Fatalf("PromptWithOptions() error = %v", err)
	}
	if reply != "最终回复" {
		t.Fatalf("reply = %q, want collected chunks", reply)
	}
	if len(updates) != 3 {
		t.Fatalf("updates = %+v, want three updates for current session", updates)
	}
	if updates[0].Update.SessionUpdate != "agent_message" || updates[0].Update.Message != "开始处理。" {
		t.Fatalf("first update = %+v, want agent message", updates[0])
	}
	if updates[1].Update.SessionUpdate != "function_call" || updates[1].Update.Name != "exec_command" {
		t.Fatalf("second update = %+v, want function call", updates[1])
	}
	if len(updates[1].Update.Raw) == 0 {
		t.Fatalf("second update Raw is empty")
	}
	if updates[2].Update.SessionUpdate != "agent_message_chunk" {
		t.Fatalf("third update = %+v, want agent message chunk", updates[2])
	}
	<-done
}

func newPipeClient(t *testing.T, workspace ...string) (*Client, *pipeServer) {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	clientWorkspace := ""
	if len(workspace) > 0 {
		clientWorkspace = workspace[0]
	}
	client := &Client{
		stdin:          clientOut,
		workspace:      clientWorkspace,
		pending:        make(map[string]chan rpcResponse),
		updateHandlers: make(map[int64]UpdateHandler),
	}
	client.nextID.Store(1)
	go client.readLoop(clientIn)
	return client, &pipeServer{
		reader: bufio.NewReader(serverIn),
		writer: serverOut,
		closers: []io.Closer{
			serverIn,
			serverOut,
			clientIn,
			clientOut,
		},
	}
}

func waitForToolCallSnapshot(t *testing.T, client *Client, sessionID, toolCallID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if client.toolCallSnapshot(sessionID, toolCallID) != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for tool call snapshot %s", toolCallID)
}

type pipeServer struct {
	reader  *bufio.Reader
	writer  io.Writer
	closers []io.Closer
}

func (s *pipeServer) readRequest(t *testing.T) Message {
	t.Helper()
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		lineCh <- line
	}()
	select {
	case line := <-lineCh:
		var msg Message
		if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &msg); err != nil {
			t.Fatalf("Unmarshal request error = %v", err)
		}
		return msg
	case err := <-errCh:
		t.Fatalf("ReadString() error = %v", err)
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for request")
	}
	return Message{}
}

func (s *pipeServer) writeResponse(t *testing.T, id *RequestID, result any) {
	t.Helper()
	if id == nil {
		t.Fatalf("response id is nil")
	}
	s.write(t, Message{JSONRPC: "2.0", ID: id, Result: mustMarshal(t, result)})
}

func (s *pipeServer) writeRequest(t *testing.T, id int64, method string, params any) {
	t.Helper()
	s.write(t, Message{JSONRPC: "2.0", ID: NewRequestID(id), Method: method, Params: mustMarshal(t, params)})
}

func (s *pipeServer) writeNotification(t *testing.T, method string, params any) {
	t.Helper()
	s.write(t, Message{JSONRPC: "2.0", Method: method, Params: mustMarshal(t, params)})
}

func (s *pipeServer) write(t *testing.T, msg Message) {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal message error = %v", err)
	}
	if _, err := s.writer.Write(append(data, '\n')); err != nil {
		t.Fatalf("Write message error = %v", err)
	}
}

func (s *pipeServer) writeRaw(t *testing.T, raw string) {
	t.Helper()
	if _, err := s.writer.Write([]byte(raw + "\n")); err != nil {
		t.Fatalf("Write raw message error = %v", err)
	}
}

func (s *pipeServer) close() {
	for _, closer := range s.closers {
		_ = closer.Close()
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal raw error = %v", err)
	}
	return data
}

func modelValueStringForTest(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}
