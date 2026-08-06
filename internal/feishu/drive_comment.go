package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

var driveReplyAtTagPattern = regexp.MustCompile(`<at\s+id=(?:"([^"]+)"|([^\s>]+))\s*></at>`)

const (
	driveCommentDeduperTTL = 30 * time.Minute
	driveCommentDeduperMax = 10000
)

type DriveComment struct {
	BotID             string
	BotOpenID         string
	Workspace         string
	FileToken         string
	FileType          string
	CommentID         string
	ReplyID           string
	NoticeType        string
	OperatorID        string
	RecipientID       string
	IsMentioned       bool
	DetailLoaded      bool
	CommentUserID     string
	CommentText       string
	CommentCreateTime int
	CommentUpdateTime int
	CommentIsSolved   bool
	CommentIsWhole    bool
	Quote             string
	ReplyUserID       string
	ReplyText         string
	ReplyCreateTime   int
	ReplyUpdateTime   int
	ReplyCount        int
	RepliesComplete   bool
	Replies           []DriveCommentReply
	DocumentURL       string
}

type DriveCommentDetail struct {
	CommentID       string
	UserID          string
	Quote           string
	CreateTime      int
	UpdateTime      int
	IsSolved        bool
	IsWhole         bool
	HasMore         bool
	PageToken       string
	RepliesComplete bool
	Replies         []DriveCommentReply
}

type DriveCommentReply struct {
	ReplyID    string
	UserID     string
	Text       string
	CreateTime int
	UpdateTime int
}

func (c DriveComment) Normalized() DriveComment {
	c.BotID = strings.TrimSpace(c.BotID)
	c.BotOpenID = strings.TrimSpace(c.BotOpenID)
	c.Workspace = strings.TrimSpace(c.Workspace)
	c.FileToken = strings.TrimSpace(c.FileToken)
	c.FileType = strings.TrimSpace(c.FileType)
	c.CommentID = strings.TrimSpace(c.CommentID)
	c.ReplyID = strings.TrimSpace(c.ReplyID)
	c.NoticeType = strings.TrimSpace(c.NoticeType)
	c.OperatorID = strings.TrimSpace(c.OperatorID)
	c.RecipientID = strings.TrimSpace(c.RecipientID)
	c.CommentUserID = strings.TrimSpace(c.CommentUserID)
	c.CommentText = strings.TrimSpace(c.CommentText)
	c.Quote = strings.TrimSpace(c.Quote)
	c.ReplyUserID = strings.TrimSpace(c.ReplyUserID)
	c.ReplyText = strings.TrimSpace(c.ReplyText)
	c.DocumentURL = strings.TrimSpace(c.DocumentURL)
	if len(c.Replies) > 0 {
		replies := make([]DriveCommentReply, 0, len(c.Replies))
		for _, reply := range c.Replies {
			reply.ReplyID = strings.TrimSpace(reply.ReplyID)
			reply.UserID = strings.TrimSpace(reply.UserID)
			reply.Text = strings.TrimSpace(reply.Text)
			if reply.ReplyID == "" && reply.Text == "" {
				continue
			}
			replies = append(replies, reply)
		}
		c.Replies = replies
	}
	return c
}

func ParseDriveComment(event *larkdrive.P2NoticeCommentAddV1) (DriveComment, error) {
	if event == nil || event.Event == nil {
		return DriveComment{}, fmt.Errorf("飞书云文档评论事件为空")
	}
	raw := event.Event
	notice := raw.NoticeMeta
	comment := DriveComment{
		CommentID:   value(raw.CommentId),
		ReplyID:     value(raw.ReplyId),
		IsMentioned: raw.IsMentioned != nil && *raw.IsMentioned,
	}
	if notice != nil {
		comment.FileToken = value(notice.FileToken)
		comment.FileType = value(notice.FileType)
		comment.NoticeType = value(notice.NoticeType)
		comment.OperatorID = openIDFromDriveUserID(notice.FromUserId)
		comment.RecipientID = openIDFromDriveUserID(notice.ToUserId)
	}
	comment.DocumentURL = driveDocumentURL(comment.FileType, comment.FileToken)
	return comment.Normalized(), nil
}

