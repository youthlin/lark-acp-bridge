package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const traceDirName = "traces"

var traceFileMaxBytes int64 = 10 * 1024 * 1024

var traceFileSafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

const traceTimestampLayout = "2006-01-02T15:04:05.000000000-07:00"

type traceTimestamp time.Time

func (t traceTimestamp) IsZero() bool {
	return time.Time(t).IsZero()
}

func (t traceTimestamp) MarshalJSON() ([]byte, error) {
	ts := time.Time(t)
	if ts.IsZero() {
		return json.Marshal("")
	}
	return json.Marshal(ts.Format(traceTimestampLayout))
}

type traceStore struct {
	dir           string
	retention     time.Duration
	mu            sync.Mutex
	lastPrunedDay string
}

type traceRecord struct {
	TS            traceTimestamp         `json:"ts"`
	Type          string                 `json:"type"`
	IsFinal       bool                   `json:"is_final,omitempty"`
	BotID         string                 `json:"bot_id,omitempty"`
	Source        string                 `json:"source,omitempty"`
	MainID        string                 `json:"main_id,omitempty"`
	SubID         string                 `json:"sub_id,omitempty"`
	SessionID     string                 `json:"session_id,omitempty"`
	MessageID     string                 `json:"message_id,omitempty"`
	AgentName     string                 `json:"agent_name,omitempty"`
	Cwd           string                 `json:"cwd,omitempty"`
	Content       string                 `json:"content,omitempty"`
	ToolCallID    string                 `json:"tool_call_id,omitempty"`
	Name          string                 `json:"name,omitempty"`
	Kind          string                 `json:"kind,omitempty"`
	Status        string                 `json:"status,omitempty"`
	Input         json.RawMessage        `json:"input,omitempty"`
	Output        json.RawMessage        `json:"output,omitempty"`
	Entries       []acp.PlanEntry        `json:"entries,omitempty"`
	Used          int64                  `json:"used,omitempty"`
	Size          int64                  `json:"size,omitempty"`
	Cost          *acp.UsageCost         `json:"cost,omitempty"`
	UpdateKind    string                 `json:"update_kind,omitempty"`
	StopReason    string                 `json:"stop_reason,omitempty"`
	Usage         acp.TokenUsage         `json:"usage,omitzero"`
	TurnUsage     acp.TokenUsage         `json:"turn_usage,omitzero"`
	SessionUsage  acp.TokenUsage         `json:"session_usage,omitzero"`
	ContextWindow acp.ContextWindowUsage `json:"context_window,omitzero"`
	RawUpdate     json.RawMessage        `json:"raw_update,omitempty"`
	RawResult     json.RawMessage        `json:"raw_result,omitempty"`
	Error         string                 `json:"error,omitempty"`
	Interrupted   bool                   `json:"interrupted,omitempty"`
}

func newTraceStore(workspace string, cfg config.TraceConfig) *traceStore {
	workspace = strings.TrimSpace(workspace)
	cfg = effectiveTraceConfig(cfg)
	if workspace == "" || !cfg.Enabled {
		return nil
	}
	if expanded, err := config.ExpandPath(workspace); err == nil {
		workspace = expanded
	} else {
		slog.Warn("展开 ACP trace workspace 失败", "workspace", workspace, "错误", err)
	}
	return &traceStore{
		dir:       workspaceLocalPath(workspace, traceDirName),
		retention: time.Duration(cfg.RetentionDays) * 24 * time.Hour,
	}
}

func effectiveTraceConfig(cfg config.TraceConfig) config.TraceConfig {
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 7
	}
	if cfg.Disabled {
		cfg.Enabled = false
		return cfg
	}
	if !cfg.Enabled {
		cfg.Enabled = true
	}
	return cfg
}

