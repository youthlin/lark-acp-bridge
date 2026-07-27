package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
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
	streamCardEmptyContent            = "\u200b"
)

var streamCardNow = time.Now

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
	cardID, err := a.createCardJSON(ctx, newStreamCardJSONWithPanels(processPanelEnabled, statusBarEnabled), "流式")
	if err != nil {
		return nil, err
	}
	if _, err := a.sendInteractiveCard(ctx, msg, cardID, "流式"); err != nil {
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
	seq := c.nextSequenceLocked()
	elements := newStreamCardProcessPanelJSON()
	return c.adapter.createCardElementsAfter(ctx, c.cardID, streamCardTextElementID, streamCardProcessPanelID, elements, seq, "创建飞书流式卡片过程组件")
}

func (c *sdkStreamCard) createUsagePanelLocked(ctx context.Context, text string) error {
	seq := c.nextSequenceLocked()
	elements := newStreamCardUsagePanelJSON(text)
	return c.adapter.createCardElementsAfter(ctx, c.cardID, c.usageTargetID, streamCardUsagePanelID, elements, seq, "创建飞书流式卡片用量明细组件")
}

func (c *sdkStreamCard) updateElementLocked(ctx context.Context, elementID string, text string) error {
	seq := c.nextSequenceLocked()
	content := streamCardUpdateContent(text)
	return c.adapter.updateCardElementContent(ctx, c.cardID, elementID, content, seq, "更新飞书流式卡片组件")
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
	seq := c.nextSequenceLocked()
	data := c.fullCardJSONLocked()
	if err := c.adapter.updateCardJSON(ctx, cardUpdateRequest{
		cardID:   c.cardID,
		data:     data,
		sequence: seq,
		action:   "普通更新飞书卡片",
		log:      true,
	}); err != nil {
		return err
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

func (c *sdkStreamCard) nextSequenceLocked() int {
	c.sequence++
	return c.sequence
}

func streamCardUpdateContent(text string) string {
	if strings.TrimSpace(text) == "" {
		return streamCardEmptyContent
	}
	return text
}

func logCardKitFailure(ctx context.Context, operation string, cardID string, elementID string, sequence int, request any, resp *larkcore.ApiResp, code int, msg string) {
	attrs := []any{
		"operation", operation,
		"card_id", cardID,
		"element_id", elementID,
		"sequence", sequence,
		"code", code,
		"msg", msg,
		"request", truncateCardKitLogValue(request),
	}
	if resp != nil {
		attrs = append(attrs,
			"request_id", resp.RequestId(),
			"status_code", resp.StatusCode,
			"response_body", truncateCardKitLogValue(string(resp.RawBody)),
		)
	}
	slog.WarnContext(ctx, "飞书 CardKit 操作失败详情", attrs...)
}

func truncateCardKitLogValue(v any) string {
	var text string
	switch value := v.(type) {
	case string:
		text = value
	default:
		data, err := json.Marshal(value)
		if err != nil {
			text = fmt.Sprint(value)
		} else {
			text = string(data)
		}
	}
	text = strings.TrimSpace(text)
	const max = 2000
	if len([]rune(text)) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max]) + "...<truncated>"
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
	seq := c.nextSequenceLocked()

	settings, _ := json.Marshal(cardJSON{"config": cardJSON{"streaming_mode": false}})
	if err := c.adapter.updateCardSettings(ctx, c.cardID, string(settings), seq, "关闭飞书流式卡片"); err != nil {
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
