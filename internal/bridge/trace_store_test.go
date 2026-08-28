package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestPromptWritesJSONLTrace(t *testing.T) {
	workspace := t.TempDir()
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
	}
	store := NewSessionStore(filepath.Join(workspace, ".local", "sessions.json"))
	if err := store.Upsert(session); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	rt := &fakeRuntime{
		promptResult: acp.PromptResult{
			Text:       "完成",
			StopReason: "end_turn",
			Usage:      acp.TokenUsage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18},
		},
		promptUpdates: []acp.PromptUpdate{
			{Update: acp.SessionUpdate{
				SessionUpdate: "agent_thought_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "先判断"},
			}},
			{Update: acp.SessionUpdate{
				SessionUpdate: "agent_thought_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "上下文"},
			}},
			{Update: acp.SessionUpdate{
				SessionUpdate: "plan",
				PlanEntries: []acp.PlanEntry{
					{Content: "运行测试", Status: "in_progress"},
				},
			}},
			{Update: acp.SessionUpdate{
				SessionUpdate: "usage_update",
				Used:          1024,
				Size:          8192,
				Raw:           json.RawMessage(`{"sessionUpdate":"usage_update","used":1024,"size":8192}`),
			}},
			{Update: acp.SessionUpdate{
				SessionUpdate: "tool_call",
				ToolCallID:    "tool-1",
				Title:         "Run tests",
				Kind:          "execute",
				Status:        "in_progress",
				RawInput:      json.RawMessage(`{"cmd":"go test ./..."}`),
				Raw:           json.RawMessage(`{"sessionUpdate":"tool_call","toolCallId":"tool-1","title":"Run tests","kind":"execute","status":"in_progress","rawInput":{"cmd":"go test ./..."}}`),
			}},
			{Update: acp.SessionUpdate{
				SessionUpdate: "tool_call_update",
				ToolCallID:    "tool-1",
				Status:        "completed",
				RawOutput:     json.RawMessage(`"ok"`),
				Raw:           json.RawMessage(`{"sessionUpdate":"tool_call_update","toolCallId":"tool-1","status":"completed","rawOutput":"ok"}`),
			}},
			{Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "完"},
			}},
			{Update: acp.SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &acp.ContentBlock{Type: "text", Text: "成"},
			}},
		},
	}
	cfg := config.Config{Bots: []config.BotConfig{{
		ID:        "bot-a",
		Workspace: workspace,
		Trace:     config.TraceConfig{Enabled: true, RetentionDays: 7},
	}}}
	svc := NewService(cfg, store)
	svc.setRuntime(rt)

	run := svc.runUserPromptWithOptionsDetailed(context.Background(), feishu.Message{
		BotID:     "bot-a",
		ChatID:    "oc_chat",
		MessageID: "om_prompt_1",
		SenderID:  testOwnerOpenID,
		Workspace: workspace,
	}, session, config.AgentConfig{}, "你好", runningTaskOptions{silentPrompt: true})
	if run.err != nil {
		t.Fatalf("runUserPromptWithOptionsDetailed() error = %v", run.err)
	}

	path := filepath.Join(workspace, ".local", "traces", "acp-session-1.jsonl")
	records := readTraceRecords(t, path)
	assertTraceRecordTimestamps(t, records)
	if got := traceRecordTypes(records); strings.Join(got, ",") != "user,thought,plan,usage,tool,assistant,turn_result" {
		t.Fatalf("record types = %v, records = %+v", got, records)
	}
	for i, record := range records {
		if record["message_id"] != "om_prompt_1" {
			t.Fatalf("record[%d] message_id = %v, want om_prompt_1; record = %+v", i, record["message_id"], record)
		}
		assertTraceRecordOmitsSessionMetadata(t, record)
	}
	if records[0]["type"] != "user" || records[0]["content"] == "" {
		t.Fatalf("first record = %+v, want user prompt", records[0])
	}
	if records[1]["type"] != "thought" || records[1]["content"] != "先判断上下文" {
		t.Fatalf("second record = %+v, want thought chunks", records[1])
	}
	if records[2]["type"] != "plan" || len(records[2]["entries"].([]any)) != 1 {
		t.Fatalf("third record = %+v, want plan entries", records[2])
	}
	if records[3]["type"] != "usage" || records[3]["used"].(float64) != 1024 || records[3]["size"].(float64) != 8192 {
		t.Fatalf("fourth record = %+v, want usage update", records[3])
	}
	if records[4]["type"] != "tool" || records[4]["tool_call_id"] != "tool-1" || records[4]["name"] != "Run tests" {
		t.Fatalf("fifth record = %+v, want tool call", records[4])
	}
	if _, ok := records[4]["raw_update"]; ok {
		t.Fatalf("tool record = %+v, want redundant raw_update omitted", records[4])
	}
	if records[5]["type"] != "assistant" || records[5]["content"] != "完成" {
		t.Fatalf("sixth record = %+v, want final assistant content", records[5])
	}
	assertTraceRecordFinal(t, records[5], true)
	if records[6]["type"] != "turn_result" || records[6]["stop_reason"] != "end_turn" {
		t.Fatalf("seventh record = %+v, want turn result", records[6])
	}
}

