package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const (
	streamCardProcessPanelID   = "panel_process"
	streamCardProcessElementID = "md_process"
	streamCardUsagePanelID     = "panel_usage_detail"
	streamCardUsageDetailID    = "md_usage_detail"
	streamCardStatusElementID  = "md_status"
	streamCardTextElementID    = "md_stream"
)

const (
	streamCardNormalUpdateAfter       = 9*time.Minute + 30*time.Second
	streamCardNormalUpdateMinInterval = 5 * time.Second
)

var streamCardNow = time.Now

type cardJSON map[string]any

func newStreamCardJSON() string {
	return newStreamCardJSONWithPanels(true, true)
}

func newStreamCardJSONWithProcessPanel(includeProcessPanel bool) string {
	return newStreamCardJSONWithPanels(includeProcessPanel, true)
}

func newStreamCardJSONWithPanels(includeProcessPanel, includeStatusBar bool) string {
	return newStreamCardJSONFromState("", "", "执行中", "", includeProcessPanel, includeStatusBar, false, true)
}

func newStreamCardJSONFromState(text, process, status, usage string, includeProcessPanel, includeStatusBar, includeUsagePanel, streamingMode bool) string {
	elements := []any{cardJSON{"tag": "markdown", "content": text, "element_id": streamCardTextElementID}}
	if includeProcessPanel {
		elements = append(elements, streamCardProcessPanelWithContent(process))
	}
	if includeStatusBar {
		if strings.TrimSpace(status) == "" {
			status = "执行中"
		}
		elements = append(elements, streamCardStatusMarkdown(status))
	}
	if includeUsagePanel {
		elements = append(elements, streamCardUsagePanel(usage))
	}
	config := cardJSON{
		"streaming_mode":   streamingMode,
		"wide_screen_mode": true,
		"width_mode":       "fill",
		"summary":          cardJSON{"content": ""},
	}
	if streamingMode {
		config["streaming_config"] = cardJSON{
			"print_frequency_ms": cardJSON{"default": 70},
			"print_step":         cardJSON{"default": 2},
			"print_strategy":     "fast",
		}
	}
	data, _ := json.Marshal(cardJSON{
		"schema": "2.0",
		"config": config,
		"body": cardJSON{
			"elements": elements,
		},
	})
	return string(data)
}

func newStreamCardProcessPanelJSON() string {
	data, _ := json.Marshal([]any{streamCardProcessPanel()})
	return string(data)
}

func newStreamCardUsagePanelJSON(content string) string {
	data, _ := json.Marshal([]any{streamCardUsagePanel(content)})
	return string(data)
}

func streamCardProcessPanel() cardJSON {
	return streamCardProcessPanelWithContent("")
}

func streamCardProcessPanelWithContent(content string) cardJSON {
	return cardJSON{
		"tag":              "collapsible_panel",
		"expanded":         false,
		"element_id":       streamCardProcessPanelID,
		"background_color": "grey",
		"header": cardJSON{
			"title": cardJSON{"tag": "plain_text", "content": "执行过程"},
		},
		"border":           cardJSON{"color": "grey", "corner_radius": "8px"},
		"vertical_spacing": "4px",
		"padding":          "8px 12px 8px 12px",
		"elements": []any{
			cardJSON{"tag": "markdown", "content": content, "element_id": streamCardProcessElementID},
		},
	}
}

func streamCardStatusMarkdown(status string) cardJSON {
	return cardJSON{
		"tag":              "markdown",
		"content":          status,
		"element_id":       streamCardStatusElementID,
		"text_size":        "notation",
		"text_align":       "left",
		"text_color":       "grey",
		"margin":           "8px 0 0 0",
		"vertical_spacing": "4px",
	}
}

func streamCardUsagePanel(content string) cardJSON {
	return cardJSON{
		"tag":              "collapsible_panel",
		"expanded":         false,
		"element_id":       streamCardUsagePanelID,
		"background_color": "grey",
		"header": cardJSON{
			"title": cardJSON{"tag": "plain_text", "content": "用量明细"},
		},
		"border":           cardJSON{"color": "grey", "corner_radius": "8px"},
		"vertical_spacing": "4px",
		"padding":          "8px 12px 8px 12px",
		"elements": []any{
			cardJSON{"tag": "markdown", "content": content, "element_id": streamCardUsageDetailID},
		},
	}
}

