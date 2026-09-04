package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

type meetingPromptInput struct {
	Meeting struct {
		Topic        string   `json:"topic,omitempty"`
		MeetingNo    string   `json:"meeting_no,omitempty"`
		Recipient    string   `json:"recipient,omitempty"`
		Participants []string `json:"participants,omitempty"`
	} `json:"meeting"`
	Previous      MeetingMinutes        `json:"previous"`
	NewTranscript []meetingPromptSpeech `json:"new_transcript,omitempty"`
	NewChat       []meetingPromptSpeech `json:"new_chat,omitempty"`
	NewDocuments  []MeetingDocument     `json:"new_documents,omitempty"`
	MeetingEnded  bool                  `json:"meeting_ended"`
}

type meetingPromptSpeech struct {
	Time    string `json:"time,omitempty"`
	Speaker string `json:"speaker,omitempty"`
	Text    string `json:"text"`
}

func meetingPrompt(state MeetingState, events []MeetingEvent, final bool) string {
	var input meetingPromptInput
	input.Meeting.Topic = state.Topic
	input.Meeting.MeetingNo = state.MeetingNo
	input.Meeting.Recipient = participantDisplayName(state.RecipientOpenID, state.Participants)
	input.Meeting.Participants = meetingParticipantNames(state.Participants)
	input.Previous = normalizeMeetingMinutes(state.Minutes)
	input.NewTranscript, input.NewChat, input.NewDocuments = meetingPromptEvents(events)
	input.MeetingEnded = final
	payload, _ := json.MarshalIndent(input, "", "  ")
	return strings.Join([]string{
		"你是静默会议助手。请根据上一版会议纪要和本批新增事件，生成一份完整的最新结构化会议纪要。",
		"只允许使用输入中明确出现的信息，不得虚构决策、负责人、截止时间或完成状态。",
		"meeting_ended=false 表示会议仍在进行，只做增量更新；meeting_ended=true 才是会议结束后的最终纪要。",
		"参会人入会/离会、开始或停止录制、共享或停止共享屏幕、邀请或移出智能体等会议操作或系统噪声，不要写入 summary、decisions、risks 或 open_questions，除非发言者明确把它作为会议议题、结论、风险或行动项讨论。",
		"TODO 只有在会议中明确提出行动项时才记录；confidence 使用 explicit，evidence 必须引用简短原话。不能确认的内容放入 open_questions。",
		"recipient 是纪要接收人；若会议明确给他分配行动项，请使用对应姓名作为 assignee。不要仅因其是接收人就创建 TODO。",
		"保留仍然有效的上一版内容，合并重复项；todos.id 在后续批次保持稳定。",
		"shared_documents 只记录输入里实际出现的文档。",
		"当本批新增内容只有会议操作、系统事件或噪声时，保持上一版纪要不变；不要为了说明信息不足而新增 open_questions。",
		"最终只能输出一个 JSON 对象，不要 Markdown 代码块、解释或额外字段。所有数组字段都必须存在。",
		"JSON schema: " + `{"summary":["..."],"decisions":["..."],"todos":[{"id":"stable-id","content":"...","assignee":"","due_at":"","status":"open","confidence":"explicit","evidence":"..."}],"risks":["..."],"open_questions":["..."],"shared_documents":[{"title":"...","url":"..."}]}`,
		"输入 JSON：",
		string(payload),
	}, "\n")
}