func TestPromptTraceRecordsMessageIDAcrossTurns(t *testing.T) {
	workspace := t.TempDir()
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
	}
	cfg := config.Config{Bots: []config.BotConfig{{
		ID:        "bot-a",
		Workspace: workspace,
		Trace:     config.TraceConfig{Enabled: true, RetentionDays: 7},
	}}}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, ".local", "sessions.json")))
	svc.setRuntime(&fakeRuntime{promptResult: acp.PromptResult{Text: "ok"}})

	for _, msgID := range []string{"om_first", "om_second"} {
		run := svc.runUserPromptWithOptionsDetailed(context.Background(), feishu.Message{
			BotID:     "bot-a",
			ChatID:    "oc_chat",
			MessageID: msgID,
			SenderID:  testOwnerOpenID,
			Workspace: workspace,
		}, session, config.AgentConfig{}, "hello "+msgID, runningTaskOptions{silentPrompt: true})
		if run.err != nil {
			t.Fatalf("runUserPromptWithOptionsDetailed(%s) error = %v", msgID, run.err)
		}
	}

	records := readTraceRecords(t, filepath.Join(workspace, ".local", "traces", "acp-session-1.jsonl"))
	if got := traceRecordTypes(records); strings.Join(got, ",") != "user,assistant,user,assistant" {
		t.Fatalf("record types = %v, records = %+v", got, records)
	}
	for i, want := range []string{"om_first", "om_first", "om_second", "om_second"} {
		if records[i]["message_id"] != want {
			t.Fatalf("record[%d] message_id = %v, want %s; record = %+v", i, records[i]["message_id"], want, records[i])
		}
	}
	assertTraceRecordFinal(t, records[1], true)
	assertTraceRecordFinal(t, records[3], true)
}

func TestTraceStoreCompactsLargeSessionFileToSummary(t *testing.T) {
	workspace := t.TempDir()
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
	}
	store := newTraceStore(workspace, config.TraceConfig{Enabled: true, RetentionDays: 7})
	oldMax := traceFileMaxBytes
	traceFileMaxBytes = 600
	t.Cleanup(func() { traceFileMaxBytes = oldMax })

	path := filepath.Join(workspace, ".local", "traces", "acp-session-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(trace dir) error = %v", err)
	}
	records := []traceRecord{
		{TS: traceTimestamp(time.Now()), Type: "user", Content: "hello"},
		{TS: traceTimestamp(time.Now()), Type: "tool", ToolCallID: "tool-1", Output: json.RawMessage(`"` + strings.Repeat("x", 800) + `"`)},
		{TS: traceTimestamp(time.Now()), Type: "assistant", Content: "working"},
		{TS: traceTimestamp(time.Now()), Type: "assistant", IsFinal: true, Content: "done"},
	}
	var data []byte
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("Marshal(%+v) error = %v", record, err)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(trace) error = %v", err)
	}
	if err := store.Append(session, traceRecord{Type: "usage", Used: 1, Size: 2}); err != nil {
		t.Fatalf("Append(usage) error = %v", err)
	}

	gotRecords := readTraceRecords(t, path)
	if got := traceRecordTypes(gotRecords); strings.Join(got, ",") != "user,assistant" {
		t.Fatalf("record types = %v, records = %+v", got, gotRecords)
	}
	if gotRecords[0]["content"] != "hello" || gotRecords[1]["content"] != "done" {
		t.Fatalf("records = %+v, want summary user and final assistant", gotRecords)
	}
	assertTraceRecordFinal(t, gotRecords[1], true)
}

