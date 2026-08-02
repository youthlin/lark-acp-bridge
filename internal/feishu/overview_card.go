package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

const overviewCardAction = "acp_overview"

func (a *Adapter) SendOverviewCard(ctx context.Context, msg Message, card OverviewCard) error {
	if a.client == nil {
		return fmt.Errorf("飞书客户端未初始化")
	}
	if _, _, err := a.createAndSendCardJSON(ctx, msg, newOverviewCardJSON(card), "全览"); err != nil {
		return err
	}
	return nil
}

func (a *Adapter) handleOverviewAction(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return permissionCardToast("error", "无效的全览卡操作"), nil
	}
	action := event.Event.Action
	value := action.Value
	selected := strings.TrimSpace(action.Option)
	if selected == "" {
		selected = stringValue(value, "value")
	}
	overview := OverviewAction{
		BotID:               stringValue(value, "bot_id"),
		ChatID:              stringValue(value, "chat_id"),
		ChatType:            stringValue(value, "chat_type"),
		ThreadID:            stringValue(value, "thread_id"),
		GroupMessageType:    stringValue(value, "group_message_type"),
		RequesterID:         stringValue(value, "requester_id"),
		CurrentACPSessionID: stringValue(value, "current_acp_session_id"),
		Action:              stringValue(value, "overview_action"),
		Target:              stringValue(value, "target"),
		Value:               selected,
	}
	if event.Event.Operator != nil {
		overview.OperatorID = strings.TrimSpace(event.Event.Operator.OpenID)
	}
	if overview.ChatID == "" || overview.Action == "" {
		return permissionCardToast("error", "全览卡操作参数无效"), nil
	}
	handler, ok := a.handler.(OverviewActionHandler)
	if !ok {
		return permissionCardToast("error", "全览卡服务未初始化"), nil
	}
	result, err := handler.HandleOverviewAction(ctx, overview)
	if err != nil {
		return permissionCardToast("error", err.Error()), nil
	}
	toastType := strings.TrimSpace(result.ToastType)
	if toastType == "" {
		toastType = "success"
	}
	toast := strings.TrimSpace(result.Toast)
	if toast == "" {
		toast = "已更新"
	}
	data, err := overviewActionCardData(result)
	if err != nil {
		return permissionCardToast("error", err.Error()), nil
	}
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: toastType, Content: toast},
		Card: &callback.Card{
			Type: "raw",
			Data: data,
		},
	}, nil
}

func overviewActionCardData(result OverviewActionResult) (map[string]any, error) {
	switch {
	case result.Overview != nil:
		return cardDataFromJSON(newOverviewCardJSON(*result.Overview))
	case result.Model != nil:
		return cardDataFromJSON(newModelSelectionCardJSON(*result.Model))
	case result.Mode != nil:
		return cardDataFromJSON(newModeSelectionCardJSON(*result.Mode))
	case result.Session != nil:
		return cardDataFromJSON(newSessionSelectionCardJSON(*result.Session))
	default:
		return nil, fmt.Errorf("全览卡操作没有返回可展示内容")
	}
}

func cardDataFromJSON(data string) (map[string]any, error) {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(data), &decoded); err != nil {
		return nil, fmt.Errorf("解析卡片 JSON 失败: %w", err)
	}
	return decoded, nil
}

