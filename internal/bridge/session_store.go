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

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

type SessionStore struct {
	path     string
	mu       sync.Mutex
	sessions map[SessionKey]Session
	history  []Session
	chats    map[ChatKey]ChatConfig
}

type sessionStoreSnapshot struct {
	sessions map[SessionKey]Session
	history  []Session
	chats    map[ChatKey]ChatConfig
}

// NewSessionStore 创建会话存储
func NewSessionStore(path string) *SessionStore {
	return &SessionStore{
		path:     path,
		sessions: make(map[SessionKey]Session),
		chats:    make(map[ChatKey]ChatConfig),
	}
}

// Load 从文件中加载历史会话
func (s *SessionStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.sessions = make(map[SessionKey]Session)
			s.chats = make(map[ChatKey]ChatConfig)
			s.history = nil
			return nil
		}
		return fmt.Errorf("读取会话映射文件: %w", err)
	}
	var file sessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("解析会话映射文件: %w", err)
	}
	s.sessions = make(map[SessionKey]Session, len(file.Sessions))
	s.chats = make(map[ChatKey]ChatConfig, len(file.Chats))
	for _, session := range file.Sessions {
		session = normalizeSessionForStore(session)
		if !session.Key.Valid() {
			continue
		}
		s.sessions[session.Key] = session
	}
	for _, chat := range file.Chats {
		chat = normalizeChatForStore(chat)
		if !chat.Key.Valid() {
			continue
		}
		s.chats[chat.Key] = chat
	}
	s.history = s.history[:0]
	for _, session := range file.History {
		session = normalizeSessionForStore(session)
		if !session.Key.Valid() || session.ACPSessionID == "" {
			continue
		}
		s.upsertHistoryLocked(session)
	}
	for _, session := range s.sessions {
		if session.ACPSessionID != "" {
			s.upsertHistoryLocked(session)
		}
	}
	s.trimHistoryLocked()
	return nil
}

func (s *SessionStore) Upsert(session Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	session = normalizeSessionForStore(session)
	if !session.Key.Valid() {
		return fmt.Errorf("会话 key 不能为空")
	}
	snapshot := s.snapshotLocked()
	now := time.Now()
	s.upsertSessionLocked(session, now)
	return s.writeOrRestoreLocked(snapshot)
}

func (s *SessionStore) UpsertWithDefaultTitle(session Session) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session = normalizeSessionForStore(session)
	if !session.Key.Valid() {
		return Session{}, fmt.Errorf("会话 key 不能为空")
	}
	if session.ACPSessionID == "" {
		return Session{}, fmt.Errorf("ACP session 不能为空")
	}
	chatKey := ChatKey{BotID: session.Key.BotID, ChatID: session.Key.ChatID}
	if !chatKey.Valid() {
		return Session{}, fmt.Errorf("chat key 不能为空")
	}
	snapshot := s.snapshotLocked()
	now := time.Now()
	chat := s.chats[chatKey]
	chat.Key = chatKey
	if chat.CreatedAt.IsZero() {
		chat.CreatedAt = now
	}
	if chat.NextSessionSeq < 1 {
		chat.NextSessionSeq = 1
	}
	session.Title = fmt.Sprintf("session#%d", chat.NextSessionSeq)
	session.ManualTitle = false
	chat.NextSessionSeq++
	chat.UpdatedAt = now

	s.upsertSessionLocked(session, now)
	s.chats[chatKey] = chat
	return session, s.writeOrRestoreLocked(snapshot)
}

func (s *SessionStore) UpdateAutomaticTitle(session Session, title string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session = normalizeSessionForStore(session)
	if !session.Key.Valid() {
		return Session{}, fmt.Errorf("会话 key 不能为空")
	}
	if latest, ok := s.sessions[session.Key]; ok {
		if latest.ACPSessionID != session.ACPSessionID || latest.ManualTitle {
			return cloneSession(latest), nil
		}
		session = cloneSession(latest)
	}
	if session.ManualTitle || session.Title == title {
		return session, nil
	}
	snapshot := s.snapshotLocked()
	session.Title = title
	s.upsertSessionLocked(session, time.Now())
	return session, s.writeOrRestoreLocked(snapshot)
}

