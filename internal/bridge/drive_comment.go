package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const sessionSourceDriveComment = "drive_comment"

const driveCommentMissingBodyReply = "处理失败：未获取到评论内容。"

// HandleDriveComment 处理飞书云文档评论事件。
func (s *Service) HandleDriveComment(ctx context.Context, comment feishu.DriveComment) error {
	comment = comment.Normalized()
	if comment.BotID == "" {
		return fmt.Errorf("云文档评论 bot_id 不能为空")
	}
	if !s.DriveCommentEnabled(comment.BotID) {
		slog.InfoContext(ctx, "云文档评论处理未开启，忽略新评论", "bot", comment.BotID, "file_token", comment.FileToken, "comment_id", comment.CommentID)
		return nil
	}
	if comment.FileToken == "" || comment.FileType == "" || comment.CommentID == "" {
		return fmt.Errorf("云文档评论字段不完整")
	}
	if driveCommentUserTextMissing(comment) {
		if !comment.IsMentioned {
			return nil
		}
		return s.replyDriveComment(ctx, comment, driveCommentMissingBodyReply)
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
		if comment.IsMentioned {
			s.replyDriveCommentError(ctx, comment, err)
		}
		return err
	}
	text := strings.TrimSpace(result.Text)
	if driveCommentShouldSuppressReply(comment, text) {
		return nil
	}
	if result.TextSet && text == "" {
		return nil
	}
	if text == "" {
		text = "已完成。"
	}
	return s.replyDriveComment(ctx, comment, text)
}

func (s *Service) replyDriveComment(ctx context.Context, comment feishu.DriveComment, text string) error {
	sent, err := s.replyDriveCommentWithOutbound(ctx, comment, text)
	if err != nil {
		return fmt.Errorf("回复云文档评论: %w", err)
	}
	if !sent {
		slog.WarnContext(ctx, "缺少云文档评论回复发送器", "file_token", comment.FileToken, "comment_id", comment.CommentID)
	}
	return nil
}

func (s *Service) replyDriveCommentError(ctx context.Context, comment feishu.DriveComment, err error) {
	if err == nil {
		return
	}
	sent, replyErr := s.replyDriveCommentWithOutbound(ctx, comment, "处理评论失败："+err.Error())
	if replyErr != nil {
		slog.WarnContext(ctx, "回复云文档评论错误失败", "file_token", comment.FileToken, "comment_id", comment.CommentID, "错误", replyErr)
		return
	}
	if !sent {
		slog.WarnContext(ctx, "缺少云文档评论错误回复发送器", "file_token", comment.FileToken, "comment_id", comment.CommentID)
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
		return TriggerRequest{}, fmt.Errorf("当前默认 agent %s 未配置 default_cwd，不能处理云文档评论", agentName)
	}
	workspace := strings.TrimSpace(comment.Workspace)
	if workspace == "" {
		workspace = s.botWorkspace(comment.BotID)
	}
	req := TriggerRequest{
		BotID:     comment.BotID,
		Key:       driveCommentSessionKey(comment),
		Workspace: workspace,
		AgentName: agentName,
		Cwd:       cwd,
		Title:     driveCommentSessionTitle(comment),
		Prompt:    driveCommentPrompt(comment),
		Metadata:  driveCommentMetadata(comment),
		Sink:      noopTriggerSink{},
	}
	if sink := s.driveCommentTraceSink(comment, cwd); sink != nil {
		req.Sink = sink
	}
	return req, nil
}

func (s *Service) DriveCommentEnabled(botID string) bool {
	bot, ok := s.botConfig(botID)
	return ok && bot.DriveComment.Enabled
}

func (s *Service) driveCommentTraceSink(comment feishu.DriveComment, cwd string) TriggerSink {
	bot, ok := s.botConfig(comment.BotID)
	if !ok || !bot.DriveComment.TraceEnabled || strings.TrimSpace(bot.DriveComment.TraceChatID) == "" {
		return nil
	}
	message := feishu.Message{
		BotID:     comment.BotID,
		ChatID:    strings.TrimSpace(bot.DriveComment.TraceChatID),
		Workspace: strings.TrimSpace(comment.Workspace),
	}
	store := s.storeForBotID(comment.BotID)
	return &driveCommentTraceSink{
		message: message,
		cwd:     cwd,
		comment: comment.Normalized(),
		show:    s.chatConfigForMessage(message),
		store:   store,
		starter: s.scheduleStreamStarter(comment.BotID),
	}
}

func driveCommentSessionKey(comment feishu.DriveComment) SessionKey {
	comment = comment.Normalized()
	return SessionKey{
		BotID:  comment.BotID,
		Source: sessionSourceDriveComment,
		MainID: comment.FileType + ":" + comment.FileToken,
		// 暂时决定按评论隔离 而不是按全文隔离会话, 因为多个段落的评论通常是并行互不影响的
		SubID: comment.CommentID,
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
		promptMetadataSection("## 云文档评论 Metadata", driveCommentOrderedMetadata(comment)),
		driveCommentThreadSection(comment),
		driveCommentInstructions(comment),
	}, driveCommentUserText(comment))
}

