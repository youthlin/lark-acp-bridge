package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/bridge"
)

const taskControlMaxBody = 1 << 20

type taskControlState struct {
	Address string `json:"address"`
	Token   string `json:"token"`
	PID     int    `json:"pid"`
}

type taskControlServer struct {
	statePath string
	state     taskControlState
	listener  net.Listener
	server    *http.Server
}

type taskControlError struct {
	Error string `json:"error"`
}

func taskControlStatePath(configPath string) string {
	return strings.TrimSpace(configPath) + ".control.json"
}

func startTaskControlServer(svc *bridge.Service, configPath string) (*taskControlServer, error) {
	if svc == nil {
		return nil, fmt.Errorf("bridge service 不能为空")
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("生成控制端点 token: %w", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("监听本地控制端点: %w", err)
	}
	control := &taskControlServer{
		statePath: taskControlStatePath(configPath),
		state: taskControlState{
			Address: "http://" + listener.Addr().String(),
			Token:   hex.EncodeToString(tokenBytes),
			PID:     os.Getpid(),
		},
		listener: listener,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tasks", func(w http.ResponseWriter, r *http.Request) {
		if subtleTokenMismatch(r.Header.Get("Authorization"), control.state.Token) {
			writeTaskControlJSON(w, http.StatusUnauthorized, taskControlError{Error: "unauthorized"})
			return
		}
		body := http.MaxBytesReader(w, r.Body, taskControlMaxBody)
		defer body.Close()
		decoder := json.NewDecoder(body)
		decoder.DisallowUnknownFields()
		var req bridge.AgentScheduledTaskCreateRequest
		if err := decoder.Decode(&req); err != nil {
			writeTaskControlJSON(w, http.StatusBadRequest, taskControlError{Error: "解析任务 JSON: " + err.Error()})
			return
		}
		if err := ensureSingleJSONValue(decoder); err != nil {
			writeTaskControlJSON(w, http.StatusBadRequest, taskControlError{Error: err.Error()})
			return
		}
		result, err := svc.CreateAgentScheduledTask(r.Context(), req)
		if err != nil {
			writeTaskControlJSON(w, http.StatusBadRequest, taskControlError{Error: err.Error()})
			return
		}
		writeTaskControlJSON(w, http.StatusCreated, result)
	})
	control.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := writeTaskControlState(control.statePath, control.state); err != nil {
		_ = listener.Close()
		return nil, err
	}
	go func() {
		_ = control.server.Serve(listener)
	}()
	return control, nil
}

func (s *taskControlServer) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	err := s.server.Shutdown(ctx)
	data, readErr := os.ReadFile(s.statePath)
	if readErr == nil {
		var current taskControlState
		if json.Unmarshal(data, &current) == nil && current.Token == s.state.Token {
			_ = os.Remove(s.statePath)
		}
	}
	return err
}

func runTaskCreate(ctx context.Context, configPath string, input io.Reader, output io.Writer) error {
	stateData, err := os.ReadFile(taskControlStatePath(configPath))
	if err != nil {
		return fmt.Errorf("读取 bridge 控制端点失败，请确认服务正在运行: %w", err)
	}
	var state taskControlState
	if err := json.Unmarshal(stateData, &state); err != nil || strings.TrimSpace(state.Address) == "" || strings.TrimSpace(state.Token) == "" {
		return fmt.Errorf("bridge 控制端点状态无效")
	}
	body, err := io.ReadAll(io.LimitReader(input, taskControlMaxBody+1))
	if err != nil {
		return fmt.Errorf("读取任务 JSON: %w", err)
	}
	if len(body) > taskControlMaxBody {
		return fmt.Errorf("任务 JSON 超过 %d 字节", taskControlMaxBody)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(state.Address, "/")+"/v1/tasks", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+state.Token)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("调用运行中的 bridge 失败: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, taskControlMaxBody+1))
	if err != nil {
		return fmt.Errorf("读取 bridge 响应: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var controlErr taskControlError
		if json.Unmarshal(responseBody, &controlErr) == nil && strings.TrimSpace(controlErr.Error) != "" {
			return errors.New(controlErr.Error)
		}
		return fmt.Errorf("bridge 返回 HTTP %d", response.StatusCode)
	}
	_, err = output.Write(responseBody)
	return err
}

func writeTaskControlState(path string, state taskControlState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".control-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func writeTaskControlJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func ensureSingleJSONValue(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("任务 JSON 后存在多余内容")
		}
		return fmt.Errorf("解析任务 JSON: %w", err)
	}
	return nil
}

func subtleTokenMismatch(header, token string) bool {
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(header)), []byte("Bearer "+token)) != 1
}
