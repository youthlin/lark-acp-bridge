package bridge

import (
	"context"
	"log/slog"
	"maps"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func (s *Service) subscribeACPStateUpdates(ctx context.Context, msg feishu.Message, key SessionKey) {
	s.acpUpdateMu.Lock()
	if old := s.acpUpdateUnsub[key]; old != nil {
		old()
	}
	unsub := s.runtime.SubscribeUpdates(key, func(sessionID string, update acp.SessionUpdate) {
		s.handleACPStateUpdate(ctx, msg, key, sessionID, update)
	})
	s.acpUpdateUnsub[key] = unsub
	s.acpUpdateMu.Unlock()
}

func (s *Service) handleACPStateUpdate(ctx context.Context, msg feishu.Message, key SessionKey, sessionID string, update acp.SessionUpdate) {
	if !isACPStateUpdate(update) {
		return
	}
	store := s.storeForMessage(msg)
	if store == nil {
		return
	}
	session, ok := store.Get(key)
	if !ok || session.ACPSessionID != sessionID {
		return
	}
	changed := applyACPStateUpdate(&session, update)
	if !changed {
		return
	}
	if err := store.Upsert(session); err != nil {
		slog.WarnContext(ctx, "保存 ACP session 状态失败", "session", sessionID, "update", update.SessionUpdate, "错误", err)
	}
}

func (s *Service) saveSessionState(ctx context.Context, msg feishu.Message, session Session) {
	store := s.storeForMessage(msg)
	if store == nil {
		return
	}
	if err := store.Upsert(session); err != nil {
		slog.WarnContext(ctx, "保存会话状态失败", "session", session.ACPSessionID, "错误", err)
	}
}

func isACPStateUpdate(update acp.SessionUpdate) bool {
	switch update.SessionUpdate {
	case "available_commands_update", "config_option_update", "session_info_update":
		return true
	case "current_mode_update":
		return strings.TrimSpace(update.ModeID) != ""
	default:
		return update.Models != nil || update.Mode != nil
	}
}

func applyACPStateUpdate(session *Session, update acp.SessionUpdate) bool {
	if session == nil {
		return false
	}
	changed := false
	switch update.SessionUpdate {
	case "available_commands_update":
		session.AvailableCommands = append([]acp.AvailableCommand(nil), update.AvailableCommands...)
		changed = true
	case "config_option_update":
		session.ConfigOptions = append([]acp.SessionConfigOption(nil), update.ConfigOptions...)
		changed = true
	case "session_info_update":
		if update.TitleSet && !session.ManualTitle && session.Title != update.Title {
			session.Title = update.Title
			changed = true
		}
		if update.UpdatedAtSet && session.ACPUpdatedAt != update.UpdatedAt {
			session.ACPUpdatedAt = update.UpdatedAt
			changed = true
		}
		if update.Meta != nil {
			session.ACPMeta = maps.Clone(update.Meta)
			changed = true
		}
	case "current_mode_update":
		modeID := strings.TrimSpace(update.ModeID)
		if modeID != "" {
			if session.Mode == nil {
				session.Mode = &acp.SessionModeState{}
			}
			if session.Mode.CurrentModeID != modeID {
				session.Mode.CurrentModeID = modeID
				changed = true
			}
		}
	}
	if update.Models != nil {
		models := *update.Models
		models.AvailableModels = append([]acp.SessionModel(nil), update.Models.AvailableModels...)
		session.Models = &models
		changed = true
	}
	if update.Mode != nil {
		mode := *update.Mode
		mode.AvailableModes = append([]acp.SessionMode(nil), update.Mode.AvailableModes...)
		session.Mode = &mode
		changed = true
	}
	return changed
}
