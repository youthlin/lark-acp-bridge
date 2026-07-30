package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"
	"github.com/youthlin/lark-acp-bridge/internal/logging"
)

const (
	driveCommentDeduperTTL = 30 * time.Minute
	driveCommentDeduperMax = 10000
)

type DriveComment struct {
	BotID       string
	BotOpenID   string
	Workspace   string
	FileToken   string
	FileType    string
	CommentID   string
	ReplyID     string
	NoticeType  string
	OperatorID  string
	RecipientID string
	IsMentioned bool
	CommentText string
	ReplyText   string
	DocumentURL string
}

type DriveCommentDetail struct {
	CommentID string
	UserID    string
	Quote     string
	Replies   []DriveCommentReply
}

type DriveCommentReply struct {
	ReplyID string
	UserID  string
	Text    string
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
	c.CommentText = strings.TrimSpace(c.CommentText)
	c.ReplyText = strings.TrimSpace(c.ReplyText)
	c.DocumentURL = strings.TrimSpace(c.DocumentURL)
	return c
}

func ParseDriveComment(event *larkdrive.P2NoticeCommentAddV1) (DriveComment, error) {
	if event == nil || event.Event == nil {
		return DriveComment{}, fmt.Errorf("飞书 Drive 评论事件为空")
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

func (a *Adapter) handleDriveCommentAdd(ctx context.Context, event *larkdrive.P2NoticeCommentAddV1) error {
	var body []byte
	if event != nil && event.EventReq != nil {
		body = event.Body
	}
	slog.DebugContext(ctx, "Bot收到 Drive 评论事件", "body", eventLogBody(body, event))

	comment, err := ParseDriveComment(event)
	if err != nil {
		slog.WarnContext(ctx, "解析飞书 Drive 评论事件失败", "错误", err)
		slog.DebugContext(ctx, "解析飞书 Drive 评论事件失败详情", "错误", err, "事件", larkcore.Prettify(event))
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

	if !comment.IsMentioned {
		slog.InfoContext(ctx, "跳过未 mention bot 的 Drive 评论事件")
		return nil
	}
	if comment.FileToken == "" || comment.FileType == "" || comment.CommentID == "" {
		slog.WarnContext(ctx, "跳过缺少必要字段的 Drive 评论事件")
		return nil
	}
	if comment.BotOpenID != "" && comment.OperatorID == comment.BotOpenID {
		slog.InfoContext(ctx, "跳过 bot 自己触发的 Drive 评论事件")
		return nil
	}
	if a.deduper != nil {
		allowed, err := a.deduper.Allow(comment.BotID, driveCommentDedupeID(comment))
		if err != nil {
			slog.ErrorContext(ctx, "记录 Drive 评论去重状态失败", "错误", err)
		}
		if !allowed {
			slog.InfoContext(ctx, "跳过重复 Drive 评论事件")
			return nil
		}
	}
	if a.driveComments != nil {
		detail, err := a.driveComments.GetComment(ctx, comment.FileToken, comment.FileType, comment.CommentID)
		if err != nil {
			slog.WarnContext(ctx, "读取 Drive 评论详情失败", "错误", err)
		} else {
			comment = comment.withDetail(detail).Normalized()
			if comment.BotOpenID != "" && driveCommentFromBot(comment, detail) {
				slog.InfoContext(ctx, "跳过 bot 自己的 Drive 评论或回复")
				return nil
			}
		}
	}
	handler, ok := a.handler.(DriveCommentHandler)
	if !ok || handler == nil {
		return nil
	}
	ctx = WithDriveCommentReplySender(ctx, a.ReplyDriveComment)
	if err := handler.HandleDriveComment(ctx, comment); err != nil {
		slog.ErrorContext(ctx, "处理 Drive 评论事件失败", "错误", err)
	}
	return nil
}

func (a *Adapter) ReplyDriveComment(ctx context.Context, comment DriveComment, text string) error {
	if a.driveComments == nil {
		slog.WarnContext(ctx, "缺少 Drive 评论回复 client", "comment_id", comment.CommentID)
		return nil
	}
	return a.driveComments.ReplyComment(ctx, comment, text)
}

func (c DriveComment) withDetail(detail DriveCommentDetail) DriveComment {
	detail.CommentID = strings.TrimSpace(detail.CommentID)
	if detail.CommentID != "" {
		c.CommentID = detail.CommentID
	}
	for _, reply := range detail.Replies {
		reply.ReplyID = strings.TrimSpace(reply.ReplyID)
		if reply.ReplyID == "" {
			continue
		}
		if c.CommentText == "" {
			c.CommentText = reply.Text
		}
		if c.ReplyID != "" && reply.ReplyID == c.ReplyID {
			c.ReplyText = reply.Text
			break
		}
	}
	return c
}

func driveCommentDetailFromGetResp(data *larkdrive.GetFileCommentRespData) DriveCommentDetail {
	if data == nil {
		return DriveCommentDetail{}
	}
	detail := DriveCommentDetail{
		CommentID: value(data.CommentId),
		UserID:    value(data.UserId),
		Quote:     value(data.Quote),
	}
	if data.ReplyList != nil {
		for _, reply := range data.ReplyList.Replies {
			if reply == nil {
				continue
			}
			detail.Replies = append(detail.Replies, DriveCommentReply{
				ReplyID: value(reply.ReplyId),
				UserID:  value(reply.UserId),
				Text:    textFromDriveReplyContent(reply.Content),
			})
		}
	}
	return detail
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
			if element.Person != nil && element.Person.UserId != nil {
				parts = append(parts, "@"+strings.TrimSpace(*element.Person.UserId))
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, ""))
}

func driveReplyContentFromText(text string) *larkdrive.ReplyContent {
	return larkdrive.NewReplyContentBuilder().Elements([]*larkdrive.ReplyElement{
		larkdrive.NewReplyElementBuilder().
			Type("text_run").
			TextRun(larkdrive.NewTextRunBuilder().Text(strings.TrimSpace(text)).Build()).
			Build(),
	}).Build()
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
