package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
)

type outboundBlockKind string

const (
	outboundBlockMarkdown outboundBlockKind = "markdown"
	outboundBlockImage    outboundBlockKind = "image"
)

type outboundBlock struct {
	Kind     outboundBlockKind
	Text     string
	Alt      string
	Path     string
	ImageKey string
}

type outboundRenderContext struct {
	BaseDir string
}

var outboundMarkdownImagePattern = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)

func parseOutboundMarkdown(text string, render outboundRenderContext) []outboundBlock {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var blocks []outboundBlock
	inFence := false
	for _, rawLine := range strings.SplitAfter(text, "\n") {
		line := strings.TrimSuffix(rawLine, "\n")
		lineBreak := ""
		if strings.HasSuffix(rawLine, "\n") {
			lineBreak = "\n"
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			blocks = appendMarkdownBlock(blocks, line+lineBreak)
			continue
		}
		if inFence {
			blocks = appendMarkdownBlock(blocks, line+lineBreak)
			continue
		}
		offset := 0
		for _, loc := range outboundMarkdownImagePattern.FindAllStringSubmatchIndex(line, -1) {
			if loc[0] > offset {
				blocks = appendMarkdownBlock(blocks, line[offset:loc[0]])
			}
			alt := strings.TrimSpace(line[loc[2]:loc[3]])
			target := strings.TrimSpace(line[loc[4]:loc[5]])
			block, ok := outboundImageBlock(alt, target, render)
			if ok {
				blocks = append(blocks, block)
			} else {
				blocks = appendMarkdownBlock(blocks, line[loc[0]:loc[1]])
			}
			offset = loc[1]
		}
		if offset < len(line) {
			blocks = appendMarkdownBlock(blocks, line[offset:])
		}
		if lineBreak != "" {
			blocks = appendMarkdownBlock(blocks, lineBreak)
		}
	}
	return compactOutboundMarkdownBlocks(blocks)
}

func outboundRenderContextFromPublic(render OutboundRenderContext) outboundRenderContext {
	return outboundRenderContext{BaseDir: strings.TrimSpace(render.BaseDir)}
}

func (a *Adapter) renderOutboundBlocks(ctx context.Context, text string, render outboundRenderContext) ([]outboundBlock, error) {
	blocks := parseOutboundMarkdown(text, render)
	imageCount := 0
	uploadedCount := 0
	for i := range blocks {
		if blocks[i].Kind != outboundBlockImage {
			continue
		}
		imageCount++
		if strings.TrimSpace(blocks[i].ImageKey) != "" {
			continue
		}
		slog.InfoContext(ctx, "准备上传出站 Markdown 图片", "path", blocks[i].Path, "base_dir", render.BaseDir)
		imageKey, err := a.uploadReplyImage(ctx, blocks[i].Path)
		if err != nil {
			slog.ErrorContext(ctx, "上传出站 Markdown 图片失败", "path", blocks[i].Path, "base_dir", render.BaseDir, "错误", err)
			return nil, err
		}
		blocks[i].ImageKey = imageKey
		uploadedCount++
	}
	if imageCount > 0 {
		slog.InfoContext(ctx, "出站 Markdown 图片渲染完成", "image_count", imageCount, "uploaded_count", uploadedCount, "base_dir", render.BaseDir)
	}
	return blocks, nil
}

func outboundBlocksHaveImage(blocks []outboundBlock) bool {
	for _, block := range blocks {
		if block.Kind == outboundBlockImage && strings.TrimSpace(block.ImageKey) != "" {
			return true
		}
	}
	return false
}

func outboundBlocksImageCount(blocks []outboundBlock) int {
	count := 0
	for _, block := range blocks {
		if block.Kind == outboundBlockImage && strings.TrimSpace(block.ImageKey) != "" {
			count++
		}
	}
	return count
}