func (a *Adapter) handleDriveCommentAdd(ctx context.Context, event *larkdrive.P2NoticeCommentAddV1) (err error) {
	defer recoverEventHandler(ctx, "drive_comment", &err)
	var body []byte
	if event != nil && event.EventReq != nil {
		body = event.Body
	}
	slog.DebugContext(ctx, "Bot收到云文档评论事件", "body", eventLogBody(body, event))

	comment, err := ParseDriveComment(event)
	if err != nil {
		slog.WarnContext(ctx, "解析飞书云文档评论事件失败", "错误", err)
		slog.DebugContext(ctx, "解析飞书云文档评论事件失败详情", "错误", err, "事件", larkcore.Prettify(event))
		return nil
	}
	comment.BotID = a.cfg.ID
	comment.BotOpenID = a.cfg.BotOpenID
	comment.Workspace = a.cfg.Workspace
	comment = comment.Normalized()
	ctx = logging.CtxAddAttr(ctx,
		slog.String("bot", comment.BotID),
		slog.String("source", "drive_comment"),
		slog.String("file_type", comment.FileType),
		slog.String("file_token", comment.FileToken),
		slog.String("comment_id", comment.CommentID),
		slog.String("reply_id", comment.ReplyID),
	)

	if comment.FileToken == "" || comment.FileType == "" || comment.CommentID == "" {
		slog.WarnContext(ctx, "跳过缺少必要字段的云文档评论事件")
		return nil
	}
	if comment.BotOpenID != "" && comment.OperatorID == comment.BotOpenID {
		slog.InfoContext(ctx, "跳过 bot 自己触发的云文档评论事件")
		return nil
	}
	if a.deduper != nil {
		allowed, err := a.deduper.Allow(comment.BotID, driveCommentDedupeID(comment))
		if err != nil {
			slog.ErrorContext(ctx, "记录云文档评论去重状态失败", "错误", err)
		}
		if !allowed {
			slog.InfoContext(ctx, "跳过重复云文档评论事件")
			return nil
		}
	}
	if a.driveComments != nil {
		// [larkDriveCommentClient.GetComment]
		detail, err := a.driveComments.GetComment(ctx, comment.FileToken, comment.FileType, comment.CommentID)
		if err != nil {
			slog.WarnContext(ctx, "读取云文档评论详情失败", "错误", err)
		} else {
			comment = comment.withDetail(detail).Normalized()
			if comment.BotOpenID != "" && driveCommentFromBot(comment, detail) {
				slog.InfoContext(ctx, "跳过 bot 自己的云文档评论或回复")
				return nil
			}
		}
	}
	handler, ok := a.handler.(DriveCommentHandler)
	if !ok || handler == nil {
		return nil
	}
	var handleErr error
	if outboundHandler, ok := handler.(OutboundDriveCommentHandler); ok {
		handleErr = outboundHandler.HandleDriveCommentWithOutbound(ctx, comment, a)
	} else {
		handleErr = handler.HandleDriveComment(ctx, comment)
	}
	if handleErr != nil {
		slog.ErrorContext(ctx, "处理云文档评论事件失败", "错误", handleErr)
	}
	return nil
}

func (a *Adapter) ReplyDriveComment(ctx context.Context, comment DriveComment, text string) error {
	if a.driveComments == nil {
		slog.WarnContext(ctx, "缺少云文档评论回复 client", "comment_id", comment.CommentID)
		return nil
	}
	return a.driveComments.ReplyComment(ctx, comment, text)
}

func (c DriveComment) withDetail(detail DriveCommentDetail) DriveComment {
	detail.CommentID = strings.TrimSpace(detail.CommentID)
	if detail.CommentID != "" {
		c.CommentID = detail.CommentID
	}
	c.DetailLoaded = true
	c.CommentUserID = strings.TrimSpace(detail.UserID)
	c.Quote = strings.TrimSpace(detail.Quote)
	c.CommentCreateTime = detail.CreateTime
	c.CommentUpdateTime = detail.UpdateTime
	c.CommentIsSolved = detail.IsSolved
	c.CommentIsWhole = detail.IsWhole
	c.ReplyCount = len(detail.Replies)
	c.RepliesComplete = detail.RepliesComplete
	c.Replies = append([]DriveCommentReply(nil), detail.Replies...)
	for _, reply := range detail.Replies {
		reply.ReplyID = strings.TrimSpace(reply.ReplyID)
		if reply.ReplyID == "" {
			continue
		}
		if c.CommentText == "" {
			c.CommentText = reply.Text
		}
		if c.ReplyID != "" && reply.ReplyID == c.ReplyID {
			c.ReplyUserID = strings.TrimSpace(reply.UserID)
			c.ReplyText = reply.Text
			c.ReplyCreateTime = reply.CreateTime
			c.ReplyUpdateTime = reply.UpdateTime
			break
		}
	}
	return c
}

func driveCommentDetailFromFileComment(comment *larkdrive.FileComment) DriveCommentDetail {
	if comment == nil {
		return DriveCommentDetail{}
	}
	detail := DriveCommentDetail{
		CommentID:       value(comment.CommentId),
		UserID:          value(comment.UserId),
		Quote:           value(comment.Quote),
		CreateTime:      valueInt(comment.CreateTime),
		UpdateTime:      valueInt(comment.UpdateTime),
		IsSolved:        valueBool(comment.IsSolved),
		IsWhole:         valueBool(comment.IsWhole),
		HasMore:         valueBool(comment.HasMore),
		PageToken:       value(comment.PageToken),
		RepliesComplete: !valueBool(comment.HasMore),
	}
	if comment.ReplyList != nil {
		for _, reply := range comment.ReplyList.Replies {
			detail.Replies = appendDriveCommentReply(detail.Replies, reply)
		}
	}
	return detail
}

