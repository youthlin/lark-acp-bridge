package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

const (
	newChatModeGroup = "group"
	newChatModeTopic = "topic"
)

type newChatRequest struct {
	Mode              string
	Name              string
	OwnerOpenID       string
	InitialUserOpenID []string
	ExtraUserOpenIDs  []string
	SkippedMentions   []string
}

func (s *Service) handleNewCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	if len(fields) >= 2 && strings.EqualFold(strings.TrimSpace(fields[1]), "chat") {
		return s.handleNewChatCommand(ctx, text, msg)
	}
	return s.newSession(ctx, fields, msg)
}

func (s *Service) handleNewChatCommand(ctx context.Context, text string, msg feishu.Message) string {
	req, errText := parseNewChatRequest(text, msg)
	if errText != "" {
		return errText
	}
	chat, sent, err := s.createChat(ctx, msg, feishu.CreateChatRequest{
		Name:          req.Name,
		Mode:          req.Mode,
		ChatType:      "private",
		OwnerOpenID:   req.OwnerOpenID,
		UserOpenIDs:   req.InitialUserOpenID,
		SetBotManager: true,
	})
	if err != nil {
		return "创建群聊失败：" + err.Error()
	}
	if !sent {
		return "当前上下文不支持创建群聊。"
	}
	if strings.TrimSpace(chat.ChatID) == "" {
		return "创建群聊失败：接口未返回 chat_id。"
	}
	var addResult feishu.AddChatMembersResult
	if len(req.ExtraUserOpenIDs) > 0 {
		var added bool
		addResult, added, err = s.addChatMembers(ctx, msg, feishu.AddChatMembersRequest{
			ChatID:      chat.ChatID,
			UserOpenIDs: req.ExtraUserOpenIDs,
		})
		if err != nil {
			return formatNewChatReply(chat, req, nil, "群已创建，但拉人入群失败："+err.Error())
		}
		if !added {
			return formatNewChatReply(chat, req, nil, "群已创建，但当前上下文不支持拉人入群。")
		}
	}
	return formatNewChatReply(chat, req, &addResult, "")
}

func parseNewChatRequest(text string, msg feishu.Message) (newChatRequest, string) {
	senderID := strings.TrimSpace(msg.SenderID)
	if senderID == "" {
		return newChatRequest{}, "当前消息缺少发送者 open_id，不能创建群聊。"
	}
	args := commandRemainder(text, 2)
	mode, titleText := parseNewChatMode(args)
	title := stripNewChatMentions(titleText, msg.Mentions)
	req := newChatRequest{
		Mode:              mode,
		Name:              normalizeNewChatName(title),
		OwnerOpenID:       senderID,
		InitialUserOpenID: []string{senderID},
	}
	req.ExtraUserOpenIDs, req.SkippedMentions = newChatMentionOpenIDs(msg)
	return req, ""
}

func parseNewChatMode(args string) (string, string) {
	args = strings.TrimSpace(args)
	if args == "" {
		return newChatModeGroup, ""
	}
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return newChatModeGroup, ""
	}
	switch strings.ToLower(strings.TrimSpace(fields[0])) {
	case "group", "普通":
		return newChatModeGroup, commandRemainder(args, 1)
	case "topic", "话题":
		return newChatModeTopic, commandRemainder(args, 1)
	default:
		return newChatModeGroup, args
	}
}

func normalizeNewChatName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return strings.Join(strings.Fields(name), " ")
}

