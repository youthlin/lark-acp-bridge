package bridge

import (
	"context"
	"log/slog"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

type intermediateReplySender interface {
	SendText(context.Context, feishu.Message, string) error
}

type sentMessageSender interface {
	SendTextMessage(context.Context, feishu.Message, string) (feishu.SentMessage, error)
}

type shareChatSender interface {
	SendShareChatMessage(context.Context, feishu.Message, string) (feishu.SentMessage, error)
}

type textMessageUpdater interface {
	UpdateText(context.Context, string, string) error
}

type loopStatusCardSender interface {
	SendLoopStatusCard(context.Context, feishu.Message, feishu.LoopStatusCardRequest) (feishu.LoopStatusCard, error)
}

type streamCardStarter interface {
	StartStreamCard(context.Context, feishu.Message, feishu.StreamCardOptions) (feishu.StreamCard, error)
}

type streamCardStarterFunc func(context.Context, feishu.Message, feishu.StreamCardOptions) (feishu.StreamCard, error)

func (f streamCardStarterFunc) StartStreamCard(ctx context.Context, msg feishu.Message, options feishu.StreamCardOptions) (feishu.StreamCard, error) {
	if f == nil {
		return nil, nil
	}
	return f(ctx, msg, options)
}

type permissionRequester interface {
	RequestPermission(context.Context, feishu.Message, acp.PermissionRequest) (acp.PermissionOutcome, error)
}

// triggerPermissionRequester 向指定 open_id（bot owner）发送私聊权限卡片。
type triggerPermissionRequester interface {
	RequestPermissionForOpenID(ctx context.Context, targetOpenID string, source string, req acp.PermissionRequest) (acp.PermissionOutcome, error)
}

type processingReactionStarter interface {
	StartProcessingReaction(context.Context, feishu.Message) func()
}

type modelSelectionCardSender interface {
	SendModelSelectionCard(context.Context, feishu.Message, feishu.ModelSelectionCard) error
}

type modeSelectionCardSender interface {
	SendModeSelectionCard(context.Context, feishu.Message, feishu.ModeSelectionCard) error
}

type sessionSelectionCardSender interface {
	SendSessionSelectionCard(context.Context, feishu.Message, feishu.SessionSelectionCard) error
}

type configDetailCardSender interface {
	SendConfigDetailCard(context.Context, feishu.Message, feishu.ConfigDetailCard) error
}

type overviewCardSender interface {
	SendOverviewCard(context.Context, feishu.Message, feishu.OverviewCard) error
}

type driveCommentReplier interface {
	ReplyDriveComment(context.Context, feishu.DriveComment, string) error
}

type driveCommentTraceChatCreator interface {
	CreateDriveCommentTraceChat(context.Context, feishu.CreateDriveCommentTraceChatRequest) (feishu.CreatedChat, error)
}

type chatCreator interface {
	CreateChat(context.Context, feishu.CreateChatRequest) (feishu.CreatedChat, error)
}

type chatMemberAdder interface {
	AddChatMembers(context.Context, feishu.AddChatMembersRequest) (feishu.AddChatMembersResult, error)
}

type driveCommentTraceBotNameProvider interface {
	DriveCommentTraceBotName(context.Context) (string, error)
}

func (s *Service) HandleFeishuMessageWithOutbound(ctx context.Context, msg feishu.Message, outbound feishu.Outbound) (string, error) {
	s.setOutbound(msg.BotID, outbound)
	return s.HandleFeishuMessage(ctx, msg)
}

func (s *Service) HandleDriveCommentWithOutbound(ctx context.Context, comment feishu.DriveComment, outbound feishu.Outbound) error {
	s.setOutbound(comment.BotID, outbound)
	return s.HandleDriveComment(ctx, comment)
}

func (s *Service) setOutbound(botID string, outbound feishu.Outbound) {
	if s == nil || outbound == nil {
		return
	}
	botID = strings.TrimSpace(botID)
	s.outboundMu.Lock()
	defer s.outboundMu.Unlock()
	if s.outbounds == nil {
		s.outbounds = make(map[string]feishu.Outbound)
	}
	s.outbounds[botID] = outbound
}

func (s *Service) setOutboundIfAbsent(botID string, outbound feishu.Outbound) {
	if s == nil || outbound == nil {
		return
	}
	botID = strings.TrimSpace(botID)
	s.outboundMu.Lock()
	defer s.outboundMu.Unlock()
	if s.outbounds == nil {
		s.outbounds = make(map[string]feishu.Outbound)
	}
	if s.outbounds[botID] == nil {
		s.outbounds[botID] = outbound
	}
}

func (s *Service) outboundForBot(botID string) feishu.Outbound {
	if s == nil {
		return nil
	}
	botID = strings.TrimSpace(botID)
	s.outboundMu.Lock()
	defer s.outboundMu.Unlock()
	return s.outbounds[botID]
}

func (s *Service) sendIntermediateReply(ctx context.Context, msg feishu.Message, text string) (bool, error) {
	sender, ok := s.outboundForBot(msg.BotID).(intermediateReplySender)
	if !ok || sender == nil {
		return false, nil
	}
	return true, sender.SendText(ctx, msg, text)
}

func (s *Service) sendTextMessageOutbound(ctx context.Context, msg feishu.Message, text string) (feishu.SentMessage, bool, error) {
	sender, ok := s.outboundForBot(msg.BotID).(sentMessageSender)
	if !ok || sender == nil {
		return feishu.SentMessage{}, false, nil
	}
	sent, err := sender.SendTextMessage(ctx, msg, text)
	return sent, true, err
}

func (s *Service) sendShareChatOutbound(ctx context.Context, msg feishu.Message, chatID string) (feishu.SentMessage, bool, error) {
	sender, ok := s.outboundForBot(msg.BotID).(shareChatSender)
	if !ok || sender == nil {
		return feishu.SentMessage{}, false, nil
	}
	sent, err := sender.SendShareChatMessage(ctx, msg, chatID)
	return sent, true, err
}

func (s *Service) updateTextMessageOutbound(ctx context.Context, msg feishu.Message, messageID string, text string) (bool, error) {
	updater, ok := s.outboundForBot(msg.BotID).(textMessageUpdater)
	if !ok || updater == nil {
		return false, nil
	}
	return true, updater.UpdateText(ctx, messageID, text)
}

func (s *Service) sendLoopStatusCard(ctx context.Context, msg feishu.Message, request feishu.LoopStatusCardRequest) (feishu.LoopStatusCard, bool, error) {
	sender, ok := s.outboundForBot(msg.BotID).(loopStatusCardSender)
	if !ok || sender == nil {
		return nil, false, nil
	}
	card, err := sender.SendLoopStatusCard(ctx, msg, request)
	return card, true, err
}

func (s *Service) streamCardStarterForMessage(msg feishu.Message) streamCardStarter {
	starter, ok := s.outboundForBot(msg.BotID).(streamCardStarter)
	if !ok || starter == nil {
		return nil
	}
	return starter
}

func (s *Service) requestPermission(ctx context.Context, msg feishu.Message, req acp.PermissionRequest) (acp.PermissionOutcome, bool, error) {
	requester, ok := s.outboundForBot(msg.BotID).(permissionRequester)
	if !ok || requester == nil {
		return acp.PermissionOutcome{}, false, nil
	}
	outcome, err := requester.RequestPermission(ctx, msg, req)
	return outcome, true, err
}

func (s *Service) startProcessingReaction(ctx context.Context, msg feishu.Message) (func(), bool) {
	starter, ok := s.outboundForBot(msg.BotID).(processingReactionStarter)
	if !ok || starter == nil {
		return nil, false
	}
	cleanup := starter.StartProcessingReaction(ctx, msg)
	if cleanup == nil {
		cleanup = func() {}
	}
	return cleanup, true
}

func (s *Service) sendModelSelectionCardOutbound(ctx context.Context, msg feishu.Message, card feishu.ModelSelectionCard) (bool, error) {
	sender, ok := s.outboundForBot(msg.BotID).(modelSelectionCardSender)
	if !ok || sender == nil {
		return false, nil
	}
	return true, sender.SendModelSelectionCard(ctx, msg, card)
}

func (s *Service) sendModeSelectionCardOutbound(ctx context.Context, msg feishu.Message, card feishu.ModeSelectionCard) (bool, error) {
	sender, ok := s.outboundForBot(msg.BotID).(modeSelectionCardSender)
	if !ok || sender == nil {
		return false, nil
	}
	return true, sender.SendModeSelectionCard(ctx, msg, card)
}

func (s *Service) sendSessionSelectionCardOutbound(ctx context.Context, msg feishu.Message, card feishu.SessionSelectionCard) (bool, error) {
	sender, ok := s.outboundForBot(msg.BotID).(sessionSelectionCardSender)
	if !ok || sender == nil {
		return false, nil
	}
	return true, sender.SendSessionSelectionCard(ctx, msg, card)
}

func (s *Service) sendConfigDetailCardOutbound(ctx context.Context, msg feishu.Message, card feishu.ConfigDetailCard) (bool, error) {
	sender, ok := s.outboundForBot(msg.BotID).(configDetailCardSender)
	if !ok || sender == nil {
		return false, nil
	}
	return true, sender.SendConfigDetailCard(ctx, msg, card)
}

func (s *Service) sendOverviewCardOutbound(ctx context.Context, msg feishu.Message, card feishu.OverviewCard) (bool, error) {
	sender, ok := s.outboundForBot(msg.BotID).(overviewCardSender)
	if !ok || sender == nil {
		return false, nil
	}
	return true, sender.SendOverviewCard(ctx, msg, card)
}

func (s *Service) replyDriveCommentWithOutbound(ctx context.Context, comment feishu.DriveComment, text string) (bool, error) {
	replier, ok := s.outboundForBot(comment.BotID).(driveCommentReplier)
	if !ok || replier == nil {
		return false, nil
	}
	return true, replier.ReplyDriveComment(ctx, comment, text)
}

func (s *Service) createDriveCommentTraceChat(ctx context.Context, msg feishu.Message, req feishu.CreateDriveCommentTraceChatRequest) (feishu.CreatedChat, bool, error) {
	creator, ok := s.outboundForBot(msg.BotID).(driveCommentTraceChatCreator)
	if !ok || creator == nil {
		return feishu.CreatedChat{}, false, nil
	}
	chat, err := creator.CreateDriveCommentTraceChat(ctx, req)
	return chat, true, err
}

func (s *Service) createChat(ctx context.Context, msg feishu.Message, req feishu.CreateChatRequest) (feishu.CreatedChat, bool, error) {
	creator, ok := s.outboundForBot(msg.BotID).(chatCreator)
	if !ok || creator == nil {
		return feishu.CreatedChat{}, false, nil
	}
	chat, err := creator.CreateChat(ctx, req)
	return chat, true, err
}

func (s *Service) addChatMembers(ctx context.Context, msg feishu.Message, req feishu.AddChatMembersRequest) (feishu.AddChatMembersResult, bool, error) {
	adder, ok := s.outboundForBot(msg.BotID).(chatMemberAdder)
	if !ok || adder == nil {
		return feishu.AddChatMembersResult{}, false, nil
	}
	result, err := adder.AddChatMembers(ctx, req)
	return result, true, err
}

func (s *Service) driveCommentTraceBotName(ctx context.Context, botID string) (string, bool, error) {
	provider, ok := s.outboundForBot(botID).(driveCommentTraceBotNameProvider)
	if !ok || provider == nil {
		return "", false, nil
	}
	name, err := provider.DriveCommentTraceBotName(ctx)
	return strings.TrimSpace(name), true, err
}

func (s *Service) mustSendIntermediateReply(ctx context.Context, msg feishu.Message, text string, missingLog string) error {
	ok, err := s.sendIntermediateReply(ctx, msg, text)
	if err != nil {
		return err
	}
	if !ok {
		slog.WarnContext(ctx, missingLog)
	}
	return nil
}