func (s *SessionStore) UpdateManualTitle(key SessionKey, acpSessionID, title string) (Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key = normalizeSessionKey(key)
	session, ok := s.sessions[key]
	if !ok || session.ACPSessionID != strings.TrimSpace(acpSessionID) {
		return cloneSession(session), false, nil
	}
	snapshot := s.snapshotLocked()
	session = cloneSession(session)
	session.Title = title
	session.ManualTitle = true
	s.upsertSessionLocked(session, time.Now())
	return session, true, s.writeOrRestoreLocked(snapshot)
}

func (s *SessionStore) ReplaceCurrentACPSession(previousACPSessionID string, replacement Session) (Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	replacement = normalizeSessionForStore(replacement)
	if !replacement.Key.Valid() {
		return Session{}, false, fmt.Errorf("会话 key 不能为空")
	}
	current, ok := s.sessions[replacement.Key]
	if !ok || current.ACPSessionID != strings.TrimSpace(previousACPSessionID) {
		return cloneSession(current), false, nil
	}
	snapshot := s.snapshotLocked()
	current = cloneSession(current)
	current.AgentName = replacement.AgentName
	current.ACPSessionID = replacement.ACPSessionID
	current.ACPUpdatedAt = replacement.ACPUpdatedAt
	current.ACPMeta = replacement.ACPMeta
	current.Cwd = replacement.Cwd
	current.Workspace = replacement.Workspace
	current.AvailableCommands = replacement.AvailableCommands
	current.ConfigOptions = replacement.ConfigOptions
	current.Models = replacement.Models
	current.Mode = replacement.Mode
	s.upsertSessionLocked(current, time.Now())
	return current, true, s.writeOrRestoreLocked(snapshot)
}

func (s *SessionStore) UpdateCurrentSession(key SessionKey, acpSessionID string, update func(*Session) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key = normalizeSessionKey(key)
	session, ok := s.sessions[key]
	if !ok || session.ACPSessionID != strings.TrimSpace(acpSessionID) || update == nil {
		return nil
	}
	session = cloneSession(session)
	if !update(&session) {
		return nil
	}
	snapshot := s.snapshotLocked()
	s.upsertSessionLocked(session, time.Now())
	return s.writeOrRestoreLocked(snapshot)
}

func (s *SessionStore) ResumeSession(key SessionKey, acpSessionID string) (Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.resumeSessionLocked(key, acpSessionID)
}

func (s *SessionStore) ResumeSessionIfCurrent(key SessionKey, currentACPSessionID, acpSessionID string) (Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key = normalizeSessionKey(key)
	current, ok := s.sessions[key]
	expectedCurrentACPSessionID := strings.TrimSpace(currentACPSessionID)
	if (ok && current.ACPSessionID != expectedCurrentACPSessionID) || (!ok && expectedCurrentACPSessionID != "") {
		return cloneSession(current), false, nil
	}
	return s.resumeSessionLocked(key, acpSessionID)
}

func (s *SessionStore) resumeSessionLocked(key SessionKey, acpSessionID string) (Session, bool, error) {
	key = normalizeSessionKey(key)
	acpSessionID = strings.TrimSpace(acpSessionID)
	if !key.Valid() || acpSessionID == "" {
		return Session{}, false, nil
	}
	for _, item := range s.history {
		if item.Key.BotID != key.BotID || item.Key.ChatID != key.ChatID || item.ACPSessionID != acpSessionID {
			continue
		}
		snapshot := s.snapshotLocked()
		session := cloneSession(item)
		session.Key = key
		s.upsertSessionLocked(session, time.Now())
		return session, true, s.writeOrRestoreLocked(snapshot)
	}
	return Session{}, false, nil
}

