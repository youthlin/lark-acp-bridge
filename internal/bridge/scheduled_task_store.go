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

type ScheduledTaskStore struct {
	path         string
	fallbackPath string
	mu           sync.Mutex
	tasks        map[string]ScheduledTask
}

type scheduledTaskStoreSnapshot struct {
	tasks map[string]ScheduledTask
}

type scheduledTaskFile struct {
	Version int             `json:"version"`
	Tasks   []ScheduledTask `json:"tasks"`
}

func NewScheduledTaskStore(path string) *ScheduledTaskStore {
	return &ScheduledTaskStore{
		path:  path,
		tasks: make(map[string]ScheduledTask),
	}
}

func NewScheduledTaskStoreWithFallback(path string, fallbackPath string) *ScheduledTaskStore {
	store := NewScheduledTaskStore(path)
	if strings.TrimSpace(fallbackPath) != strings.TrimSpace(path) {
		store.fallbackPath = fallbackPath
	}
	return store
}

func (s *ScheduledTaskStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.readPathLocked())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.tasks = make(map[string]ScheduledTask)
			return nil
		}
		return fmt.Errorf("读取定时任务文件: %w", err)
	}
	var file scheduledTaskFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("解析定时任务文件: %w", err)
	}
	tasks := make(map[string]ScheduledTask, len(file.Tasks))
	for _, task := range file.Tasks {
		task = normalizeScheduledTask(task)
		if !validScheduledTask(task) {
			continue
		}
		tasks[task.ID] = task
	}
	s.tasks = tasks
	return nil
}

func (s *ScheduledTaskStore) readPathLocked() string {
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

func (s *ScheduledTaskStore) Upsert(task ScheduledTask) (ScheduledTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task = normalizeScheduledTask(task)
	if !validScheduledTask(task) {
		return ScheduledTask{}, fmt.Errorf("定时任务字段不完整")
	}
	snapshot := s.snapshotLocked()
	now := time.Now()
	if existing, ok := s.tasks[task.ID]; ok && task.CreatedAt.IsZero() {
		task.CreatedAt = existing.CreatedAt
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	task.UpdatedAt = now
	s.tasks[task.ID] = task
	if err := s.writeOrRestoreLocked(snapshot); err != nil {
		return ScheduledTask{}, err
	}
	return task, nil
}

func (s *ScheduledTaskStore) Get(id string) (ScheduledTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[strings.TrimSpace(id)]
	return task, ok
}

func (s *ScheduledTaskStore) Update(id string, update func(*ScheduledTask)) (ScheduledTask, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id = strings.TrimSpace(id)
	task, ok := s.tasks[id]
	if !ok {
		return ScheduledTask{}, false, nil
	}
	snapshot := s.snapshotLocked()
	if update != nil {
		update(&task)
	}
	task = normalizeScheduledTask(task)
	if !validScheduledTask(task) {
		s.tasks = snapshot.tasks
		return ScheduledTask{}, false, fmt.Errorf("定时任务字段不完整")
	}
	task.UpdatedAt = time.Now()
	s.tasks[id] = task
	if err := s.writeOrRestoreLocked(snapshot); err != nil {
		return ScheduledTask{}, false, err
	}
	return task, true, nil
}

func (s *ScheduledTaskStore) List() []ScheduledTask {
	s.mu.Lock()
	defer s.mu.Unlock()

	tasks := make([]ScheduledTask, 0, len(s.tasks))
	for _, task := range s.tasks {
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].ID < tasks[j].ID
	})
	return tasks
}

func (s *ScheduledTaskStore) Delete(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id = strings.TrimSpace(id)
	if _, ok := s.tasks[id]; !ok {
		return false, nil
	}
	snapshot := s.snapshotLocked()
	delete(s.tasks, id)
	if err := s.writeOrRestoreLocked(snapshot); err != nil {
		return false, err
	}
	return true, nil
}

func (s *ScheduledTaskStore) snapshotLocked() scheduledTaskStoreSnapshot {
	snapshot := scheduledTaskStoreSnapshot{tasks: make(map[string]ScheduledTask, len(s.tasks))}
	for id, task := range s.tasks {
		snapshot.tasks[id] = task
	}
	return snapshot
}

func (s *ScheduledTaskStore) writeOrRestoreLocked(snapshot scheduledTaskStoreSnapshot) error {
	if err := s.writeLocked(); err != nil {
		s.tasks = snapshot.tasks
		return err
	}
	return nil
}

func (s *ScheduledTaskStore) writeLocked() error {
	file := scheduledTaskFile{
		Version: 1,
		Tasks:   make([]ScheduledTask, 0, len(s.tasks)),
	}
	for _, task := range s.tasks {
		file.Tasks = append(file.Tasks, task)
	}
	sort.Slice(file.Tasks, func(i, j int) bool {
		return file.Tasks[i].ID < file.Tasks[j].ID
	})
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("编码定时任务文件: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("创建定时任务目录: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时定时任务文件: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换定时任务文件: %w", err)
	}
	return nil
}
