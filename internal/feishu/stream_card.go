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
	streamCardProcessPanelID    = "panel_process"
	streamCardProcessElementID  = "md_process"
	streamCardUsagePanelID      = "panel_usage_detail"
	streamCardUsageDetailID     = "md_usage_detail"
	streamCardStatusElementID   = "md_status"
	streamCardTextElementID     = "md_stream"
	streamCardMetadataElementID = "md_metadata"
	streamCardSourceElementID   = "md_source"
	streamCardFooterElementID   = "md_footer"
)

const (
	streamCardNormalUpdateAfter       = 9*time.Minute + 30*time.Second
	streamCardNormalUpdateMinInterval = 5 * time.Second
	streamCardEmptyContent            = "\u200b"
	streamCardInitialStatus           = "⏳ 0s"
	streamCardDefaultProcessTitle     = "执行过程"
)

var streamCardNow = time.Now

func newStreamCardJSON() string {
	return newStreamCardJSONWithPanels(true, true)
}

func newStreamCardJSONWithProcessPanel(includeProcessPanel bool) string {
	return newStreamCardJSONWithPanels(includeProcessPanel, true)
}

func newStreamCardJSONWithPanels(includeProcessPanel, includeStatusBar bool) string {
	return newStreamCardJSONFromState("", "", streamCardInitialStatus, "", includeProcessPanel, includeStatusBar, false, true, StreamCardMeta{})
}

func newStreamCardJSONFromState(text, process, status, usage string, includeProcessPanel, includeStatusBar, includeUsagePanel, streamingMode bool, meta StreamCardMeta) string {
	return newStreamCardJSONFromBlocks([]outboundBlock{{Kind: outboundBlockMarkdown, Text: text}}, process, status, usage, includeProcessPanel, includeStatusBar, includeUsagePanel, streamingMode, meta)
}

func newStreamCardJSONFromBlocks(blocks []outboundBlock, process, status, usage string, includeProcessPanel, includeStatusBar, includeUsagePanel, streamingMode bool, meta StreamCardMeta) string {
	return newStreamCardJSONFromBlocksWithProcessTitle(blocks, process, status, usage, includeProcessPanel, includeStatusBar, includeUsagePanel, streamingMode, meta, "")
}