type sdkStreamCard struct {
	adapter *Adapter
	cardID  string
	created time.Time

	mu               sync.Mutex
	sequence         int
	closed           bool
	streamingClosed  bool
	processCreated   bool
	statusCreated    bool
	usageCreated     bool
	usageTargetID    string
	lastNormalUpdate time.Time
	text             string
	process          string
	status           string
	usageDetail      string
}

func (a *Adapter) StartStreamCard(ctx context.Context, msg Message) (StreamCard, error) {
	if a.client == nil {
		return nil, fmt.Errorf("飞书客户端未初始化")
	}
	processPanelEnabled := StreamCardProcessPanelEnabled(ctx)
	statusBarEnabled := StreamCardStatusBarEnabled(ctx)
	cardResp, err := a.client.Cardkit.V1.Card.Create(ctx, larkcardkit.NewCreateCardReqBuilder().
		Body(larkcardkit.NewCreateCardReqBodyBuilder().
			Type("card_json").
			Data(newStreamCardJSONWithPanels(processPanelEnabled, statusBarEnabled)).
			Build()).
		Build())
	if err != nil {
		return nil, fmt.Errorf("创建飞书流式卡片: %w", err)
	}
	if !cardResp.Success() {
		return nil, fmt.Errorf("创建飞书流式卡片返回错误: code=%d msg=%s", cardResp.Code, cardResp.Msg)
	}
	if cardResp.Data == nil || cardResp.Data.CardId == nil || *cardResp.Data.CardId == "" {
		return nil, fmt.Errorf("创建飞书流式卡片未返回 card_id")
	}
	cardID := *cardResp.Data.CardId
	if err := a.sendInteractiveCard(ctx, msg, cardID); err != nil {
		return nil, err
	}
	initialStatus := ""
	if statusBarEnabled {
		initialStatus = "执行中"
	}
	return &sdkStreamCard{
		adapter:        a,
		cardID:         cardID,
		created:        streamCardNow(),
		processCreated: processPanelEnabled,
		statusCreated:  statusBarEnabled,
		status:         initialStatus,
		usageTargetID:  streamCardUsageTargetID(processPanelEnabled, statusBarEnabled),
	}, nil
}

func streamCardUsageTargetID(processPanelEnabled, statusBarEnabled bool) string {
	if statusBarEnabled {
		return streamCardStatusElementID
	}
	if processPanelEnabled {
		return streamCardProcessPanelID
	}
	return streamCardTextElementID
}