func (s *SessionStore) upsertSessionLocked(session Session, now time.Time) {
	session = cloneSession(session)
	if session.CreatedAt.IsZero() {
		if existing, ok := s.sessions[session.Key]; ok {
			if existing.ACPSessionID == session.ACPSessionID {
				session.CreatedAt = existing.CreatedAt
			} else {
				session.CreatedAt = now
			}
		} else {
			session.CreatedAt = now
		}
	}
	session.UpdatedAt = now
	s.sessions[session.Key] = cloneSession(session)
	if session.ACPSessionID != "" {
		s.upsertHistoryLocked(cloneSession(session))
		s.trimHistoryLocked()
	}
}

func (s *SessionStore) Get(key SessionKey) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key = normalizeSessionKey(key)
	session, ok := s.sessions[key]
	return cloneSession(session), ok
}

func (s *SessionStore) GetChat(key ChatKey) (ChatConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key = normalizeChatKey(key)
	chat, ok := s.chats[key]
	return chat, ok
}

func (s *SessionStore) UpsertChat(chat ChatConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	chat = normalizeChatForStore(chat)
	if !chat.Key.Valid() {
		return fmt.Errorf("chat key 不能为空")
	}
	snapshot := s.snapshotLocked()
	s.upsertChatLocked(chat, time.Now())
	return s.writeOrRestoreLocked(snapshot)
}

func (s *SessionStore) InsertChatIfAbsent(chat ChatConfig) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	chat = normalizeChatForStore(chat)
	if !chat.Key.Valid() {
		return false, fmt.Errorf("chat key 不能为空")
	}
	if _, ok := s.chats[chat.Key]; ok {
		return false, nil
	}
	snapshot := s.snapshotLocked()
	s.upsertChatLocked(chat, time.Now())
	return true, s.writeOrRestoreLocked(snapshot)
}

func (s *SessionStore) UpdateChat(chat ChatConfig, update func(*ChatConfig)) (ChatConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	chat = normalizeChatForStore(chat)
	if !chat.Key.Valid() {
		return ChatConfig{}, fmt.Errorf("chat key 不能为空")
	}
	if latest, ok := s.chats[chat.Key]; ok {
		chat = latest
	}
	if update == nil {
		return chat, nil
	}
	update(&chat)
	snapshot := s.snapshotLocked()
	chat = s.upsertChatLocked(chat, time.Now())
	return chat, s.writeOrRestoreLocked(snapshot)
}

func (s *SessionStore) upsertChatLocked(chat ChatConfig, now time.Time) ChatConfig {
	if chat.CreatedAt.IsZero() {
		if existing, ok := s.chats[chat.Key]; ok {
			chat.CreatedAt = existing.CreatedAt
		} else {
			chat.CreatedAt = now
		}
	}
	chat.UpdatedAt = now
	s.chats[chat.Key] = chat
	return chat
}

func (s *SessionStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *SessionStore) ListByChat(botID, chatID string) []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	botID = strings.TrimSpace(botID)
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return nil
	}
	items := make([]Session, 0)
	for _, session := range s.history {
		if session.Key.BotID == botID && session.Key.ChatID == chatID && session.ACPSessionID != "" {
			items = append(items, cloneSession(session))
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].UpdatedAt.After(items[j].UpdatedAt)
	})
	return items
}

func (s *SessionStore) upsertHistoryLocked(session Session) {
	for i, existing := range s.history {
		if sameHistorySession(existing, session) {
			if session.CreatedAt.IsZero() {
				session.CreatedAt = existing.CreatedAt
			}
			if session.UpdatedAt.IsZero() {
				session.UpdatedAt = existing.UpdatedAt
			}
			s.history[i] = session
			return
		}
	}
	s.history = append(s.history, session)
}

