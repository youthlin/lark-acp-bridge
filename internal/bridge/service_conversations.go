package bridge

import (
	"context"

	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

// Service 保留会话 facade，具体的会话选择和生命周期由 conversationManager 负责。
func (s *Service) newSession(ctx context.Context, fields []string, msg feishu.Message) string {
	return s.conversationManager.newSession(ctx, fields, msg)
}

func (s *Service) createSession(ctx context.Context, fields []string, msg feishu.Message) (Session, config.AgentConfig, string, string) {
	return s.conversationManager.createSession(ctx, fields, msg)
}

func (s *Service) createForkSession(ctx context.Context, target feishu.Message, source Session, origin SessionForkOrigin, title string) (Session, config.AgentConfig, error) {
	return s.conversationManager.createForkSession(ctx, target, source, origin, title)
}

func (s *Service) inheritNewSessionConfig(ctx context.Context, msg feishu.Message, session Session, inherited inheritedSessionConfig) Session {
	return s.conversationManager.inheritNewSessionConfig(ctx, msg, session, inherited)
}

func (s *Service) refreshACPSession(ctx context.Context, msg feishu.Message, session Session, agent config.AgentConfig) (Session, error) {
	return s.conversationManager.refreshACPSession(ctx, msg, session, agent)
}

func (s *Service) resumeSessionByID(ctx context.Context, msg feishu.Message, acpSessionID string, expectedCurrentACPSessionID *string) (Session, string) {
	return s.conversationManager.resumeSessionByID(ctx, msg, acpSessionID, expectedCurrentACPSessionID)
}

func (s *Service) selectionSession(msg feishu.Message, acpSessionID string, expiredMessage string) (Session, error) {
	return s.conversationManager.selectionSession(msg, acpSessionID, expiredMessage)
}

func (s *Service) defaultNewSessionCwd(msg feishu.Message) (string, string, string) {
	return s.conversationManager.defaultNewSessionCwd(msg)
}

func (s *Service) resolveNewSessionCwdArg(arg string, msg feishu.Message) (string, bool, string) {
	return s.conversationManager.resolveNewSessionCwdArg(arg, msg)
}

func (s *Service) updateAutomaticSessionTitle(ctx context.Context, msg feishu.Message, session Session, userText string) Session {
	return s.conversationManager.updateAutomaticSessionTitle(ctx, msg, session, userText)
}

func (s *Service) defaultAgentName() string {
	return s.conversationManager.defaultAgentName()
}

func (s *Service) chatAgentName(msg feishu.Message) string {
	return s.conversationManager.chatAgentName(msg)
}

func (s *Service) storeForMessage(msg feishu.Message) *SessionStore {
	return s.conversationManager.storeForMessage(msg)
}

func (s *Service) findSession(msg feishu.Message) (Session, bool) {
	return s.conversationManager.findSession(msg)
}

func (s *Service) chatConfigForMessage(msg feishu.Message) ChatConfig {
	return s.conversationManager.chatConfigForMessage(msg)
}
