package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
)

const (
	sessionSourceIM             = "im"
	maxMessageSessionBindings   = 2000
	messageSessionBindingMaxAge = 7 * 24 * time.Hour
)

type SessionStore struct {
	path         string
	fallbackPath string
	mu           sync.Mutex
	sessions     map[SessionKey]Session
	history      []Session
	chats        map[ChatKey]ChatConfig
	messages     map[messageBindingKey]MessageSessionBinding
}

type sessionStoreSnapshot struct {
	sessions map[SessionKey]Session
	history  []Session
	chats    map[ChatKey]ChatConfig
	messages map[messageBindingKey]MessageSessionBinding
}

// NewSessionStore 创建会话存储
func NewSessionStore(path string) *SessionStore {
	return &SessionStore{
		path:     path,
		sessions: make(map[SessionKey]Session),
		chats:    make(map[ChatKey]ChatConfig),
		messages: make(map[messageBindingKey]MessageSessionBinding),
	}
}

// NewSessionStoreWithFallback writes to path and reads fallbackPath only when path
// does not exist. It keeps older workspace roots readable while moving state into .local.
func NewSessionStoreWithFallback(path string, fallbackPath string) *SessionStore {
	store := NewSessionStore(path)
	if strings.TrimSpace(fallbackPath) != strings.TrimSpace(path) {
		store.fallbackPath = fallbackPath
	}
	return store
}

// Load 从文件中加载历史会话
func (s *SessionStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.readPathLocked()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.sessions = make(map[SessionKey]Session)
			s.chats = make(map[ChatKey]ChatConfig)
			s.messages = make(map[messageBindingKey]MessageSessionBinding)
			s.history = nil
			return nil
		}
		return fmt.Errorf("读取会话映射文件: %w", err)
	}
	var file sessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		// 会话映射文件损坏：备份原文件后以空库启动，避免单个坏文件导致整个服务无法启动。
		// 下次写入会重新生成正常的 sessions.json。
		s.resetLocked()
		if backupErr := s.backupCorruptFileLocked(path, data, err); backupErr != nil {
			slog.Warn("备份损坏的会话映射文件失败", "路径", path, "错误", backupErr)
		}
		return nil
	}
	s.sessions = make(map[SessionKey]Session, len(file.Sessions))
	s.chats = make(map[ChatKey]ChatConfig, len(file.Chats))
	s.messages = make(map[messageBindingKey]MessageSessionBinding, len(file.Messages))
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
	for _, binding := range file.Messages {
		binding = normalizeMessageBinding(binding)
		if !validMessageBinding(binding) {
			continue
		}
		s.messages[messageBindingKeyFromBinding(binding)] = binding
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
	s.pruneMessageBindingsLocked(time.Now())
	if sessionFileHasLegacySessionKeyShape(data) {
		if err := s.writeLocked(); err != nil {
			slog.Warn("转换旧会话 key 写回失败", "路径", s.path, "读取路径", path, "错误", err)
		}
	}
	return nil
}

func (s *SessionStore) readPathLocked() string {
	path := strings.TrimSpace(s.path)
	if path == "" {
		return s.path
	}
	if _, err := os.Stat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return path
	}
	fallbackPath := strings.TrimSpace(s.fallbackPath)
	if fallbackPath == "" || fallbackPath == path {
		return path
	}
	if _, err := os.Stat(fallbackPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return fallbackPath
	}
	return path
}

// resetLocked 把内存状态置空，调用方必须持有 s.mu。
func (s *SessionStore) resetLocked() {
	s.sessions = make(map[SessionKey]Session)
	s.chats = make(map[ChatKey]ChatConfig)
	s.messages = make(map[messageBindingKey]MessageSessionBinding)
	s.history = nil
}