func appendDriveCommentReply(replies []DriveCommentReply, reply *larkdrive.FileCommentReply) []DriveCommentReply {
	if reply == nil {
		return replies
	}
	return append(replies, DriveCommentReply{
		ReplyID:    value(reply.ReplyId),
		UserID:     value(reply.UserId),
		Text:       textFromDriveReplyContent(reply.Content),
		CreateTime: valueInt(reply.CreateTime),
		UpdateTime: valueInt(reply.UpdateTime),
	})
}

func textFromDriveReplyContent(content *larkdrive.ReplyContent) string {
	if content == nil {
		return ""
	}
	var parts []string
	for _, element := range content.Elements {
		if element == nil || element.Type == nil {
			continue
		}
		switch strings.TrimSpace(*element.Type) {
		case "text_run":
			if element.TextRun != nil && element.TextRun.Text != nil {
				parts = append(parts, strings.TrimSpace(*element.TextRun.Text))
			}
		case "docs_link":
			if element.DocsLink != nil && element.DocsLink.Url != nil {
				parts = append(parts, strings.TrimSpace(*element.DocsLink.Url))
			}
		case "person":
			if element.Person != nil {
				if userID := value(element.Person.UserId); userID != "" {
					parts = append(parts, "@"+userID)
				}
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, ""))
}

func driveReplyContentFromText(text string) *larkdrive.ReplyContent {
	text = strings.TrimSpace(text)
	if text == "" {
		return larkdrive.NewReplyContentBuilder().Build()
	}
	var elements []*larkdrive.ReplyElement
	last := 0
	for _, loc := range driveReplyAtTagPattern.FindAllStringSubmatchIndex(text, -1) {
		if len(loc) < 6 {
			continue
		}
		if loc[0] > last {
			elements = appendDriveReplyTextRun(elements, text[last:loc[0]])
		}
		userID := ""
		if loc[2] >= 0 && loc[3] >= 0 {
			userID = strings.TrimSpace(text[loc[2]:loc[3]])
		} else if loc[4] >= 0 && loc[5] >= 0 {
			userID = strings.TrimSpace(text[loc[4]:loc[5]])
		}
		if userID != "" {
			elements = append(elements, larkdrive.NewReplyElementBuilder().
				Type("person").
				Person(larkdrive.NewPersonBuilder().UserId(userID).Build()).
				Build())
		}
		last = loc[1]
	}
	if last < len(text) {
		elements = appendDriveReplyTextRun(elements, text[last:])
	}
	return larkdrive.NewReplyContentBuilder().Elements(elements).Build()
}

func appendDriveReplyTextRun(elements []*larkdrive.ReplyElement, text string) []*larkdrive.ReplyElement {
	if text == "" {
		return elements
	}
	return append(elements, larkdrive.NewReplyElementBuilder().
		Type("text_run").
		TextRun(larkdrive.NewTextRunBuilder().Text(text).Build()).
		Build())
}

func openIDFromDriveUserID(userID *larkdrive.UserId) string {
	if userID == nil {
		return ""
	}
	if openID := value(userID.OpenId); openID != "" {
		return openID
	}
	if userID := value(userID.UserId); userID != "" {
		return userID
	}
	return value(userID.UnionId)
}

func driveDocumentURL(fileType, fileToken string) string {
	fileType = strings.TrimSpace(fileType)
	fileToken = strings.TrimSpace(fileToken)
	if fileType == "" || fileToken == "" {
		return ""
	}
	pathType := strings.ToLower(fileType)
	switch pathType {
	case "doc":
		pathType = "docs"
	case "sheet":
		pathType = "sheets"
	case "bitable":
		pathType = "base"
	}
	return "https://feishu.cn/" + pathType + "/" + fileToken
}

func driveCommentDedupeID(comment DriveComment) string {
	comment = comment.Normalized()
	return strings.Join([]string{
		"drive_comment",
		comment.FileType,
		comment.FileToken,
		comment.CommentID,
		comment.ReplyID,
	}, "\x00")
}

func driveCommentFromBot(comment DriveComment, detail DriveCommentDetail) bool {
	botOpenID := strings.TrimSpace(comment.BotOpenID)
	if botOpenID == "" {
		return false
	}
	if comment.ReplyID == "" && strings.TrimSpace(detail.UserID) == botOpenID {
		return true
	}
	for _, reply := range detail.Replies {
		if strings.TrimSpace(reply.ReplyID) == comment.ReplyID && strings.TrimSpace(reply.UserID) == botOpenID {
			return true
		}
	}
	return false
}
