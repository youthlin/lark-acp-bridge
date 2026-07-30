package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const sessionSourceDriveComment = "drive_comment"

// HandleDriveComment 处理飞书 Drive 评论 mention 事件。
func (s *Service) HandleDriveComment(ctx context.Context, comment feishu.DriveComment) error {
	comment = comment.Normalized()
	if !comment.IsMentioned {
		return nil
	}
	if comment.BotID == "" {
		return fmt.Errorf("云文档评论 bot_id 不能为空")
	}
	if comment.FileToken == "" || comment.FileType == "" || comment.CommentID == "" {
		return fmt.Errorf("云文档评论字段不完整")
	}
	if _, err := ensureWorkspace(comment.Workspace, comment.BotID); err != nil {
		return fmt.Errorf("初始化 workspace 失败: %w", err)
	}
	req, err := s.driveCommentTriggerRequest(comment)
	if err != nil {
		return err
	}
	result, err := s.runTriggerPrompt(ctx, req)
	if err != nil {
		s.replyDriveCommentError(ctx, comment, err)
		return err
	}
	text := strings.TrimSpace(result.Text)
	if text == "" {
		text = "已完成。"
	}
	sent, err := feishu.ReplyDriveComment(ctx, comment, text)
	if err != nil {
		return fmt.Errorf("回复 Drive 评论: %w", err)
	}
	if !sent {
		slog.WarnContext(ctx, "缺少 Drive 评论回复发送器", "file_token", comment.FileToken, "comment_id", comment.CommentID)
	}
	return nil
}

func (s *Service) replyDriveCommentError(ctx context.Context, comment feishu.DriveComment, err error) {
	if err == nil {
		return
	}
	sent, replyErr := feishu.ReplyDriveComment(ctx, comment, "处理评论失败："+err.Error())
	if replyErr != nil {
		slog.WarnContext(ctx, "回复 Drive 评论错误失败", "file_token", comment.FileToken, "comment_id", comment.CommentID, "错误", replyErr)
		return
	}
	if !sent {
		slog.WarnContext(ctx, "缺少 Drive 评论错误回复发送器", "file_token", comment.FileToken, "comment_id", comment.CommentID)
	}
}

func (s *Service) driveCommentTriggerRequest(comment feishu.DriveComment) (TriggerRequest, error) {
	comment = comment.Normalized()
	agentName := s.defaultAgentName()
	if agentName == "" {
		return TriggerRequest{}, fmt.Errorf("未配置默认 agent")
	}
	agent, ok := s.registry.Get(agentName)
	if !ok {
		return TriggerRequest{}, fmt.Errorf("未找到默认 agent 配置: %s", agentName)
	}
	cwd := strings.TrimSpace(agent.DefaultCwd)
	if cwd == "" {
		return TriggerRequest{}, fmt.Errorf("当前默认 agent %s 未配置 default_cwd，不能处理 Drive 评论", agentName)
	}
	workspace := strings.TrimSpace(comment.Workspace)
	if workspace == "" {
		workspace = s.botWorkspace(comment.BotID)
	}
	return TriggerRequest{
		BotID:     comment.BotID,
		Key:       driveCommentSessionKey(comment),
		Workspace: workspace,
		AgentName: agentName,
		Cwd:       cwd,
		Title:     driveCommentSessionTitle(comment),
		Prompt:    driveCommentPrompt(comment),
		Metadata:  driveCommentMetadata(comment),
		Sink:      noopTriggerSink{},
	}, nil
}

func driveCommentSessionKey(comment feishu.DriveComment) SessionKey {
	comment = comment.Normalized()
	return SessionKey{
		BotID:  comment.BotID,
		Source: sessionSourceDriveComment,
		MainID: comment.FileType + ":" + comment.FileToken,
		SubID:  comment.CommentID,
	}
}

func driveCommentSessionTitle(comment feishu.DriveComment) string {
	comment = comment.Normalized()
	if comment.FileType == "" || comment.FileToken == "" || comment.CommentID == "" {
		return "云文档评论"
	}
	return "云文档评论 " + comment.FileType + ":" + comment.FileToken + "#" + comment.CommentID
}

func driveCommentPrompt(comment feishu.DriveComment) string {
	return promptWithUserMessage([]string{
		promptMetadataSection("## Drive Comment Metadata", driveCommentOrderedMetadata(comment)),
	}, driveCommentUserText(comment))
}

func driveCommentUserText(comment feishu.DriveComment) string {
	comment = comment.Normalized()
	if comment.ReplyText != "" {
		return comment.ReplyText
	}
	if comment.CommentText != "" {
		return comment.CommentText
	}
	return "（用户在飞书云文档评论中提及你，但本次未读取到评论正文，请结合 metadata 回复。）"
}

func driveCommentMetadata(comment feishu.DriveComment) map[string]string {
	metadata := make(map[string]string)
	for _, field := range driveCommentOrderedMetadata(comment) {
		key := strings.TrimSpace(field.Key)
		value := strings.TrimSpace(field.Value)
		if key != "" && value != "" {
			metadata[key] = value
		}
	}
	return metadata
}

func driveCommentOrderedMetadata(comment feishu.DriveComment) orderedPromptMetadata {
	comment = comment.Normalized()
	return orderedPromptMetadata{
		{"source", sessionSourceDriveComment},
		{"file_token", comment.FileToken},
		{"file_type", comment.FileType},
		{"comment_id", comment.CommentID},
		{"reply_id", comment.ReplyID},
		{"notice_type", comment.NoticeType},
		{"operator_open_id", comment.OperatorID},
		{"recipient_open_id", comment.RecipientID},
		{"comment_content", comment.CommentText},
		{"reply_content", comment.ReplyText},
		{"document_url", comment.DocumentURL},
	}
}