func TestTraceRecordTimestampMarshalsAsFixedTS(t *testing.T) {
	data, err := json.Marshal(traceRecord{
		TS:      traceTimestamp(time.Date(2026, 8, 26, 12, 34, 56, 120000000, time.UTC)),
		Type:    "assistant",
		IsFinal: true,
		Content: "done",
	})
	if err != nil {
		t.Fatalf("Marshal trace record error = %v", err)
	}
	text := string(data)
	if !strings.Contains(text, `"ts":"2026-08-26T12:34:56.120000000+00:00"`) {
		t.Fatalf("trace record = %s, want fixed-width ts with trailing zeros", text)
	}
	if strings.Contains(text, `"timestamp"`) {
		t.Fatalf("trace record = %s, want no timestamp field", text)
	}
}

func TestTraceToolOutputCompactsRepeatedFields(t *testing.T) {
	workspace := t.TempDir()
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
	}
	cfg := config.Config{Bots: []config.BotConfig{{
		ID:        "bot-a",
		Workspace: workspace,
		Trace:     config.TraceConfig{Enabled: true, RetentionDays: 7},
	}}}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, ".local", "sessions.json")))
	recorder := svc.newTraceRecorder(session, "hello")
	rawInput := json.RawMessage(`{"call_id":"call-1","process_id":"42","command":["zsh","-c","echo ok"],"cwd":"/tmp","parsed_cmd":[{"type":"unknown","cmd":"echo ok"}],"source":"unified_exec_startup"}`)
	recorder.OnUpdate(acp.PromptUpdate{Update: acp.SessionUpdate{
		SessionUpdate: "tool_call",
		ToolCallID:    "call-1",
		Title:         "echo ok",
		Kind:          "execute",
		Status:        "in_progress",
		RawInput:      rawInput,
		Raw:           json.RawMessage(`{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"echo ok","kind":"execute","status":"in_progress","locations":[{"path":"/tmp"}],"rawInput":{"call_id":"call-1","process_id":"42","command":["zsh","-c","echo ok"],"cwd":"/tmp","parsed_cmd":[{"type":"unknown","cmd":"echo ok"}],"source":"unified_exec_startup"}}`),
	}})
	recorder.OnUpdate(acp.PromptUpdate{Update: acp.SessionUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    "call-1",
		Status:        "completed",
		RawOutput:     json.RawMessage(`{"call_id":"call-1","process_id":"42","command":["zsh","-c","echo ok"],"cwd":"/tmp","parsed_cmd":[{"type":"unknown","cmd":"echo ok"}],"source":"unified_exec_startup","stdout":"ok\n","stderr":"","aggregated_output":"ok\n","formatted_output":"ok\n","exit_code":0,"duration":{"secs":0,"nanos":1},"status":"completed"}`),
		Raw:           json.RawMessage(`{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"completed","rawOutput":{"call_id":"call-1","process_id":"42","command":["zsh","-c","echo ok"],"cwd":"/tmp","parsed_cmd":[{"type":"unknown","cmd":"echo ok"}],"source":"unified_exec_startup","stdout":"ok\n","stderr":"","aggregated_output":"ok\n","formatted_output":"ok\n","exit_code":0,"duration":{"secs":0,"nanos":1},"status":"completed"}}`),
	}})
	recorder.Complete(acp.PromptResult{}, nil)

	records := readTraceRecords(t, filepath.Join(workspace, ".local", "traces", "acp-session-1.jsonl"))
	if got := traceRecordTypes(records); strings.Join(got, ",") != "user,tool" {
		t.Fatalf("record types = %v, records = %+v", got, records)
	}
	tool := records[1]
	output, ok := tool["output"].(map[string]any)
	if !ok {
		t.Fatalf("tool output = %#v, want object", tool["output"])
	}
	for _, key := range []string{"call_id", "process_id", "command", "cwd", "parsed_cmd", "source", "status", "stderr", "aggregated_output", "formatted_output"} {
		if _, ok := output[key]; ok {
			t.Fatalf("tool output = %+v, want no repeated %q field", output, key)
		}
	}
	if output["stdout"] != "ok\n" || output["exit_code"].(float64) != 0 {
		t.Fatalf("tool output = %+v, want compact command result", output)
	}
	rawUpdate, ok := tool["raw_update"].(map[string]any)
	if !ok {
		t.Fatalf("raw_update = %#v, want locations-only object", tool["raw_update"])
	}
	if _, ok := rawUpdate["rawInput"]; ok {
		t.Fatalf("raw_update = %+v, want no rawInput", rawUpdate)
	}
	if _, ok := rawUpdate["rawOutput"]; ok {
		t.Fatalf("raw_update = %+v, want no rawOutput", rawUpdate)
	}
	if _, ok := rawUpdate["locations"]; !ok {
		t.Fatalf("raw_update = %+v, want non-promoted locations preserved", rawUpdate)
	}
}

