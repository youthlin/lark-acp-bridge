package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"
)

type larkDriveCommentClient struct {
	client *lark.Client
}

func (c larkDriveCommentClient) GetComment(ctx context.Context, fileToken, fileType, commentID string) (DriveCommentDetail, error) {
	if c.client == nil {
		return DriveCommentDetail{}, fmt.Errorf("飞书客户端未初始化")
	}
	fileToken = strings.TrimSpace(fileToken)
	fileType = strings.TrimSpace(fileType)
	commentID = strings.TrimSpace(commentID)
	if fileToken == "" || fileType == "" || commentID == "" {
		return DriveCommentDetail{}, fmt.Errorf("云文档评论参数不完整")
	}
	body := larkdrive.NewBatchQueryFileCommentReqBodyBuilder().
		CommentIds([]string{commentID}).
		Build()
	req := larkdrive.NewBatchQueryFileCommentReqBuilder().
		FileToken(fileToken).
		FileType(fileType).
		UserIdType(larkdrive.UserIdTypeBatchQueryFileCommentOpenId).
		Body(body).
		Build()
	resp, err := c.client.Drive.V1.FileComment.BatchQuery(ctx, req)
	_ = c.client.Drive.V1.FileComment.Get // 这个Get是只能获取指定的「全文评论」 不是侧边栏局部评论
	if err != nil {
		return DriveCommentDetail{}, fmt.Errorf("调用飞书获取云文档评论接口: %w", err)
	}
	if resp == nil || !resp.Success() {
		code, msg := 0, ""
		if resp != nil {
			code, msg = resp.Code, resp.Msg
		}
		return DriveCommentDetail{}, fmt.Errorf("飞书获取云文档评论接口返回错误: code=%d msg=%s", code, msg)
	}
	if resp.Data == nil {
		return DriveCommentDetail{}, fmt.Errorf("飞书获取云文档评论接口未返回数据")
	}
	for _, item := range resp.Data.Items {
		if item == nil || strings.TrimSpace(value(item.CommentId)) != commentID {
			continue
		}
		detail := driveCommentDetailFromFileComment(item)
		if detail.HasMore {
			replies, err := c.listCommentReplies(ctx, fileToken, fileType, commentID)
			if err != nil {
				slog.WarnContext(ctx, "读取云文档评论回复列表失败，使用批量接口返回的部分回复", "错误", err)
				return detail, nil
			}
			detail.Replies = replies
			detail.HasMore = false
			detail.PageToken = ""
			detail.RepliesComplete = true
		}
		return detail, nil
	}
	return DriveCommentDetail{}, fmt.Errorf("飞书获取云文档评论接口未返回评论: comment_id=%s", commentID)
}

func (c larkDriveCommentClient) listCommentReplies(ctx context.Context, fileToken, fileType, commentID string) ([]DriveCommentReply, error) {
	var replies []DriveCommentReply
	pageToken := ""
	for {
		builder := larkdrive.NewListFileCommentReplyReqBuilder().
			FileToken(fileToken).
			CommentId(commentID).
			FileType(fileType).
			PageSize(100).
			UserIdType(larkdrive.UserIdTypeListFileCommentReplyOpenId)
		if pageToken != "" {
			builder.PageToken(pageToken)
		}
		resp, err := c.client.Drive.V1.FileCommentReply.List(ctx, builder.Build())
		if err != nil {
			return nil, fmt.Errorf("调用飞书获取云文档评论回复接口: %w", err)
		}
		if resp == nil || !resp.Success() {
			code, msg := 0, ""
			if resp != nil {
				code, msg = resp.Code, resp.Msg
			}
			return nil, fmt.Errorf("飞书获取云文档评论回复接口返回错误: code=%d msg=%s", code, msg)
		}
		if resp.Data == nil {
			return nil, fmt.Errorf("飞书获取云文档评论回复接口未返回数据")
		}
		for _, reply := range resp.Data.Items {
			replies = appendDriveCommentReply(replies, reply)
		}
		if !valueBool(resp.Data.HasMore) {
			return replies, nil
		}
		pageToken = value(resp.Data.PageToken)
		if pageToken == "" {
			return replies, nil
		}
	}
}

func (c larkDriveCommentClient) ReplyComment(ctx context.Context, comment DriveComment, text string) error {
	if c.client == nil {
		return fmt.Errorf("飞书客户端未初始化")
	}
	comment = comment.Normalized()
	text = strings.TrimSpace(text)
	if comment.FileToken == "" || comment.FileType == "" || comment.CommentID == "" {
		return fmt.Errorf("Drive 评论参数不完整")
	}
	if text == "" {
		return nil
	}
	body := larkdrive.NewCreateFileCommentReplyReqBodyBuilder().
		Content(driveReplyContentFromText(text)).
		Build()
	req := larkdrive.NewCreateFileCommentReplyReqBuilder().
		FileToken(comment.FileToken).
		CommentId(comment.CommentID).
		FileType(comment.FileType).
		UserIdType(larkdrive.UserIdTypeCreateFileCommentReplyOpenId).
		Body(body).
		Build()
	resp, err := c.client.Drive.V1.FileCommentReply.Create(ctx, req)
	if err != nil {
		return fmt.Errorf("调用飞书回复云文档评论接口: %w", err)
	}
	if resp == nil || !resp.Success() {
		code, msg := 0, ""
		if resp != nil {
			code, msg = resp.Code, resp.Msg
		}
		return fmt.Errorf("飞书回复云文档评论接口返回错误: code=%d msg=%s", code, msg)
	}
	return nil
}