func newOverviewCardJSON(card OverviewCard) string {
	elements := []any{
		overviewSessionElement(card),
		overviewRuntimeElement(card),
	}
	if agentSelect := overviewAgentSelectElement(card); agentSelect != nil {
		elements = append(elements, agentSelect)
	}
	if sessionSelect := overviewSessionSelectElement(card); sessionSelect != nil {
		elements = append(elements, sessionSelect)
	}
	if modelSelect := overviewModelSelectElement(card); modelSelect != nil {
		elements = append(elements, modelSelect)
	}
	if modeSelect := overviewModeSelectElement(card); modeSelect != nil {
		elements = append(elements, modeSelect)
	}
	elements = append(elements, overviewOpenActionElements(card)...)
	elements = append(elements, overviewShowActionElements(card)...)
	elements = append(elements, overviewCommandHintsElement(card.CommandHints))
	elements = append(elements, overviewCommandActionElements(card)...)
	data, _ := json.Marshal(cardJSON{
		"schema": "2.0",
		"config": cardJSON{
			"update_multi": true,
			"width_mode":   "default",
			"summary":      cardJSON{"content": "当前聊天配置与状态全览"},
		},
		"header": cardJSON{
			"title":    cardJSON{"tag": "plain_text", "content": "当前聊天全览"},
			"subtitle": cardJSON{"tag": "plain_text", "content": overviewHeaderSubtitle(card)},
			"template": "blue",
			"icon":     cardJSON{"tag": "standard_icon", "token": "setting_colorful"},
		},
		"body": cardJSON{
			"direction":        "vertical",
			"padding":          "12px 12px 20px 12px",
			"vertical_spacing": "12px",
			"elements":         elements,
		},
	})
	return string(data)
}

func overviewHeaderSubtitle(card OverviewCard) string {
	parts := []string{overviewInline(defaultString(card.ChatAgentName, "未知 agent"))}
	if card.HasSession {
		parts = append(parts, overviewInline(defaultString(card.SessionTitle, "当前会话")))
	} else {
		parts = append(parts, "未创建会话")
	}
	return strings.Join(parts, " | ")
}

func overviewSessionElement(card OverviewCard) cardJSON {
	lines := []string{"**当前会话**"}
	if !card.HasSession {
		lines = append(lines, "尚未创建 ACP session。发送普通文本或 `/new` 后创建。")
	} else {
		lines = append(lines,
			"标题："+overviewInline(defaultString(card.SessionTitle, "未知")),
			"agent："+overviewInline(defaultString(card.AgentName, "未知")),
			"cwd："+overviewInline(defaultString(card.Cwd, "未知")),
			"session："+overviewInline(shortOverviewID(card.CurrentACPSessionID)),
			"model："+overviewInline(defaultString(card.Model, "未知")),
			"mode："+overviewInline(defaultString(card.Mode, "未知")),
		)
		if strings.TrimSpace(card.CompactStatus) != "" {
			lines = append(lines, "compact："+overviewInline(card.CompactStatus))
		}
	}
	return overviewContainer("blue-100", "blue-50", strings.Join(lines, "\n"))
}

func overviewRuntimeElement(card OverviewCard) cardJSON {
	lines := []string{
		"**运行状态**",
		"运行态：" + overviewInline(defaultString(card.RuntimeStatus, "未知")),
		"队列：" + overviewInline(defaultString(card.QueueStatus, "未知")),
		"知识库：" + overviewInline(defaultString(card.WikiStatus, "未知")),
		"loop：" + overviewInline(defaultString(card.LoopStatus, "未知")),
		"ACP错误：" + overviewInline(defaultString(card.ACPErrorStatus, "无")),
	}
	return overviewContainer("grey-200", "grey-50", strings.Join(lines, "\n"))
}

func overviewAgentSelectElement(card OverviewCard) any {
	options := make([]any, 0, len(card.AgentOptions))
	current := ""
	for _, option := range card.AgentOptions {
		value := strings.TrimSpace(option.Value)
		if value == "" {
			continue
		}
		if option.Current {
			current = value
		}
		text := strings.TrimSpace(option.Text)
		if text == "" {
			text = value
		}
		options = append(options, cardJSON{
			"text":  cardJSON{"tag": "plain_text", "content": text},
			"value": value,
		})
	}
	if len(options) == 0 {
		return nil
	}
	selectElement := cardJSON{
		"tag":         "select_static",
		"element_id":  "overview_agent",
		"width":       "fill",
		"placeholder": cardJSON{"tag": "plain_text", "content": "切换当前聊天默认 agent"},
		"options":     options,
		"behaviors": []any{
			cardJSON{
				"type":  "callback",
				"value": overviewActionValue(card, "set_agent", "", ""),
			},
		},
	}
	if current != "" {
		selectElement["initial_option"] = current
	}
	return selectElement
}

