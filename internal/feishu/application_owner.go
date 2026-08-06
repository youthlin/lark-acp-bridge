package feishu

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

func (a *Adapter) resolveBotOpenID(ctx context.Context) {
	if strings.TrimSpace(a.cfg.BotOpenID) != "" || a.applications == nil {
		a.cfg.BotOpenID = strings.TrimSpace(a.cfg.BotOpenID)
		return
	}
	info, err := a.applications.GetBotInfo(ctx)
	if err != nil {
		slog.Warn("获取飞书机器人 open_id 失败，群聊 at 过滤需要手动配置 bot_open_id", "bot", a.cfg.ID, "err", err)
		return
	}
	openID := strings.TrimSpace(info.OpenID)
	if openID == "" {
		slog.Warn("飞书机器人 open_id 为空，群聊 at 过滤需要手动配置 bot_open_id", "bot", a.cfg.ID)
		return
	}
	a.cfg.BotOpenID = openID
	slog.Info("已解析飞书机器人 open_id", "bot", a.cfg.ID)
}

func (a *Adapter) DriveCommentTraceBotName(ctx context.Context) (string, error) {
	if a == nil || a.applications == nil {
		return "", nil
	}
	info, botErr := a.applications.GetBotInfo(ctx)
	if botErr == nil {
		if name := strings.TrimSpace(info.Name); name != "" {
			return name, nil
		}
	} else {
		slog.WarnContext(ctx, "获取飞书机器人名称失败，将继续尝试应用名称", "bot", a.cfg.ID, "err", botErr)
	}
	app, appErr := a.applications.GetApplication(ctx)
	if appErr == nil {
		return strings.TrimSpace(app.AppName), nil
	}
	if botErr != nil {
		return "", fmt.Errorf("获取机器人信息: %w; 获取应用信息: %w", botErr, appErr)
	}
	return "", fmt.Errorf("获取应用信息: %w", appErr)
}

func (a *Adapter) resolveOwnerOpenIDs(ctx context.Context) {
	if len(a.cfg.OwnerOpenIDs) > 0 || a.applications == nil {
		return
	}
	owners, err := a.fetchOwnerOpenIDs(ctx)
	if err != nil {
		slog.Warn("获取飞书应用协作者失败，群聊权限卡片需要手动配置 bot owner", "bot", a.cfg.ID, "err", err)
		return
	}
	if len(owners) == 0 {
		slog.Warn("飞书应用协作者中未解析到 bot owner，群聊权限卡片需要手动配置 bot owner", "bot", a.cfg.ID)
		return
	}
	a.cfg.OwnerOpenIDs = owners
	slog.Info("已从飞书应用协作者解析 bot owner", "bot", a.cfg.ID, "数量", len(owners))
}

func (a *Adapter) fetchOwnerOpenIDs(ctx context.Context) ([]string, error) {
	var ids []string
	app, appErr := a.applications.GetApplication(ctx)
	if appErr == nil {
		ids = append(ids, app.OwnerID, app.CreatorID)
	} else {
		slog.Warn("获取飞书应用信息失败，将继续尝试读取应用协作者", "bot", a.cfg.ID, "err", appErr)
	}
	collaborators, collabErr := a.applications.GetCollaborators(ctx)
	if collabErr == nil {
		for _, item := range collaborators {
			if applicationCollaboratorCanApprove(item.Type) {
				ids = append(ids, item.UserID)
			}
		}
	}
	ids = normalizeOpenIDs(ids)
	if len(ids) > 0 {
		return ids, nil
	}
	if appErr != nil && collabErr != nil {
		return nil, fmt.Errorf("获取应用信息: %w; 获取应用协作者: %w", appErr, collabErr)
	}
	if collabErr != nil {
		return nil, fmt.Errorf("获取应用协作者: %w", collabErr)
	}
	return nil, nil
}

func applicationCollaboratorCanApprove(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "owner", "administrator", "developer":
		return true
	default:
		return false
	}
}

func normalizeOpenIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
