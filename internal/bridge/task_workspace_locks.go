package bridge

import (
	"path/filepath"
	"strings"
)

type workspaceTaskLocks map[string]*runningTask

func newWorkspaceTaskLocks() workspaceTaskLocks {
	return make(workspaceTaskLocks)
}

func (locks workspaceTaskLocks) busy(key SessionKey, workspace string, tasks map[SessionKey]*runningTask, wikiTasks map[runtimeKey]*runningTask) bool {
	key = normalizeSessionKey(key)
	workspace = normalizeWorkspaceLockPath(workspace)
	if workspace != "" {
		if task := locks[workspace]; task != nil {
			return true
		}
	}
	for taskKey, task := range tasks {
		if task == nil {
			continue
		}
		if normalizeSessionKey(taskKey) == key {
			return true
		}
		if sameWorkspaceLockPath(workspace, task.session.Workspace) {
			return true
		}
	}
	for runtime, task := range wikiTasks {
		if task == nil {
			continue
		}
		if normalizeSessionKey(runtime.SessionKey) == key {
			return true
		}
		if sameWorkspaceLockPath(workspace, task.session.Workspace) {
			return true
		}
	}
	return false
}

func (locks *workspaceTaskLocks) set(workspace string, task *runningTask) {
	workspace = normalizeWorkspaceLockPath(workspace)
	if workspace == "" || task == nil {
		return
	}
	if *locks == nil {
		*locks = newWorkspaceTaskLocks()
	}
	(*locks)[workspace] = task
}

func (locks workspaceTaskLocks) clear(workspace string, task *runningTask) {
	workspace = normalizeWorkspaceLockPath(workspace)
	if workspace == "" || task == nil || locks == nil {
		return
	}
	if locks[workspace] == task {
		delete(locks, workspace)
	}
}

func (locks workspaceTaskLocks) clearAll() {
	for workspace := range locks {
		delete(locks, workspace)
	}
}

func normalizeWorkspaceLockPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return filepath.Clean(path)
}

func sameWorkspaceLockPath(a string, b string) bool {
	a = normalizeWorkspaceLockPath(a)
	b = normalizeWorkspaceLockPath(b)
	return a != "" && b != "" && a == b
}