func meetingPromptEvents(events []MeetingEvent) ([]meetingPromptSpeech, []meetingPromptSpeech, []MeetingDocument) {
	events = append([]MeetingEvent(nil), events...)
	sortMeetingEvents(events)
	var transcript []meetingPromptSpeech
	var chat []meetingPromptSpeech
	var documents []MeetingDocument
	seenDocuments := make(map[string]struct{})
	for _, event := range events {
		switch event.Type {
		case "transcript_received":
			transcript = appendMergedMeetingSpeech(transcript, event)
		case "chat_received":
			chat = appendMergedMeetingSpeech(chat, event)
		}
		title := strings.TrimSpace(event.Payload["document_title"])
		url := strings.TrimSpace(event.Payload["document_url"])
		if title != "" || url != "" {
			key := title + "\x00" + url
			if _, ok := seenDocuments[key]; !ok {
				seenDocuments[key] = struct{}{}
				documents = append(documents, MeetingDocument{Title: title, URL: url})
			}
		}
	}
	return transcript, chat, documents
}

func appendMergedMeetingSpeech(items []meetingPromptSpeech, event MeetingEvent) []meetingPromptSpeech {
	text := strings.TrimSpace(event.Text)
	if text == "" {
		return items
	}
	speaker := meetingSpeakerName(event)
	if len(items) > 0 && items[len(items)-1].Speaker == speaker {
		items[len(items)-1].Text = joinMeetingSpeech(items[len(items)-1].Text, text)
		return items
	}
	items = append(items, meetingPromptSpeech{
		Time:    meetingPromptTime(event.Time),
		Speaker: speaker,
		Text:    text,
	})
	return items
}

func joinMeetingSpeech(left, right string) string {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" {
		return right
	}
	if right == "" {
		return left
	}
	return left + " " + right
}

func meetingPromptTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}

func meetingSpeakerName(event MeetingEvent) string {
	if name := strings.TrimSpace(event.ActorName); name != "" {
		return name
	}
	return strings.TrimSpace(event.ActorID)
}

func participantDisplayName(openID string, participants map[string]string) string {
	openID = strings.TrimSpace(openID)
	if openID == "" {
		return ""
	}
	if name := strings.TrimSpace(participants[openID]); name != "" {
		return name
	}
	return ""
}

func meetingParticipantNames(participants map[string]string) []string {
	names := make([]string, 0, len(participants))
	seen := make(map[string]struct{}, len(participants))
	for _, name := range participants {
		display := strings.TrimSpace(name)
		if display == "" {
			continue
		}
		if _, ok := seen[display]; ok {
			continue
		}
		seen[display] = struct{}{}
		names = append(names, display)
	}
	sort.Strings(names)
	return names
}

func parseMeetingMinutes(text string) (MeetingMinutes, error) {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		if len(lines) >= 3 && strings.HasPrefix(strings.TrimSpace(lines[0]), "```") && strings.TrimSpace(lines[len(lines)-1]) == "```" {
			text = strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		return MeetingMinutes{}, fmt.Errorf("解析会议纪要 JSON: %w", err)
	}
	for _, field := range []string{"summary", "decisions", "todos", "risks", "open_questions", "shared_documents"} {
		raw, ok := fields[field]
		if !ok {
			return MeetingMinutes{}, fmt.Errorf("会议纪要 JSON 缺少字段 %s", field)
		}
		if value := bytes.TrimSpace(raw); len(value) == 0 || value[0] != '[' {
			return MeetingMinutes{}, fmt.Errorf("会议纪要 JSON 字段 %s 必须是数组", field)
		}
	}
	decoder := json.NewDecoder(bytes.NewBufferString(text))
	decoder.DisallowUnknownFields()
	var minutes MeetingMinutes
	if err := decoder.Decode(&minutes); err != nil {
		return MeetingMinutes{}, fmt.Errorf("解析会议纪要 JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return MeetingMinutes{}, err
	}
	minutes = normalizeMeetingMinutes(minutes)
	for i := range minutes.Todos {
		todo := &minutes.Todos[i]
		todo.ID = strings.TrimSpace(todo.ID)
		todo.Content = strings.TrimSpace(todo.Content)
		todo.Assignee = strings.TrimSpace(todo.Assignee)
		todo.DueAt = strings.TrimSpace(todo.DueAt)
		todo.Status = strings.TrimSpace(todo.Status)
		todo.Confidence = strings.ToLower(strings.TrimSpace(todo.Confidence))
		todo.Evidence = strings.TrimSpace(todo.Evidence)
		if todo.ID == "" || todo.Content == "" {
			return MeetingMinutes{}, fmt.Errorf("会议纪要 todos[%d] 缺少 id 或 content", i)
		}
		if todo.Confidence != "explicit" || todo.Evidence == "" {
			return MeetingMinutes{}, fmt.Errorf("会议纪要 todos[%d] 缺少明确证据", i)
		}
	}
	return minutes, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("解析会议纪要 JSON 尾部: %w", err)
	}
	return fmt.Errorf("会议纪要 JSON 后存在额外内容")
}