func (s *SessionStore) trimHistoryLocked() {
	if len(s.history) <= maxSessionHistoryPerChat {
		return
	}
	grouped := make(map[ChatKey][]int)
	for i, session := range s.history {
		if !session.Key.Valid() || session.ACPSessionID == "" {
			continue
		}
		key := ChatKey{BotID: session.Key.BotID, ChatID: session.Key.ChatID}
		grouped[key] = append(grouped[key], i)
	}
	keep := make([]bool, len(s.history))
	for _, indexes := range grouped {
		sort.SliceStable(indexes, func(i, j int) bool {
			return s.history[indexes[i]].UpdatedAt.After(s.history[indexes[j]].UpdatedAt)
		})
		for i, index := range indexes {
			if i >= maxSessionHistoryPerChat {
				break
			}
			keep[index] = true
		}
	}
	trimmed := s.history[:0]
	for i, session := range s.history {
		if keep[i] {
			trimmed = append(trimmed, session)
		}
	}
	s.history = trimmed
}

func sameHistorySession(a, b Session) bool {
	return a.Key.BotID == b.Key.BotID &&
		a.Key.ChatID == b.Key.ChatID &&
		a.ACPSessionID == b.ACPSessionID
}

func (s *SessionStore) snapshotLocked() sessionStoreSnapshot {
	snapshot := sessionStoreSnapshot{
		sessions: make(map[SessionKey]Session, len(s.sessions)),
		history:  make([]Session, len(s.history)),
		chats:    make(map[ChatKey]ChatConfig, len(s.chats)),
	}
	for key, session := range s.sessions {
		snapshot.sessions[key] = cloneSession(session)
	}
	for i, session := range s.history {
		snapshot.history[i] = cloneSession(session)
	}
	for key, chat := range s.chats {
		snapshot.chats[key] = chat
	}
	return snapshot
}

func (s *SessionStore) writeOrRestoreLocked(snapshot sessionStoreSnapshot) error {
	if err := s.writeLocked(); err != nil {
		s.sessions = snapshot.sessions
		s.history = snapshot.history
		s.chats = snapshot.chats
		return err
	}
	return nil
}

func (s *SessionStore) writeLocked() error {
	file := sessionFile{
		Version:  1,
		Sessions: make([]Session, 0, len(s.sessions)),
		History:  make([]Session, 0, len(s.history)),
		Chats:    make([]ChatConfig, 0, len(s.chats)),
	}
	for _, session := range s.sessions {
		file.Sessions = append(file.Sessions, persistedSession(session))
	}
	sort.Slice(file.Sessions, func(i, j int) bool {
		return sessionKeyLess(file.Sessions[i].Key, file.Sessions[j].Key)
	})
	for _, session := range s.history {
		file.History = append(file.History, persistedSession(session))
	}
	for _, chat := range s.chats {
		file.Chats = append(file.Chats, chat)
	}
	sort.Slice(file.Chats, func(i, j int) bool {
		return chatKeyLess(file.Chats[i].Key, file.Chats[j].Key)
	})
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("编码会话映射文件: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("创建会话映射目录: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时会话映射文件: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换会话映射文件: %w", err)
	}
	return nil
}

func persistedSession(session Session) Session {
	session.AvailableCommands = nil
	session.ConfigOptions = persistedConfigOptions(session.ConfigOptions)
	session.Models = persistedModelState(session.Models)
	session.Mode = persistedModeState(session.Mode)
	return session
}

func persistedConfigOptions(options []acp.SessionConfigOption) []acp.SessionConfigOption {
	if len(options) == 0 {
		return nil
	}
	persisted := make([]acp.SessionConfigOption, 0, len(options))
	for _, option := range options {
		if strings.TrimSpace(option.ID) == "" {
			continue
		}
		persisted = append(persisted, acp.SessionConfigOption{
			ID:           option.ID,
			Name:         option.Name,
			Category:     option.Category,
			Type:         option.Type,
			CurrentValue: option.CurrentValue,
		})
	}
	if len(persisted) == 0 {
		return nil
	}
	return persisted
}

