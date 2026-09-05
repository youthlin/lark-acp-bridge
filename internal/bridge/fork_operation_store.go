package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	forkStatePreparing      = "preparing"
	forkStateTargetCreated  = "target_created"
	forkStateSessionCreated = "session_created"
	forkStateBootstrapping  = "bootstrapping"
	forkStateReady          = "ready"
	forkStateFailed         = "failed"
	forkReadyRetention      = 7 * 24 * time.Hour
	forkFailedRetention     = 24 * time.Hour
)

type ForkOperation struct {
	ID                    string            `json:"id"`
	Revision              uint64            `json:"revision"`
	State                 string            `json:"state"`
	Source                SessionForkOrigin `json:"source"`
	SourceTitle           string            `json:"source_title,omitempty"`
	TargetTitle           string            `json:"target_title,omitempty"`
	TargetKey             SessionKey        `json:"target_key,omitempty"`
	TargetChatID          string            `json:"target_chat_id,omitempty"`
	TargetThreadID        string            `json:"target_thread_id,omitempty"`
	TargetRootID          string            `json:"target_root_id,omitempty"`
	TargetMessageID       string            `json:"target_message_id,omitempty"`
	TargetSession         string            `json:"target_session,omitempty"`
	BundlePath            string            `json:"bundle_path,omitempty"`
	Error                 string            `json:"error,omitempty"`
	OriginalNoticeSent    bool              `json:"original_notice_sent,omitempty"`
	OriginalShareChatSent bool              `json:"original_share_chat_sent,omitempty"`
	InviteWarning         string            `json:"invite_warning,omitempty"`
	ExtraUserOpenIDs      []string          `json:"extra_user_open_ids,omitempty"`
	CreatedAt             time.Time         `json:"created_at"`
	UpdatedAt             time.Time         `json:"updated_at"`
}

type forkOperationFile struct {
	Version    int             `json:"version"`
	Operations []ForkOperation `json:"operations,omitempty"`
}

type forkOperationStore struct {
	dir        string
	indexPath  string
	mu         sync.Mutex
	operations map[string]ForkOperation
}

func newForkOperationStore(workspace string) *forkOperationStore {
	dir := workspaceLocalPath(workspace, "forks")
	return &forkOperationStore{
		dir:        dir,
		indexPath:  filepath.Join(dir, "index.json"),
		operations: make(map[string]ForkOperation),
	}
}

func (s *forkOperationStore) Load() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.indexPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("读取 session fork 操作记录: %w", err)
	}
	var file forkOperationFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("解析 session fork 操作记录: %w", err)
	}
	s.operations = make(map[string]ForkOperation, len(file.Operations))
	for _, operation := range file.Operations {
		operation = normalizeForkOperation(operation)
		if operation.ID != "" {
			s.operations[operation.ID] = operation
		}
	}
	prunedDirs := s.pruneLocked(time.Now())
	if err := s.writeLocked(); err != nil {
		return err
	}
	removeForkArtifactDirs(prunedDirs)
	return nil
}

func (s *forkOperationStore) RecoverInterrupted() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for id, operation := range s.operations {
		if operation.State == forkStateReady || operation.State == forkStateFailed {
			continue
		}
		operation.State = forkStateFailed
		if strings.TrimSpace(operation.TargetChatID) == "" {
			operation.Error = "Bridge 在创建分支目标位置前重启，请在源位置执行 /session fork retry"
		} else {
			operation.Error = "Bridge 在分支初始化期间重启，请在目标位置执行 /session fork retry"
		}
		operation.Revision++
		operation.UpdatedAt = time.Now()
		s.operations[id] = operation
		changed = true
	}
	if !changed {
		return nil
	}
	return s.writeLocked()
}