func newStreamCardJSONFromBlocksWithProcessTitle(blocks []outboundBlock, process, status, usage string, includeProcessPanel, includeStatusBar, includeUsagePanel, streamingMode bool, meta StreamCardMeta, processTitle string) string {
	elements := outboundBlocksStreamCardElements(blocks)
	meta = normalizeStreamCardMeta(meta)
	if meta.SourceURL != "" {
		elements = append([]any{streamCardSourceLink(meta.SourceURL)}, elements...)
	}
	if meta.Metadata != "" {
		elements = append([]any{streamCardMetadata(meta.Metadata)}, elements...)
	}
	if includeProcessPanel {
		elements = append(elements, streamCardProcessPanelWithTitle(process, processTitle))
	}
	if includeUsagePanel {
		elements = append(elements, streamCardUsagePanel(usage))
	}
	if includeStatusBar {
		if strings.TrimSpace(status) == "" {
			status = streamCardInitialStatus
		}
		elements = append(elements, streamCardStatusMarkdown(status))
	}
	if meta.Footer != "" {
		elements = append(elements, streamCardFooter(meta.Footer))
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
	card := cardJSON{
		"schema": "2.0",
		"config": config,
		"body": cardJSON{
			"elements": elements,
		},
	}
	if header := streamCardHeader(meta); header != nil {
		card["header"] = header
	}
	data, _ := json.Marshal(card)
	return string(data)
}

func streamCardImageElementID(index int) string {
	return fmt.Sprintf("img_stream_%d", index)
}

func newStreamCardProcessPanelJSON() string {
	return newStreamCardProcessPanelJSONWithTitle("")
}

func newStreamCardProcessPanelJSONWithTitle(title string) string {
	data, _ := json.Marshal([]any{streamCardProcessPanelWithTitle("", title)})
	return string(data)
}

func newStreamCardUsagePanelJSON(content string) string {
	data, _ := json.Marshal([]any{streamCardUsagePanel(content)})
	return string(data)
}

func streamCardProcessPanelWithTitle(content string, title string) cardJSON {
	title = normalizedStreamCardProcessTitle(title)
	return cardJSON{
		"tag":              "collapsible_panel",
		"expanded":         false,
		"element_id":       streamCardProcessPanelID,
		"background_color": "grey",
		"header": cardJSON{
			"title": cardJSON{"tag": "plain_text", "content": title},
		},
		"border":           cardJSON{"color": "grey", "corner_radius": "8px"},
		"vertical_spacing": "4px",
		"padding":          "8px 12px 8px 12px",
		"elements": []any{
			cardJSON{"tag": "markdown", "content": content, "element_id": streamCardProcessElementID},
		},
	}
}

func normalizedStreamCardProcessTitle(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return streamCardDefaultProcessTitle
	}
	return title
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

func streamCardSourceLink(url string) cardJSON {
	return cardJSON{
		"tag":              "markdown",
		"content":          "📄 [查看原文](" + url + ")",
		"element_id":       streamCardSourceElementID,
		"text_size":        "notation",
		"vertical_spacing": "4px",
	}
}

func streamCardMetadata(content string) cardJSON {
	return cardJSON{
		"tag":              "markdown",
		"content":          content,
		"element_id":       streamCardMetadataElementID,
		"text_size":        "notation",
		"vertical_spacing": "4px",
	}
}

func streamCardFooter(content string) cardJSON {
	return cardJSON{
		"tag":              "markdown",
		"content":          content,
		"element_id":       streamCardFooterElementID,
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

func normalizeStreamCardMeta(meta StreamCardMeta) StreamCardMeta {
	meta.Title = strings.TrimSpace(meta.Title)
	meta.Subtitle = strings.TrimSpace(meta.Subtitle)
	meta.Metadata = strings.TrimSpace(meta.Metadata)
	meta.SourceURL = strings.TrimSpace(meta.SourceURL)
	meta.Footer = strings.TrimSpace(meta.Footer)
	return meta
}

func streamCardHeader(meta StreamCardMeta) cardJSON {
	meta = normalizeStreamCardMeta(meta)
	if meta.Title == "" && meta.Subtitle == "" {
		return nil
	}
	header := cardJSON{
		"template": "blue",
	}
	if !meta.HideHeaderIcon {
		header["icon"] = cardJSON{"tag": "standard_icon", "token": "time_colorful"}
	}
	if meta.Title != "" {
		header["title"] = cardJSON{"tag": "plain_text", "content": meta.Title}
	}
	if meta.Subtitle != "" {
		header["subtitle"] = cardJSON{"tag": "plain_text", "content": meta.Subtitle}
	}
	return header
}

type sdkStreamCard struct {
	adapter *Adapter
	cardID  string
	message SentMessage
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
	finalBlocks      []outboundBlock
	process          string
	processTitle     string
	status           string
	usageDetail      string
	meta             StreamCardMeta
}

func (a *Adapter) StartStreamCard(ctx context.Context, msg Message, options StreamCardOptions) (StreamCard, error) {
	if a.client == nil {
		return nil, fmt.Errorf("飞书客户端未初始化")
	}
	processPanelEnabled := options.ProcessPanelEnabled
	statusBarEnabled := options.StatusBarEnabled
	processTitle := normalizedStreamCardProcessTitle(options.ProcessTitle)
	meta := normalizeStreamCardMeta(options.Meta)
	cardID, err := a.createCardJSON(ctx, newStreamCardJSONFromBlocksWithProcessTitle([]outboundBlock{{Kind: outboundBlockMarkdown}}, "", streamCardInitialStatus, "", processPanelEnabled, statusBarEnabled, false, true, meta, processTitle), "流式")
	if err != nil {
		return nil, err
	}
	sent, err := a.sendInteractiveCard(ctx, msg, cardID, "流式")
	if err != nil {
		return nil, err
	}
	initialStatus := ""
	if statusBarEnabled {
		initialStatus = streamCardInitialStatus
	}
	return &sdkStreamCard{
		adapter:        a,
		cardID:         cardID,
		message:        sent,
		created:        streamCardNow(),
		meta:           meta,
		processCreated: processPanelEnabled,
		processTitle:   processTitle,
		statusCreated:  statusBarEnabled,
		status:         initialStatus,
		usageTargetID:  streamCardUsageTargetID(processPanelEnabled, statusBarEnabled),
	}, nil
}

func (c *sdkStreamCard) Message() SentMessage {
	if c == nil {
		return SentMessage{}
	}
	return c.message
}

func streamCardUsageTargetID(processPanelEnabled, statusBarEnabled bool) string {
	// 用量面板位于状态栏之上:有状态栏时插到状态栏前一个元素(过程面板或正文)之后,
	// 无状态栏时作为末尾组件插到过程面板或正文之后。
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

func (c *sdkStreamCard) SetFinalText(ctx context.Context, text string, render OutboundRenderContext) error {
	blocks, err := c.adapter.renderOutboundBlocks(ctx, text, outboundRenderContextFromPublic(render))
	if err != nil {
		slog.ErrorContext(ctx, "渲染 ACP 流式卡片最终文本失败", "card_id", c.cardID, "base_dir", render.BaseDir, "错误", err)
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	normalizedText := strings.TrimSpace(text)
	hasImage := outboundBlocksHaveImage(blocks)
	if !hasImage && strings.TrimSpace(c.text) == normalizedText && !c.shouldUseNormalUpdateLocked() {
		return nil
	}
	c.text = normalizedText
	c.finalBlocks = blocks
	if hasImage || c.shouldUseNormalUpdateLocked() {
		if hasImage {
			slog.InfoContext(ctx, "ACP 流式卡片最终文本包含图片，使用完整卡片更新", "card_id", c.cardID, "image_count", outboundBlocksImageCount(blocks), "base_dir", render.BaseDir)
		}
		c.streamingClosed = true
		if err := c.updateFullCardLocked(ctx, true); err != nil {
			slog.ErrorContext(ctx, "更新 ACP 流式卡片最终文本失败", "card_id", c.cardID, "has_image", hasImage, "image_count", outboundBlocksImageCount(blocks), "错误", err)
			return err
		}
		return nil
	}
	return c.handleStreamMutationErrorLocked(ctx, c.updateElementLocked(ctx, streamCardTextElementID, c.text))
}

func (c *sdkStreamCard) UpdateMeta(ctx context.Context, meta StreamCardMeta) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.meta = normalizeStreamCardMeta(meta)
	c.streamingClosed = true
	return c.updateFullCardLocked(ctx, true)
}

func (c *sdkStreamCard) createProcessPanelLocked(ctx context.Context) error {
	seq := c.nextSequenceLocked()
	elements := newStreamCardProcessPanelJSONWithTitle(c.processTitle)
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
	blocks := c.finalBlocks
	if len(blocks) == 0 {
		blocks = []outboundBlock{{Kind: outboundBlockMarkdown, Text: c.text}}
	}
	return newStreamCardJSONFromBlocksWithProcessTitle(blocks, c.process, c.status, c.usageDetail, includeProcess, includeStatus, includeUsage, false, c.meta, c.processTitle)
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