func persistedModelState(state *acp.SessionModelState) *acp.SessionModelState {
	if state == nil {
		return nil
	}
	current := strings.TrimSpace(state.CurrentModelID)
	if current == "" {
		return nil
	}
	return &acp.SessionModelState{CurrentModelID: current}
}

func persistedModeState(state *acp.SessionModeState) *acp.SessionModeState {
	if state == nil {
		return nil
	}
	current := strings.TrimSpace(state.CurrentModeID)
	if current == "" {
		return nil
	}
	return &acp.SessionModeState{CurrentModeID: current}
}

func sessionKeyLess(a, b SessionKey) bool {
	if a.BotID != b.BotID {
		return a.BotID < b.BotID
	}
	if a.ChatID != b.ChatID {
		return a.ChatID < b.ChatID
	}
	return a.ThreadID < b.ThreadID
}

func chatKeyLess(a, b ChatKey) bool {
	if a.BotID != b.BotID {
		return a.BotID < b.BotID
	}
	return a.ChatID < b.ChatID
}

func normalizeSessionForStore(session Session) Session {
	session.Key = normalizeSessionKey(session.Key)
	session.AgentName = strings.TrimSpace(session.AgentName)
	session.ACPSessionID = strings.TrimSpace(session.ACPSessionID)
	return session
}

func cloneSession(session Session) Session {
	session.ACPMeta = cloneJSONMap(session.ACPMeta)
	session.AvailableCommands = cloneAvailableCommands(session.AvailableCommands)
	session.ConfigOptions = cloneConfigOptions(session.ConfigOptions)
	if session.Models != nil {
		models := *session.Models
		models.AvailableModels = append([]acp.SessionModel(nil), session.Models.AvailableModels...)
		session.Models = &models
	}
	if session.Mode != nil {
		mode := *session.Mode
		mode.AvailableModes = append([]acp.SessionMode(nil), session.Mode.AvailableModes...)
		session.Mode = &mode
	}
	return session
}

func cloneJSONMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = cloneJSONValue(item)
	}
	return cloned
}

func cloneJSONValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneJSONMap(value)
	case []any:
		cloned := make([]any, len(value))
		for i, item := range value {
			cloned[i] = cloneJSONValue(item)
		}
		return cloned
	default:
		return value
	}
}

func cloneAvailableCommands(commands []acp.AvailableCommand) []acp.AvailableCommand {
	if commands == nil {
		return nil
	}
	cloned := append([]acp.AvailableCommand(nil), commands...)
	for i := range cloned {
		if cloned[i].Input != nil {
			input := *cloned[i].Input
			cloned[i].Input = &input
		}
	}
	return cloned
}

func cloneConfigOptions(options []acp.SessionConfigOption) []acp.SessionConfigOption {
	if options == nil {
		return nil
	}
	cloned := append([]acp.SessionConfigOption(nil), options...)
	for i := range cloned {
		cloned[i].Options = append([]acp.SessionConfigOptionValue(nil), cloned[i].Options...)
	}
	return cloned
}

func normalizeChatForStore(chat ChatConfig) ChatConfig {
	chat.Key = normalizeChatKey(chat.Key)
	chat.AgentName = strings.TrimSpace(chat.AgentName)
	return chat
}

func normalizeSessionKey(key SessionKey) SessionKey {
	return SessionKey{
		BotID:    strings.TrimSpace(key.BotID),
		ChatID:   strings.TrimSpace(key.ChatID),
		ThreadID: strings.TrimSpace(key.ThreadID),
	}
}

func normalizeChatKey(key ChatKey) ChatKey {
	return ChatKey{
		BotID:  strings.TrimSpace(key.BotID),
		ChatID: strings.TrimSpace(key.ChatID),
	}
}

type sessionFile struct {
	Version  int          `json:"version"`
	Sessions []Session    `json:"sessions"`
	History  []Session    `json:"history,omitempty"`
	Chats    []ChatConfig `json:"chats,omitempty"`
}