// backupCorruptFileLocked 把损坏的持久化文件重命名为带时间戳的 .corrupt-* 副本，
// 以便保留现场供排查，同时让后续写入能重新创建正常文件。调用方必须持有 s.mu。
func (s *SessionStore) backupCorruptFileLocked(path string, data []byte, cause error) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	backupPath := fmt.Sprintf("%s.corrupt-%s", path, time.Now().Format("20060102T150405.000000000"))
	if err := os.WriteFile(backupPath, data, 0o600); err != nil {
		return fmt.Errorf("写入损坏文件备份: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("移除损坏的原文件: %w", err)
	}
	slog.Warn("会话映射文件已损坏，已备份并以空库启动",
		"路径", path, "备份", backupPath, "错误", cause)
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
	chatKey := chatKeyFromSessionKey(session.Key)
	snapshot := s.snapshotLocked()
	now := time.Now()
	nextSeq := s.nextSessionSeqLocked(session.Key)
	if chatKey.Valid() {
		chat := s.chats[chatKey]
		chat.Key = chatKey
		if chat.CreatedAt.IsZero() {
			chat.CreatedAt = now
		}
		if chat.NextSessionSeq < 1 {
			chat.NextSessionSeq = nextSeq
		}
		nextSeq = chat.NextSessionSeq
		chat.NextSessionSeq++
		chat.UpdatedAt = now
		s.chats[chatKey] = chat
	}
	session.Title = fmt.Sprintf("session#%d", nextSeq)
	session.ManualTitle = false

	s.upsertSessionLocked(session, now)
	return session, s.writeOrRestoreLocked(snapshot)
}

func (s *SessionStore) nextSessionSeqLocked(key SessionKey) int {
	nextSeq := 1
	main := sessionMain(key)
	for _, session := range s.history {
		if !sameSessionMain(session.Key, key) || strings.TrimSpace(session.ACPSessionID) == "" {
			continue
		}
		nextSeq++
	}
	if main.Source == sessionSourceIM {
		if chat := s.chats[ChatKey{BotID: main.BotID, ChatID: main.MainID}]; chat.NextSessionSeq > nextSeq {
			nextSeq = chat.NextSessionSeq
		}
	}
	return nextSeq
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
	current.WorkspacePrompted = false
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

func (s *SessionStore) ResetWorkspacePromptedForAllSessions() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	changed := 0
	for _, session := range s.sessions {
		if session.WorkspacePrompted {
			changed++
		}
	}
	for _, session := range s.history {
		if session.WorkspacePrompted {
			changed++
		}
	}
	if changed == 0 {
		return 0, nil
	}
	snapshot := s.snapshotLocked()
	for key, session := range s.sessions {
		if !session.WorkspacePrompted {
			continue
		}
		session = cloneSession(session)
		session.WorkspacePrompted = false
		s.sessions[key] = session
	}
	for i, session := range s.history {
		if !session.WorkspacePrompted {
			continue
		}
		session = cloneSession(session)
		session.WorkspacePrompted = false
		s.history[i] = session
	}
	return changed, s.writeOrRestoreLocked(snapshot)
}

func (s *SessionStore) BindMessageToSession(binding MessageSessionBinding) (MessageSessionBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	binding = normalizeMessageBinding(binding)
	if !validMessageBinding(binding) {
		return MessageSessionBinding{}, fmt.Errorf("消息会话绑定字段不完整")
	}
	snapshot := s.snapshotLocked()
	now := time.Now()
	if existing, ok := s.messages[messageBindingKeyFromBinding(binding)]; ok && !existing.CreatedAt.IsZero() {
		binding.CreatedAt = existing.CreatedAt
	}
	if binding.CreatedAt.IsZero() {
		binding.CreatedAt = now
	}
	binding.UpdatedAt = now
	s.messages[messageBindingKeyFromBinding(binding)] = binding
	s.pruneMessageBindingsLocked(now)
	return binding, s.writeOrRestoreLocked(snapshot)
}

func (s *SessionStore) SessionForMessage(botID, chatID, messageID string) (Session, MessageSessionBinding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := messageBindingKey{
		BotID:     strings.TrimSpace(botID),
		ChatID:    strings.TrimSpace(chatID),
		MessageID: strings.TrimSpace(messageID),
	}
	if key.BotID == "" && key.ChatID == "" && key.MessageID == "" {
		return Session{}, MessageSessionBinding{}, false
	}
	binding, ok := s.messages[key]
	if !ok {
		return Session{}, MessageSessionBinding{}, false
	}
	session, ok := s.sessions[normalizeSessionKey(binding.SessionKey)]
	if !ok {
		return Session{}, binding, false
	}
	return cloneSession(session), binding, true
}