func driveCommentInstructions(comment feishu.DriveComment) string {
	lines := []string{
		"## 云文档评论处理规则",
		"",
		"- 只基于本 prompt 提供的评论正文和 metadata 回复。",
		"- 如需更多文档正文上下文，可以使用 lark-cli 读取当前云文档正文。",
		"- 不要调用 lark-cli、飞书 API 或其它工具读取、回复、修改当前云文档评论。",
		"- 如果要回复某条 reply，请使用 `<at id=\"ou_openid\"></at>回复内容` 格式，其中 `ou_openid` 使用该 reply 的 `reply_user_id` 或评论线程中对应回复的 user。",
		"- 不要声明你已经或即将写回评论；bridge 会把你的最终正文写回评论。",
	}
	if comment.Normalized().IsMentioned {
		lines = append(lines,
			"- 本次评论事件提及了当前 bot，必须回复。",
			"- 最终只输出要回复给用户的正文。",
		)
	} else {
		lines = append(lines,
			"- 本次评论事件没有提及当前 bot，请先判断是否需要回复。",
			"- 如果评论线程与当前会话、你的职责或正在处理的任务无关，最终只输出 SILENT。",
			"- 如果需要回复，请正常处理评论线程，不要解释本判断规则。",
		)
	}
	return strings.Join(lines, "\n")
}

func driveCommentUserTextMissing(comment feishu.DriveComment) bool {
	comment = comment.Normalized()
	return comment.CommentText == "" && comment.ReplyText == ""
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

func driveCommentShouldSuppressReply(comment feishu.DriveComment, reply string) bool {
	if comment.Normalized().IsMentioned {
		return false
	}
	reply = strings.TrimSpace(reply)
	return reply == "" || isSilentReplySentinel(reply)
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
		{"is_mentioned", strconv.FormatBool(comment.IsMentioned)},
		{"operator_open_id", comment.OperatorID},
		{"recipient_open_id", comment.RecipientID},
		{"comment_user_id", comment.CommentUserID},
		{"comment_create_time", driveCommentIntMetadata(comment.CommentCreateTime)},
		{"comment_update_time", driveCommentIntMetadata(comment.CommentUpdateTime)},
		{"comment_is_solved", driveCommentBoolMetadata(comment.DetailLoaded, comment.CommentIsSolved)},
		{"comment_is_whole", driveCommentBoolMetadata(comment.DetailLoaded, comment.CommentIsWhole)},
		{"quote", comment.Quote},
		{"comment_content", comment.CommentText},
		{"reply_user_id", comment.ReplyUserID},
		{"reply_create_time", driveCommentIntMetadata(comment.ReplyCreateTime)},
		{"reply_update_time", driveCommentIntMetadata(comment.ReplyUpdateTime)},
		{"reply_content", comment.ReplyText},
		{"reply_count", driveCommentIntMetadata(comment.ReplyCount)},
		{"replies_complete", driveCommentBoolMetadata(comment.DetailLoaded, comment.RepliesComplete)},
		{"document_url", comment.DocumentURL},
	}
}

func driveCommentThreadSection(comment feishu.DriveComment) string {
	comment = comment.Normalized()
	var lines []string
	lines = append(lines, "## 云文档评论线程")
	if comment.DocumentURL != "" {
		lines = append(lines, "", "文档链接："+comment.DocumentURL)
	}
	if comment.Quote != "" {
		lines = append(lines, "", "引用正文：", comment.Quote)
	}
	if comment.CommentText != "" {
		lines = append(lines, "", "评论根内容：")
		lines = append(lines, driveCommentThreadItem("", comment.CommentUserID, comment.CommentCreateTime, comment.CommentUpdateTime, comment.CommentText, comment.ReplyID == ""))
	}
	if len(comment.Replies) > 0 {
		lines = append(lines, "", "评论回复列表：")
		rootIndex := driveCommentRootReplyIndex(comment)
		for i, reply := range comment.Replies {
			if i == rootIndex {
				continue
			}
			current := comment.ReplyID != "" && strings.TrimSpace(reply.ReplyID) == comment.ReplyID
			lines = append(lines, driveCommentThreadItem(strconv.Itoa(i+1)+". "+strings.TrimSpace(reply.ReplyID), reply.UserID, reply.CreateTime, reply.UpdateTime, reply.Text, current))
		}
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func driveCommentRootReplyIndex(comment feishu.DriveComment) int {
	comment = comment.Normalized()
	if comment.CommentText == "" {
		return -1
	}
	for i, reply := range comment.Replies {
		if strings.TrimSpace(reply.Text) != comment.CommentText {
			continue
		}
		if comment.CommentUserID == "" || strings.TrimSpace(reply.UserID) == comment.CommentUserID {
			return i
		}
	}
	return -1
}

func driveCommentThreadItem(id, userID string, createTime int, updateTime int, text string, current bool) string {
	var attrs []string
	if strings.TrimSpace(id) != "" {
		attrs = append(attrs, strings.TrimSpace(id))
	}
	if strings.TrimSpace(userID) != "" {
		attrs = append(attrs, "user="+strings.TrimSpace(userID))
	}
	if createTime != 0 {
		attrs = append(attrs, "create_time="+strconv.Itoa(createTime))
	}
	if updateTime != 0 {
		attrs = append(attrs, "update_time="+strconv.Itoa(updateTime))
	}
	if current {
		attrs = append(attrs, "current_event=true")
	}
	prefix := "- "
	if len(attrs) > 0 {
		prefix += "[" + strings.Join(attrs, ", ") + "] "
	}
	return prefix + strings.TrimSpace(text)
}

func driveCommentIntMetadata(value int) string {
	if value == 0 {
		return ""
	}
	return strconv.Itoa(value)
}

func driveCommentBoolMetadata(known bool, value bool) string {
	if !known {
		return ""
	}
	return strconv.FormatBool(value)
}
