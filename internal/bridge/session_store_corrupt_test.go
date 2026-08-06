package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionStoreLoadCorruptFileBacksUpAndStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "sessions.json")
	corrupt := []byte("{this is not valid json")

	store := NewSessionStore(storePath)
	// 预置内存状态并写出正常文件，确认损坏加载后内存会被清空。
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	if _, err := store.UpsertWithDefaultTitle(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-stale",
		Cwd:          "/repo",
	}); err != nil {
		t.Fatalf("UpsertWithDefaultTitle() error = %v", err)
	}
	// 把文件改坏，模拟磁盘损坏/写入中断。
	if err := os.WriteFile(storePath, corrupt, 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt) error = %v", err)
	}

	if err := store.Load(); err != nil {
		t.Fatalf("Load() error = %v, want nil with empty store on corrupt file", err)
	}
	if store.Count() != 0 {
		t.Fatalf("Count() = %d, want empty store after corrupt load", store.Count())
	}

	// 原损坏文件应已被移走（不再存在于原路径）。
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("original corrupt file still exists, err = %v", err)
	}
	// 应生成一个 .corrupt-* 备份，内容与原文件一致。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	var backup string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sessions.json.corrupt-") {
			backup = filepath.Join(dir, e.Name())
			break
		}
	}
	if backup == "" {
		t.Fatal("no .corrupt-* backup created")
	}
	got, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("ReadFile(backup) error = %v", err)
	}
	if string(got) != string(corrupt) {
		t.Fatalf("backup content = %q, want %q", got, corrupt)
	}

	// 空库写入应能重新生成正常文件。
	if _, err := store.UpsertWithDefaultTitle(Session{
		Key:          key,
		AgentName:    "traex",
		ACPSessionID: "acp-new",
		Cwd:          "/repo",
	}); err != nil {
		t.Fatalf("UpsertWithDefaultTitle() after recovery error = %v", err)
	}
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("sessions.json not recreated, err = %v", err)
	}
}