func (s *SessionStore) FirstMessageForSession(botID, chatID string, sessionKey SessionKey) (MessageSessionBinding, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	botID = strings.TrimSpace(botID)
	chatID = strings.TrimSpace(chatID)
	sessionKey = normalizeSessionKey(sessionKey)
	var first MessageSessionBinding
	found := false
	for _, binding := range s.messages {
		binding = normalizeMessageBinding(binding)
		if binding.BotID != botID || binding.ChatID != chatID || binding.SessionKey != sessionKey {
			continue
		}
		if !found || messageBindingCreatedBefore(binding, first) {
			first = binding
			found = true
		}
	}
	return first, found
}

func (s *SessionStore) ResumeSession(key SessionKey, acpSessionID string) (Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.resumeSessionLocked(key, acpSessionID)
}

func (s *SessionStore) SessionByACPSessionID(botID, acpSessionID string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	botID = strings.TrimSpace(botID)
	acpSessionID = strings.TrimSpace(acpSessionID)
	if acpSessionID == "" {
		return Session{}, false
	}
	var (
		best  Session
		found bool
	)
	for _, session := range s.sessions {
		if !sessionMatchesACPSessionID(session, botID, acpSessionID) {
			continue
		}
		if !found || session.UpdatedAt.After(best.UpdatedAt) || session.UpdatedAt.Equal(best.UpdatedAt) && sessionKeyLess(session.Key, best.Key) {
			best = cloneSession(session)
			found = true
		}
	}
	if found {
		return best, true
	}
	for _, session := range s.history {
		if !sessionMatchesACPSessionID(session, botID, acpSessionID) {
			continue
		}
		if !found || session.UpdatedAt.After(best.UpdatedAt) || session.UpdatedAt.Equal(best.UpdatedAt) && sessionKeyLess(session.Key, best.Key) {
			best = cloneSession(session)
			found = true
		}
	}
	return best, found
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
		if !sameSessionMain(item.Key, key) || item.ACPSessionID != acpSessionID {
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
	return cloneChatConfig(chat), ok
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
		chat = cloneChatConfig(latest)
	}
	if update == nil {
		return cloneChatConfig(chat), nil
	}
	update(&chat)
	snapshot := s.snapshotLocked()
	chat = s.upsertChatLocked(chat, time.Now())
	return cloneChatConfig(chat), s.writeOrRestoreLocked(snapshot)
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
	chat = normalizeChatForStore(chat)
	s.chats[chat.Key] = cloneChatConfig(chat)
	return chat
}

func (s *SessionStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *SessionStore) ListByChat(botID, chatID string) []Session {
	return s.ListByMain(SessionKey{BotID: botID, Source: sessionSourceIM, MainID: chatID})
}