func (s *forkOperationStore) Put(operation ForkOperation) error {
	if s == nil {
		return fmt.Errorf("session fork 操作存储未初始化")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	operation = normalizeForkOperation(operation)
	if operation.ID == "" {
		return fmt.Errorf("session fork operation id 为空")
	}
	now := time.Now()
	if previous, ok := s.operations[operation.ID]; ok {
		operation.Revision = previous.Revision + 1
		if operation.CreatedAt.IsZero() {
			operation.CreatedAt = previous.CreatedAt
		}
	} else {
		operation.Revision = 1
	}
	if operation.CreatedAt.IsZero() {
		operation.CreatedAt = now
	}
	operation.UpdatedAt = now
	previous := cloneForkOperations(s.operations)
	s.operations[operation.ID] = operation
	prunedDirs := s.pruneLocked(now)
	if err := s.writeLocked(); err != nil {
		s.operations = previous
		return err
	}
	removeForkArtifactDirs(prunedDirs)
	return nil
}

func (s *forkOperationStore) PutIfCommandAbsent(operation ForkOperation) (ForkOperation, bool, error) {
	if s == nil {
		return ForkOperation{}, false, fmt.Errorf("session fork 操作存储未初始化")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	operation = normalizeForkOperation(operation)
	if operation.ID == "" || operation.Source.ForkCommandMessageID == "" {
		return ForkOperation{}, false, fmt.Errorf("session fork operation 幂等字段为空")
	}
	for _, existing := range s.operations {
		if existing.Source.ForkCommandMessageID == operation.Source.ForkCommandMessageID {
			return existing, false, nil
		}
	}
	now := time.Now()
	if operation.CreatedAt.IsZero() {
		operation.CreatedAt = now
	}
	operation.Revision = 1
	operation.UpdatedAt = now
	previous := cloneForkOperations(s.operations)
	s.operations[operation.ID] = operation
	prunedDirs := s.pruneLocked(now)
	if err := s.writeLocked(); err != nil {
		s.operations = previous
		return ForkOperation{}, false, err
	}
	removeForkArtifactDirs(prunedDirs)
	return operation, true, nil
}

// ClaimRetry 原子地把指定版本的失败 operation 抢占为 bootstrap 状态。状态和
// revision 必须同时匹配，避免旧请求在另一次 retry 已失败后再次抢占。
func (s *forkOperationStore) ClaimRetry(id string, expectedRevision uint64) (ForkOperation, bool, error) {
	return s.claimRetry(id, expectedRevision, forkStateBootstrapping)
}

// ClaimSourceRetry 原子地抢占尚未创建目标位置的失败 operation。
func (s *forkOperationStore) ClaimSourceRetry(id string, expectedRevision uint64) (ForkOperation, bool, error) {
	return s.claimRetry(id, expectedRevision, forkStatePreparing)
}

func (s *forkOperationStore) claimRetry(id string, expectedRevision uint64, nextState string) (ForkOperation, bool, error) {
	if s == nil {
		return ForkOperation{}, false, fmt.Errorf("session fork 操作存储未初始化")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return ForkOperation{}, false, fmt.Errorf("session fork operation id 为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	operation, ok := s.operations[id]
	if !ok {
		return ForkOperation{}, false, fmt.Errorf("session fork operation %s 不存在", id)
	}
	if operation.State != forkStateFailed || operation.Revision != expectedRevision {
		return operation, false, nil
	}
	previous := cloneForkOperations(s.operations)
	operation.State = nextState
	operation.Error = ""
	operation.Revision++
	operation.UpdatedAt = time.Now()
	s.operations[id] = operation
	prunedDirs := s.pruneLocked(operation.UpdatedAt)
	if err := s.writeLocked(); err != nil {
		s.operations = previous
		return previous[id], false, err
	}
	removeForkArtifactDirs(prunedDirs)
	return operation, true, nil
}

func cloneForkOperations(operations map[string]ForkOperation) map[string]ForkOperation {
	cloned := make(map[string]ForkOperation, len(operations))
	for id, operation := range operations {
		cloned[id] = operation
	}
	return cloned
}

func (s *forkOperationStore) Get(id string) (ForkOperation, bool) {
	if s == nil {
		return ForkOperation{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	operation, ok := s.operations[strings.TrimSpace(id)]
	return operation, ok
}

func (s *forkOperationStore) GetByCommand(messageID string) (ForkOperation, bool) {
	if s == nil {
		return ForkOperation{}, false
	}
	messageID = strings.TrimSpace(messageID)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, operation := range s.operations {
		if operation.Source.ForkCommandMessageID == messageID {
			return operation, true
		}
	}
	return ForkOperation{}, false
}

func (s *forkOperationStore) GetByTarget(key SessionKey) (ForkOperation, bool) {
	if s == nil {
		return ForkOperation{}, false
	}
	key = normalizeSessionKey(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, operation := range s.operations {
		if normalizeSessionKey(operation.TargetKey) == key {
			return operation, true
		}
	}
	return ForkOperation{}, false
}

func (s *forkOperationStore) GetByTargetMessageIDs(botID, chatID string, messageIDs ...string) (ForkOperation, bool) {
	if s == nil {
		return ForkOperation{}, false
	}
	botID = strings.TrimSpace(botID)
	chatID = strings.TrimSpace(chatID)
	ids := make(map[string]struct{}, len(messageIDs))
	for _, messageID := range messageIDs {
		if messageID = strings.TrimSpace(messageID); messageID != "" {
			ids[messageID] = struct{}{}
		}
	}
	if chatID == "" || len(ids) == 0 {
		return ForkOperation{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, operation := range s.operations {
		if operation.TargetKey.BotID != botID || operation.TargetChatID != chatID {
			continue
		}
		for _, candidate := range []string{operation.TargetThreadID, operation.TargetRootID, operation.TargetMessageID} {
			if _, ok := ids[strings.TrimSpace(candidate)]; ok {
				return operation, true
			}
		}
	}
	return ForkOperation{}, false
}

func (s *forkOperationStore) GetFailedWithoutTargetBySource(key SessionKey) (ForkOperation, bool) {
	if s == nil {
		return ForkOperation{}, false
	}
	key = normalizeSessionKey(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest ForkOperation
	found := false
	for _, operation := range s.operations {
		if operation.State != forkStateFailed || strings.TrimSpace(operation.TargetChatID) != "" ||
			normalizeSessionKey(operation.Source.SourceKey) != key {
			continue
		}
		if !found || operation.UpdatedAt.After(latest.UpdatedAt) {
			latest = operation
			found = true
		}
	}
	return latest, found
}

func (s *forkOperationStore) writeLocked() error {
	operations := make([]ForkOperation, 0, len(s.operations))
	for _, operation := range s.operations {
		operations = append(operations, operation)
	}
	sort.Slice(operations, func(i, j int) bool {
		return operations[i].CreatedAt.Before(operations[j].CreatedAt)
	})
	data, err := json.MarshalIndent(forkOperationFile{Version: 1, Operations: operations}, "", "  ")
	if err != nil {
		return fmt.Errorf("编码 session fork 操作记录: %w", err)
	}
	return writePrivateFileAtomic(s.indexPath, append(data, '\n'))
}

func (s *forkOperationStore) pruneLocked(now time.Time) []string {
	var dirs []string
	for id, operation := range s.operations {
		age := now.Sub(operation.UpdatedAt)
		retention := forkReadyRetention
		if operation.State == forkStateFailed {
			retention = forkFailedRetention
		}
		if operation.UpdatedAt.IsZero() || age <= retention {
			continue
		}
		delete(s.operations, id)
		dirs = append(dirs, filepath.Join(s.dir, traceSafeFileName(id)))
	}
	return dirs
}

func removeForkArtifactDirs(dirs []string) {
	for _, dir := range dirs {
		_ = os.RemoveAll(dir)
	}
}

func normalizeForkOperation(operation ForkOperation) ForkOperation {
	operation.ID = strings.TrimSpace(operation.ID)
	operation.State = strings.TrimSpace(operation.State)
	operation.SourceTitle = strings.TrimSpace(operation.SourceTitle)
	operation.TargetTitle = strings.TrimSpace(operation.TargetTitle)
	operation.TargetKey = normalizeSessionKey(operation.TargetKey)
	operation.TargetChatID = strings.TrimSpace(operation.TargetChatID)
	operation.TargetThreadID = strings.TrimSpace(operation.TargetThreadID)
	operation.TargetRootID = strings.TrimSpace(operation.TargetRootID)
	operation.TargetMessageID = strings.TrimSpace(operation.TargetMessageID)
	operation.TargetSession = strings.TrimSpace(operation.TargetSession)
	operation.BundlePath = strings.TrimSpace(operation.BundlePath)
	operation.Error = strings.TrimSpace(operation.Error)
	operation.InviteWarning = strings.TrimSpace(operation.InviteWarning)
	operation.ExtraUserOpenIDs = normalizeForkOpenIDs(operation.ExtraUserOpenIDs)
	return operation
}

func normalizeForkOpenIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	normalized := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}
	return normalized
}

func writePrivateFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	remove = false
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
