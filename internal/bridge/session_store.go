package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type SessionStore struct {
	path     string
	mu       sync.Mutex
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
		if !session.Key.Valid() {
			continue
		}
		s.sessions[session.Key] = session
	}
	for _, chat := range file.Chats {
		if !chat.Key.Valid() {
			continue
		}
		s.chats[chat.Key] = chat
	}
	s.history = s.history[:0]
	for _, session := range file.History {
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

	if !session.Key.Valid() {
		return fmt.Errorf("会话 key 不能为空")
	}
	now := time.Now()
	s.upsertSessionLocked(session, now)
	return s.writeLocked()
}

func (s *SessionStore) UpsertWithDefaultTitle(session Session) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

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
	return session, s.writeLocked()
}

func (s *SessionStore) upsertSessionLocked(session Session, now time.Time) {
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
	s.sessions[session.Key] = session
	if session.ACPSessionID != "" {
		s.upsertHistoryLocked(session)
		s.trimHistoryLocked()
	}
}

func (s *SessionStore) Get(key SessionKey) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[key]
	return session, ok
}

func (s *SessionStore) GetChat(key ChatKey) (ChatConfig, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chat, ok := s.chats[key]
	return chat, ok
}

func (s *SessionStore) UpsertChat(chat ChatConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !chat.Key.Valid() {
		return fmt.Errorf("chat key 不能为空")
	}
	now := time.Now()
	if chat.CreatedAt.IsZero() {
		if existing, ok := s.chats[chat.Key]; ok {
			chat.CreatedAt = existing.CreatedAt
		} else {
			chat.CreatedAt = now
		}
	}
	chat.UpdatedAt = now
	s.chats[chat.Key] = chat
	return s.writeLocked()
}

func (s *SessionStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *SessionStore) ListByChat(botID, chatID string) []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if chatID == "" {
		return nil
	}
	items := make([]Session, 0)
	for _, session := range s.history {
		if session.Key.BotID == botID && session.Key.ChatID == chatID && session.ACPSessionID != "" {
			items = append(items, session)
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

func (s *SessionStore) writeLocked() error {
	file := sessionFile{
		Version:  1,
		Sessions: make([]Session, 0, len(s.sessions)),
		History:  make([]Session, 0, len(s.history)),
		Chats:    make([]ChatConfig, 0, len(s.chats)),
	}
	for _, session := range s.sessions {
		file.Sessions = append(file.Sessions, session)
	}
	file.History = append(file.History, s.history...)
	for _, chat := range s.chats {
		file.Chats = append(file.Chats, chat)
	}
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

type sessionFile struct {
	Version  int          `json:"version"`
	Sessions []Session    `json:"sessions"`
	History  []Session    `json:"history,omitempty"`
	Chats    []ChatConfig `json:"chats,omitempty"`
}