func (s *SessionStore) ListByMain(key SessionKey) []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	key = normalizeSessionKey(key)
	if !key.Valid() {
		return nil
	}
	items := make([]Session, 0)
	for _, session := range s.history {
		if sameSessionMain(session.Key, key) && session.ACPSessionID != "" {
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
	grouped := make(map[sessionMainKey][]int)
	for i, session := range s.history {
		if !session.Key.Valid() || session.ACPSessionID == "" {
			continue
		}
		grouped[sessionMain(session.Key)] = append(grouped[sessionMain(session.Key)], i)
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

func (s *SessionStore) pruneMessageBindingsLocked(now time.Time) {
	if len(s.messages) == 0 {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	for key, binding := range s.messages {
		binding = normalizeMessageBinding(binding)
		if !validMessageBinding(binding) {
			delete(s.messages, key)
			continue
		}
		if _, ok := s.sessions[binding.SessionKey]; !ok {
			delete(s.messages, key)
			continue
		}
		if messageBindingTooOld(binding, now) {
			delete(s.messages, key)
			continue
		}
		if normalizedKey := messageBindingKeyFromBinding(binding); normalizedKey != key {
			delete(s.messages, key)
			s.messages[normalizedKey] = binding
		} else {
			s.messages[key] = binding
		}
	}
	if len(s.messages) <= maxMessageSessionBindings {
		return
	}
	bindings := make([]MessageSessionBinding, 0, len(s.messages))
	for _, binding := range s.messages {
		bindings = append(bindings, normalizeMessageBinding(binding))
	}
	sort.SliceStable(bindings, func(i, j int) bool {
		return messageBindingUpdatedAfter(bindings[i], bindings[j])
	})
	s.messages = make(map[messageBindingKey]MessageSessionBinding, maxMessageSessionBindings)
	for i, binding := range bindings {
		if i >= maxMessageSessionBindings {
			break
		}
		s.messages[messageBindingKeyFromBinding(binding)] = binding
	}
}

func sameHistorySession(a, b Session) bool {
	return sameSessionMain(a.Key, b.Key) &&
		a.ACPSessionID == b.ACPSessionID
}

func sessionMatchesACPSessionID(session Session, botID, acpSessionID string) bool {
	session = normalizeSessionForStore(session)
	return botID != "" && session.Key.Valid() && session.Key.BotID == botID && session.ACPSessionID == acpSessionID
}

func (s *SessionStore) snapshotLocked() sessionStoreSnapshot {
	snapshot := sessionStoreSnapshot{
		sessions: make(map[SessionKey]Session, len(s.sessions)),
		history:  make([]Session, len(s.history)),
		chats:    make(map[ChatKey]ChatConfig, len(s.chats)),
		messages: make(map[messageBindingKey]MessageSessionBinding, len(s.messages)),
	}
	for key, session := range s.sessions {
		snapshot.sessions[key] = cloneSession(session)
	}
	for i, session := range s.history {
		snapshot.history[i] = cloneSession(session)
	}
	for key, chat := range s.chats {
		snapshot.chats[key] = cloneChatConfig(chat)
	}
	for key, binding := range s.messages {
		snapshot.messages[key] = binding
	}
	return snapshot
}

func (s *SessionStore) writeOrRestoreLocked(snapshot sessionStoreSnapshot) error {
	if err := s.writeLocked(); err != nil {
		s.sessions = snapshot.sessions
		s.history = snapshot.history
		s.chats = snapshot.chats
		s.messages = snapshot.messages
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
		Messages: make([]MessageSessionBinding, 0, len(s.messages)),
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
		file.Chats = append(file.Chats, persistedChatConfig(chat))
	}
	for _, binding := range s.messages {
		file.Messages = append(file.Messages, binding)
	}
	sort.Slice(file.Chats, func(i, j int) bool {
		return chatKeyLess(file.Chats[i].Key, file.Chats[j].Key)
	})
	sort.Slice(file.Messages, func(i, j int) bool {
		return messageBindingLess(file.Messages[i], file.Messages[j])
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
	a = normalizeSessionKey(a)
	b = normalizeSessionKey(b)
	if a.BotID != b.BotID {
		return a.BotID < b.BotID
	}
	if sessionKeySource(a) != sessionKeySource(b) {
		return sessionKeySource(a) < sessionKeySource(b)
	}
	if sessionKeyMainID(a) != sessionKeyMainID(b) {
		return sessionKeyMainID(a) < sessionKeyMainID(b)
	}
	return a.SubID < b.SubID
}

func chatKeyLess(a, b ChatKey) bool {
	if a.BotID != b.BotID {
		return a.BotID < b.BotID
	}
	return a.ChatID < b.ChatID
}

func messageBindingLess(a, b MessageSessionBinding) bool {
	a = normalizeMessageBinding(a)
	b = normalizeMessageBinding(b)
	if a.BotID != b.BotID {
		return a.BotID < b.BotID
	}
	if a.ChatID != b.ChatID {
		return a.ChatID < b.ChatID
	}
	return a.MessageID < b.MessageID
}

func messageBindingCreatedBefore(a, b MessageSessionBinding) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		if a.CreatedAt.IsZero() {
			return false
		}
		if b.CreatedAt.IsZero() {
			return true
		}
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.MessageID < b.MessageID
}

func messageBindingTooOld(binding MessageSessionBinding, now time.Time) bool {
	if messageSessionBindingMaxAge <= 0 || now.IsZero() {
		return false
	}
	timestamp := binding.UpdatedAt
	if timestamp.IsZero() {
		timestamp = binding.CreatedAt
	}
	if timestamp.IsZero() {
		return false
	}
	return timestamp.Before(now.Add(-messageSessionBindingMaxAge))
}

func messageBindingUpdatedAfter(a, b MessageSessionBinding) bool {
	ta := messageBindingUpdatedAt(a)
	tb := messageBindingUpdatedAt(b)
	if !ta.Equal(tb) {
		if ta.IsZero() {
			return false
		}
		if tb.IsZero() {
			return true
		}
		return ta.After(tb)
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		if a.CreatedAt.IsZero() {
			return false
		}
		if b.CreatedAt.IsZero() {
			return true
		}
		return a.CreatedAt.After(b.CreatedAt)
	}
	return messageBindingLess(a, b)
}

func messageBindingUpdatedAt(binding MessageSessionBinding) time.Time {
	if !binding.UpdatedAt.IsZero() {
		return binding.UpdatedAt
	}
	return binding.CreatedAt
}

func normalizeSessionForStore(session Session) Session {
	session.Key = normalizeSessionKey(session.Key)
	session.AgentName = strings.TrimSpace(session.AgentName)
	session.ACPSessionID = strings.TrimSpace(session.ACPSessionID)
	session.ContextWindow = normalizeContextWindowUsagePtr(session.ContextWindow)
	if session.AutoCompactPct < 0 {
		session.AutoCompactPct = 0
	}
	if !session.AutoCompact {
		session.AutoCompacting = false
	}
	return session
}

func cloneSession(session Session) Session {
	session.ACPMeta = cloneJSONMap(session.ACPMeta)
	if session.ContextWindow != nil {
		contextWindow := *session.ContextWindow
		session.ContextWindow = &contextWindow
	}
	if session.LastAutoCompactAt != nil {
		last := *session.LastAutoCompactAt
		session.LastAutoCompactAt = &last
	}
	session.AvailableCommands = cloneAvailableCommands(session.AvailableCommands)
	session.ConfigOptions = cloneConfigOptions(session.ConfigOptions)
	if session.Models != nil {
		models := *session.Models
		models.AvailableModels = append([]acp.SessionModel(nil), session.Models.AvailableModels...)
		for i := range models.AvailableModels {
			models.AvailableModels[i].Meta = cloneJSONMap(models.AvailableModels[i].Meta)
		}
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
		for j := range cloned[i].Options {
			cloned[i].Options[j].Meta = cloneJSONMap(cloned[i].Options[j].Meta)
		}
	}
	return cloned
}

func normalizeChatForStore(chat ChatConfig) ChatConfig {
	chat.Key = normalizeChatKey(chat.Key)
	chat.AgentName = strings.TrimSpace(chat.AgentName)
	chat.AgentConfigs = normalizeChatAgentConfigs(chat.AgentConfigs)
	return chat
}

func normalizeChatAgentConfigs(configs map[string]ChatAgentConfig) map[string]ChatAgentConfig {
	if len(configs) == 0 {
		return nil
	}
	normalized := make(map[string]ChatAgentConfig, len(configs))
	for agentName, cfg := range configs {
		agentName = strings.TrimSpace(agentName)
		cfg.Mode = strings.TrimSpace(cfg.Mode)
		cfg.Model = strings.TrimSpace(cfg.Model)
		if agentName == "" || cfg.empty() {
			continue
		}
		normalized[agentName] = cfg
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func (c ChatAgentConfig) empty() bool {
	return c.Mode == "" && c.Model == ""
}

func cloneChatConfig(chat ChatConfig) ChatConfig {
	if chat.AgentConfigs != nil {
		configs := make(map[string]ChatAgentConfig, len(chat.AgentConfigs))
		for agentName, cfg := range chat.AgentConfigs {
			configs[agentName] = cfg
		}
		chat.AgentConfigs = configs
	}
	return chat
}

func persistedChatConfig(chat ChatConfig) ChatConfig {
	return normalizeChatForStore(chat)
}

type messageBindingKey struct {
	BotID     string
	ChatID    string
	MessageID string
}

func normalizeMessageBinding(binding MessageSessionBinding) MessageSessionBinding {
	binding.BotID = strings.TrimSpace(binding.BotID)
	binding.ChatID = strings.TrimSpace(binding.ChatID)
	binding.MessageID = strings.TrimSpace(binding.MessageID)
	binding.SessionKey = normalizeSessionKey(binding.SessionKey)
	if binding.SessionKey.BotID == "" {
		binding.SessionKey.BotID = binding.BotID
	}
	return binding
}

func validMessageBinding(binding MessageSessionBinding) bool {
	binding = normalizeMessageBinding(binding)
	return binding.BotID != "" && binding.ChatID != "" && binding.MessageID != "" && binding.SessionKey.Valid()
}

func messageBindingKeyFromBinding(binding MessageSessionBinding) messageBindingKey {
	binding = normalizeMessageBinding(binding)
	return messageBindingKey{
		BotID:     binding.BotID,
		ChatID:    binding.ChatID,
		MessageID: binding.MessageID,
	}
}

func normalizeSessionKey(key SessionKey) SessionKey {
	normalized := SessionKey{
		BotID:  strings.TrimSpace(key.BotID),
		Source: strings.TrimSpace(key.Source),
		MainID: strings.TrimSpace(key.MainID),
		SubID:  strings.TrimSpace(key.SubID),
	}
	if normalized.Source == "" {
		normalized.Source = sessionSourceIM
	}
	return normalized
}

func normalizeChatKey(key ChatKey) ChatKey {
	return ChatKey{
		BotID:  strings.TrimSpace(key.BotID),
		ChatID: strings.TrimSpace(key.ChatID),
	}
}

type sessionMainKey struct {
	BotID  string
	Source string
	MainID string
}

func sessionKeySource(key SessionKey) string {
	source := strings.TrimSpace(key.Source)
	if source == "" {
		return sessionSourceIM
	}
	return source
}

func sessionKeyMainID(key SessionKey) string {
	return strings.TrimSpace(key.MainID)
}

func sessionMain(key SessionKey) sessionMainKey {
	key = normalizeSessionKey(key)
	return sessionMainKey{
		BotID:  key.BotID,
		Source: sessionKeySource(key),
		MainID: sessionKeyMainID(key),
	}
}

func sameSessionMain(a, b SessionKey) bool {
	return sessionMain(a) == sessionMain(b)
}

func chatKeyFromSessionKey(key SessionKey) ChatKey {
	key = normalizeSessionKey(key)
	if sessionKeySource(key) != sessionSourceIM {
		return ChatKey{BotID: key.BotID}
	}
	return ChatKey{BotID: key.BotID, ChatID: sessionKeyMainID(key)}
}

func sessionFileHasLegacySessionKeyShape(data []byte) bool {
	type rawSession struct {
		Key map[string]json.RawMessage `json:"key"`
	}
	type rawMessage struct {
		SessionKey map[string]json.RawMessage `json:"session_key"`
	}
	var raw struct {
		Sessions []rawSession `json:"sessions"`
		History  []rawSession `json:"history"`
		Messages []rawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	for _, session := range raw.Sessions {
		if sessionKeyShapeHasLegacyFields(session.Key) {
			return true
		}
	}
	for _, session := range raw.History {
		if sessionKeyShapeHasLegacyFields(session.Key) {
			return true
		}
	}
	for _, message := range raw.Messages {
		if sessionKeyShapeHasLegacyFields(message.SessionKey) {
			return true
		}
	}
	return false
}

func sessionKeyShapeHasLegacyFields(key map[string]json.RawMessage) bool {
	if len(key) == 0 {
		return false
	}
	for _, field := range []string{"chat_id", "thread_id", "parent_id"} {
		if _, ok := key[field]; ok {
			return true
		}
	}
	return false
}

type sessionFile struct {
	Version  int                     `json:"version"`
	Sessions []Session               `json:"sessions"`
	History  []Session               `json:"history,omitempty"`
	Chats    []ChatConfig            `json:"chats,omitempty"`
	Messages []MessageSessionBinding `json:"messages,omitempty"`
}