func (a *Adapter) sendInteractiveCard(ctx context.Context, msg Message, cardID string) error {
	content, err := json.Marshal(map[string]any{
		"type": "card",
		"data": map[string]string{"card_id": cardID},
	})
	if err != nil {
		return fmt.Errorf("编码飞书卡片消息内容: %w", err)
	}
	if msg.IsPrivateChat() && strings.TrimSpace(msg.MessageID) == "" {
		if msg.ChatID == "" {
			return fmt.Errorf("飞书 chat_id 为空")
		}
		resp, err := a.client.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
			ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
			Body(larkim.NewCreateMessageReqBodyBuilder().
				ReceiveId(msg.ChatID).
				MsgType("interactive").
				Content(string(content)).
				Build()).
			Build())
		if err != nil {
			return fmt.Errorf("发送飞书流式卡片消息: %w", err)
		}
		if !resp.Success() {
			return fmt.Errorf("发送飞书流式卡片消息返回错误: code=%d msg=%s", resp.Code, resp.Msg)
		}
		return nil
	}
	if msg.MessageID == "" {
		return fmt.Errorf("飞书 message_id 为空")
	}
	replyInThread := replyInThreadForMessage(msg)
	resp, err := a.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(msg.MessageID).
		Body(&larkim.ReplyMessageReqBody{
			Content:       larkcore.StringPtr(string(content)),
			MsgType:       larkcore.StringPtr("interactive"),
			ReplyInThread: &replyInThread,
		}).
		Build())
	if err != nil {
		return fmt.Errorf("回复飞书流式卡片消息: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("回复飞书流式卡片消息返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *sdkStreamCard) UpdateProcess(ctx context.Context, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.process = text
	if c.shouldUseNormalUpdateLocked() {
		c.streamingClosed = true
		c.processCreated = true
		return c.updateFullCardLocked(ctx, false)
	}
	if !c.processCreated {
		if err := c.createProcessPanelLocked(ctx); err != nil {
			return c.handleStreamMutationErrorLocked(ctx, err)
		}
		c.processCreated = true
	}
	return c.handleStreamMutationErrorLocked(ctx, c.updateElementLocked(ctx, streamCardProcessElementID, text))
}

func (c *sdkStreamCard) UpdateStatus(ctx context.Context, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.status = text
	if c.shouldUseNormalUpdateLocked() {
		c.streamingClosed = true
		c.statusCreated = true
		return c.updateFullCardLocked(ctx, false)
	}
	return c.handleStreamMutationErrorLocked(ctx, c.updateElementLocked(ctx, streamCardStatusElementID, text))
}

func (c *sdkStreamCard) UpdateUsageDetail(ctx context.Context, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.usageDetail = text
	if c.shouldUseNormalUpdateLocked() {
		c.streamingClosed = true
		c.usageCreated = true
		return c.updateFullCardLocked(ctx, false)
	}
	if !c.usageCreated {
		if err := c.createUsagePanelLocked(ctx, text); err != nil {
			return c.handleStreamMutationErrorLocked(ctx, err)
		}
		c.usageCreated = true
		return nil
	}
	return c.handleStreamMutationErrorLocked(ctx, c.updateElementLocked(ctx, streamCardUsageDetailID, text))
}

func (c *sdkStreamCard) UpdateText(ctx context.Context, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.text = text
	if c.shouldUseNormalUpdateLocked() {
		c.streamingClosed = true
		return c.updateFullCardLocked(ctx, false)
	}
	return c.handleStreamMutationErrorLocked(ctx, c.updateElementLocked(ctx, streamCardTextElementID, text))
}

func (c *sdkStreamCard) createProcessPanelLocked(ctx context.Context) error {
	c.sequence++
	seq := c.sequence
	resp, err := c.adapter.client.Cardkit.V1.CardElement.Create(ctx, larkcardkit.NewCreateCardElementReqBuilder().
		CardId(c.cardID).
		Body(larkcardkit.NewCreateCardElementReqBodyBuilder().
			Type(larkcardkit.TypeInsertAfter).
			TargetElementId(streamCardTextElementID).
			Elements(newStreamCardProcessPanelJSON()).
			Sequence(seq).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("创建飞书流式卡片过程组件: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("创建飞书流式卡片过程组件返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *sdkStreamCard) createUsagePanelLocked(ctx context.Context, text string) error {
	c.sequence++
	seq := c.sequence
	resp, err := c.adapter.client.Cardkit.V1.CardElement.Create(ctx, larkcardkit.NewCreateCardElementReqBuilder().
		CardId(c.cardID).
		Body(larkcardkit.NewCreateCardElementReqBodyBuilder().
			Type(larkcardkit.TypeInsertAfter).
			TargetElementId(c.usageTargetID).
			Elements(newStreamCardUsagePanelJSON(text)).
			Sequence(seq).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("创建飞书流式卡片用量明细组件: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("创建飞书流式卡片用量明细组件返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *sdkStreamCard) updateElementLocked(ctx context.Context, elementID string, text string) error {
	c.sequence++
	seq := c.sequence
	resp, err := c.adapter.client.Cardkit.V1.CardElement.Content(ctx, larkcardkit.NewContentCardElementReqBuilder().
		CardId(c.cardID).
		ElementId(elementID).
		Body(larkcardkit.NewContentCardElementReqBodyBuilder().
			Content(text).
			Sequence(seq).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("更新飞书流式卡片组件: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("更新飞书流式卡片组件返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (c *sdkStreamCard) shouldUseNormalUpdateLocked() bool {
	if c.streamingClosed {
		return true
	}
	if c.created.IsZero() {
		return false
	}
	return !streamCardNow().Before(c.created.Add(streamCardNormalUpdateAfter))
}

func (c *sdkStreamCard) handleStreamMutationErrorLocked(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if !isStreamCardStreamingClosedError(err) {
		return err
	}
	c.streamingClosed = true
	if c.process != "" {
		c.processCreated = true
	}
	if c.status != "" {
		c.statusCreated = true
	}
	if c.usageDetail != "" {
		c.usageCreated = true
	}
	if fallbackErr := c.updateFullCardLocked(ctx, true); fallbackErr != nil {
		return fmt.Errorf("%w；普通卡片更新失败: %v", err, fallbackErr)
	}
	return nil
}

func (c *sdkStreamCard) updateFullCardLocked(ctx context.Context, force bool) error {
	now := streamCardNow()
	if !force && !c.lastNormalUpdate.IsZero() && now.Sub(c.lastNormalUpdate) < streamCardNormalUpdateMinInterval {
		return nil
	}
	resp, err := c.adapter.client.Cardkit.V1.Card.Update(ctx, larkcardkit.NewUpdateCardReqBuilder().
		CardId(c.cardID).
		Body(larkcardkit.NewUpdateCardReqBodyBuilder().
			Card(larkcardkit.NewCardBuilder().
				Type("card_json").
				Data(c.fullCardJSONLocked()).
				Build()).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("普通更新飞书卡片: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("普通更新飞书卡片返回错误: code=%d msg=%s", resp.Code, resp.Msg)
	}
	c.lastNormalUpdate = now
	return nil
}

func (c *sdkStreamCard) fullCardJSONLocked() string {
	includeProcess := c.processCreated || strings.TrimSpace(c.process) != ""
	includeStatus := c.statusCreated || strings.TrimSpace(c.status) != ""
	includeUsage := c.usageCreated || strings.TrimSpace(c.usageDetail) != ""
	return newStreamCardJSONFromState(c.text, c.process, c.status, c.usageDetail, includeProcess, includeStatus, includeUsage, false)
}

func isStreamCardStreamingClosedError(err error) bool {
	if err == nil {
		return false
	}
	var codeErr *larkcore.CodeError
	if errors.As(err, &codeErr) && streamCardStreamingClosedCode(codeErr.Code) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "200850") ||
		strings.Contains(msg, "300309") ||
		strings.Contains(msg, "card streaming timeout") ||
		strings.Contains(msg, "streaming mode is closed")
}

func streamCardStreamingClosedCode(code int) bool {
	return code == 200850 || code == 300309
}

func (c *sdkStreamCard) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if c.shouldUseNormalUpdateLocked() {
		c.streamingClosed = true
		if err := c.updateFullCardLocked(ctx, true); err != nil {
			return err
		}
		c.closed = true
		return nil
	}
	c.sequence++
	seq := c.sequence

	settings, _ := json.Marshal(cardJSON{"config": cardJSON{"streaming_mode": false}})
	resp, err := c.adapter.client.Cardkit.V1.Card.Settings(ctx, larkcardkit.NewSettingsCardReqBuilder().
		CardId(c.cardID).
		Body(larkcardkit.NewSettingsCardReqBodyBuilder().
			Settings(string(settings)).
			Sequence(seq).
			Build()).
		Build())
	if err != nil {
		err = fmt.Errorf("关闭飞书流式卡片: %w", err)
		if isStreamCardStreamingClosedError(err) {
			c.streamingClosed = true
			if fallbackErr := c.updateFullCardLocked(ctx, true); fallbackErr != nil {
				return fmt.Errorf("%w；普通卡片更新失败: %v", err, fallbackErr)
			}
			c.closed = true
			return nil
		}
		return err
	}
	if !resp.Success() {
		err := fmt.Errorf("关闭飞书流式卡片返回错误: code=%d msg=%s", resp.Code, resp.Msg)
		if isStreamCardStreamingClosedError(err) {
			c.streamingClosed = true
			if fallbackErr := c.updateFullCardLocked(ctx, true); fallbackErr != nil {
				return fmt.Errorf("%w；普通卡片更新失败: %v", err, fallbackErr)
			}
			c.closed = true
			return nil
		}
		return err
	}
	c.closed = true
	return nil
}
