package bridge

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	forkBundleVersion      = 1
	forkToolInputMaxRunes  = 2000
	forkToolOutputMaxRunes = 4000
)

var errNoCompletedForkTurn = errors.New("当前没有已正常结束、可供分叉的消息")

type forkManifest struct {
	Version               int        `json:"version"`
	ForkID                string     `json:"fork_id"`
	SourceSessionID       string     `json:"source_session_id"`
	SourceSessionKey      SessionKey `json:"source_session_key"`
	SourceTitle           string     `json:"source_title,omitempty"`
	SourceTracePath       string     `json:"source_trace_path"`
	SourceSnapshotSeq     uint64     `json:"source_snapshot_seq"`
	SourceCutoffSeq       uint64     `json:"source_cutoff_seq"`
	SourceCutoffMessageID string     `json:"source_cutoff_message_id,omitempty"`
	ForkCommandMessageID  string     `json:"fork_command_message_id,omitempty"`
	Forced                bool       `json:"forced,omitempty"`
	CreatedBy             string     `json:"created_by,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
}

type forkTraceSnapshot struct {
	SnapshotSeq     uint64
	CutoffSeq       uint64
	CutoffMessageID string
	LastUserText    string
	Records         []traceRecord
}

type forkBundle struct {
	ID           string
	Dir          string
	ManifestPath string
	ContextPath  string
	Manifest     forkManifest
}

func (s *traceStore) snapshotSeqLocked(session Session) (uint64, error) {
	path := s.sessionPath(session)
	if next, ok := s.nextSeq[path]; ok && next > 0 {
		return next - 1, nil
	}
	seq, err := traceFileMaxSeq(path)
	if err != nil {
		return 0, fmt.Errorf("读取源 trace 高水位: %w", err)
	}
	return seq, nil
}

func readForkTraceSnapshot(path string, snapshotSeq uint64) (forkTraceSnapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return forkTraceSnapshot{}, fmt.Errorf("源会话 trace 不存在")
		}
		return forkTraceSnapshot{}, fmt.Errorf("打开源会话 trace: %w", err)
	}
	defer file.Close()

	var records []traceRecord
	completed := make(map[string]bool)
	users := make(map[string]string)
	lineNumber := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		lineNumber++
		var record traceRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return forkTraceSnapshot{}, fmt.Errorf("解析源 trace 第 %d 行: %w", lineNumber, err)
		}
		if record.Seq == 0 || record.Seq > snapshotSeq || forkTraceRecordIsBackground(record) {
			continue
		}
		record.MessageID = strings.TrimSpace(record.MessageID)
		if record.MessageID == "" {
			continue
		}
		if record.Type == "user" {
			record.Content = forkUserContent(record.Content)
		}
		records = append(records, record)
		if record.Type == "user" {
			users[record.MessageID] = record.Content
		}
		if record.Type == "turn_result" && users[record.MessageID] != "" {
			completed[record.MessageID] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return forkTraceSnapshot{}, fmt.Errorf("读取源会话 trace: %w", err)
	}

	snapshot := forkTraceSnapshot{SnapshotSeq: snapshotSeq}
	for _, record := range records {
		if record.Type != "turn_result" || !completed[record.MessageID] {
			continue
		}
		if record.Seq >= snapshot.CutoffSeq {
			snapshot.CutoffSeq = record.Seq
			snapshot.CutoffMessageID = record.MessageID
			snapshot.LastUserText = users[record.MessageID]
		}
	}
	if snapshot.CutoffSeq == 0 {
		return forkTraceSnapshot{SnapshotSeq: snapshotSeq}, errNoCompletedForkTurn
	}
	for _, record := range records {
		if record.Seq > snapshot.CutoffSeq || !completed[record.MessageID] || !forkTraceRecordKept(record) {
			continue
		}
		snapshot.Records = append(snapshot.Records, sanitizeForkTraceRecord(record))
	}
	return snapshot, nil
}

func forkTraceRecordIsBackground(record traceRecord) bool {
	return strings.EqualFold(strings.TrimSpace(record.Source), "wiki") ||
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(record.MessageID)), "wiki_")
}

func forkTraceRecordKept(record traceRecord) bool {
	switch strings.TrimSpace(record.Type) {
	case "user", "turn_result":
		return true
	case "assistant":
		return record.IsFinal
	case "tool":
		return true
	default:
		return false
	}
}

func sanitizeForkTraceRecord(record traceRecord) traceRecord {
	record.Content = redactSensitiveValuesForDisplay(record.Content)
	record.Error = redactSensitiveValuesForDisplay(record.Error)
	record.Cause = redactSensitiveValuesForDisplay(record.Cause)
	record.Input = sanitizeForkJSON(record.Input, forkToolInputMaxRunes)
	record.Output = sanitizeForkJSON(record.Output, forkToolOutputMaxRunes)
	record.RawUpdate = nil
	record.RawResult = nil
	return record
}

func sanitizeForkJSON(raw json.RawMessage, maxRunes int) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return marshalTraceValue(truncateRunes(redactSensitiveValuesForDisplay(string(raw)), maxRunes))
	}
	value = sanitizeForkJSONValue(value, maxRunes)
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	sanitized := redactSensitiveValuesForDisplay(string(data))
	if len([]rune(sanitized)) <= maxRunes && json.Valid([]byte(sanitized)) {
		return json.RawMessage(sanitized)
	}
	return marshalTraceValue(truncateRunes(sanitized, maxRunes))
}

func sanitizeForkJSONValue(value any, maxRunes int) any {
	switch value := value.(type) {
	case string:
		return truncateRunes(redactSensitiveValuesForDisplay(value), maxRunes)
	case []any:
		for i := range value {
			value[i] = sanitizeForkJSONValue(value[i], maxRunes)
		}
		return value
	case map[string]any:
		for key, item := range value {
			value[key] = sanitizeForkJSONValue(item, maxRunes)
		}
		return value
	default:
		return value
	}
}

func writeForkBundle(workspace string, manifest forkManifest, records []traceRecord) (forkBundle, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return forkBundle{}, fmt.Errorf("当前会话没有 workspace，无法保存分支上下文")
	}
	if manifest.ForkID == "" {
		manifest.ForkID = newForkID()
	}
	manifest.Version = forkBundleVersion
	if manifest.CreatedAt.IsZero() {
		manifest.CreatedAt = time.Now()
	}
	dir := workspaceLocalPath(workspace, "forks", manifest.ForkID)
	manifestPath := filepath.Join(dir, "manifest.json")
	contextPath := filepath.Join(dir, "context.jsonl")
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return forkBundle{}, fmt.Errorf("编码 session fork manifest: %w", err)
	}
	var contextData strings.Builder
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			return forkBundle{}, fmt.Errorf("编码 session fork context: %w", err)
		}
		contextData.Write(data)
		contextData.WriteByte('\n')
	}
	if err := writePrivateFileAtomic(contextPath, []byte(contextData.String())); err != nil {
		return forkBundle{}, fmt.Errorf("写入 session fork context: %w", err)
	}
	if err := writePrivateFileAtomic(manifestPath, append(manifestData, '\n')); err != nil {
		return forkBundle{}, fmt.Errorf("写入 session fork manifest: %w", err)
	}
	return forkBundle{ID: manifest.ForkID, Dir: dir, ManifestPath: manifestPath, ContextPath: contextPath, Manifest: manifest}, nil
}

func newForkID() string {
	var value [8]byte
	if _, err := rand.Read(value[:]); err == nil {
		return "fork_" + hex.EncodeToString(value[:])
	}
	return "fork_" + time.Now().UTC().Format("20060102T150405.000000000Z")
}

func forkUserSummary(text string) string {
	text = strings.Join(strings.Fields(redactSensitiveValuesForDisplay(text)), " ")
	const limit = 60
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-3]) + "..."
}

func forkUserContent(text string) string {
	const heading = "## User Message\n"
	if index := strings.Index(text, heading); index >= 0 && (index == 0 || strings.HasSuffix(text[:index], "\n\n")) {
		text = text[index+len(heading):]
	}
	lines := strings.Split(text, "\n")
	out := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "image_key:") || strings.HasPrefix(trimmed, "local_path:") {
			continue
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
