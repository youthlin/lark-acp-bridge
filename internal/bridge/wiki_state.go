package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const wikiStateVersion = 1

type wikiState struct {
	Version          int                           `json:"version"`
	Companions       map[string]wikiCompanionState `json:"companions,omitempty"`
	AtAutoCompanions map[string]wikiCompanionState `json:"at_auto_companions,omitempty"`
	Sources          map[string]wikiSourceState    `json:"sources,omitempty"`
}

type wikiCompanionState struct {
	AgentName    string    `json:"agent_name"`
	ACPSessionID string    `json:"acp_session_id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type wikiSourceState struct {
	SessionKey      SessionKey `json:"session_key"`
	AgentName       string     `json:"agent_name"`
	CommittedSeq    uint64     `json:"committed_seq"`
	CommittedTS     time.Time  `json:"committed_ts,omitempty"`
	LastCompleteSeq uint64     `json:"last_complete_seq"`
	LastActivityAt  time.Time  `json:"last_activity_at,omitempty"`
	DueAt           time.Time  `json:"due_at,omitempty"`
	LastSuccessAt   time.Time  `json:"last_success_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	LastSummary     string     `json:"last_summary,omitempty"`
	CursorLost      bool       `json:"cursor_lost,omitempty"`
}

type wikiStateStore struct {
	path  string
	mu    sync.Mutex
	state wikiState
}

func newWikiStateStore(workspace string) *wikiStateStore {
	return &wikiStateStore{
		path: filepath.Join(workspaceLocalPath(workspace, "wiki"), "state.json"),
		state: wikiState{
			Version:          wikiStateVersion,
			Companions:       make(map[string]wikiCompanionState),
			AtAutoCompanions: make(map[string]wikiCompanionState),
			Sources:          make(map[string]wikiSourceState),
		},
	}
}

func (s *wikiStateStore) Load() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取 wiki 状态: %w", err)
	}
	var state wikiState
	if err := json.Unmarshal(data, &state); err != nil {
		backup := fmt.Sprintf("%s.corrupt-%s", s.path, time.Now().Format("20060102T150405.000000000"))
		if writeErr := os.WriteFile(backup, data, 0o600); writeErr != nil {
			return fmt.Errorf("解析 wiki 状态: %w；备份损坏文件失败: %v", err, writeErr)
		}
		if removeErr := os.Remove(s.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("解析 wiki 状态: %w；移除损坏文件失败: %v", err, removeErr)
		}
		slog.Warn("wiki 状态文件已损坏，已备份并以空状态启动", "path", s.path, "backup", backup, "错误", err)
		return nil
	}
	if state.Companions == nil {
		state.Companions = make(map[string]wikiCompanionState)
	}
	if state.AtAutoCompanions == nil {
		state.AtAutoCompanions = make(map[string]wikiCompanionState)
	}
	if state.Sources == nil {
		state.Sources = make(map[string]wikiSourceState)
	}
	state.Version = wikiStateVersion
	s.state = state
	return nil
}

func (s *wikiStateStore) snapshot() wikiState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneWikiState(s.state)
}

func (s *wikiStateStore) update(update func(*wikiState)) error {
	if s == nil {
		return fmt.Errorf("wiki 状态存储未初始化")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneWikiState(s.state)
	update(&next)
	next.Version = wikiStateVersion
	if err := writeWikiStateAtomic(s.path, next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func cloneWikiState(state wikiState) wikiState {
	copy := wikiState{
		Version:          state.Version,
		Companions:       make(map[string]wikiCompanionState, len(state.Companions)),
		AtAutoCompanions: make(map[string]wikiCompanionState, len(state.AtAutoCompanions)),
		Sources:          make(map[string]wikiSourceState, len(state.Sources)),
	}
	for key, value := range state.Companions {
		copy.Companions[key] = value
	}
	for key, value := range state.AtAutoCompanions {
		copy.AtAutoCompanions[key] = value
	}
	for key, value := range state.Sources {
		copy.Sources[key] = value
	}
	return copy
}

func writeWikiStateAtomic(path string, state wikiState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建 wiki 状态目录: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("编码 wiki 状态: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".wiki-state-*.json")
	if err != nil {
		return fmt.Errorf("创建 wiki 临时状态: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入 wiki 临时状态: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭 wiki 临时状态: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("替换 wiki 状态: %w", err)
	}
	return nil
}

func wikiSourceID(sessionID string) string {
	return strings.TrimSpace(sessionID)
}