func TestTraceTurnResultCompactsRawResult(t *testing.T) {
	workspace := t.TempDir()
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
	}
	cfg := config.Config{Bots: []config.BotConfig{{
		ID:        "bot-a",
		Workspace: workspace,
		Trace:     config.TraceConfig{Enabled: true, RetentionDays: 7},
	}}}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, ".local", "sessions.json")))
	recorder := svc.newTraceRecorder(session, "hello")
	recorder.appendAssistant("answer")
	recorder.Complete(acp.PromptResult{
		Text:       "answer",
		StopReason: "end_turn",
		Usage:      acp.TokenUsage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18},
		Meta: acp.PromptResultMeta{TraeTokenUsage: &acp.TraeTokenUsage{
			TurnDisplay:    acp.TokenUsage{InputTokens: 10, OutputTokens: 7, TotalTokens: 17},
			SessionDisplay: acp.TokenUsage{InputTokens: 20, OutputTokens: 8, TotalTokens: 28},
			ContextWindow:  acp.ContextWindowUsage{Used: 1024, Size: 8192},
		}},
		Raw: json.RawMessage(`{"stopReason":"end_turn","usage":{"inputTokens":11,"outputTokens":7,"totalTokens":18},"_meta":{"_trae/tokenUsage":{"turnDisplay":{"inputTokens":10,"outputTokens":7,"totalTokens":17},"sessionDisplay":{"inputTokens":20,"outputTokens":8,"totalTokens":28},"contextWindow":{"used":1024,"size":8192}}}}`),
	}, nil)

	records := readTraceRecords(t, filepath.Join(workspace, ".local", "traces", "acp-session-1.jsonl"))
	if got := traceRecordTypes(records); strings.Join(got, ",") != "user,assistant,turn_result" {
		t.Fatalf("record types = %v, records = %+v", got, records)
	}
	assertTraceRecordFinal(t, records[1], true)
	result := records[2]
	if result["type"] != "turn_result" || result["stop_reason"] != "end_turn" {
		t.Fatalf("turn result = %+v, want stop reason", result)
	}
	if _, ok := result["raw_result"]; ok {
		t.Fatalf("turn result = %+v, want redundant raw_result omitted", result)
	}
	if _, ok := result["turn_usage"].(map[string]any); !ok {
		t.Fatalf("turn result = %+v, want turn_usage", result)
	}
	if _, ok := result["session_usage"].(map[string]any); !ok {
		t.Fatalf("turn result = %+v, want session_usage", result)
	}
	if _, ok := result["context_window"].(map[string]any); !ok {
		t.Fatalf("turn result = %+v, want context_window", result)
	}
}

