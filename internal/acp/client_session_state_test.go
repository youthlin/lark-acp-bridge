package acp

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCloseSessionClearsToolCallAndPermissionState(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{
		ProtocolVersion: 1,
		AgentCapabilities: AgentCapabilities{
			SessionCapabilities: SessionCapabilities{Close: map[string]any{}},
		},
	}
	client.setPermissionHandler("session-1", context.Background(), func(context.Context, PermissionRequest) (PermissionOutcome, error) {
		return PermissionOutcome{}, nil
	})
	client.rememberToolCallUpdate("session-1", json.RawMessage(`{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"Run tests"}`))
	if client.toolCallSnapshot("session-1", "call-1") == nil {
		t.Fatal("setup failed: tool call snapshot missing")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/close" {
			t.Errorf("method = %q, want session/close", req.Method)
		}
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	if err := client.CloseSession(context.Background(), "session-1"); err != nil {
		t.Fatalf("CloseSession() error = %v", err)
	}
	<-done

	if _, ok := client.toolCalls["session-1"]; ok {
		t.Fatal("toolCalls were not cleared after close")
	}
	if _, ok := client.permissionScopes["session-1"]; ok {
		t.Fatal("permissionScopes were not cleared after close")
	}
}

func TestDeleteSessionClearsToolCallAndPermissionState(t *testing.T) {
	client, server := newPipeClient(t)
	defer server.close()
	client.initialize = InitializeResult{
		ProtocolVersion: 1,
		AgentCapabilities: AgentCapabilities{
			SessionCapabilities: SessionCapabilities{Delete: map[string]any{}},
		},
	}
	client.setPermissionHandler("session-1", context.Background(), func(context.Context, PermissionRequest) (PermissionOutcome, error) {
		return PermissionOutcome{}, nil
	})
	client.rememberToolCallUpdate("session-1", json.RawMessage(`{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"Run tests"}`))

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := server.readRequest(t)
		if req.Method != "session/delete" {
			t.Errorf("method = %q, want session/delete", req.Method)
		}
		server.writeResponse(t, req.ID, map[string]any{})
	}()

	if err := client.DeleteSession(context.Background(), "session-1"); err != nil {
		t.Fatalf("DeleteSession() error = %v", err)
	}
	<-done

	if _, ok := client.toolCalls["session-1"]; ok {
		t.Fatal("toolCalls were not cleared after delete")
	}
	if _, ok := client.permissionScopes["session-1"]; ok {
		t.Fatal("permissionScopes were not cleared after delete")
	}
}