func newChatMentionOpenIDs(msg feishu.Message) ([]string, []string) {
	seen := map[string]struct{}{
		strings.TrimSpace(msg.SenderID):  {},
		strings.TrimSpace(msg.BotOpenID): {},
	}
	var ids []string
	var skipped []string
	skippedSeen := make(map[string]struct{})
	for _, mention := range msg.Mentions {
		id := strings.TrimSpace(mention.ID)
		if id != "" && id == strings.TrimSpace(msg.BotOpenID) {
			continue
		}
		if mention.Type != "" && !strings.EqualFold(strings.TrimSpace(mention.Type), "user") {
			label := newChatMentionLabel(mention)
			if label != "" {
				if _, ok := skippedSeen[label]; !ok {
					skippedSeen[label] = struct{}{}
					skipped = append(skipped, label)
				}
			}
			continue
		}
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, skipped
}

func newChatMentionLabel(mention feishu.Mention) string {
	name := strings.TrimSpace(mention.Name)
	id := strings.TrimSpace(mention.ID)
	switch {
	case name != "" && id != "":
		return name + "(" + id + ")"
	case name != "":
		return name
	default:
		return id
	}
}

func stripNewChatMentions(text string, mentions []feishu.Mention) string {
	for _, mention := range mentions {
		if key := strings.TrimSpace(mention.Key); key != "" {
			text = strings.ReplaceAll(text, key, "")
		}
		id := strings.TrimSpace(mention.ID)
		if name := strings.TrimSpace(mention.Name); name != "" {
			if id != "" {
				text = strings.ReplaceAll(text, "@"+name+"("+id+")", "")
			}
			text = strings.ReplaceAll(text, "@"+name, "")
		}
		if id != "" {
			text = strings.ReplaceAll(text, "("+id+")", "")
		}
	}
	return strings.TrimSpace(text)
}

func formatNewChatReply(chat feishu.CreatedChat, req newChatRequest, addResult *feishu.AddChatMembersResult, warning string) string {
	var lines []string
	if strings.TrimSpace(warning) != "" {
		lines = append(lines, warning)
	} else {
		lines = append(lines, "已创建群聊。")
	}
	lines = append(lines,
		"类型："+displayNewChatMode(req.Mode),
		"chat_id："+strings.TrimSpace(chat.ChatID),
		"群主："+strings.TrimSpace(req.OwnerOpenID),
	)
	if strings.TrimSpace(req.Name) != "" {
		lines = append(lines, "群名："+strings.TrimSpace(req.Name))
	} else if strings.TrimSpace(chat.Name) != "" {
		lines = append(lines, "群名："+strings.TrimSpace(chat.Name))
	}
	if len(req.ExtraUserOpenIDs) == 0 {
		lines = append(lines, "额外成员：无")
		if len(req.SkippedMentions) > 0 {
			lines = append(lines, "跳过非用户 mention："+strings.Join(req.SkippedMentions, ", "))
		}
		return strings.Join(lines, "\n")
	}
	if addResult == nil {
		lines = append(lines,
			fmt.Sprintf("额外成员：未完成 0/%d", len(req.ExtraUserOpenIDs)),
			"待确认："+strings.Join(req.ExtraUserOpenIDs, ", "),
		)
		return strings.Join(lines, "\n")
	}
	failed := failedNewChatMemberOpenIDs(addResult)
	successCount := len(req.ExtraUserOpenIDs) - len(failed)
	if successCount < 0 {
		successCount = 0
	}
	lines = append(lines, fmt.Sprintf("额外成员：成功 %d/%d", successCount, len(req.ExtraUserOpenIDs)))
	if len(failed) > 0 {
		lines = append(lines, "未拉入："+strings.Join(failed, ", "))
	}
	if len(req.SkippedMentions) > 0 {
		lines = append(lines, "跳过非用户 mention："+strings.Join(req.SkippedMentions, ", "))
	}
	return strings.Join(lines, "\n")
}

func displayNewChatMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), newChatModeTopic) {
		return "话题群"
	}
	return "普通群"
}

func failedNewChatMemberOpenIDs(result *feishu.AddChatMembersResult) []string {
	if result == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var failed []string
	for _, ids := range [][]string{result.InvalidOpenIDs, result.NotExistedOpenIDs, result.PendingApprovalOpenIDs} {
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			failed = append(failed, id)
		}
	}
	return failed
}