func outboundBlocksPostContent(blocks []outboundBlock) (string, error) {
	content := make([][]cardJSON, 0, len(blocks))
	for _, block := range blocks {
		switch block.Kind {
		case outboundBlockMarkdown:
			if strings.TrimSpace(block.Text) == "" {
				continue
			}
			content = append(content, []cardJSON{{"tag": "md", "text": strings.TrimSpace(block.Text)}})
		case outboundBlockImage:
			imageKey := strings.TrimSpace(block.ImageKey)
			if imageKey == "" {
				continue
			}
			content = append(content, []cardJSON{{"tag": "img", "image_key": imageKey}})
		}
	}
	data, err := json.Marshal(cardJSON{
		"zh_cn": cardJSON{
			"content": content,
		},
	})
	if err != nil {
		return "", fmt.Errorf("编码飞书富文本消息内容: %w", err)
	}
	return string(data), nil
}

func outboundBlocksStreamCardElements(blocks []outboundBlock) []any {
	elements := make([]any, 0, len(blocks)+1)
	hasTextAnchor := false
	for idx, block := range blocks {
		switch block.Kind {
		case outboundBlockMarkdown:
			text := strings.TrimSpace(sanitizeStreamCardMarkdownContent(block.Text))
			if text == "" {
				continue
			}
			element := cardJSON{"tag": "markdown", "content": text}
			if !hasTextAnchor {
				element["element_id"] = streamCardTextElementID
				hasTextAnchor = true
			}
			elements = append(elements, element)
		case outboundBlockImage:
			imageKey := strings.TrimSpace(block.ImageKey)
			if imageKey == "" {
				continue
			}
			alt := strings.TrimSpace(block.Alt)
			element := cardJSON{
				"tag":           "img",
				"img_key":       imageKey,
				"alt":           cardJSON{"tag": "plain_text", "content": alt},
				"scale_type":    "fit_horizontal",
				"preview":       true,
				"element_id":    streamCardImageElementID(idx),
				"corner_radius": "4px",
			}
			if alt != "" {
				element["title"] = cardJSON{"tag": "plain_text", "content": alt}
			}
			elements = append(elements, element)
		}
	}
	if !hasTextAnchor {
		elements = append([]any{cardJSON{"tag": "markdown", "content": streamCardEmptyContent, "element_id": streamCardTextElementID}}, elements...)
	}
	return elements
}

func outboundImageBlock(alt string, target string, render outboundRenderContext) (outboundBlock, bool) {
	target = trimReplyImagePath(target)
	if target == "" {
		return outboundBlock{}, false
	}
	if looksLikeFeishuImageKey(target) {
		return outboundBlock{Kind: outboundBlockImage, Alt: alt, ImageKey: target}, true
	}
	if strings.Contains(target, "://") && !strings.HasPrefix(strings.ToLower(target), "file://") {
		return outboundBlock{}, false
	}
	path := target
	if strings.HasPrefix(strings.ToLower(path), "file://") {
		path = strings.TrimPrefix(path, "file://")
	}
	if strings.HasPrefix(path, "~/") || filepath.IsAbs(path) {
		return outboundBlock{Kind: outboundBlockImage, Alt: alt, Path: path}, true
	}
	if base := strings.TrimSpace(render.BaseDir); base != "" {
		return outboundBlock{Kind: outboundBlockImage, Alt: alt, Path: filepath.Join(base, path)}, true
	}
	return outboundBlock{}, false
}

func appendMarkdownBlock(blocks []outboundBlock, text string) []outboundBlock {
	if text == "" {
		return blocks
	}
	return append(blocks, outboundBlock{Kind: outboundBlockMarkdown, Text: text})
}

func compactOutboundMarkdownBlocks(blocks []outboundBlock) []outboundBlock {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]outboundBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Kind == outboundBlockMarkdown && block.Text == "" {
			continue
		}
		if block.Kind == outboundBlockMarkdown && len(out) > 0 && out[len(out)-1].Kind == outboundBlockMarkdown {
			out[len(out)-1].Text += block.Text
			continue
		}
		out = append(out, block)
	}
	for i := range out {
		if out[i].Kind == outboundBlockMarkdown {
			out[i].Text = strings.TrimSpace(out[i].Text)
		}
	}
	return out
}

func looksLikeFeishuImageKey(target string) bool {
	target = strings.TrimSpace(target)
	return strings.HasPrefix(target, "img_")
}