func overviewSessionSelectElement(card OverviewCard) any {
	options := make([]any, 0, len(card.SessionOptions))
	currentSessionID := strings.TrimSpace(card.CurrentACPSessionID)
	hasCurrent := false
	for _, option := range card.SessionOptions {
		value := strings.TrimSpace(option.ACPSessionID)
		if value == "" {
			continue
		}
		if value == currentSessionID {
			hasCurrent = true
		}
		options = append(options, cardJSON{
			"text":  cardJSON{"tag": "plain_text", "content": sessionOptionDisplayName(option)},
			"value": value,
		})
	}
	if len(options) == 0 {
		return nil
	}
	selectElement := cardJSON{
		"tag":         "select_static",
		"element_id":  "overview_session",
		"width":       "fill",
		"placeholder": cardJSON{"tag": "plain_text", "content": "切换历史会话"},
		"options":     options,
		"behaviors": []any{
			cardJSON{
				"type":  "callback",
				"value": overviewActionValue(card, "set_session", "", ""),
			},
		},
	}
	if hasCurrent {
		selectElement["initial_option"] = currentSessionID
	}
	return selectElement
}

func overviewModelSelectElement(card OverviewCard) any {
	options := make([]any, 0, len(card.ModelOptions))
	current := strings.TrimSpace(card.Model)
	hasCurrent := false
	for _, option := range card.ModelOptions {
		value := strings.TrimSpace(option.Value)
		if value == "" {
			continue
		}
		if value == current {
			hasCurrent = true
		}
		options = append(options, cardJSON{
			"text":  cardJSON{"tag": "plain_text", "content": modelOptionDisplayName(option)},
			"value": value,
		})
	}
	if len(options) == 0 {
		return nil
	}
	selectElement := cardJSON{
		"tag":         "select_static",
		"element_id":  "overview_model",
		"width":       "fill",
		"placeholder": cardJSON{"tag": "plain_text", "content": "切换当前会话模型"},
		"options":     options,
		"behaviors": []any{
			cardJSON{
				"type":  "callback",
				"value": overviewActionValue(card, "set_model", "", ""),
			},
		},
	}
	if hasCurrent {
		selectElement["initial_option"] = current
	}
	return selectElement
}

func overviewModeSelectElement(card OverviewCard) any {
	options := make([]any, 0, len(card.ModeOptions))
	current := strings.TrimSpace(card.Mode)
	hasCurrent := false
	for _, option := range card.ModeOptions {
		value := strings.TrimSpace(option.Value)
		if value == "" {
			continue
		}
		if value == current {
			hasCurrent = true
		}
		options = append(options, cardJSON{
			"text":  cardJSON{"tag": "plain_text", "content": modeOptionDisplayName(option)},
			"value": value,
		})
	}
	if len(options) == 0 {
		return nil
	}
	selectElement := cardJSON{
		"tag":         "select_static",
		"element_id":  "overview_mode",
		"width":       "fill",
		"placeholder": cardJSON{"tag": "plain_text", "content": "切换当前会话模式"},
		"options":     options,
		"behaviors": []any{
			cardJSON{
				"type":  "callback",
				"value": overviewActionValue(card, "set_mode", "", ""),
			},
		},
	}
	if hasCurrent {
		selectElement["initial_option"] = current
	}
	return selectElement
}

func overviewOpenActionElements(card OverviewCard) []any {
	text := "知识沉淀已关闭"
	buttonType := "default"
	if card.WikiEnabled {
		text = "知识沉淀已开启"
		buttonType = "primary"
	}
	buttons := []cardJSON{
		overviewButton(text, overviewActionValue(card, "toggle_wiki", "wiki", overviewNextSwitch(card.WikiEnabled)), buttonType),
	}
	return overviewButtonRows(buttons, 1)
}

