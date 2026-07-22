package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientInitializeAdvertisesWorkspaceFS(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace string
		wantFS    bool
	}{
		{name: "workspace configured", workspace: t.TempDir(), wantFS: true},
		{name: "workspace empty", workspace: "", wantFS: false},
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
					ClientCapabilities struct {
						FS struct {
							ReadTextFile  bool `json:"readTextFile"`
							WriteTextFile bool `json:"writeTextFile"`
						} `json:"fs"`
					} `json:"clientCapabilities"`
				}
				if err := json.Unmarshal(req.Params, &params); err != nil {
					t.Errorf("Unmarshal params error = %v", err)
					return
				}
				if params.ClientCapabilities.FS.ReadTextFile != tc.wantFS {
					t.Errorf("readTextFile = %v, want %v", params.ClientCapabilities.FS.ReadTextFile, tc.wantFS)
				}
				if params.ClientCapabilities.FS.WriteTextFile != tc.wantFS {
					t.Errorf("writeTextFile = %v, want %v", params.ClientCapabilities.FS.WriteTextFile, tc.wantFS)
				}
				server.writeResponse(t, req.ID, map[string]any{})
			}()

			if err := client.Initialize(context.Background()); err != nil {
				t.Fatalf("Initialize() error = %v", err)
			}
			<-done
		})
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
		server.writeResponse(t, req.ID, map[string]string{"sessionId": "session-1"})
	}()

	sessionID, err := client.NewSession(context.Background(), "/repo")
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	if sessionID != "session-1" {
		t.Fatalf("sessionID = %q, want session-1", sessionID)
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
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	if err := client.CancelSession(context.Background(), "session-1"); err != nil {
		t.Fatalf("CancelSession() error = %v", err)
	}
	<-done
}

func TestClientHandlesWorkspaceFileRequests(t *testing.T) {
	workspace := t.TempDir()
	client, server := newPipeClient(t, workspace)
	defer server.close()

	target := filepath.Join(workspace, "SOUL.md")
	server.writeRequest(t, 100, "fs/write_text_file", map[string]any{
		"path":    target,
		"content": "# SOUL\n\n名字：小助手\n",
	})
	writeResp := server.readRequest(t)
	if writeResp.Error != nil {
		t.Fatalf("write response error = %+v", writeResp.Error)
	}
	var writeResult struct {
		Created bool `json:"created"`
	}
	if err := json.Unmarshal(writeResp.Result, &writeResult); err != nil {
		t.Fatalf("Unmarshal write result error = %v", err)
	}
	if !writeResult.Created {
		t.Fatalf("created = false, want true")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "# SOUL\n\n名字：小助手\n" {
		t.Fatalf("file content = %q", string(data))
	}

	server.writeRequest(t, 101, "fs/read_text_file", map[string]any{
		"path": "SOUL.md",
	})
	readResp := server.readRequest(t)
	if readResp.Error != nil {
		t.Fatalf("read response error = %+v", readResp.Error)
	}
	var readResult struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(readResp.Result, &readResult); err != nil {
		t.Fatalf("Unmarshal read result error = %v", err)
	}
	if readResult.Content != string(data) {
		t.Fatalf("read content = %q, want %q", readResult.Content, string(data))
	}

	outside := filepath.Join(workspace, "..", "outside.md")
	server.writeRequest(t, 102, "fs/write_text_file", map[string]any{
		"path":    outside,
		"content": "outside",
	})
	outsideResp := server.readRequest(t)
	if outsideResp.Error == nil || !strings.Contains(outsideResp.Error.Message, "超出 workspace") {
		t.Fatalf("outside response = %+v, want workspace error", outsideResp)
	}
	if _, err := os.Stat(filepath.Clean(outside)); !os.IsNotExist(err) {
		t.Fatalf("outside file should not be written, stat err = %v", err)
	}

	_ = client
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
		pending:        make(map[int64]chan rpcResponse),
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

func (s *pipeServer) writeResponse(t *testing.T, id *int64, result any) {
	t.Helper()
	if id == nil {
		t.Fatalf("response id is nil")
	}
	s.write(t, Message{JSONRPC: "2.0", ID: id, Result: mustMarshal(t, result)})
}

func (s *pipeServer) writeRequest(t *testing.T, id int64, method string, params any) {
	t.Helper()
	s.write(t, Message{JSONRPC: "2.0", ID: &id, Method: method, Params: mustMarshal(t, params)})
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
