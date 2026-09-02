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

func TestClientInitializeAdvertisesSupportedClientCapabilities(t *testing.T) {
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
					ClientInfo         ImplementationInfo         `json:"clientInfo"`
				}
				if err := json.Unmarshal(req.Params, &params); err != nil {
					t.Errorf("Unmarshal params error = %v", err)
					return
				}
				if params.ClientInfo.Name != "lark-acp-bridge" {
					t.Errorf("clientInfo.name = %q, want lark-acp-bridge", params.ClientInfo.Name)
				}
				if params.ClientInfo.Title != "Lark ACP Bridge" {
					t.Errorf("clientInfo.title = %q, want Lark ACP Bridge", params.ClientInfo.Title)
				}
				if params.ClientInfo.Version == "" {
					t.Errorf("clientInfo.version is empty, want declared")
				}
				if _, ok := params.ClientCapabilities["fs"]; ok {
					t.Errorf("clientCapabilities declares fs, want omitted")
				}
				if _, ok := params.ClientCapabilities["terminal"]; ok {
					t.Errorf("clientCapabilities declares terminal, want omitted")
				}
				var capabilities struct {
					ClientCapabilities struct {
						Session struct {
							ConfigOptions struct {
								Boolean map[string]any `json:"boolean"`
							} `json:"configOptions"`
						} `json:"session"`
					} `json:"clientCapabilities"`
				}
				if err := json.Unmarshal(req.Params, &capabilities); err != nil {
					t.Errorf("Unmarshal boolean capability error = %v", err)
					return
				}
				if capabilities.ClientCapabilities.Session.ConfigOptions.Boolean == nil {
					t.Errorf("clientCapabilities.session.configOptions.boolean = nil, want declared")
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
			"protocolVersion": 1,
			"agentCapabilities": map[string]any{
				"_meta": map[string]any{
					"zed.dev": map[string]any{
						"workspace": true,
					},
				},
			},
			"agentInfo": map[string]any{"name": "test-agent"},
			"authMethods": []map[string]any{
				{
					"id":          "agent-login",
					"name":        "Agent Login",
					"description": "Sign in using the agent's login flow",
				},
				{
					"id":   "explicit-agent-login",
					"name": "Explicit Agent Login",
					"type": "agent",
				},
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
	if len(methods) != 2 {
		t.Fatalf("AuthMethods = %+v, want two stored auth methods", methods)
	}
	if methods[0].ID != "agent-login" || methods[0].Name != "Agent Login" || methods[0].Description != "Sign in using the agent's login flow" || methods[0].Type != "agent" {
		t.Fatalf("first AuthMethod = %+v, want default agent type and metadata", methods[0])
	}
	if methods[1].ID != "explicit-agent-login" || methods[1].Type != "agent" {
		t.Fatalf("second AuthMethod = %+v, want explicit agent type", methods[1])
	}
	zedMeta, ok := client.initialize.AgentCapabilities.Meta["zed.dev"].(map[string]any)
	if !ok {
		t.Fatalf("AgentCapabilities.Meta = %#v, want zed.dev object", client.initialize.AgentCapabilities.Meta)
	}
	if workspace, ok := zedMeta["workspace"].(bool); !ok || !workspace {
		t.Fatalf("AgentCapabilities.Meta = %#v, want zed.dev.workspace true", client.initialize.AgentCapabilities.Meta)
	}
}

func TestClientAuthenticateUsesDeclaredMethodID(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{
		ProtocolVersion: 1,
		AuthMethods: []AuthMethod{
			{ID: "agent-login", Name: "Agent Login"},
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "authenticate" {
			t.Errorf("method = %q, want authenticate", req.Method)
		}
		var params struct {
			MethodID string `json:"methodId"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("Unmarshal params error = %v", err)
			return
		}
		if params.MethodID != "agent-login" {
			t.Errorf("methodId = %q, want agent-login", params.MethodID)
		}
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	if err := client.Authenticate(context.Background(), "agent-login"); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	<-done
}

func TestClientAuthenticateRejectsUndeclaredMethodID(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{
		ProtocolVersion: 1,
		AuthMethods: []AuthMethod{
			{ID: "agent-login"},
		},
	}

	for _, tc := range []struct {
		name     string
		methodID string
		want     string
	}{
		{name: "empty", methodID: " ", want: "ACP auth method id 为空"},
		{name: "undeclared", methodID: "browser-login", want: "ACP agent 未声明 auth method: browser-login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := client.Authenticate(context.Background(), tc.methodID)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Authenticate() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestClientAuthenticateRequiresInitialize(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	err := client.Authenticate(context.Background(), "agent-login")
	if err == nil || !strings.Contains(err.Error(), "ACP client 尚未 initialize") {
		t.Fatalf("Authenticate() error = %v, want initialize required error", err)
	}
}

func TestClientLogoutRequiresCapability(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	if err := client.Logout(context.Background()); err == nil || !strings.Contains(err.Error(), "auth.logout") {
		t.Fatalf("Logout() error = %v, want missing logout capability", err)
	}
}

func TestClientLogoutRequiresInitialize(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	err := client.Logout(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ACP client 尚未 initialize") {
		t.Fatalf("Logout() error = %v, want initialize required error", err)
	}
}

func TestClientLogoutSendsRequestWhenSupported(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{
		ProtocolVersion: 1,
		AgentCapabilities: AgentCapabilities{
			Auth: AuthCapabilities{
				Logout: map[string]any{},
			},
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "logout" {
			t.Errorf("method = %q, want logout", req.Method)
		}
		var params map[string]any
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("Unmarshal params error = %v", err)
			return
		}
		if len(params) != 0 {
			t.Errorf("params = %#v, want empty object", params)
		}
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	if err := client.Logout(context.Background()); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	<-done
}

func TestClientInitializeClosesOnUnsupportedProtocolVersion(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "initialize" {
			t.Errorf("method = %q, want initialize", req.Method)
		}
		server.writeResponse(t, req.ID, map[string]any{
			"protocolVersion":   2,
			"agentCapabilities": map[string]any{},
			"agentInfo":         map[string]any{"name": "future-agent"},
		})
	}()

	err := client.Initialize(context.Background())
	if err == nil || !strings.Contains(err.Error(), "不支持的 ACP protocolVersion: 2") {
		t.Fatalf("Initialize() error = %v, want unsupported protocol version", err)
	}
	<-done

	if _, err := client.NewSession(context.Background(), "/repo"); err == nil {
		t.Fatal("NewSession() error = nil, want closed client write error")
	}
}

func TestClientNewSessionSendsMCPServers(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

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
				{
					"id":           "temperature",
					"name":         "Temperature",
					"type":         "slider",
					"currentValue": 0.8,
				},
			},
			"mode": map[string]any{
				"currentModeId": "default",
				"availableModes": []map[string]any{
					{"id": "default", "name": "Default"},
					{"id": "plan", "name": "Plan"},
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
	if sessionInfo.Mode == nil || sessionInfo.Mode.CurrentModeID != "default" || len(sessionInfo.Mode.AvailableModes) != 2 || sessionInfo.Mode.AvailableModes[1].ModeID != "plan" {
		t.Fatalf("Mode = %+v, want default mode state", sessionInfo.Mode)
	}
	<-done
}

func TestSessionInfoTrimsAvailableCommands(t *testing.T) {
	var sessionInfo SessionInfo
	if err := json.Unmarshal([]byte(`{
		"sessionId": "session-1",
		"availableCommands": [
			{
				"name": " review ",
				"description": " Review current changes ",
				"input": {"hint": " optional focus "}
			}
		]
	}`), &sessionInfo); err != nil {
		t.Fatalf("Unmarshal SessionInfo error = %v", err)
	}
	if len(sessionInfo.AvailableCommands) != 1 {
		t.Fatalf("AvailableCommands = %+v, want one command", sessionInfo.AvailableCommands)
	}
	cmd := sessionInfo.AvailableCommands[0]
	if cmd.Name != "review" || cmd.Description != "Review current changes" {
		t.Fatalf("command = %+v, want trimmed name and description", cmd)
	}
	if cmd.Input == nil || cmd.Input.Hint != "optional focus" {
		t.Fatalf("command input = %+v, want trimmed hint", cmd.Input)
	}
}

func TestClientNewSessionRequiresInitialize(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	_, err := client.NewSession(context.Background(), "/repo")
	if err == nil || !strings.Contains(err.Error(), "ACP client 尚未 initialize") {
		t.Fatalf("NewSession() error = %v, want initialize required error", err)
	}
}

func TestClientSessionRestoreRequiresInitialize(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	for _, tc := range []struct {
		name string
		call func(context.Context) error
	}{
		{
			name: "load",
			call: func(ctx context.Context) error {
				_, err := client.LoadSession(ctx, "session-1", "/repo")
				return err
			},
		},
		{
			name: "resume",
			call: func(ctx context.Context) error {
				_, err := client.ResumeSession(ctx, "session-1", "/repo")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(context.Background())
			if err == nil || !strings.Contains(err.Error(), "ACP client 尚未 initialize") {
				t.Fatalf("call error = %v, want initialize required error", err)
			}
		})
	}
}

func TestClientSessionLifecycleRejectsRelativeCwd(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{
		ProtocolVersion: 1,
		AgentCapabilities: AgentCapabilities{
			LoadSession: true,
			SessionCapabilities: SessionCapabilities{
				Resume: map[string]any{},
			},
		},
	}

	for _, tc := range []struct {
		name string
		call func(context.Context) error
	}{
		{
			name: "new",
			call: func(ctx context.Context) error {
				_, err := client.NewSession(ctx, "relative/repo")
				return err
			},
		},
		{
			name: "load",
			call: func(ctx context.Context) error {
				_, err := client.LoadSession(ctx, "session-1", "relative/repo")
				return err
			},
		},
		{
			name: "resume",
			call: func(ctx context.Context) error {
				_, err := client.ResumeSession(ctx, "session-1", "relative/repo")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(context.Background())
			if err == nil || !strings.Contains(err.Error(), "ACP session cwd 必须是绝对路径") {
				t.Fatalf("call error = %v, want absolute cwd error", err)
			}
		})
	}
}

func TestClientSessionRestoreRejectsEmptySessionID(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{
		ProtocolVersion: 1,
		AgentCapabilities: AgentCapabilities{
			LoadSession: true,
			SessionCapabilities: SessionCapabilities{
				Resume: map[string]any{},
			},
		},
	}

	for _, tc := range []struct {
		name string
		call func(context.Context) error
	}{
		{
			name: "load",
			call: func(ctx context.Context) error {
				_, err := client.LoadSession(ctx, " ", "/repo")
				return err
			},
		},
		{
			name: "resume",
			call: func(ctx context.Context) error {
				_, err := client.ResumeSession(ctx, " ", "/repo")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(context.Background())
			if err == nil || !strings.Contains(err.Error(), "ACP session id 为空") {
				t.Fatalf("call error = %v, want empty session id error", err)
			}
		})
	}
}

func TestClientLoadSessionRequiresCapability(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	_, err := client.LoadSession(context.Background(), "session-1", "/repo")
	if err == nil || !strings.Contains(err.Error(), "ACP agent 未声明 loadSession capability") {
		t.Fatalf("LoadSession() error = %v, want missing loadSession capability", err)
	}
}

func TestClientSessionRestoreSendsMCPServers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		setup  func(*Client)
		call   func(context.Context, *Client) error
	}{
		{
			name:   "load",
			method: "session/load",
			setup: func(client *Client) {
				client.initialize = InitializeResult{
					ProtocolVersion: 1,
					AgentCapabilities: AgentCapabilities{
						LoadSession: true,
					},
				}
			},
			call: func(ctx context.Context, client *Client) error {
				_, err := client.LoadSession(ctx, "session-1", "/repo")
				return err
			},
		},
		{
			name:   "resume",
			method: "session/resume",
			setup: func(client *Client) {
				client.initialize = InitializeResult{
					ProtocolVersion: 1,
					AgentCapabilities: AgentCapabilities{
						SessionCapabilities: SessionCapabilities{
							Resume: map[string]any{},
						},
					},
				}
			},
			call: func(ctx context.Context, client *Client) error {
				_, err := client.ResumeSession(ctx, "session-1", "/repo")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := newPipeClient(t)
			defer server.close()
			tc.setup(client)

			done := make(chan struct{})
			go func() {
				defer close(done)
				req := server.readRequest(t)
				if req.Method != tc.method {
					t.Errorf("method = %q, want %s", req.Method, tc.method)
				}
				var params struct {
					SessionID  string `json:"sessionId"`
					Cwd        string `json:"cwd"`
					MCPServers []any  `json:"mcpServers"`
				}
				if err := json.Unmarshal(req.Params, &params); err != nil {
					t.Errorf("Unmarshal params error = %v", err)
					return
				}
				if params.SessionID != "session-1" {
					t.Errorf("sessionId = %q, want session-1", params.SessionID)
				}
				if params.Cwd != "/repo" {
					t.Errorf("cwd = %q, want /repo", params.Cwd)
				}
				if params.MCPServers == nil || len(params.MCPServers) != 0 {
					t.Errorf("mcpServers = %#v, want empty array", params.MCPServers)
				}
				server.writeResponse(t, req.ID, map[string]any{"sessionId": "session-1"})
			}()

			if err := tc.call(context.Background(), client); err != nil {
				t.Fatalf("%s error = %v", tc.method, err)
			}
			<-done
		})
	}
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
	if err := json.Unmarshal([]byte(`{"sessionId":"session-1","modes":{"currentModeId":"default","availableModes":[{"id":"default","name":"Default"}]}}`), &legacyModes); err != nil {
		t.Fatalf("Unmarshal legacy modes error = %v", err)
	}
	if legacyModes.Mode == nil || legacyModes.Mode.CurrentModeID != "default" || len(legacyModes.Mode.AvailableModes) != 1 || legacyModes.Mode.AvailableModes[0].ModeID != "default" {
		t.Fatalf("Mode = %+v, want legacy modes fallback", legacyModes.Mode)
	}

	var legacyModeID SessionInfo
	if err := json.Unmarshal([]byte(`{"sessionId":"session-1","modes":{"currentModeId":"default","availableModes":[{"modeId":"plan","name":"Plan"}]}}`), &legacyModeID); err != nil {
		t.Fatalf("Unmarshal legacy modeId error = %v", err)
	}
	if legacyModeID.Mode == nil || len(legacyModeID.Mode.AvailableModes) != 1 || legacyModeID.Mode.AvailableModes[0].ModeID != "plan" {
		t.Fatalf("Mode = %+v, want legacy modeId fallback", legacyModeID.Mode)
	}
}

func TestSessionInfoTrimsModelStateFields(t *testing.T) {
	var info SessionInfo
	if err := json.Unmarshal([]byte(`{
		"sessionId":"session-1",
		"models":{
			"currentModelId":" gpt-5.5 ",
			"availableModels":[{"modelId":" gpt-5.5 ","name":" GPT-5.5 ","description":" default model ","_meta":{"trae":{"load":{"percent":10}}}}]
		}
	}`), &info); err != nil {
		t.Fatalf("Unmarshal model state error = %v", err)
	}
	if info.Models == nil || info.Models.CurrentModelID != "gpt-5.5" {
		t.Fatalf("Models = %+v, want trimmed current model", info.Models)
	}
	if len(info.Models.AvailableModels) != 1 {
		t.Fatalf("AvailableModels = %+v, want one model", info.Models.AvailableModels)
	}
	got := info.Models.AvailableModels[0]
	if got.ModelID != "gpt-5.5" || got.Name != "GPT-5.5" || got.Description != "default model" {
		t.Fatalf("model = %+v, want trimmed fields", got)
	}
	if percent, ok := TraeModelLoadPercent(got.Meta); !ok || percent != 10 {
		t.Fatalf("model meta = %+v, want load percent 10", got.Meta)
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

func TestClientSessionRestoreSendsAdditionalDirectoriesWhenSupported(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		setup  func(*Client)
		call   func(context.Context, *Client) error
	}{
		{
			name:   "load",
			method: "session/load",
			setup: func(client *Client) {
				client.initialize = InitializeResult{
					ProtocolVersion: 1,
					AgentCapabilities: AgentCapabilities{
						LoadSession: true,
						SessionCapabilities: SessionCapabilities{
							AdditionalDirectories: map[string]any{},
						},
					},
				}
			},
			call: func(ctx context.Context, client *Client) error {
				_, err := client.LoadSession(ctx, "session-1", "/repo")
				return err
			},
		},
		{
			name:   "resume",
			method: "session/resume",
			setup: func(client *Client) {
				client.initialize = InitializeResult{
					ProtocolVersion: 1,
					AgentCapabilities: AgentCapabilities{
						SessionCapabilities: SessionCapabilities{
							AdditionalDirectories: map[string]any{},
							Resume:                map[string]any{},
						},
					},
				}
			},
			call: func(ctx context.Context, client *Client) error {
				_, err := client.ResumeSession(ctx, "session-1", "/repo")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			client, server := newPipeClient(t, workspace)
			defer server.close()
			tc.setup(client)

			done := make(chan struct{})
			go func() {
				defer close(done)
				req := server.readRequest(t)
				if req.Method != tc.method {
					t.Errorf("method = %q, want %s", req.Method, tc.method)
				}
				var params struct {
					SessionID             string   `json:"sessionId"`
					Cwd                   string   `json:"cwd"`
					AdditionalDirectories []string `json:"additionalDirectories"`
				}
				if err := json.Unmarshal(req.Params, &params); err != nil {
					t.Errorf("Unmarshal params error = %v", err)
					return
				}
				if params.SessionID != "session-1" {
					t.Errorf("sessionId = %q, want session-1", params.SessionID)
				}
				if params.Cwd != "/repo" {
					t.Errorf("cwd = %q, want /repo", params.Cwd)
				}
				if len(params.AdditionalDirectories) != 1 || params.AdditionalDirectories[0] != workspace {
					t.Errorf("additionalDirectories = %#v, want workspace", params.AdditionalDirectories)
				}
				server.writeResponse(t, req.ID, map[string]any{"sessionId": "session-1"})
			}()

			if err := tc.call(context.Background(), client); err != nil {
				t.Fatalf("%s error = %v", tc.method, err)
			}
			<-done
		})
	}
}

func TestClientCloseSessionRequiresCapability(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	if err := client.CloseSession(context.Background(), "session-1"); err == nil || !strings.Contains(err.Error(), "sessionCapabilities.close") {
		t.Fatalf("CloseSession() error = %v, want missing close capability", err)
	}
}

func TestClientCloseSessionRequiresInitialize(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	err := client.CloseSession(context.Background(), "session-1")
	if err == nil || !strings.Contains(err.Error(), "ACP client 尚未 initialize") {
		t.Fatalf("CloseSession() error = %v, want initialize required error", err)
	}
}

func TestCapabilitiesTreatExplicitFalseAsUnsupported(t *testing.T) {
	sessionCapabilities := SessionCapabilities{
		Resume:                false,
		Close:                 false,
		Delete:                true,
		List:                  map[string]any{},
		AdditionalDirectories: false,
	}
	if sessionCapabilities.SupportsResume() {
		t.Fatal("SupportsResume() = true, want false for explicit false")
	}
	if sessionCapabilities.SupportsClose() {
		t.Fatal("SupportsClose() = true, want false for explicit false")
	}
	if !sessionCapabilities.SupportsDelete() {
		t.Fatal("SupportsDelete() = false, want true for explicit true")
	}
	if !sessionCapabilities.SupportsList() {
		t.Fatal("SupportsList() = false, want true for object capability")
	}
	if sessionCapabilities.SupportsAdditionalDirectories() {
		t.Fatal("SupportsAdditionalDirectories() = true, want false for explicit false")
	}

	if (AuthCapabilities{Logout: false}).SupportsLogout() {
		t.Fatal("SupportsLogout() = true, want false for explicit false")
	}
	if !(AuthCapabilities{Logout: true}).SupportsLogout() {
		t.Fatal("SupportsLogout() = false, want true for explicit true")
	}
}

func TestClientCloseSessionSendsRequestWhenSupported(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{
		ProtocolVersion: 1,
		AgentCapabilities: AgentCapabilities{
			SessionCapabilities: SessionCapabilities{
				Close: map[string]any{},
			},
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/close" {
			t.Errorf("method = %q, want session/close", req.Method)
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
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	if err := client.CloseSession(context.Background(), "session-1"); err != nil {
		t.Fatalf("CloseSession() error = %v", err)
	}
	<-done
}

func TestClientDeleteSessionRequiresCapability(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	if err := client.DeleteSession(context.Background(), "session-1"); err == nil || !strings.Contains(err.Error(), "sessionCapabilities.delete") {
		t.Fatalf("DeleteSession() error = %v, want missing delete capability", err)
	}
	if err := client.DeleteSession(context.Background(), " "); err == nil || !strings.Contains(err.Error(), "ACP session id 为空") {
		t.Fatalf("DeleteSession() empty id error = %v, want empty session id error", err)
	}
}

func TestClientDeleteSessionRequiresInitialize(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	err := client.DeleteSession(context.Background(), "session-1")
	if err == nil || !strings.Contains(err.Error(), "ACP client 尚未 initialize") {
		t.Fatalf("DeleteSession() error = %v, want initialize required error", err)
	}
}

func TestClientDeleteSessionSendsRequestWhenSupported(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{
		ProtocolVersion: 1,
		AgentCapabilities: AgentCapabilities{
			SessionCapabilities: SessionCapabilities{
				Delete: map[string]any{},
			},
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/delete" {
			t.Errorf("method = %q, want session/delete", req.Method)
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
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	if err := client.DeleteSession(context.Background(), "session-1"); err != nil {
		t.Fatalf("DeleteSession() error = %v", err)
	}
	<-done
}

func TestClientListSessionsRequiresCapability(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	if _, err := client.ListSessions(context.Background(), SessionListOptions{}); err == nil || !strings.Contains(err.Error(), "sessionCapabilities.list") {
		t.Fatalf("ListSessions() error = %v, want missing list capability", err)
	}
}

func TestClientListSessionsRequiresInitialize(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	_, err := client.ListSessions(context.Background(), SessionListOptions{})
	if err == nil || !strings.Contains(err.Error(), "ACP client 尚未 initialize") {
		t.Fatalf("ListSessions() error = %v, want initialize required error", err)
	}
}

func TestClientListSessionsRejectsRelativeCwd(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{
		ProtocolVersion: 1,
		AgentCapabilities: AgentCapabilities{
			SessionCapabilities: SessionCapabilities{
				List: map[string]any{},
			},
		},
	}

	_, err := client.ListSessions(context.Background(), SessionListOptions{Cwd: "relative/repo"})
	if err == nil || !strings.Contains(err.Error(), "ACP session cwd 必须是绝对路径") {
		t.Fatalf("ListSessions() error = %v, want absolute cwd error", err)
	}
}

func TestClientListSessionsSendsParamsAndParsesResult(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{
		ProtocolVersion: 1,
		AgentCapabilities: AgentCapabilities{
			SessionCapabilities: SessionCapabilities{
				List: map[string]any{},
			},
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/list" {
			t.Errorf("method = %q, want session/list", req.Method)
		}
		var params struct {
			Cwd    string `json:"cwd"`
			Cursor string `json:"cursor"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("Unmarshal params error = %v", err)
			return
		}
		if params.Cwd != "/repo" {
			t.Errorf("cwd = %q, want /repo", params.Cwd)
		}
		if params.Cursor != "opaque.cursor/token==" {
			t.Errorf("cursor = %q, want opaque cursor", params.Cursor)
		}
		server.writeResponse(t, req.ID, map[string]any{
			"sessions": []map[string]any{
				{
					"sessionId":             "session-1",
					"cwd":                   "/repo",
					"additionalDirectories": []string{"/workspace"},
					"title":                 "实现 session list",
					"updatedAt":             "2025-10-29T14:22:15Z",
					"_meta": map[string]any{
						"messageCount": 12,
					},
				},
			},
			"nextCursor": "next.cursor",
		})
	}()

	result, err := client.ListSessions(context.Background(), SessionListOptions{
		Cwd:    "/repo",
		Cursor: "opaque.cursor/token==",
	})
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if result.NextCursor != "next.cursor" {
		t.Fatalf("NextCursor = %q, want next.cursor", result.NextCursor)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("Sessions len = %d, want 1", len(result.Sessions))
	}
	session := result.Sessions[0]
	if session.SessionID != "session-1" || session.Cwd != "/repo" || session.Title != "实现 session list" || session.UpdatedAt != "2025-10-29T14:22:15Z" {
		t.Fatalf("session = %+v, want parsed session metadata", session)
	}
	if len(session.AdditionalDirectories) != 1 || session.AdditionalDirectories[0] != "/workspace" {
		t.Fatalf("AdditionalDirectories = %#v, want /workspace", session.AdditionalDirectories)
	}
	if session.Meta["messageCount"] != float64(12) {
		t.Fatalf("Meta = %#v, want messageCount 12", session.Meta)
	}
	<-done
}

func TestClientListSessionsAllowsEmptyParamsAndNormalizesEmptySessions(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{
		ProtocolVersion: 1,
		AgentCapabilities: AgentCapabilities{
			SessionCapabilities: SessionCapabilities{
				List: map[string]any{},
			},
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/list" {
			t.Errorf("method = %q, want session/list", req.Method)
		}
		var params map[string]any
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("Unmarshal params error = %v", err)
			return
		}
		if len(params) != 0 {
			t.Errorf("params = %#v, want empty object", params)
		}
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	result, err := client.ListSessions(context.Background(), SessionListOptions{})
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if result.Sessions == nil {
		t.Fatal("Sessions = nil, want empty slice")
	}
	if len(result.Sessions) != 0 {
		t.Fatalf("Sessions = %+v, want empty slice", result.Sessions)
	}
	if result.NextCursor != "" {
		t.Fatalf("NextCursor = %q, want empty", result.NextCursor)
	}
	<-done
}

func TestClientSetConfigOption(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

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
					"id":           "temperature",
					"name":         "Temperature",
					"type":         "slider",
					"currentValue": 0.8,
				},
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

func TestClientSetConfigOptionBooleanIncludesType(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

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
			Type      string `json:"type"`
			Value     bool   `json:"value"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("Unmarshal params error = %v", err)
			return
		}
		if params.SessionID != "session-1" || params.ConfigID != "brave_mode" || params.Type != "boolean" || !params.Value {
			t.Errorf("params = %+v, want boolean config update", params)
		}
		server.writeResponse(t, req.ID, map[string]any{
			"configOptions": []map[string]any{
				{
					"id":           "brave_mode",
					"name":         "Brave Mode",
					"type":         "boolean",
					"currentValue": true,
				},
			},
		})
	}()

	options, err := client.SetConfigOption(context.Background(), "session-1", "brave_mode", true)
	if err != nil {
		t.Fatalf("SetConfigOption() error = %v", err)
	}
	if len(options) != 1 || options[0].ID != "brave_mode" || options[0].Type != "boolean" || options[0].CurrentValue != true {
		t.Fatalf("options = %+v, want updated boolean option", options)
	}
	<-done
}

func TestSessionUpdateIgnoresUnsupportedConfigOptionType(t *testing.T) {
	var update SessionUpdate
	if err := json.Unmarshal([]byte(`{
		"sessionUpdate": "config_option_update",
		"configOptions": [
			{
				"id": "temperature",
				"name": "Temperature",
				"type": "slider",
				"currentValue": 0.8
			},
			{
				"id": "model",
				"name": "Model",
				"type": "select",
				"currentValue": "gpt-5.6",
				"options": [
					{"value": "gpt-5.6", "name": "GPT-5.6"}
				]
			}
		]
	}`), &update); err != nil {
		t.Fatalf("Unmarshal SessionUpdate error = %v", err)
	}
	if len(update.ConfigOptions) != 1 || update.ConfigOptions[0].ID != "model" {
		t.Fatalf("ConfigOptions = %+v, want only supported model option", update.ConfigOptions)
	}
}

func TestSessionUpdateTrimsAvailableCommands(t *testing.T) {
	var update SessionUpdate
	if err := json.Unmarshal([]byte(`{
		"sessionUpdate": "available_commands_update",
		"availableCommands": [
			{
				"name": " review ",
				"description": " Review current changes ",
				"input": {"hint": " optional focus "}
			}
		]
	}`), &update); err != nil {
		t.Fatalf("Unmarshal SessionUpdate error = %v", err)
	}
	if len(update.AvailableCommands) != 1 {
		t.Fatalf("AvailableCommands = %+v, want one command", update.AvailableCommands)
	}
	cmd := update.AvailableCommands[0]
	if cmd.Name != "review" || cmd.Description != "Review current changes" {
		t.Fatalf("command = %+v, want trimmed name and description", cmd)
	}
	if cmd.Input == nil || cmd.Input.Hint != "optional focus" {
		t.Fatalf("command input = %+v, want trimmed hint", cmd.Input)
	}
}

func TestConfigOptionsTrimStructuredFields(t *testing.T) {
	var info SessionInfo
	if err := json.Unmarshal([]byte(`{
		"sessionId": "session-1",
		"configOptions": [
			{
				"id": " model ",
				"name": " Model ",
				"category": " model ",
				"type": " select ",
				"currentValue": " gpt-5.6 ",
				"options": [
					{"value": " gpt-5.6 ", "name": " GPT-5.6 ", "_meta": {"trae": {"load": {"percent": 47}}}}
				]
			}
		]
	}`), &info); err != nil {
		t.Fatalf("Unmarshal SessionInfo error = %v", err)
	}
	if len(info.ConfigOptions) != 1 {
		t.Fatalf("ConfigOptions = %+v, want one normalized option", info.ConfigOptions)
	}
	option := info.ConfigOptions[0]
	if option.ID != "model" || option.Category != "model" || option.Type != "select" {
		t.Fatalf("option = %+v, want trimmed id/category/type", option)
	}
	if len(option.Options) != 1 || option.Options[0].Value != "gpt-5.6" || option.Options[0].Name != "GPT-5.6" {
		t.Fatalf("option values = %+v, want trimmed value/name", option.Options)
	}
	if percent, ok := TraeModelLoadPercent(option.Options[0].Meta); !ok || percent != 47 {
		t.Fatalf("option meta = %+v, want load percent 47", option.Options[0].Meta)
	}
}

func TestClientSetMode(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/set_mode" {
			t.Errorf("method = %q, want session/set_mode", req.Method)
		}
		var params struct {
			SessionID string `json:"sessionId"`
			ModeID    string `json:"modeId"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("Unmarshal params error = %v", err)
			return
		}
		if params.SessionID != "session-1" || params.ModeID != "plan" {
			t.Errorf("params = %+v, want mode update", params)
		}
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	if err := client.SetMode(context.Background(), "session-1", "plan"); err != nil {
		t.Fatalf("SetMode() error = %v", err)
	}
	<-done
}

func TestClientSetModeRejectsMissingRequiredFields(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	for _, tc := range []struct {
		name      string
		sessionID string
		modeID    string
		want      string
	}{
		{name: "empty session", sessionID: " ", modeID: "plan", want: "ACP session id 为空"},
		{name: "empty mode", sessionID: "session-1", modeID: " ", want: "ACP mode id 为空"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := client.SetMode(context.Background(), tc.sessionID, tc.modeID)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SetMode() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestClientSetModeRequiresInitialize(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	err := client.SetMode(context.Background(), "session-1", "plan")
	if err == nil || !strings.Contains(err.Error(), "ACP client 尚未 initialize") {
		t.Fatalf("SetMode() error = %v, want initialize required error", err)
	}
}

func TestClientSetConfigOptionRejectsMissingRequiredFields(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	for _, tc := range []struct {
		name      string
		sessionID string
		configID  string
		want      string
	}{
		{name: "empty session", sessionID: " ", configID: "model", want: "ACP session id 为空"},
		{name: "empty config", sessionID: "session-1", configID: " ", want: "ACP config id 为空"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.SetConfigOption(context.Background(), tc.sessionID, tc.configID, "value")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SetConfigOption() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestClientSetConfigOptionRequiresInitialize(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	_, err := client.SetConfigOption(context.Background(), "session-1", "model", "gpt-5.6")
	if err == nil || !strings.Contains(err.Error(), "ACP client 尚未 initialize") {
		t.Fatalf("SetConfigOption() error = %v, want initialize required error", err)
	}
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
	client.initialize = InitializeResult{ProtocolVersion: 1}

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

func TestClientCancelSessionRejectsEmptySessionID(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	if err := client.CancelSession(context.Background(), " "); err == nil || !strings.Contains(err.Error(), "ACP session id 为空") {
		t.Fatalf("CancelSession() error = %v, want empty session id error", err)
	}
}

func TestClientCancelSessionRequiresInitialize(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	err := client.CancelSession(context.Background(), "session-1")
	if err == nil || !strings.Contains(err.Error(), "ACP client 尚未 initialize") {
		t.Fatalf("CancelSession() error = %v, want initialize required error", err)
	}
}

func TestClientRejectsUndeclaredClientCapabilityRequests(t *testing.T) {
	_, server := newPipeClient(t, t.TempDir())
	defer server.close()

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
		{
			method: "terminal/create",
			params: map[string]any{
				"sessionId": "session-1",
				"command":   "go",
				"args":      []string{"test", "./..."},
				"cwd":       "/tmp",
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

func TestClientWriteUsesNewlineDelimitedJSON(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	client := &Client{stdin: writer}

	done := make(chan error, 1)
	go func() {
		done <- client.write(Message{
			JSONRPC: "2.0",
			Method:  "session/cancel",
		})
	}()

	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString() error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("write() error = %v", err)
	}
	if !strings.HasSuffix(line, "\n") {
		t.Fatalf("line = %q, want trailing newline", line)
	}
	if strings.Contains(strings.TrimSuffix(line, "\n"), "\n") {
		t.Fatalf("line = %q, want exactly one JSON-RPC message line", line)
	}
	var msg Message
	if err := json.Unmarshal([]byte(strings.TrimSuffix(line, "\n")), &msg); err != nil {
		t.Fatalf("Unmarshal written line error = %v", err)
	}
	if msg.JSONRPC != "2.0" || msg.Method != "session/cancel" {
		t.Fatalf("message = %+v, want session/cancel JSON-RPC message", msg)
	}
}

func TestClientReplyResultMapsCanceledError(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		client.replyResult(Message{
			JSONRPC: "2.0",
			ID:      NewRequestID(1),
		}, nil, context.Canceled)
	}()

	resp := server.readRequest(t)
	if resp.Error == nil {
		t.Fatal("response error = nil, want cancelled error")
	}
	if resp.Error.Code != -32800 || resp.Error.Message != "Request Cancelled" {
		t.Fatalf("response error = %+v, want -32800 Request Cancelled", resp.Error)
	}
	<-done
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
	server.writeNotification(t, "session/update", map[string]any{
		"sessionId": "session-1",
		"update": map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    "call-1",
			"status":        "in_progress",
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
		if req.ToolCallState == nil || req.ToolCallState.Title != "Run tests" || req.ToolCallState.Kind != "execute" || req.ToolCallState.Status != "in_progress" {
			t.Fatalf("toolCallState = %+v, want merged Run tests execute in_progress", req.ToolCallState)
		}
		if len(req.Options) != 2 {
			t.Fatalf("options = %+v, want two permission options", req.Options)
		}
		if req.Options[0].OptionID != "allow-once" || req.Options[0].Name != "Allow once" || req.Options[0].Kind != "allow_once" {
			t.Fatalf("first option = %+v, want allow-once name and kind", req.Options[0])
		}
		if req.Options[1].OptionID != "reject" || req.Options[1].Name != "Reject" || req.Options[1].Kind != "reject_once" {
			t.Fatalf("second option = %+v, want reject name and kind", req.Options[1])
		}
		if !strings.Contains(string(req.ToolCallState.RawInput), "go test ./...") {
			t.Fatalf("rawInput = %s, want command", req.ToolCallState.RawInput)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for permission request")
	}
}

func TestClientPermissionRequestNormalizesHandlerRequestFields(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	gotReq := make(chan PermissionRequest, 1)
	generation := client.setPermissionHandler("session-1", context.Background(), func(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
		gotReq <- req
		return PermissionOutcome{Outcome: "selected", OptionID: "allow-once"}, nil
	})
	defer client.clearPermissionHandler("session-1", generation)

	server.writeRaw(t, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":" session-1 ","toolCall":{"toolCallId":" call-1 ","title":" Run tests ","kind":" execute ","status":" pending "},"options":[{"optionId":" allow-once ","name":" Allow once ","kind":" allow_once "}]}}`)

	resp := server.readRequest(t)
	if resp.Error != nil {
		t.Fatalf("permission response error = %+v", resp.Error)
	}
	var result PermissionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Unmarshal permission result error = %v", err)
	}
	if result.Outcome.Outcome != "selected" || result.Outcome.OptionID != "allow-once" {
		t.Fatalf("permission outcome = %+v, want selected allow-once", result.Outcome)
	}
	select {
	case req := <-gotReq:
		if req.SessionID != "session-1" {
			t.Fatalf("sessionID = %q, want session-1", req.SessionID)
		}
		if req.ToolCall.ToolCallID != "call-1" || req.ToolCall.Title != "Run tests" || req.ToolCall.Kind != "execute" || req.ToolCall.Status != "pending" {
			t.Fatalf("toolCall = %+v, want trimmed fields", req.ToolCall)
		}
		if len(req.Options) != 1 {
			t.Fatalf("options = %+v, want one permission option", req.Options)
		}
		if req.Options[0].OptionID != "allow-once" || req.Options[0].Name != "Allow once" || req.Options[0].Kind != "allow_once" {
			t.Fatalf("option = %+v, want trimmed fields", req.Options[0])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for permission request")
	}
}

func TestClientPermissionRequestNormalizesDefaultRejectOption(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	server.writeRaw(t, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"session-1","options":[{"optionId":" reject ","kind":" reject_once "}]}}`)

	resp := server.readRequest(t)
	if resp.Error != nil {
		t.Fatalf("permission response error = %+v", resp.Error)
	}
	var result PermissionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Unmarshal permission result error = %v", err)
	}
	if result.Outcome.Outcome != "selected" || result.Outcome.OptionID != "reject" {
		t.Fatalf("permission outcome = %+v, want selected reject", result.Outcome)
	}

	_ = client
}

func TestClientPermissionRequestRejectsSelectedWithoutOptionID(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	generation := client.setPermissionHandler("session-1", context.Background(), func(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
		return PermissionOutcome{Outcome: "selected"}, nil
	})
	defer client.clearPermissionHandler("session-1", generation)

	server.writeRaw(t, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"session-1","toolCall":{"toolCallId":"call-1"},"options":[{"optionId":"allow-once","kind":"allow_once"}]}}`)

	resp := server.readRequest(t)
	if resp.Error != nil {
		t.Fatalf("permission response error = %+v", resp.Error)
	}
	var result PermissionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Unmarshal permission result error = %v", err)
	}
	if result.Outcome.Outcome != "cancelled" || result.Outcome.OptionID != "" {
		t.Fatalf("permission outcome = %+v, want cancelled without option id", result.Outcome)
	}
}

func TestClientPermissionRequestRejectsSelectedUnknownOptionID(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	generation := client.setPermissionHandler("session-1", context.Background(), func(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
		return PermissionOutcome{Outcome: "selected", OptionID: "allow-forever"}, nil
	})
	defer client.clearPermissionHandler("session-1", generation)

	server.writeRaw(t, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"session-1","toolCall":{"toolCallId":"call-1"},"options":[{"optionId":"allow-once","kind":"allow_once"}]}}`)

	resp := server.readRequest(t)
	if resp.Error != nil {
		t.Fatalf("permission response error = %+v", resp.Error)
	}
	var result PermissionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Unmarshal permission result error = %v", err)
	}
	if result.Outcome.Outcome != "cancelled" || result.Outcome.OptionID != "" {
		t.Fatalf("permission outcome = %+v, want cancelled for unknown option id", result.Outcome)
	}
}

func TestClientPermissionRequestRejectsUnknownOutcome(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	generation := client.setPermissionHandler("session-1", context.Background(), func(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
		return PermissionOutcome{Outcome: "deferred", OptionID: "allow-once"}, nil
	})
	defer client.clearPermissionHandler("session-1", generation)

	server.writeRaw(t, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"session-1","toolCall":{"toolCallId":"call-1"},"options":[{"optionId":"allow-once","kind":"allow_once"}]}}`)

	resp := server.readRequest(t)
	if resp.Error != nil {
		t.Fatalf("permission response error = %+v", resp.Error)
	}
	var result PermissionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Unmarshal permission result error = %v", err)
	}
	if result.Outcome.Outcome != "cancelled" || result.Outcome.OptionID != "" {
		t.Fatalf("permission outcome = %+v, want cancelled for unknown outcome", result.Outcome)
	}
}

func TestClientPermissionRequestDoesNotCrossSessionHandlers(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	generation := client.setPermissionHandler("session-1", context.Background(), func(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
		t.Fatal("handler for session-1 should not receive permission request from other-session")
		return PermissionOutcome{}, nil
	})
	defer client.clearPermissionHandler("session-1", generation)

	server.writeRaw(t, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"other-session","toolCall":{"toolCallId":"call-1"},"options":[{"optionId":"reject","kind":"reject_once"}]}}`)

	resp := server.readRequest(t)
	if resp.Error != nil {
		t.Fatalf("permission response error = %+v", resp.Error)
	}
	var result PermissionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Unmarshal permission result error = %v", err)
	}
	if result.Outcome.Outcome != "selected" || result.Outcome.OptionID != "reject" {
		t.Fatalf("permission outcome = %+v, want default reject from other session", result.Outcome)
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

func TestClientPermissionRequestHandlesCancelRequestNotification(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	handlerStarted := make(chan struct{})
	generation := client.setPermissionHandler("session-1", context.Background(), func(ctx context.Context, req PermissionRequest) (PermissionOutcome, error) {
		close(handlerStarted)
		<-ctx.Done()
		return PermissionOutcome{}, ctx.Err()
	})
	defer client.clearPermissionHandler("session-1", generation)

	server.writeRaw(t, `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission","params":{"sessionId":"session-1","toolCall":{"toolCallId":"call-1"},"options":[{"optionId":"allow-once","kind":"allow_once"}]}}`)
	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for permission handler")
	}
	server.writeRaw(t, `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"id":"perm-1"}}`)

	resp := server.readRequest(t)
	if resp.Error != nil {
		t.Fatalf("permission response error = %+v", resp.Error)
	}
	var result PermissionResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("Unmarshal permission result error = %v", err)
	}
	if result.Outcome.Outcome != "cancelled" || result.Outcome.OptionID != "" {
		t.Fatalf("permission outcome = %+v, want cancelled after cancel_request", result.Outcome)
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
	client.initialize = InitializeResult{ProtocolVersion: 1}

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

func TestClientPromptRejectsEmptySessionID(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	_, err := client.Prompt(context.Background(), " ", "你好")
	if err == nil || !strings.Contains(err.Error(), "ACP session id 为空") {
		t.Fatalf("Prompt() error = %v, want empty session id error", err)
	}
}

func TestClientPromptRequiresInitialize(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()

	_, err := client.Prompt(context.Background(), "session-1", "你好")
	if err == nil || !strings.Contains(err.Error(), "ACP client 尚未 initialize") {
		t.Fatalf("Prompt() error = %v, want initialize required error", err)
	}
}

func TestClientPromptWaitsForServerResponseAfterContextCancellation(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

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

	cancelReq := server.readRequest(t)
	if cancelReq.Method != "session/cancel" {
		t.Fatalf("cancel method = %q, want session/cancel", cancelReq.Method)
	}
	if cancelReq.ID != nil {
		t.Fatalf("cancel request id = %v, want notification", cancelReq.ID.Key())
	}
	var cancelParams struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(cancelReq.Params, &cancelParams); err != nil {
		t.Fatalf("Unmarshal cancel params error = %v", err)
	}
	if cancelParams.SessionID != "session-1" {
		t.Fatalf("cancel sessionId = %q, want session-1", cancelParams.SessionID)
	}

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

func TestClientPromptLifecycleReportsCancellationWait(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan PromptLifecycleEvent, 16)
	result := make(chan error, 1)
	go func() {
		_, err := client.PromptWithOptions(ctx, "session-1", "开始 review", PromptOptions{
			OnLifecycle: func(event PromptLifecycleEvent) {
				events <- event
			},
		})
		result <- err
		close(events)
	}()

	req := server.readRequest(t)
	if req.Method != "session/prompt" {
		t.Fatalf("method = %q, want session/prompt", req.Method)
	}
	cancel()
	cancelReq := server.readRequest(t)
	if cancelReq.Method != "session/cancel" {
		t.Fatalf("cancel method = %q, want session/cancel", cancelReq.Method)
	}
	server.writeResponse(t, req.ID, map[string]any{"stopReason": "cancelled"})
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("PromptWithOptions() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PromptWithOptions() did not return")
	}

	var got []PromptLifecycleEvent
	for event := range events {
		got = append(got, event)
	}
	stages := make([]string, 0, len(got))
	byStage := make(map[string]PromptLifecycleEvent)
	for _, event := range got {
		stages = append(stages, event.Stage)
		byStage[event.Stage] = event
		if event.SessionID != "session-1" {
			t.Fatalf("event %+v session id, want session-1", event)
		}
	}
	wantStages := []string{"request_written", "context_done", "cancel_sent", "cancel_wait_started", "cancel_wait_finished"}
	for _, want := range wantStages {
		if _, ok := byStage[want]; !ok {
			t.Fatalf("lifecycle stages = %v, want %s", stages, want)
		}
	}
	if byStage["request_written"].Method != "session/prompt" || byStage["request_written"].RequestID == "" {
		t.Fatalf("request_written event = %+v, want method and request id", byStage["request_written"])
	}
	if !errors.Is(byStage["context_done"].Err, context.Canceled) {
		t.Fatalf("context_done err = %v, want context.Canceled", byStage["context_done"].Err)
	}
	if byStage["cancel_wait_finished"].Elapsed <= 0 {
		t.Fatalf("cancel_wait_finished event = %+v, want elapsed", byStage["cancel_wait_finished"])
	}
}

func TestClientPromptReturnsAfterCancellationWaitTimeout(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}
	oldTimeout := promptCancelResponseTimeout
	promptCancelResponseTimeout = 30 * time.Millisecond
	defer func() {
		promptCancelResponseTimeout = oldTimeout
	}()

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan PromptLifecycleEvent, 16)
	result := make(chan error, 1)
	go func() {
		_, err := client.PromptWithOptions(ctx, "session-1", "开始 review", PromptOptions{
			OnLifecycle: func(event PromptLifecycleEvent) {
				events <- event
			},
		})
		result <- err
		close(events)
	}()

	req := server.readRequest(t)
	if req.Method != "session/prompt" {
		t.Fatalf("method = %q, want session/prompt", req.Method)
	}
	cancel()
	cancelReq := server.readRequest(t)
	if cancelReq.Method != "session/cancel" {
		t.Fatalf("cancel method = %q, want session/cancel", cancelReq.Method)
	}

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("PromptWithOptions() error = %v, want context.Canceled", err)
		}
		if !errors.Is(err, ErrCancelResponseTimeout) {
			t.Fatalf("PromptWithOptions() error = %v, want ErrCancelResponseTimeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PromptWithOptions() did not return after cancellation wait timeout")
	}

	var got []PromptLifecycleEvent
	for event := range events {
		got = append(got, event)
	}
	stages := make([]string, 0, len(got))
	byStage := make(map[string]PromptLifecycleEvent)
	for _, event := range got {
		stages = append(stages, event.Stage)
		byStage[event.Stage] = event
	}
	if _, ok := byStage["cancel_wait_timeout"]; !ok {
		t.Fatalf("lifecycle stages = %v, want cancel_wait_timeout", stages)
	}
	if _, ok := byStage["cancel_wait_finished"]; ok {
		t.Fatalf("lifecycle stages = %v, did not want cancel_wait_finished", stages)
	}
	if byStage["cancel_wait_timeout"].WaitDuration <= 0 {
		t.Fatalf("cancel_wait_timeout event = %+v, want wait duration", byStage["cancel_wait_timeout"])
	}
}

func TestClientPromptIncludesJSONRPCErrorDetail(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

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

func TestClientPromptMapsJSONRPCCancelledError(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	go func() {
		req := server.readRequest(t)
		server.writeRaw(t, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%s,"error":{"code":-32800,"message":"Request Cancelled"}}`,
			req.ID.Key(),
		))
	}()

	_, err := client.Prompt(context.Background(), "session-1", "取消中的请求")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prompt() error = %v, want context.Canceled", err)
	}
}

func TestClientPromptFailsOnNonJSONRPCStdout(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	go func() {
		req := server.readRequest(t)
		if req.Method != "session/prompt" {
			t.Errorf("method = %q, want session/prompt", req.Method)
		}
		server.writeRaw(t, "agent log on stdout")
	}()

	_, err := client.Prompt(context.Background(), "session-1", "新的普通消息")
	if err == nil || !strings.Contains(err.Error(), "ACP server stdout 输出非 JSON-RPC 消息") {
		t.Fatalf("Prompt() error = %v, want non JSON-RPC stdout error", err)
	}
}

func TestClientPromptWithOptionsReportsSessionUpdates(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

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
			"sessionId": "session-1",
			"update": map[string]any{
				"sessionUpdate": "available_commands_update",
				"availableCommands": []map[string]any{
					{
						"name":        "web",
						"description": "Search the web for information",
						"input": map[string]any{
							"hint": "query to search for",
						},
					},
				},
			},
		})
		server.writeNotification(t, "session/update", map[string]any{
			"sessionId": "session-1",
			"update": map[string]any{
				"sessionUpdate": "session_info_update",
				"title":         "新的会话标题",
				"updatedAt":     "2025-10-29T14:22:15Z",
				"_meta": map[string]any{
					"priority": "high",
				},
			},
		})
		server.writeNotification(t, "session/update", map[string]any{
			"sessionId": "session-1",
			"update": map[string]any{
				"sessionUpdate": "current_mode_update",
				"modeId":        "plan",
			},
		})
		server.writeNotification(t, "session/update", map[string]any{
			"sessionId": "session-1",
			"update": map[string]any{
				"sessionUpdate": "agent_message",
				"content": map[string]any{
					"type":        "resource_link",
					"uri":         "file:///repo/report.pdf",
					"name":        "report.pdf",
					"mimeType":    "application/pdf",
					"title":       "分析报告",
					"description": "生成的分析报告",
					"size":        1024000,
				},
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
			"update": map[string]any{
				"sessionUpdate": "agent_message",
				"message":       "缺少 sessionId 时忽略",
			},
		})
		server.writeNotification(t, "session/update", map[string]any{
			"sessionId": "session-1",
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content": map[string]any{
					"type": "text",
					"text": "最终回复",
					"annotations": map[string]any{
						"audience": "user",
					},
					"_meta": map[string]any{
						"traceparent": "00-abc",
					},
				},
			},
		})
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	var updates []PromptUpdate
	result, err := client.PromptWithOptions(context.Background(), "session-1", "你好", PromptOptions{
		OnUpdate: func(update PromptUpdate) {
			updates = append(updates, update)
		},
	})
	if err != nil {
		t.Fatalf("PromptWithOptions() error = %v", err)
	}
	if result.Text != "最终回复" {
		t.Fatalf("reply = %q, want collected chunks", result.Text)
	}
	if len(updates) != 7 {
		t.Fatalf("updates = %+v, want seven updates for current session", updates)
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
	if updates[2].Update.SessionUpdate != "available_commands_update" || len(updates[2].Update.AvailableCommands) != 1 {
		t.Fatalf("third update = %+v, want available commands update", updates[2])
	}
	cmd := updates[2].Update.AvailableCommands[0]
	if cmd.Name != "web" || cmd.Description != "Search the web for information" || cmd.Input == nil || cmd.Input.Hint != "query to search for" {
		t.Fatalf("available command = %+v, want parsed input hint", cmd)
	}
	if updates[3].Update.SessionUpdate != "session_info_update" || updates[3].Update.Title != "新的会话标题" || updates[3].Update.UpdatedAt != "2025-10-29T14:22:15Z" {
		t.Fatalf("fourth update = %+v, want session info update", updates[3])
	}
	if updates[3].Update.Meta["priority"] != "high" {
		t.Fatalf("fourth update meta = %#v, want priority high", updates[3].Update.Meta)
	}
	if updates[4].Update.SessionUpdate != "current_mode_update" || updates[4].Update.ModeID != "plan" {
		t.Fatalf("fifth update = %+v, want current mode update", updates[4])
	}
	if updates[5].Update.SessionUpdate != "agent_message" || updates[5].Update.Content == nil {
		t.Fatalf("sixth update = %+v, want resource link message", updates[5])
	}
	resourceLink := updates[5].Update.Content
	if resourceLink.Type != "resource_link" || resourceLink.URI != "file:///repo/report.pdf" || resourceLink.Name != "report.pdf" || resourceLink.MIMEType != "application/pdf" || resourceLink.Title != "分析报告" || resourceLink.Description != "生成的分析报告" || resourceLink.Size != 1024000 {
		t.Fatalf("resource link = %+v, want parsed resource_link fields", resourceLink)
	}
	if updates[6].Update.SessionUpdate != "agent_message_chunk" {
		t.Fatalf("seventh update = %+v, want agent message chunk", updates[6])
	}
	if updates[6].Update.Content.Annotations["audience"] != "user" {
		t.Fatalf("seventh update annotations = %#v, want audience user", updates[6].Update.Content.Annotations)
	}
	if updates[6].Update.Content.Meta["traceparent"] != "00-abc" {
		t.Fatalf("seventh update meta = %#v, want traceparent 00-abc", updates[6].Update.Content.Meta)
	}
	<-done
}

func TestClientPromptTrimsSessionUpdateIdentityFields(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/prompt" {
			t.Errorf("method = %q, want session/prompt", req.Method)
		}
		server.writeNotification(t, "session/update", map[string]any{
			"sessionId": " session-1 ",
			"update": map[string]any{
				"sessionUpdate": "tool_call",
				"toolCallId":    " call-1 ",
				"title":         "Run tests",
			},
		})
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	var updates []PromptUpdate
	if _, err := client.PromptWithOptions(context.Background(), "session-1", "运行测试", PromptOptions{
		OnUpdate: func(update PromptUpdate) {
			updates = append(updates, update)
		},
	}); err != nil {
		t.Fatalf("PromptWithOptions() error = %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("updates = %+v, want one update for trimmed session id", updates)
	}
	if updates[0].SessionID != "session-1" {
		t.Fatalf("update sessionID = %q, want session-1", updates[0].SessionID)
	}
	if updates[0].Update.ToolCallID != "call-1" {
		t.Fatalf("toolCallID = %q, want call-1", updates[0].Update.ToolCallID)
	}
	if tool := client.toolCallSnapshot("session-1", "call-1"); tool == nil || tool.ToolCallID != "call-1" || tool.Title != "Run tests" {
		t.Fatalf("toolCallSnapshot = %+v, want trimmed call-1 Run tests", tool)
	}
	<-done
}

func TestClientPromptParsesAudioContentBlock(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

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
				"content": map[string]any{
					"type":     "audio",
					"mimeType": "audio/wav",
					"data":     "UklGRiQAAABXQVZFZm10IBAAAAAB",
					"annotations": map[string]any{
						"audience": "assistant",
					},
				},
			},
		})
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	var updates []PromptUpdate
	if _, err := client.PromptWithOptions(context.Background(), "session-1", "生成音频", PromptOptions{
		OnUpdate: func(update PromptUpdate) {
			updates = append(updates, update)
		},
	}); err != nil {
		t.Fatalf("PromptWithOptions() error = %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("updates = %+v, want one audio update", updates)
	}
	content := updates[0].Update.Content
	if content == nil {
		t.Fatalf("content = nil, want audio content")
	}
	if content.Type != "audio" || content.MIMEType != "audio/wav" || content.Data != "UklGRiQAAABXQVZFZm10IBAAAAAB" {
		t.Fatalf("content = %+v, want parsed audio block", content)
	}
	if content.Annotations["audience"] != "assistant" {
		t.Fatalf("annotations = %#v, want assistant audience", content.Annotations)
	}
	<-done
}

func TestClientPromptParsesBlobResourceContentBlock(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

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
				"content": map[string]any{
					"type": "resource",
					"resource": map[string]any{
						"uri":      "file:///repo/image.png",
						"mimeType": "image/png",
						"blob":     "iVBORw0KGgo=",
					},
				},
			},
		})
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	var updates []PromptUpdate
	if _, err := client.PromptWithOptions(context.Background(), "session-1", "查看资源", PromptOptions{
		OnUpdate: func(update PromptUpdate) {
			updates = append(updates, update)
		},
	}); err != nil {
		t.Fatalf("PromptWithOptions() error = %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("updates = %+v, want one resource update", updates)
	}
	content := updates[0].Update.Content
	if content == nil || content.Type != "resource" || content.Resource == nil {
		t.Fatalf("content = %+v, want resource content", content)
	}
	if content.Resource.URI != "file:///repo/image.png" || content.Resource.MIMEType != "image/png" || content.Resource.Blob != "iVBORw0KGgo=" {
		t.Fatalf("resource = %+v, want parsed blob resource", content.Resource)
	}
	<-done
}

func TestClientPromptParsesToolCallContentArray(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	done := make(chan struct{})
	oldText := "debug=false\n"
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/prompt" {
			t.Errorf("method = %q, want session/prompt", req.Method)
		}
		server.writeNotification(t, "session/update", map[string]any{
			"sessionId": "session-1",
			"update": map[string]any{
				"sessionUpdate": "tool_call_update",
				"toolCallId":    "call-1",
				"status":        "in_progress",
				"locations": []map[string]any{
					{
						"path": "/tmp/config.txt",
						"line": 2,
					},
				},
				"rawInput": map[string]any{
					"command": "cat /tmp/config.txt",
				},
				"rawOutput": "debug=true\n",
				"content": []map[string]any{
					{
						"type": "content",
						"content": map[string]any{
							"type": "text",
							"text": "Found config.",
						},
					},
					{
						"type": "content",
						"content": map[string]any{
							"type":     "image",
							"mimeType": "image/png",
							"data":     "iVBORw0KGgo=",
						},
					},
					{
						"type": "content",
						"content": map[string]any{
							"type": "resource",
							"resource": map[string]any{
								"uri":      "file:///repo/config.txt",
								"mimeType": "text/plain",
								"text":     "debug=true\n",
							},
						},
					},
					{
						"type":    "diff",
						"path":    "/tmp/config.txt",
						"oldText": oldText,
						"newText": "debug=true\n",
					},
					{
						"type":       "terminal",
						"terminalId": "term-1",
					},
				},
			},
		})
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	var updates []PromptUpdate
	if _, err := client.PromptWithOptions(context.Background(), "session-1", "检查配置", PromptOptions{
		OnUpdate: func(update PromptUpdate) {
			updates = append(updates, update)
		},
	}); err != nil {
		t.Fatalf("PromptWithOptions() error = %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("updates = %+v, want one update", updates)
	}
	update := updates[0].Update
	if update.SessionUpdate != "tool_call_update" || update.ToolCallID != "call-1" || update.Status != "in_progress" {
		t.Fatalf("update = %+v, want tool call update metadata", update)
	}
	if !strings.Contains(string(update.Locations), "/tmp/config.txt") {
		t.Fatalf("Locations = %s, want config path", update.Locations)
	}
	if !strings.Contains(string(update.RawInput), "cat /tmp/config.txt") {
		t.Fatalf("RawInput = %s, want command", update.RawInput)
	}
	if string(update.RawOutput) != `"debug=true\n"` {
		t.Fatalf("RawOutput = %s, want raw output string", update.RawOutput)
	}
	if update.Content != nil {
		t.Fatalf("Content = %+v, want nil for tool call content array", update.Content)
	}
	if string(update.ContentRaw) == "" {
		t.Fatalf("ContentRaw is empty")
	}
	if len(update.ToolCallContent) != 5 {
		t.Fatalf("ToolCallContent = %+v, want five items", update.ToolCallContent)
	}
	if update.ToolCallContent[0].Type != "content" || update.ToolCallContent[0].Content == nil || update.ToolCallContent[0].Content.Text != "Found config." {
		t.Fatalf("first ToolCallContent = %+v, want text content", update.ToolCallContent[0])
	}
	if update.ToolCallContent[1].Type != "content" || update.ToolCallContent[1].Content == nil || update.ToolCallContent[1].Content.Type != "image" || update.ToolCallContent[1].Content.MIMEType != "image/png" || update.ToolCallContent[1].Content.Data != "iVBORw0KGgo=" {
		t.Fatalf("second ToolCallContent = %+v, want image content", update.ToolCallContent[1])
	}
	if update.ToolCallContent[2].Type != "content" || update.ToolCallContent[2].Content == nil || update.ToolCallContent[2].Content.Type != "resource" || update.ToolCallContent[2].Content.Resource == nil || update.ToolCallContent[2].Content.Resource.URI != "file:///repo/config.txt" || update.ToolCallContent[2].Content.Resource.MIMEType != "text/plain" || update.ToolCallContent[2].Content.Resource.Text != "debug=true\n" {
		t.Fatalf("third ToolCallContent = %+v, want text resource content", update.ToolCallContent[2])
	}
	if update.ToolCallContent[3].Type != "diff" || update.ToolCallContent[3].Path != "/tmp/config.txt" || update.ToolCallContent[3].OldText == nil || *update.ToolCallContent[3].OldText != oldText || update.ToolCallContent[3].NewText != "debug=true\n" {
		t.Fatalf("fourth ToolCallContent = %+v, want diff", update.ToolCallContent[3])
	}
	if update.ToolCallContent[4].Type != "terminal" || update.ToolCallContent[4].TerminalID != "term-1" {
		t.Fatalf("fifth ToolCallContent = %+v, want terminal", update.ToolCallContent[4])
	}
	<-done
}

func TestSessionUpdateToolCallDefaults(t *testing.T) {
	var toolCall SessionUpdate
	if err := json.Unmarshal([]byte(`{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"Read file"}`), &toolCall); err != nil {
		t.Fatalf("Unmarshal tool_call error = %v", err)
	}
	if toolCall.Status != "pending" {
		t.Fatalf("tool_call status = %q, want pending", toolCall.Status)
	}
	if toolCall.Kind != "other" {
		t.Fatalf("tool_call kind = %q, want other", toolCall.Kind)
	}

	var toolCallUpdate SessionUpdate
	if err := json.Unmarshal([]byte(`{"sessionUpdate":"tool_call_update","toolCallId":"call-1"}`), &toolCallUpdate); err != nil {
		t.Fatalf("Unmarshal tool_call_update error = %v", err)
	}
	if toolCallUpdate.Status != "" {
		t.Fatalf("tool_call_update status = %q, want empty incremental update", toolCallUpdate.Status)
	}
	if toolCallUpdate.Kind != "" {
		t.Fatalf("tool_call_update kind = %q, want empty incremental update", toolCallUpdate.Kind)
	}
}

func TestSessionInfoUpdateTracksNullTitle(t *testing.T) {
	var update SessionUpdate
	if err := json.Unmarshal([]byte(`{"sessionUpdate":"session_info_update","title":null}`), &update); err != nil {
		t.Fatalf("Unmarshal session_info_update error = %v", err)
	}
	if !update.TitleSet {
		t.Fatal("TitleSet = false, want true for explicit null title")
	}
	if update.Title != "" {
		t.Fatalf("Title = %q, want empty title for null", update.Title)
	}
}

func TestSessionInfoUpdateTracksNullUpdatedAt(t *testing.T) {
	var update SessionUpdate
	if err := json.Unmarshal([]byte(`{"sessionUpdate":"session_info_update","updatedAt":null}`), &update); err != nil {
		t.Fatalf("Unmarshal session_info_update error = %v", err)
	}
	if !update.UpdatedAtSet {
		t.Fatal("UpdatedAtSet = false, want true for explicit null updatedAt")
	}
	if update.UpdatedAt != "" {
		t.Fatalf("UpdatedAt = %q, want empty updatedAt for null", update.UpdatedAt)
	}
}

func TestSessionUpdateTrimsModelStateFields(t *testing.T) {
	var update SessionUpdate
	if err := json.Unmarshal([]byte(`{
		"sessionUpdate":"session_state_update",
		"models":{
			"currentModelId":" gpt-5.6 ",
			"availableModels":[{"modelId":" gpt-5.6 ","name":" GPT-5.6 ","description":" next model "}]
		}
	}`), &update); err != nil {
		t.Fatalf("Unmarshal session_state_update error = %v", err)
	}
	if update.Models == nil || update.Models.CurrentModelID != "gpt-5.6" {
		t.Fatalf("Models = %+v, want trimmed current model", update.Models)
	}
	if len(update.Models.AvailableModels) != 1 {
		t.Fatalf("AvailableModels = %+v, want one model", update.Models.AvailableModels)
	}
	got := update.Models.AvailableModels[0]
	if got.ModelID != "gpt-5.6" || got.Name != "GPT-5.6" || got.Description != "next model" {
		t.Fatalf("model = %+v, want trimmed fields", got)
	}
}

func TestClientPromptStopReasonIsNotReplyText(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/prompt" {
			t.Errorf("method = %q, want session/prompt", req.Method)
		}
		server.writeResponse(t, req.ID, map[string]any{"stopReason": "max_tokens"})
	}()

	result, err := client.PromptWithOptions(context.Background(), "session-1", "继续", PromptOptions{})
	if err != nil {
		t.Fatalf("PromptWithOptions() error = %v", err)
	}
	if result.StopReason != "max_tokens" {
		t.Fatalf("StopReason = %q, want max_tokens", result.StopReason)
	}
	if result.Text != "" {
		t.Fatalf("Text = %q, want empty text because stopReason is not reply body", result.Text)
	}
	<-done
}

func TestClientPromptWithOptionsParsesResultUsage(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{ProtocolVersion: 1}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		server.writeNotification(t, "session/update", map[string]any{
			"sessionId": "session-1",
			"update": map[string]any{
				"sessionUpdate": "usage_update",
				"used":          53000,
				"size":          200000,
				"cost": map[string]any{
					"amount":   0.045,
					"currency": "USD",
				},
			},
		})
		server.writeResponse(t, req.ID, map[string]any{
			"stopReason": "end_turn",
			"usage": map[string]any{
				"inputTokens":      1200,
				"outputTokens":     345,
				"totalTokens":      1545,
				"cachedReadTokens": 1000,
			},
			"_meta": map[string]any{
				"_trae/tokenUsage": map[string]any{
					"turnDisplay": map[string]any{
						"inputTokens":  987,
						"outputTokens": 654,
					},
					"contextWindow": map[string]any{
						"used": 53000,
						"size": 200000,
					},
				},
			},
		})
	}()

	var updates []PromptUpdate
	result, err := client.PromptWithOptions(context.Background(), "session-1", "你好", PromptOptions{
		OnUpdate: func(update PromptUpdate) {
			updates = append(updates, update)
		},
	})
	if err != nil {
		t.Fatalf("PromptWithOptions() error = %v", err)
	}
	if result.StopReason != "end_turn" {
		t.Fatalf("StopReason = %q, want end_turn", result.StopReason)
	}
	if result.Usage.InputTokens != 1200 || result.Usage.OutputTokens != 345 || result.Usage.CachedReadTokens != 1000 {
		t.Fatalf("Usage = %+v, want prompt usage", result.Usage)
	}
	if result.Meta.TraeTokenUsage == nil {
		t.Fatal("TraeTokenUsage = nil, want parsed metadata")
	}
	if result.Meta.TraeTokenUsage.TurnDisplay.InputTokens != 987 || result.Meta.TraeTokenUsage.TurnDisplay.OutputTokens != 654 {
		t.Fatalf("TurnDisplay = %+v, want parsed turn display usage", result.Meta.TraeTokenUsage.TurnDisplay)
	}
	if result.Meta.TraeTokenUsage.ContextWindow.Used != 53000 || result.Meta.TraeTokenUsage.ContextWindow.Size != 200000 {
		t.Fatalf("ContextWindow = %+v, want parsed context window", result.Meta.TraeTokenUsage.ContextWindow)
	}
	if len(updates) != 1 || updates[0].Update.SessionUpdate != "usage_update" || updates[0].Update.Used != 53000 || updates[0].Update.Size != 200000 {
		t.Fatalf("updates = %+v, want parsed usage_update", updates)
	}
	if updates[0].Update.Cost == nil || updates[0].Update.Cost.Amount != 0.045 || updates[0].Update.Cost.Currency != "USD" {
		t.Fatalf("Cost = %+v, want 0.045 USD", updates[0].Update.Cost)
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