func overviewShowActionElements(card OverviewCard) []any {
	buttons := []cardJSON{
		overviewShowButton(card, "计划", "plan", card.Show.Plan),
		overviewShowButton(card, "思考", "thought", card.Show.Thought),
		overviewShowButton(card, "工具", "tool", card.Show.Tool),
		overviewShowButton(card, "过程", "step", card.Show.Step),
		overviewShowButton(card, "状态", "status", card.Show.Status),
		overviewShowButton(card, "用量", "used", card.Show.Used),
	}
	return overviewButtonRows(buttons, 3)
}

func overviewShowButton(card OverviewCard, text string, target string, current bool) cardJSON {
	buttonType := "default"
	if current {
		buttonType = "primary"
	}
	return overviewButton(text, overviewActionValue(card, "toggle_show", target, overviewNextSwitch(current)), buttonType)
}

func overviewCommandHintsElement(hints []string) cardJSON {
	lines := []string{"**命令入口**"}
	for _, hint := range hints {
		hint = strings.TrimSpace(hint)
		if hint == "" {
			continue
		}
		lines = append(lines, "`"+overviewInline(hint)+"`")
	}
	return overviewContainer("grey-200", "grey-50", strings.Join(lines, "\n"))
}

func overviewCommandActionElements(card OverviewCard) []any {
	buttons := []cardJSON{
		overviewButton("新的会话", overviewActionValue(card, "new_session", "", ""), "default"),
		overviewButton("查看帮助", overviewActionValue(card, "show_help", "", ""), "default"),
	}
	return overviewButtonRows(buttons, 2)
}

func overviewActionValue(card OverviewCard, action string, target string, value string) cardJSON {
	return cardJSON{
		"action":                 overviewCardAction,
		"overview_action":        action,
		"target":                 target,
		"value":                  value,
		"bot_id":                 card.BotID,
		"chat_id":                card.ChatID,
		"chat_type":              card.ChatType,
		"thread_id":              card.ThreadID,
		"group_message_type":     card.GroupMessageType,
		"requester_id":           card.RequesterID,
		"current_acp_session_id": strings.TrimSpace(card.CurrentACPSessionID),
	}
}

func overviewButton(text string, value cardJSON, buttonType string) cardJSON {
	return cardJSON{
		"tag":   "button",
		"type":  buttonType,
		"width": "fill",
		"text":  cardJSON{"tag": "plain_text", "content": text},
		"behaviors": []any{
			cardJSON{
				"type":  "callback",
				"value": value,
			},
		},
	}
}

func overviewButtonRows(buttons []cardJSON, perRow int) []any {
	if perRow <= 0 {
		perRow = 1
	}
	rows := make([]any, 0, (len(buttons)+perRow-1)/perRow)
	for start := 0; start < len(buttons); start += perRow {
		end := start + perRow
		if end > len(buttons) {
			end = len(buttons)
		}
		columns := make([]any, 0, end-start)
		for _, button := range buttons[start:end] {
			columns = append(columns, cardJSON{
				"tag":      "column",
				"width":    "auto",
				"elements": []any{button},
			})
		}
		rows = append(rows, cardJSON{
			"tag":                "column_set",
			"flex_mode":          "none",
			"horizontal_spacing": "8px",
			"columns":            columns,
		})
	}
	return rows
}

func overviewContainer(border string, background string, content string) cardJSON {
	return cardJSON{
		"tag":              "interactive_container",
		"width":            "fill",
		"has_border":       true,
		"border_color":     border,
		"corner_radius":    "8px",
		"background_style": background,
		"padding":          "12px 12px 12px 12px",
		"disabled":         true,
		"elements": []any{
			cardJSON{"tag": "markdown", "content": content},
		},
	}
}

func overviewNextSwitch(current bool) string {
	if current {
		return "off"
	}
	return "on"
}

func overviewInline(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "未知"
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "`", "'"), "\n", " ")
	runes := []rune(text)
	if len(runes) <= 80 {
		return text
	}
	return string(runes[:80]) + "..."
}

func shortOverviewID(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "未知"
	}
	runes := []rune(text)
	if len(runes) <= 16 {
		return text
	}
	return string(runes[:8]) + "..." + string(runes[len(runes)-6:])
}
