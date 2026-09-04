package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

type sdkMeetingCard struct {
	adapter *Adapter
	mu      sync.Mutex
	state   MeetingCardSnapshot
}

func (a *Adapter) StartMeetingCard(ctx context.Context, recipientOpenID string, view MeetingCardView) (MeetingCard, error) {
	if a == nil || a.client == nil {
		return nil, fmt.Errorf("飞书客户端未初始化")
	}
	cardID, err := a.createCardJSON(ctx, newMeetingCardJSON(view), "会议纪要")
	if err != nil {
		return nil, err
	}
	sent, err := a.sendInteractiveCardMessageToOpenID(ctx, recipientOpenID, cardID, "会议纪要")
	if err != nil {
		return nil, err
	}
	return &sdkMeetingCard{adapter: a, state: MeetingCardSnapshot{CardID: cardID, MessageID: sent.MessageID, ChatID: sent.ChatID}}, nil
}

func (a *Adapter) RestoreMeetingCard(state MeetingCardSnapshot) MeetingCard {
	return &sdkMeetingCard{adapter: a, state: state}
}

func (c *sdkMeetingCard) Snapshot() MeetingCardSnapshot {
	if c == nil {
		return MeetingCardSnapshot{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *sdkMeetingCard) Update(ctx context.Context, view MeetingCardView) error {
	if c == nil || c.adapter == nil || c.adapter.client == nil || strings.TrimSpace(c.state.CardID) == "" {
		return fmt.Errorf("飞书会议纪要卡片未初始化")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.Sequence++
	return c.adapter.updateCardJSON(ctx, cardUpdateRequest{
		cardID: c.state.CardID, sequence: c.state.Sequence, data: newMeetingCardJSON(view), action: "更新飞书会议纪要卡片", log: true,
	})
}

func newMeetingCardJSON(view MeetingCardView) string {
	data, _ := json.Marshal(newMeetingCardData(view))
	return string(data)
}

func newMeetingCardData(view MeetingCardView) cardJSON {
	topic := strings.TrimSpace(view.Topic)
	if topic == "" {
		topic = "未命名会议"
	}
	final := strings.EqualFold(strings.TrimSpace(view.Status), "completed")
	status := "会议进行中，纪要持续更新"
	template := "blue"
	if final {
		status, template = "会议已结束，可转发", "green"
	}
	if strings.TrimSpace(view.Error) != "" {
		status, template = "会议纪要更新异常", "orange"
	}
	elements := []any{meetingCardMarkdown("**状态：**" + status + meetingCardTimeText(view))}
	elements = append(elements, meetingCardSection("摘要", view.Summary, "等待会议内容。"))
	elements = append(elements, meetingCardSection("决策", view.Decisions, "暂无明确决策。"))
	elements = append(elements, meetingCardTodoSection(view.Todos))
	elements = append(elements, meetingCardSection("风险", view.Risks, "暂无明确风险。"))
	elements = append(elements, meetingCardSection("待确认", view.OpenQuestions, "暂无待确认问题。"))
	if len(view.SharedDocuments) > 0 {
		lines := make([]string, 0, len(view.SharedDocuments))
		for _, doc := range view.SharedDocuments {
			title, url := strings.TrimSpace(doc.Title), strings.TrimSpace(doc.URL)
			if title == "" {
				title = url
			}
			if url != "" {
				lines = append(lines, fmt.Sprintf("- [%s](%s)", title, url))
			} else if title != "" {
				lines = append(lines, "- "+title)
			}
		}
		elements = append(elements, meetingCardMarkdown("**共享文档**\n"+strings.Join(lines, "\n")))
	}
	if errText := strings.TrimSpace(view.Error); errText != "" {
		elements = append(elements, meetingCardMarkdown("**最近错误**\n"+errText))
	}
	if updated := strings.TrimSpace(view.UpdatedAt); updated != "" {
		elements = append(elements, cardJSON{"tag": "note", "elements": []any{cardJSON{"tag": "plain_text", "content": "最近更新：" + updated}}})
	}
	subtitle := strings.TrimSpace(view.MeetingNo)
	return cardJSON{
		"schema": "2.0",
		"config": cardJSON{"update_multi": true, "wide_screen_mode": true, "width_mode": "fill", "summary": cardJSON{"content": topic + " - " + status}},
		"header": cardJSON{"title": cardJSON{"tag": "plain_text", "content": topic}, "subtitle": cardJSON{"tag": "plain_text", "content": subtitle}, "template": template, "icon": cardJSON{"tag": "standard_icon", "token": "minutes_colorful"}},
		"body":   cardJSON{"direction": "vertical", "padding": "12px 12px 16px 12px", "vertical_spacing": "12px", "elements": elements},
	}
}

func meetingCardMarkdown(content string) cardJSON {
	return cardJSON{"tag": "markdown", "content": strings.TrimSpace(content)}
}

func meetingCardSection(title string, items []string, empty string) cardJSON {
	lines := make([]string, 0, len(items))
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			lines = append(lines, "- "+item)
		}
	}
	if len(lines) == 0 {
		lines = append(lines, empty)
	}
	return meetingCardMarkdown("**" + title + "**\n" + strings.Join(lines, "\n"))
}

func meetingCardTodoSection(todos []MeetingCardTodo) cardJSON {
	lines := make([]string, 0, len(todos))
	for _, todo := range todos {
		if strings.TrimSpace(todo.Content) == "" {
			continue
		}
		detail := []string{}
		if value := strings.TrimSpace(todo.Assignee); value != "" {
			detail = append(detail, "负责人："+value)
		}
		if value := strings.TrimSpace(todo.DueAt); value != "" {
			detail = append(detail, "截止："+value)
		}
		line := "- " + strings.TrimSpace(todo.Content)
		if len(detail) > 0 {
			line += "（" + strings.Join(detail, "；") + "）"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = append(lines, "暂无明确 TODO。")
	}
	return meetingCardMarkdown("**TODO**\n" + strings.Join(lines, "\n"))
}

func meetingCardTimeText(view MeetingCardView) string {
	parts := []string{}
	if value := strings.TrimSpace(view.StartedAt); value != "" {
		parts = append(parts, "开始："+value)
	}
	if value := strings.TrimSpace(view.EndedAt); value != "" {
		parts = append(parts, "结束："+value)
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n" + strings.Join(parts, " ｜ ")
}