func (s *traceStore) Append(session Session, record traceRecord) error {
	if s == nil {
		return nil
	}
	record = normalizeTraceRecord(session, record)
	if strings.TrimSpace(record.Type) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("创建 trace 目录: %w", err)
	}
	s.pruneLocked(time.Now())
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("编码 trace 记录: %w", err)
	}
	data = append(data, '\n')
	path := s.sessionPath(session)
	compacted, err := compactTraceFileIfNeeded(path, int64(len(data)))
	if err != nil {
		slog.Warn("压缩 ACP trace 文件失败", "path", path, "错误", err)
	}
	if compacted && !traceRecordKeptAfterCompaction(record) {
		slog.Debug("跳过触发压缩的非摘要 ACP trace 记录", "path", path, "type", record.Type, "message_id", record.MessageID)
		return nil
	}
	if !traceRecordKeptAfterCompaction(record) && traceFileWouldExceed(path, int64(len(data))) {
		slog.Debug("跳过非摘要 ACP trace 记录以限制文件大小", "path", path, "type", record.Type, "message_id", record.MessageID)
		return nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("打开 trace 文件: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("写入 trace 文件: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭 trace 文件: %w", err)
	}
	return nil
}

func compactTraceFileIfNeeded(path string, incomingBytes int64) (bool, error) {
	if !traceFileWouldExceed(path, incomingBytes) {
		return false, nil
	}
	return true, compactTraceFileToSummary(path)
}