func TestTriggerPromptWritesJSONLTraceForNonIMSources(t *testing.T) {
	for _, tt := range []struct {
		name           string
		key            SessionKey
		traceMessageID string
	}{
		{
			name:           "schedule",
			key:            SessionKey{BotID: "bot-a", Source: sessionSourceSchedule, MainID: "task:daily", SubID: "run:1"},
			traceMessageID: "schedule_daily_run_1",
		},
		{
			name:           "drive_comment",
			key:            SessionKey{BotID: "bot-a", Source: sessionSourceDriveComment, MainID: "docx:token", SubID: "comment-1"},
			traceMessageID: "drive_comment_docx_token_comment-1",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			store := NewSessionStore(filepath.Join(workspace, ".local", "sessions.json"))
			cfg := config.Config{
				Bots: []config.BotConfig{{
					ID:        "bot-a",
					Workspace: workspace,
					Trace:     config.TraceConfig{Enabled: true, RetentionDays: 7},
				}},
				AgentList: []config.NamedAgentConfig{{Name: "traex", AgentConfig: config.AgentConfig{Command: "traex"}}},
			}
			svc := NewService(cfg, store)
			rt := &fakeRuntime{
				newSessionInfo: acp.SessionInfo{SessionID: "acp-" + tt.name},
				promptResult: acp.PromptResult{
					Text:       "done",
					StopReason: "end_turn",
				},
				promptUpdates: []acp.PromptUpdate{
					{Update: acp.SessionUpdate{SessionUpdate: "status", Message: "preparing"}},
					{Update: acp.SessionUpdate{
						SessionUpdate: "agent_message_chunk",
						Content:       &acp.ContentBlock{Type: "text", Text: "done"},
					}},
				},
			}
			svc.setRuntime(rt)

			result, err := svc.runTriggerPrompt(context.Background(), TriggerRequest{
				BotID:          "bot-a",
				Key:            tt.key,
				TraceMessageID: tt.traceMessageID,
				Workspace:      workspace,
				AgentName:      "traex",
				Cwd:            t.TempDir(),
				Prompt:         "run " + tt.name,
			})
			if err != nil {
				t.Fatalf("runTriggerPrompt() error = %v", err)
			}

			records := readTraceRecords(t, filepath.Join(workspace, ".local", "traces", result.Session.ACPSessionID+".jsonl"))
			if got := traceRecordTypes(records); strings.Join(got, ",") != "user,status,assistant,turn_result" {
				t.Fatalf("record types = %v, records = %+v", got, records)
			}
			if records[0]["source"] != tt.key.Source || records[0]["sub_id"] != tt.key.SubID {
				t.Fatalf("first record key fields = %+v, want trigger source key", records[0])
			}
			for i, record := range records {
				if record["message_id"] != tt.traceMessageID {
					t.Fatalf("record[%d] message_id = %v, want %s; record = %+v", i, record["message_id"], tt.traceMessageID, record)
				}
				assertTraceRecordOmitsSessionMetadata(t, record)
			}
			if records[1]["type"] != "status" || records[1]["content"] != "preparing" {
				t.Fatalf("second record = %+v, want status update", records[1])
			}
			if records[2]["type"] != "assistant" || records[2]["content"] != "done" {
				t.Fatalf("third record = %+v, want final assistant update", records[2])
			}
			assertTraceRecordFinal(t, records[2], true)
		})
	}
}

func TestNonIMTraceMessageIDBuilders(t *testing.T) {
	for _, tt := range []struct {
		name string
		got  string
		want string
	}{
		{
			name: "trigger fallback",
			got: triggerTraceMessageID(SessionKey{
				BotID:  "bot-a",
				Source: sessionSourceSchedule,
				MainID: "task:daily",
				SubID:  "run:1",
			}),
			want: "schedule_task_daily_run_1",
		},
		{
			name: "scheduled task run",
			got: scheduledTaskTraceMessageID(ScheduledTask{
				ID: "daily:report",
			}, "run:1"),
			want: "schedule_daily_report_run_1",
		},
		{
			name: "drive comment",
			got: driveCommentTraceMessageID(feishu.DriveComment{
				FileType:  "docx",
				FileToken: "tok:en",
				CommentID: "comment-1",
				ReplyID:   "reply:2",
			}),
			want: "drive_comment_docx_tok_en_comment-1_reply_2",
		},
		{
			name: "wiki",
			got: wikiTraceMessageID(Session{
				ACPSessionID: "acp:session",
			}, 3),
			want: "wiki_acp_session_generation_3",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("trace message id = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestAgentMessageUpdateWritesFinalAssistantOnly(t *testing.T) {
	workspace := t.TempDir()
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
	}
	cfg := config.Config{Bots: []config.BotConfig{{
		ID:        "bot-a",
		Workspace: workspace,
		Trace:     config.TraceConfig{Enabled: true, RetentionDays: 7},
	}}}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, ".local", "sessions.json")))
	recorder := svc.newTraceRecorder(session, "hello")
	recorder.OnUpdate(acp.PromptUpdate{Update: acp.SessionUpdate{
		SessionUpdate: "agent_message",
		Message:       "  answer with spacing  ",
		Raw:           json.RawMessage(`{"sessionUpdate":"agent_message","message":"  answer with spacing  "}`),
	}})
	recorder.Complete(acp.PromptResult{}, nil)

	records := readTraceRecords(t, filepath.Join(workspace, ".local", "traces", "acp-session-1.jsonl"))
	if got := traceRecordTypes(records); strings.Join(got, ",") != "user,assistant" {
		t.Fatalf("record types = %v, records = %+v", got, records)
	}
	assertTraceRecordFinal(t, records[1], true)
	if records[1]["content"] != "  answer with spacing  " {
		t.Fatalf("final assistant content = %q, want original spacing", records[1]["content"])
	}
	if _, ok := records[1]["raw_update"]; ok {
		t.Fatalf("final assistant record = %+v, want no duplicate raw_update", records[1])
	}
}