func validateMeetingMinutes(previous MeetingMinutes, events []MeetingEvent, minutes MeetingMinutes) error {
	evidenceSources := make([]string, 0, len(previous.Todos)+len(events))
	for _, todo := range previous.Todos {
		if evidence := strings.TrimSpace(todo.Evidence); evidence != "" {
			evidenceSources = append(evidenceSources, evidence)
		}
	}
	transcript, chat, _ := meetingPromptEvents(events)
	for _, speech := range transcript {
		if text := strings.TrimSpace(speech.Text); text != "" {
			evidenceSources = append(evidenceSources, text)
		}
	}
	for _, speech := range chat {
		if text := strings.TrimSpace(speech.Text); text != "" {
			evidenceSources = append(evidenceSources, text)
		}
	}
	documentSources := make([]MeetingDocument, 0, len(previous.SharedDocuments)+len(events))
	documentSources = append(documentSources, previous.SharedDocuments...)
	for _, event := range events {
		if text := strings.TrimSpace(event.Text); text != "" {
			evidenceSources = append(evidenceSources, text)
		}
		title := strings.TrimSpace(event.Payload["document_title"])
		url := strings.TrimSpace(event.Payload["document_url"])
		if title != "" || url != "" {
			documentSources = append(documentSources, MeetingDocument{Title: title, URL: url})
		}
	}
	seenTodoIDs := make(map[string]struct{}, len(minutes.Todos))
	for i, todo := range minutes.Todos {
		if _, duplicate := seenTodoIDs[todo.ID]; duplicate {
			return fmt.Errorf("会议纪要 todos[%d] 的 id 重复: %s", i, todo.ID)
		}
		seenTodoIDs[todo.ID] = struct{}{}
		if !containedInMeetingSources(todo.Evidence, evidenceSources) {
			return fmt.Errorf("会议纪要 todos[%d] 的 evidence 无法回溯到会议原话", i)
		}
	}
	for i, doc := range minutes.SharedDocuments {
		if !meetingDocumentSupported(doc, documentSources, events) {
			return fmt.Errorf("会议纪要 shared_documents[%d] 无法回溯到会议事件", i)
		}
	}
	return nil
}

func containedInMeetingSources(value string, sources []string) bool {
	value = strings.TrimSpace(value)
	for _, source := range sources {
		if strings.Contains(source, value) {
			return true
		}
	}
	return false
}

func meetingDocumentSupported(doc MeetingDocument, sources []MeetingDocument, events []MeetingEvent) bool {
	doc.Title = strings.TrimSpace(doc.Title)
	doc.URL = strings.TrimSpace(doc.URL)
	if doc.Title == "" && doc.URL == "" {
		return false
	}
	for _, source := range sources {
		if doc.URL != "" && doc.URL == strings.TrimSpace(source.URL) {
			return true
		}
		if doc.URL == "" && doc.Title != "" && doc.Title == strings.TrimSpace(source.Title) {
			return true
		}
	}
	for _, event := range events {
		if doc.URL != "" && strings.Contains(event.Text, doc.URL) {
			return true
		}
		if doc.URL == "" && doc.Title != "" && strings.Contains(event.Text, doc.Title) {
			return true
		}
	}
	return false
}
