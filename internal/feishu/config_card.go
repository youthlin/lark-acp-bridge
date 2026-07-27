package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
)

func (a *Adapter) SendConfigDetailCard(ctx context.Context, msg Message, card ConfigDetailCard) error {
	if a.client == nil {
		return fmt.Errorf("飞书客户端未初始化")
	}
	cardResp, err := a.client.Cardkit.V1.Card.Create(ctx, larkcardkit.NewCreateCardReqBuilder().
		Body(larkcardkit.NewCreateCardReqBodyBuilder().
			Type("card_json").
			Data(newConfigDetailCardJSON(card)).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("创建飞书配置项详情卡片: %w", err)
	}
	if !cardResp.Success() {
		return fmt.Errorf("创建飞书配置项详情卡片返回错误: code=%d msg=%s", cardResp.Code, cardResp.Msg)
	}
	cardID := ""
	if cardResp.Data != nil {
		cardID = normalizedCardID(cardResp.Data.CardId)
	}
	if cardID == "" {
		return fmt.Errorf("创建飞书配置项详情卡片未返回 card_id")
	}
	if _, err := a.sendInteractiveCard(ctx, msg, cardID); err != nil {
		return fmt.Errorf("发送飞书配置项详情卡片: %w", err)
	}
	return nil
}

func newConfigDetailCardJSON(card ConfigDetailCard) string {
	elements := []any{
		configDetailSummaryElement(card),
	}
	if len(card.Options) > 0 {
		elements = append(elements, configDetailOptionsElement(card.Options))
	}
	if command := strings.TrimSpace(card.SetCommand); command != "" {
		elements = append(elements, cardJSON{
			"tag":              "interactive_container",
			"width":            "fill",
			"has_border":       true,
			"border_color":     "grey-200",
			"corner_radius":    "8px",
			"background_style": "grey-50",
			"padding":          "10px 12px 10px 12px",
			"disabled":         true,
			"elements": []any{
				cardJSON{"tag": "markdown", "content": "**设置命令**\n`" + configCardInline(command) + "`"},
			},
		})
	}
	data, _ := json.Marshal(cardJSON{
		"schema": "2.0",
		"config": cardJSON{
			"update_multi": true,
			"width_mode":   "default",
			"summary":      cardJSON{"content": "ACP 配置项：" + configCardInline(card.ID)},
		},
		"header": cardJSON{
			"title":    cardJSON{"tag": "plain_text", "content": "ACP 配置项：" + configCardInline(card.ID)},
			"subtitle": cardJSON{"tag": "plain_text", "content": configDetailSubtitle(card)},
			"template": "blue",
			"icon":     cardJSON{"tag": "standard_icon", "token": "notice_colorful"},
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

func configDetailSummaryElement(card ConfigDetailCard) cardJSON {
	lines := []string{
		"**当前值**",
		configCardInline(defaultString(card.CurrentValue, "未知")),
		"",
		"**类型**：" + configCardInline(defaultString(card.Type, "unknown")),
	}
	if name := strings.TrimSpace(card.Name); name != "" && name != strings.TrimSpace(card.ID) {
		lines = append(lines, "**名称**："+configCardInline(name))
	}
	if category := strings.TrimSpace(card.Category); category != "" {
		lines = append(lines, "**分类**："+configCardInline(category))
	}
	if description := strings.TrimSpace(card.Description); description != "" {
		lines = append(lines, "", "**说明**", configCardBlockText(description))
	}
	return cardJSON{
		"tag":              "interactive_container",
		"width":            "fill",
		"has_border":       true,
		"border_color":     "blue-100",
		"corner_radius":    "8px",
		"background_style": "blue-50",
		"padding":          "12px 12px 12px 12px",
		"disabled":         true,
		"elements": []any{
			cardJSON{"tag": "markdown", "content": strings.Join(lines, "\n")},
		},
	}
}

func configDetailOptionsElement(options []ConfigOptionValue) cardJSON {
	lines := []string{"**可选值**"}
	for _, option := range options {
		value := strings.TrimSpace(option.Value)
		if value == "" {
			continue
		}
		line := "- " + configCardInline(configOptionValueDisplayName(option))
		if option.Current {
			line += "  **当前**"
		}
		if description := strings.TrimSpace(option.Description); description != "" {
			line += "\n  " + configCardBlockText(description)
		}
		lines = append(lines, line)
	}
	return cardJSON{
		"tag":              "interactive_container",
		"width":            "fill",
		"has_border":       true,
		"border_color":     "grey-200",
		"corner_radius":    "8px",
		"background_style": "grey-50",
		"padding":          "12px 12px 12px 12px",
		"disabled":         true,
		"elements": []any{
			cardJSON{"tag": "markdown", "content": strings.Join(lines, "\n")},
		},
	}
}

func configDetailSubtitle(card ConfigDetailCard) string {
	parts := make([]string, 0, 3)
	if name := strings.TrimSpace(card.Name); name != "" && name != strings.TrimSpace(card.ID) {
		parts = append(parts, name)
	}
	if category := strings.TrimSpace(card.Category); category != "" {
		parts = append(parts, category)
	}
	if optionType := strings.TrimSpace(card.Type); optionType != "" {
		parts = append(parts, optionType)
	}
	if len(parts) == 0 {
		return "配置项详情"
	}
	return configCardInline(strings.Join(parts, " | "))
}

func configOptionValueDisplayName(option ConfigOptionValue) string {
	value := strings.TrimSpace(option.Value)
	name := strings.TrimSpace(option.Name)
	if name != "" && name != value {
		return name + "（" + value + "）"
	}
	return value
}

func configCardInline(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.ReplaceAll(strings.ReplaceAll(text, "`", "'"), "\n", " ")
	runes := []rune(text)
	if len(runes) <= 100 {
		return text
	}
	return string(runes[:100]) + "..."
}

func configCardBlockText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.ReplaceAll(text, "`", "'")
	lines := strings.Fields(text)
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, " ")
}

func defaultString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