func TestTraceSeparatesIntermediateAssistantFromFinalAssistant(t *testing.T) {
	workspace := t.TempDir()
	session := Session{
		Key:          normalizeSessionKey(imSessionKey("bot-a", "oc_chat", "")),
		AgentName:    "traex",
		ACPSessionID: "acp-session-1",
		Cwd:          t.TempDir(),
		Workspace:    workspace,
	}
	cfg := config.Config{Bots: []config.BotConfig{{
		ID:        "bot-a",
		Workspace: workspace,
		Trace:     config.TraceConfig{Enabled: true, RetentionDays: 7},
	}}}
	svc := NewService(cfg, NewSessionStore(filepath.Join(workspace, ".local", "sessions.json")))
	recorder := svc.newTraceRecorder(session, "hello")
	recorder.OnUpdate(acp.PromptUpdate{Update: acp.SessionUpdate{
		SessionUpdate: "agent_message_chunk",
		Content:       &acp.ContentBlock{Type: "text", Text: "先执行检查"},
	}})
	recorder.OnUpdate(acp.PromptUpdate{Update: acp.SessionUpdate{
		SessionUpdate: "tool_call",
		ToolCallID:    "tool-1",
		Title:         "Run tests",
		Kind:          "execute",
		Status:        "in_progress",
		RawInput:      json.RawMessage(`{"cmd":"go test ./..."}`),
	}})
	recorder.OnUpdate(acp.PromptUpdate{Update: acp.SessionUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallID:    "tool-1",
		Status:        "completed",
		RawOutput:     json.RawMessage(`"ok"`),
	}})
	recorder.OnUpdate(acp.PromptUpdate{Update: acp.SessionUpdate{
		SessionUpdate: "agent_message_chunk",
		Content:       &acp.ContentBlock{Type: "text", Text: "最终回复"},
	}})
	recorder.Complete(acp.PromptResult{Text: "最终回复", StopReason: "end_turn"}, nil)

	records := readTraceRecords(t, filepath.Join(workspace, ".local", "traces", "acp-session-1.jsonl"))
	if got := traceRecordTypes(records); strings.Join(got, ",") != "user,assistant,tool,assistant,turn_result" {
		t.Fatalf("record types = %v, records = %+v", got, records)
	}
	if records[1]["content"] != "先执行检查" {
		t.Fatalf("intermediate assistant = %+v, want process assistant", records[1])
	}
	assertTraceRecordFinal(t, records[1], false)
	if records[3]["content"] != "最终回复" {
		t.Fatalf("final assistant = %+v, want final reply", records[3])
	}
	assertTraceRecordFinal(t, records[3], true)
}

func traceRecordTypes(records []map[string]any) []string {
	types := make([]string, 0, len(records))
	for _, record := range records {
		types = append(types, record["type"].(string))
	}
	return types
}

func readTraceRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open(%s) error = %v", path, err)
	}
	defer file.Close()
	var records []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("Unmarshal trace line %q error = %v", scanner.Text(), err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("Scan(%s) error = %v", path, err)
	}
	return records
}

func assertTraceRecordTimestamps(t *testing.T, records []map[string]any) {
	t.Helper()
	for i, record := range records {
		if _, ok := record["timestamp"]; ok {
			t.Fatalf("record[%d] = %+v, want ts field instead of timestamp", i, record)
		}
		ts, ok := record["ts"].(string)
		if !ok || strings.TrimSpace(ts) == "" {
			t.Fatalf("record[%d] ts = %#v, want non-empty string", i, record["ts"])
		}
		if _, err := time.Parse(traceTimestampLayout, ts); err != nil {
			t.Fatalf("record[%d] ts = %q, want %s format: %v", i, ts, traceTimestampLayout, err)
		}
	}
}

func assertTraceRecordOmitsSessionMetadata(t *testing.T, record map[string]any) {
	t.Helper()
	for _, key := range []string{"bot_id", "session_id", "agent_name", "main_id", "cwd"} {
		if _, ok := record[key]; ok {
			t.Fatalf("record = %+v, want no redundant session metadata field %q", record, key)
		}
	}
}

func assertTraceRecordFinal(t *testing.T, record map[string]any, want bool) {
	t.Helper()
	got, ok := record["is_final"]
	if want {
		if !ok || got != true {
			t.Fatalf("record = %+v, want is_final=true", record)
		}
		return
	}
	if ok && got != false {
		t.Fatalf("record = %+v, want non-final assistant", record)
	}
}