func traceFileWouldExceed(path string, incomingBytes int64) bool {
	if traceFileMaxBytes <= 0 {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Size()+incomingBytes > traceFileMaxBytes
}

func compactTraceFileToSummary(path string) error {
	in, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(path), ".trace-compact-*.jsonl")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	reader := bufio.NewReader(in)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 && traceLineKeptAfterCompaction(line) {
			if _, err := tmp.Write(line); err != nil {
				_ = tmp.Close()
				return err
			}
			if len(line) == 0 || line[len(line)-1] != '\n' {
				if _, err := tmp.Write([]byte{'\n'}); err != nil {
					_ = tmp.Close()
					return err
				}
			}
		}
		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		_ = tmp.Close()
		return readErr
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func traceLineKeptAfterCompaction(line []byte) bool {
	var record struct {
		Type    string `json:"type"`
		IsFinal bool   `json:"is_final,omitempty"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &record); err != nil {
		return false
	}
	return traceRecordKeptAfterCompaction(traceRecord{Type: record.Type, IsFinal: record.IsFinal})
}

func traceRecordKeptAfterCompaction(record traceRecord) bool {
	switch strings.TrimSpace(record.Type) {
	case "user":
		return true
	case "assistant":
		return record.IsFinal
	default:
		return false
	}
}

func (s *traceStore) pruneLocked(now time.Time) {
	if s == nil || s.retention <= 0 || now.IsZero() {
		return
	}
	day := now.Format("2006-01-02")
	if s.lastPrunedDay == day {
		return
	}
	s.lastPrunedDay = day
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-s.retention)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
	}
}

func (s *traceStore) sessionPath(session Session) string {
	sessionID := strings.TrimSpace(session.ACPSessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(session.Title)
	}
	if sessionID == "" {
		sessionID = session.Key.BotID + "-" + session.Key.MainID + "-" + session.Key.SubID
	}
	return filepath.Join(s.dir, traceSafeFileName(sessionID)+".jsonl")
}

func normalizeTraceRecord(session Session, record traceRecord) traceRecord {
	if record.TS.IsZero() {
		record.TS = traceTimestamp(time.Now())
	}
	record.Type = strings.TrimSpace(record.Type)
	record.BotID = strings.TrimSpace(firstNonEmpty(record.BotID, session.Key.BotID))
	record.Source = strings.TrimSpace(firstNonEmpty(record.Source, session.Key.Source))
	record.MainID = strings.TrimSpace(firstNonEmpty(record.MainID, session.Key.MainID, session.Key.ChatID))
	record.SubID = strings.TrimSpace(firstNonEmpty(record.SubID, session.Key.SubID))
	record.SessionID = strings.TrimSpace(firstNonEmpty(record.SessionID, session.ACPSessionID))
	record.AgentName = strings.TrimSpace(firstNonEmpty(record.AgentName, session.AgentName))
	record.Cwd = strings.TrimSpace(firstNonEmpty(record.Cwd, session.Cwd))
	record.ToolCallID = strings.TrimSpace(record.ToolCallID)
	record.Name = strings.TrimSpace(record.Name)
	record.Kind = strings.TrimSpace(record.Kind)
	record.Status = strings.TrimSpace(record.Status)
	record.UpdateKind = strings.TrimSpace(record.UpdateKind)
	record.StopReason = strings.TrimSpace(record.StopReason)
	record.Error = strings.TrimSpace(record.Error)
	return record
}

func traceSafeFileName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown-session"
	}
	value = traceFileSafeChars.ReplaceAllString(value, "_")
	value = strings.Trim(value, "._-")
	if value == "" {
		return "unknown-session"
	}
	if len(value) > 160 {
		value = value[:160]
	}
	return value
}

type traceRecorder struct {
	store             *traceStore
	session           Session
	messageID         string
	assistantMu       sync.Mutex
	assistant         strings.Builder
	finalAssistantSet bool
	toolMu            sync.Mutex
	tools             map[string]*traceToolAggregate
	toolSequence      int
	updateMu          sync.Mutex
	update            *traceUpdateAggregate
}

type traceToolAggregate struct {
	id     string
	name   string
	kind   string
	status string
	input  json.RawMessage
	output json.RawMessage
	raw    json.RawMessage
	rawSet bool
}

type traceUpdateAggregate struct {
	key    string
	record traceRecord
}

func newTraceRecorder(store *traceStore, session Session, prompt string) *traceRecorder {
	return newTraceRecorderWithMessageID(store, session, prompt, "")
}

func newTraceRecorderWithMessageID(store *traceStore, session Session, prompt string, messageID string) *traceRecorder {
	if store == nil {
		return nil
	}
	recorder := &traceRecorder{
		store:     store,
		session:   session,
		messageID: strings.TrimSpace(messageID),
		tools:     make(map[string]*traceToolAggregate),
	}
	recorder.append(traceRecord{Type: "user", Content: prompt})
	return recorder
}

func (s *Service) traceStoreForSession(session Session) *traceStore {
	if s == nil {
		return nil
	}
	s.traceStoreMu.RLock()
	defer s.traceStoreMu.RUnlock()
	if s.traceStores == nil {
		return nil
	}
	botID := strings.TrimSpace(session.Key.BotID)
	if store, ok := s.traceStores[botID]; ok {
		return store
	}
	return s.traceStores[""]
}

func (s *Service) setTraceStore(botID string, store *traceStore) {
	if s == nil {
		return
	}
	s.traceStoreMu.Lock()
	defer s.traceStoreMu.Unlock()
	if s.traceStores == nil {
		s.traceStores = make(map[string]*traceStore)
	}
	s.traceStores[strings.TrimSpace(botID)] = store
}

func (s *Service) newTraceRecorder(session Session, prompt string) *traceRecorder {
	return newTraceRecorder(s.traceStoreForSession(session), session, prompt)
}

func (s *Service) newTraceRecorderForMessage(session Session, msg feishu.Message, prompt string) *traceRecorder {
	return newTraceRecorderWithMessageID(s.traceStoreForSession(session), session, prompt, msg.MessageID)
}

func (s *Service) newTraceRecorderWithMessageID(session Session, prompt string, messageID string) *traceRecorder {
	return newTraceRecorderWithMessageID(s.traceStoreForSession(session), session, prompt, messageID)
}

func traceMessageID(prefix string, parts ...string) string {
	prefix = traceSafeFileName(prefix)
	if prefix == "" || prefix == "unknown-session" {
		prefix = "trace"
	}
	values := []string{prefix}
	for _, part := range parts {
		part = traceSafeFileName(part)
		if part != "" && part != "unknown-session" {
			values = append(values, part)
		}
	}
	return strings.Join(values, "_")
}

func tracePromptOptions(recorder *traceRecorder, opts acp.PromptOptions) acp.PromptOptions {
	if recorder == nil {
		return opts
	}
	onUpdate := opts.OnUpdate
	opts.OnUpdate = func(update acp.PromptUpdate) {
		recorder.OnUpdate(update)
		if onUpdate != nil {
			onUpdate(update)
		}
	}
	return opts
}

func (r *traceRecorder) OnUpdate(update acp.PromptUpdate) {
	if r == nil {
		return
	}
	u := update.Update
	kind := promptUpdateKind(update)
	if chunk, ok := promptUpdateChunk(update); ok {
		switch chunk.Target {
		case promptChunkTargetText:
			r.flushProcessUpdates()
			r.appendAssistant(chunk.Text)
		case promptChunkTargetThought:
			r.recordProcessChunk("thought", kind, u, chunk.Text)
		case promptChunkTargetPlan:
			r.flushAssistant()
			r.recordProcessChunk("plan", kind, u, chunk.Text)
		case promptChunkTargetTool:
			r.flushAssistant()
			r.flushProcessUpdates()
			r.recordToolChunk(kind, u, chunk.Text)
		default:
			if chunk.FinalBoundary || isFinalTextBoundaryUpdateKind(kind) {
				r.flushAssistant()
			}
			r.recordProcessChunk(traceRecordTypeForUpdate(kind), kind, u, chunk.Text)
		}
		return
	}
	if isToolPromptUpdateKind(kind) {
		r.flushAssistant()
		r.flushProcessUpdates()
		r.recordToolUpdate(kind, u)
		return
	}
	switch kind {
	case "agent_message", "assistant_message", "message":
		text := traceFirstNonBlank(u.Message, traceContentText(u.Content), traceRawText(u.Raw))
		if text != "" {
			r.flushProcessUpdates()
			r.appendAssistant(text)
		}
		if text == "" || traceContentIsNonText(u.Content) {
			r.recordProcessUpdate("update", kind, u)
		}
	case "plan":
		r.flushAssistant()
		r.recordProcessUpdate("plan", kind, u)
	case "thought", "reasoning":
		r.recordProcessUpdate("thought", kind, u)
	case "status", "progress":
		r.recordProcessUpdate("status", kind, u)
	case "usage_update":
		r.recordProcessUpdate("usage", kind, u)
	default:
		if shouldTraceRawUpdate(kind, u) {
			if isFinalTextBoundaryUpdateKind(kind) {
				r.flushAssistant()
			}
			r.recordProcessUpdate(traceRecordTypeForUpdate(kind), kind, u)
		}
	}
}

func (r *traceRecorder) Complete(result acp.PromptResult, err error) {
	if r == nil {
		return
	}
	r.flushProcessUpdates()
	r.flushTools()
	r.flushFinalAssistant()
	if !r.hasFinalAssistant() && strings.TrimSpace(result.Text) != "" {
		r.markFinalAssistant()
		r.append(traceRecord{Type: "assistant", IsFinal: true, Content: result.Text})
	}
	if err != nil {
		r.append(traceRecord{Type: "error", Error: err.Error()})
	}
	if promptResultHasUsageDetail(result) || len(result.Raw) > 0 || strings.TrimSpace(result.StopReason) != "" {
		r.append(traceTurnResultRecord(result))
	}
}

func (r *traceRecorder) Interrupted(reason string) {
	if r == nil {
		return
	}
	r.flushProcessUpdates()
	r.flushTools()
	r.flushAssistant()
	r.append(traceRecord{Type: "error", Error: reason, Interrupted: true})
}

func (r *traceRecorder) recordToolChunk(kind string, u acp.SessionUpdate, text string) {
	if text == "" {
		return
	}
	id := strings.TrimSpace(u.ToolCallID)
	r.toolMu.Lock()
	if id == "" {
		r.toolSequence++
		id = fmt.Sprintf("tool-%d", r.toolSequence)
	}
	tool := r.tools[id]
	if tool == nil {
		tool = &traceToolAggregate{id: id}
		r.tools[id] = tool
	}
	if name := toolDisplayName(u); name != "" {
		tool.name = name
	}
	if u.Kind != "" {
		tool.kind = u.Kind
	}
	if u.Status != "" {
		tool.status = u.Status
	}
	if len(tool.output) == 0 {
		tool.output = marshalTraceValue(text)
	} else {
		tool.output = appendJSONText(tool.output, text)
	}
	if len(u.Raw) > 0 {
		if raw := compactTraceToolRawUpdate(u.Raw, nil, nil); len(raw) > 0 {
			tool.raw = raw
			tool.rawSet = true
		}
	}
	r.toolMu.Unlock()
}

func (r *traceRecorder) recordToolUpdate(kind string, u acp.SessionUpdate) {
	id := strings.TrimSpace(u.ToolCallID)
	r.toolMu.Lock()
	if id == "" {
		r.toolSequence++
		id = fmt.Sprintf("tool-%d", r.toolSequence)
	}
	tool := r.tools[id]
	if tool == nil {
		tool = &traceToolAggregate{id: id}
		r.tools[id] = tool
	}
	if name := toolDisplayName(u); name != "" {
		tool.name = name
	}
	if u.Kind != "" {
		tool.kind = u.Kind
	}
	if u.Status != "" {
		tool.status = u.Status
	}
	if len(u.RawInput) > 0 {
		tool.input = cloneTraceRaw(u.RawInput)
	}
	if len(u.RawOutput) > 0 {
		tool.output = compactTraceToolOutput(tool.input, u.RawOutput, tool.status)
	}
	if len(u.Raw) > 0 {
		raw := compactTraceToolRawUpdate(u.Raw, u.RawInput, u.RawOutput)
		if len(raw) > 0 {
			tool.raw = raw
			tool.rawSet = true
		}
	}
	if strings.Contains(kind, "output") && len(tool.output) == 0 {
		if text := traceFirstNonBlank(u.Message, traceContentText(u.Content), traceRawText(u.Raw)); text != "" {
			tool.output = marshalTraceValue(text)
		}
	}
	complete := toolTraceComplete(kind, tool.status)
	if complete {
		delete(r.tools, id)
	}
	record := traceToolRecord(tool)
	r.toolMu.Unlock()
	if complete {
		r.append(record)
	}
}

func (r *traceRecorder) flushTools() {
	if r == nil {
		return
	}
	r.toolMu.Lock()
	tools := make([]*traceToolAggregate, 0, len(r.tools))
	for _, tool := range r.tools {
		tools = append(tools, tool)
	}
	r.tools = make(map[string]*traceToolAggregate)
	r.toolMu.Unlock()
	for _, tool := range tools {
		r.append(traceToolRecord(tool))
	}
}

func traceToolRecord(tool *traceToolAggregate) traceRecord {
	if tool == nil {
		return traceRecord{Type: "tool"}
	}
	record := traceRecord{
		Type:       "tool",
		ToolCallID: tool.id,
		Name:       tool.name,
		Kind:       tool.kind,
		Status:     tool.status,
		Input:      append(json.RawMessage(nil), tool.input...),
		Output:     compactTraceToolOutput(tool.input, tool.output, tool.status),
	}
	if tool.rawSet {
		record.RawUpdate = append(json.RawMessage(nil), tool.raw...)
	}
	return record
}

func traceTurnResultRecord(result acp.PromptResult) traceRecord {
	record := traceRecord{
		Type:       "turn_result",
		StopReason: result.StopReason,
		Usage:      result.Usage,
	}
	if tokenUsage := result.Meta.TraeTokenUsage; tokenUsage != nil {
		record.TurnUsage = tokenUsage.TurnDisplay
		record.SessionUsage = tokenUsage.SessionDisplay
		record.ContextWindow = tokenUsage.ContextWindow
	}
	if raw := compactTracePromptRawResult(result.Raw); len(raw) > 0 {
		record.RawResult = raw
	}
	return record
}

func compactTraceToolOutput(input json.RawMessage, output json.RawMessage, status string) json.RawMessage {
	output = cloneTraceRaw(output)
	if len(output) == 0 {
		return nil
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(output, &out); err != nil {
		return output
	}
	var in map[string]json.RawMessage
	_ = json.Unmarshal(input, &in)
	for key, value := range out {
		if inValue, ok := in[key]; ok && rawMessageEqual(value, inValue) {
			delete(out, key)
		}
	}
	if outputStatus, ok := rawStringField(out, "status"); ok && outputStatus == strings.TrimSpace(status) {
		delete(out, "status")
	}
	compactTraceToolOutputTextFields(out)
	return marshalTraceRawObject(out)
}

func compactTraceToolOutputTextFields(out map[string]json.RawMessage) {
	if len(out) == 0 {
		return
	}
	stdout, stdoutOK := rawStringField(out, "stdout")
	stderr, stderrOK := rawStringField(out, "stderr")
	combined := ""
	if stdoutOK {
		combined += stdout
	}
	if stderrOK {
		combined += stderr
	}
	aggregated, aggregatedOK := rawStringField(out, "aggregated_output")
	if aggregatedOK && ((stdoutOK && stderrOK && aggregated == combined) || (stdoutOK && aggregated == stdout && (!stderrOK || stderr == "")) || (stderrOK && aggregated == stderr && (!stdoutOK || stdout == ""))) {
		delete(out, "aggregated_output")
	}
	if formatted, ok := rawStringField(out, "formatted_output"); ok {
		switch {
		case aggregatedOK && formatted == aggregated:
			delete(out, "formatted_output")
		case (stdoutOK || stderrOK) && formatted == combined:
			delete(out, "formatted_output")
		case stdoutOK && formatted == stdout && (!stderrOK || stderr == ""):
			delete(out, "formatted_output")
		case stderrOK && formatted == stderr && (!stdoutOK || stdout == ""):
			delete(out, "formatted_output")
		}
	}
	dropEmptyRawStringField(out, "stdout")
	dropEmptyRawStringField(out, "stderr")
	dropEmptyRawStringField(out, "aggregated_output")
	dropEmptyRawStringField(out, "formatted_output")
}

func compactTraceToolRawUpdate(raw json.RawMessage, rawInput json.RawMessage, rawOutput json.RawMessage) json.RawMessage {
	raw = cloneTraceRaw(raw)
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	for _, key := range []string{"sessionUpdate", "toolCallId", "name", "title", "kind", "status"} {
		delete(fields, key)
	}
	if value, ok := fields["rawInput"]; ok && (traceRawIsEmpty(value) || (len(rawInput) > 0 && rawMessageEqual(value, rawInput))) {
		delete(fields, "rawInput")
	}
	if value, ok := fields["rawOutput"]; ok && (traceRawIsEmpty(value) || (len(rawOutput) > 0 && rawMessageEqual(value, rawOutput))) {
		delete(fields, "rawOutput")
	}
	return marshalTraceRawObject(fields)
}

func compactTracePromptRawResult(raw json.RawMessage) json.RawMessage {
	raw = cloneTraceRaw(raw)
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	delete(fields, "stopReason")
	delete(fields, "usage")
	if metaRaw, ok := fields["_meta"]; ok {
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(metaRaw, &meta); err == nil {
			delete(meta, "_trae/tokenUsage")
			if compacted := marshalTraceRawObject(meta); len(compacted) > 0 {
				fields["_meta"] = compacted
			} else {
				delete(fields, "_meta")
			}
		}
	}
	return marshalTraceRawObject(fields)
}

func cloneTraceRaw(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if traceRawIsEmpty(raw) {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func traceRawIsEmpty(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

func marshalTraceRawObject(fields map[string]json.RawMessage) json.RawMessage {
	if len(fields) == 0 {
		return nil
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return data
}

func rawStringField(fields map[string]json.RawMessage, key string) (string, bool) {
	if fields == nil {
		return "", false
	}
	raw, ok := fields[key]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func dropEmptyRawStringField(fields map[string]json.RawMessage, key string) {
	if value, ok := rawStringField(fields, key); ok && value == "" {
		delete(fields, key)
	}
}

func rawMessageEqual(a json.RawMessage, b json.RawMessage) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	var av any
	var bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return string(a) == string(b)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return string(a) == string(b)
	}
	return reflect.DeepEqual(av, bv)
}

func (r *traceRecorder) recordProcessChunk(recordType string, kind string, u acp.SessionUpdate, text string) {
	if text == "" {
		return
	}
	key := traceUpdateKey(recordType, kind)
	var previous *traceUpdateAggregate
	r.updateMu.Lock()
	if r.update == nil || r.update.key != key {
		previous = r.update
		record := traceProcessRecord(recordType, kind, u)
		record.Content = ""
		r.update = &traceUpdateAggregate{key: key, record: record}
	}
	r.update.record.Content += text
	if r.update.record.RawUpdate == nil && len(u.Raw) > 0 && !isPromptChunkKind(kind) {
		r.update.record.RawUpdate = append(json.RawMessage(nil), u.Raw...)
	}
	r.updateMu.Unlock()
	r.appendProcessAggregate(previous)
}

func (r *traceRecorder) recordProcessUpdate(recordType string, kind string, u acp.SessionUpdate) {
	r.flushProcessUpdates()
	record := traceProcessRecord(recordType, kind, u)
	if strings.TrimSpace(record.Content) == "" && len(record.Entries) == 0 && len(record.RawUpdate) == 0 && record.Status == "" && record.Used == 0 && record.Size == 0 && record.Cost == nil {
		return
	}
	if isPromptChunkKind(kind) {
		r.recordProcessChunk(recordType, kind, u, record.Content)
		return
	}
	r.append(record)
}

func (r *traceRecorder) flushProcessUpdates() {
	if r == nil {
		return
	}
	var update *traceUpdateAggregate
	r.updateMu.Lock()
	update = r.update
	r.update = nil
	r.updateMu.Unlock()
	r.appendProcessAggregate(update)
}

func (r *traceRecorder) appendProcessAggregate(update *traceUpdateAggregate) {
	if update == nil {
		return
	}
	record := update.record
	if strings.TrimSpace(record.Content) == "" && len(record.Entries) == 0 && len(record.RawUpdate) == 0 && record.Status == "" && record.Used == 0 && record.Size == 0 && record.Cost == nil {
		return
	}
	r.append(record)
}

func traceProcessRecord(recordType string, kind string, u acp.SessionUpdate) traceRecord {
	record := traceRecord{
		Type:       strings.TrimSpace(recordType),
		UpdateKind: strings.TrimSpace(kind),
		Name:       traceFirstNonBlank(u.Name, rawName(u.Raw)),
		Status:     strings.TrimSpace(u.Status),
		Content:    traceFirstNonBlank(u.Message, traceContentText(u.Content), tracePlanUpdateText(u), traceRawText(u.Raw)),
		Used:       u.Used,
		Size:       u.Size,
		Cost:       u.Cost,
	}
	if len(u.PlanEntries) > 0 {
		record.Entries = append([]acp.PlanEntry(nil), u.PlanEntries...)
	}
	if len(u.Raw) > 0 && !isPromptChunkKind(kind) {
		record.RawUpdate = append(json.RawMessage(nil), u.Raw...)
	}
	return record
}

func traceRecordTypeForUpdate(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch {
	case isThoughtUpdateKind(kind):
		return "thought"
	case isPlanUpdateKind(kind):
		return "plan"
	case kind == "status" || kind == "progress" || promptUpdateKindHasToken(kind, "status") || promptUpdateKindHasToken(kind, "progress"):
		return "status"
	case kind == "usage_update":
		return "usage"
	case kind == "agent_message" || kind == "assistant_message" || kind == "message" || strings.Contains(kind, "message"):
		return "assistant"
	default:
		return "update"
	}
}

func shouldTraceRawUpdate(kind string, u acp.SessionUpdate) bool {
	if strings.TrimSpace(kind) == "" {
		return len(u.Raw) > 0
	}
	if kind == "usage_update" {
		return true
	}
	if isACPStateUpdate(u) {
		return false
	}
	return len(u.Raw) > 0 || strings.TrimSpace(u.Message) != "" || u.Content != nil
}

func (r *traceRecorder) appendAssistant(text string) {
	if r == nil || text == "" {
		return
	}
	r.assistantMu.Lock()
	r.assistant.WriteString(text)
	r.assistantMu.Unlock()
}

func (r *traceRecorder) flushAssistant() {
	r.flushAssistantAs("assistant", false)
}

func (r *traceRecorder) flushFinalAssistant() {
	r.flushAssistantAs("assistant", true)
}

func (r *traceRecorder) flushAssistantAs(recordType string, isFinal bool) {
	if r == nil {
		return
	}
	recordType = strings.TrimSpace(recordType)
	if recordType == "" {
		recordType = "assistant"
	}
	r.assistantMu.Lock()
	content := r.assistant.String()
	if strings.TrimSpace(content) != "" {
		r.assistant.Reset()
		if isFinal {
			r.finalAssistantSet = true
		}
	} else {
		r.assistant.Reset()
		content = ""
	}
	r.assistantMu.Unlock()
	if content != "" {
		r.append(traceRecord{Type: recordType, IsFinal: isFinal, Content: content})
	}
}

func (r *traceRecorder) markFinalAssistant() {
	if r == nil {
		return
	}
	r.assistantMu.Lock()
	defer r.assistantMu.Unlock()
	r.finalAssistantSet = true
}

func (r *traceRecorder) hasFinalAssistant() bool {
	if r == nil {
		return false
	}
	r.assistantMu.Lock()
	defer r.assistantMu.Unlock()
	return r.finalAssistantSet || strings.TrimSpace(r.assistant.String()) != ""
}

func traceUpdateKey(recordType string, kind string) string {
	return strings.TrimSpace(recordType) + ":" + strings.TrimSpace(kind)
}

func appendJSONText(raw json.RawMessage, text string) json.RawMessage {
	var current string
	if err := json.Unmarshal(raw, &current); err == nil {
		return marshalTraceValue(current + text)
	}
	return raw
}

func traceFirstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func traceContentText(content *acp.ContentBlock) string {
	if content == nil {
		return ""
	}
	if content.Type != "" && content.Type != "text" && content.Type != "output_text" {
		return ""
	}
	return content.Text
}

func traceContentIsNonText(content *acp.ContentBlock) bool {
	if content == nil {
		return false
	}
	return content.Type != "" && content.Type != "text" && content.Type != "output_text"
}

func tracePlanUpdateText(u acp.SessionUpdate) string {
	if len(u.PlanEntries) == 0 {
		return ""
	}
	lines := make([]string, 0, len(u.PlanEntries))
	for _, entry := range u.PlanEntries {
		content := traceFirstNonBlank(entry.ActiveForm, metaString(entry.Meta, "activeForm"), entry.Content)
		if strings.TrimSpace(content) == "" {
			continue
		}
		lines = append(lines, "- "+planStatusIcon(entry.Status)+" "+content)
	}
	return strings.Join(lines, "\n")
}

func traceRawText(raw json.RawMessage) string {
	return rawTextValue(rawValue(raw))
}

func toolTraceComplete(kind string, status string) bool {
	if strings.Contains(kind, "output") || strings.Contains(kind, "error") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "complete", "success", "succeeded", "done", "failed", "failure", "error":
		return true
	default:
		return false
	}
}

func marshalTraceValue(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
}

func (r *traceRecorder) append(record traceRecord) {
	if r == nil || r.store == nil {
		return
	}
	if strings.TrimSpace(record.MessageID) == "" {
		record.MessageID = r.messageID
	}
	if err := r.store.Append(r.session, record); err != nil {
		slog.Warn("写入 ACP trace 失败", "session", r.session.ACPSessionID, "错误", err)
	}
}
