package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func (s *Service) handleConfigCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	session, ok := s.findSession(msg)
	if !ok || strings.TrimSpace(session.ACPSessionID) == "" {
		return "当前会话还没有 ACP session，发送普通文本或 /new 后再查看或设置配置项。"
	}
	if len(fields) == 1 {
		return formatConfigStatus(session)
	}
	configID := strings.TrimSpace(fields[1])
	opt, ok := findConfigOption(session, configID)
	if !ok {
		return "未知配置项：" + configID + "\n\n" + formatConfigStatus(session)
	}
	if len(fields) == 2 {
		return s.sendConfigDetailCard(ctx, msg, opt)
	}
	target := commandRemainder(text, 2)
	if target == "" {
		return "请使用 /config <id> <value> 设置配置项。"
	}
	value, display, err := s.setSessionConfigOption(ctx, msg, session, opt, target)
	if err != nil {
		if errors.Is(err, errUnknownConfigValue) {
			return "配置项 " + opt.ID + " 不支持该值：" + target + "\n\n" + formatConfigOptionDetail(opt)
		}
		return err.Error()
	}
	if display == "" {
		display = configOptionValueString(value)
	}
	return "已设置配置项 " + opt.ID + "：" + display
}

func (s *Service) sendConfigDetailCard(ctx context.Context, msg feishu.Message, opt acp.SessionConfigOption) string {
	sent, err := feishu.SendConfigDetailCard(ctx, msg, configDetailCard(opt))
	if err != nil {
		slog.ErrorContext(ctx, "发送配置项详情卡片失败", "错误", err)
		return "发送配置项详情卡片失败：" + err.Error()
	}
	if !sent {
		return formatConfigOptionDetail(opt)
	}
	return ""
}

var errUnknownConfigValue = errors.New("未知配置项取值")

func (s *Service) setSessionConfigOption(ctx context.Context, msg feishu.Message, session Session, opt acp.SessionConfigOption, target string) (any, string, error) {
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		return nil, "", fmt.Errorf("未找到 agent 配置：%s", session.AgentName)
	}
	var value any
	var display string
	switch strings.TrimSpace(opt.Type) {
	case "select":
		resolved, ok := resolveConfigOptionValue(opt, target)
		if !ok {
			return nil, "", fmt.Errorf("%w：%s", errUnknownConfigValue, target)
		}
		value = resolved
		display = configOptionDisplayName(opt, resolved)
	case "boolean":
		resolved, ok := resolveBooleanConfigOptionValue(target)
		if !ok {
			return nil, "", fmt.Errorf("%w：%s", errUnknownConfigValue, target)
		}
		value = resolved
		display = strconv.FormatBool(resolved)
	default:
		return nil, "", fmt.Errorf("当前 bridge 不支持设置 %s 类型的配置项：%s", opt.Type, opt.ID)
	}
	options, err := s.runtime.SetConfigOption(ctx, session, agent, opt.ID, value)
	if err != nil {
		slog.ErrorContext(ctx, "设置 ACP config option 失败", "config_id", opt.ID, "value", value, "错误", err)
		return nil, "", fmt.Errorf("设置配置项失败：%w", err)
	}
	updatedSession := session
	updatedSession.ConfigOptions = options
	if updated, ok := findConfigOption(updatedSession, opt.ID); ok {
		if strings.TrimSpace(updated.Type) == "boolean" {
			display = configOptionValueString(updated.CurrentValue)
		} else {
			display = configOptionDisplayName(updated, configOptionValueString(updated.CurrentValue))
		}
	}
	s.updateSessionState(ctx, msg, session, func(current *Session) {
		current.ConfigOptions = append([]acp.SessionConfigOption(nil), options...)
	})
	return value, display, nil
}

func (s *Service) handleModelCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	session, ok := s.findSession(msg)
	if !ok || strings.TrimSpace(session.ACPSessionID) == "" {
		return "当前会话还没有 ACP session，发送普通文本或 /new 后再查看或设置模型。"
	}
	if len(fields) == 1 {
		return s.sendModelSelectionCard(ctx, msg, session)
	}
	target := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	if target == "" {
		return "请使用 /model <model> 设置当前会话模型。"
	}
	value, _, err := s.setSessionModel(ctx, msg, session, target)
	if err != nil {
		if errors.Is(err, errUnknownModel) {
			return "未知模型：" + target + "\n\n" + formatModelStatus(session)
		}
		return err.Error()
	}
	if value == "" {
		return "未知模型：" + target + "\n\n" + formatModelStatus(session)
	}
	return "已设置当前会话模型：" + value
}

var errUnknownModel = errors.New("未知模型")

func (s *Service) sendModelSelectionCard(ctx context.Context, msg feishu.Message, session Session) string {
	modelOpt, ok := findModelConfigOption(session)
	if !ok {
		return "当前 ACP server 没有上报 model 配置项，无法通过 /model 设置。"
	}
	options := modelSelectionOptions(session, modelOpt)
	if len(options) == 0 {
		return "当前 ACP server 没有上报可选模型，请使用 /model <model> 设置。"
	}
	sent, err := feishu.SendModelSelectionCard(ctx, msg, feishu.ModelSelectionCard{
		BotID:            session.Key.BotID,
		ChatID:           session.Key.ChatID,
		ThreadID:         session.Key.SubID,
		GroupMessageType: msg.GroupMessageType,
		ACPSessionID:     session.ACPSessionID,
		RequesterID:      msg.SenderID,
		CurrentModel:     currentModelDisplay(session),
		Options:          options,
	})
	if err != nil {
		slog.ErrorContext(ctx, "发送模型选择卡片失败", "错误", err)
		return "发送模型选择卡片失败：" + err.Error()
	}
	if !sent {
		return formatModelStatus(session)
	}
	return ""
}

func modelSelectionOptions(session Session, modelOpt acp.SessionConfigOption) []feishu.ModelOption {
	options := make([]feishu.ModelOption, 0, len(modelOpt.Options))
	for _, option := range modelOpt.Options {
		if strings.TrimSpace(option.Value) == "" {
			continue
		}
		options = append(options, feishu.ModelOption{Value: option.Value, Name: option.Name})
	}
	if len(options) > 0 || session.Models == nil {
		return options
	}
	options = make([]feishu.ModelOption, 0, len(session.Models.AvailableModels))
	for _, model := range session.Models.AvailableModels {
		if strings.TrimSpace(model.ModelID) == "" {
			continue
		}
		options = append(options, feishu.ModelOption{Value: model.ModelID, Name: model.Name})
	}
	return options
}

func (s *Service) handleModeCommand(ctx context.Context, text string, msg feishu.Message) string {
	fields := strings.Fields(text)
	session, ok := s.findSession(msg)
	if !ok || strings.TrimSpace(session.ACPSessionID) == "" {
		return "当前会话还没有 ACP session，发送普通文本或 /new 后再查看或设置模式。"
	}
	if len(fields) == 1 {
		return s.sendModeSelectionCard(ctx, msg, session)
	}
	target := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	if target == "" {
		return "请使用 /mode <mode> 设置当前会话模式。"
	}
	value, _, err := s.setSessionMode(ctx, msg, session, target)
	if err != nil {
		if errors.Is(err, errUnknownMode) {
			return "未知模式：" + target + "\n\n" + formatModeStatus(session)
		}
		return err.Error()
	}
	if value == "" {
		return "未知模式：" + target + "\n\n" + formatModeStatus(session)
	}
	return "已设置当前会话模式：" + value
}

var errUnknownMode = errors.New("未知模式")

func (s *Service) sendModeSelectionCard(ctx context.Context, msg feishu.Message, session Session) string {
	modeOpt, ok := findModeConfigOption(session)
	if !ok && session.Mode == nil {
		return "当前 ACP server 没有上报 mode 配置项或 legacy modes，无法通过 /mode 设置。"
	}
	options := modeSelectionOptions(session, modeOpt)
	if len(options) == 0 {
		return "当前 ACP server 没有上报可选模式，请使用 /mode <mode> 设置。"
	}
	sent, err := feishu.SendModeSelectionCard(ctx, msg, feishu.ModeSelectionCard{
		BotID:            session.Key.BotID,
		ChatID:           session.Key.ChatID,
		ThreadID:         session.Key.SubID,
		GroupMessageType: msg.GroupMessageType,
		ACPSessionID:     session.ACPSessionID,
		RequesterID:      msg.SenderID,
		CurrentMode:      currentModeDisplay(session),
		Options:          options,
	})
	if err != nil {
		slog.ErrorContext(ctx, "发送模式选择卡片失败", "错误", err)
		return "发送模式选择卡片失败：" + err.Error()
	}
	if !sent {
		return formatModeStatus(session)
	}
	return ""
}

func modeSelectionOptions(session Session, modeOpt acp.SessionConfigOption) []feishu.ModeOption {
	options := make([]feishu.ModeOption, 0, len(modeOpt.Options))
	for _, option := range modeOpt.Options {
		if strings.TrimSpace(option.Value) == "" {
			continue
		}
		options = append(options, feishu.ModeOption{Value: option.Value, Name: option.Name})
	}
	if len(options) > 0 || session.Mode == nil {
		return options
	}
	options = make([]feishu.ModeOption, 0, len(session.Mode.AvailableModes))
	for _, mode := range session.Mode.AvailableModes {
		if strings.TrimSpace(mode.ModeID) == "" {
			continue
		}
		options = append(options, feishu.ModeOption{Value: mode.ModeID, Name: mode.Name})
	}
	return options
}

func (s *Service) HandleModeSelection(ctx context.Context, selection feishu.ModeSelection) (string, error) {
	if err := validateSelectionRequester(selection.RequesterID, selection.OperatorID, "模式", "mode", "设置模式"); err != nil {
		return "", err
	}
	msg := feishu.Message{
		BotID:            selection.BotID,
		ChatID:           selection.ChatID,
		ThreadID:         selection.ThreadID,
		GroupMessageType: selection.GroupMessageType,
	}
	session, err := s.selectionSession(
		msg,
		selection.ACPSessionID,
		"该模式选择卡片已过期，请重新发送 /mode",
	)
	if err != nil {
		return "", err
	}
	_, display, err := s.setSessionMode(ctx, msg, session, selection.Mode)
	if err != nil {
		return "", err
	}
	return display, nil
}

func (s *Service) setSessionMode(ctx context.Context, msg feishu.Message, session Session, target string) (string, string, error) {
	modeOpt, hasModeOpt := findModeConfigOption(session)
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		return "", "", fmt.Errorf("未找到 agent 配置：%s", session.AgentName)
	}
	if !hasModeOpt {
		value, ok := resolveLegacyModeValue(session.Mode, target)
		if !ok {
			return "", "", fmt.Errorf("%w：%s", errUnknownMode, target)
		}
		if err := s.runtime.SetMode(ctx, session, agent, value); err != nil {
			slog.ErrorContext(ctx, "设置 ACP legacy mode 失败", "mode", value, "错误", err)
			return "", "", fmt.Errorf("设置模式失败：%w", err)
		}
		if session.Mode == nil {
			session.Mode = &acp.SessionModeState{}
		}
		session.Mode.CurrentModeID = value
		s.updateSessionState(ctx, msg, session, func(current *Session) {
			if current.Mode == nil {
				current.Mode = &acp.SessionModeState{}
			}
			current.Mode.CurrentModeID = value
		})
		return value, legacyModeDisplayName(session.Mode, value), nil
	}
	value, ok := resolveModeValue(modeOpt, target)
	if !ok {
		return "", "", fmt.Errorf("%w：%s", errUnknownMode, target)
	}
	options, err := s.runtime.SetConfigOption(ctx, session, agent, modeOpt.ID, value)
	if err != nil {
		slog.ErrorContext(ctx, "设置 ACP mode 失败", "mode", value, "错误", err)
		return "", "", fmt.Errorf("设置模式失败：%w", err)
	}
	updatedSession := session
	updatedSession.ConfigOptions = options
	currentModeID := ""
	if modeOpt, ok := findModeConfigOption(updatedSession); ok {
		if configOptionValueString(modeOpt.CurrentValue) == value && updatedSession.Mode != nil {
			currentModeID = value
		}
	}
	s.updateSessionState(ctx, msg, session, func(current *Session) {
		current.ConfigOptions = append([]acp.SessionConfigOption(nil), options...)
		if currentModeID != "" {
			if current.Mode == nil {
				current.Mode = &acp.SessionModeState{}
			}
			current.Mode.CurrentModeID = currentModeID
		}
	})
	return value, configOptionDisplayName(modeOpt, value), nil
}

func (s *Service) HandleModelSelection(ctx context.Context, selection feishu.ModelSelection) (string, error) {
	if err := validateSelectionRequester(selection.RequesterID, selection.OperatorID, "模型", "model", "设置模型"); err != nil {
		return "", err
	}
	msg := feishu.Message{
		BotID:            selection.BotID,
		ChatID:           selection.ChatID,
		ThreadID:         selection.ThreadID,
		GroupMessageType: selection.GroupMessageType,
	}
	session, err := s.selectionSession(
		msg,
		selection.ACPSessionID,
		"该模型选择卡片已过期，请重新发送 /model",
	)
	if err != nil {
		return "", err
	}
	_, display, err := s.setSessionModel(ctx, msg, session, selection.Model)
	if err != nil {
		return "", err
	}
	return display, nil
}

func (s *Service) setSessionModel(ctx context.Context, msg feishu.Message, session Session, target string) (string, string, error) {
	modelOpt, ok := findModelConfigOption(session)
	if !ok {
		return "", "", fmt.Errorf("当前 ACP server 没有上报 model 配置项，无法设置模型")
	}
	value, ok := resolveModelValue(modelOpt, target)
	if !ok {
		return "", "", fmt.Errorf("%w：%s", errUnknownModel, target)
	}
	agent, ok := s.registry.Get(session.AgentName)
	if !ok {
		return "", "", fmt.Errorf("未找到 agent 配置：%s", session.AgentName)
	}
	options, err := s.runtime.SetConfigOption(ctx, session, agent, modelOpt.ID, value)
	if err != nil {
		slog.ErrorContext(ctx, "设置 ACP model 失败", "model", value, "错误", err)
		return "", "", fmt.Errorf("设置模型失败：%w", err)
	}
	updatedSession := session
	updatedSession.ConfigOptions = options
	currentModelID := ""
	if modelOpt, ok := findModelConfigOption(updatedSession); ok {
		if modelValueString(modelOpt.CurrentValue) == value && updatedSession.Models != nil {
			currentModelID = value
		}
	}
	s.updateSessionState(ctx, msg, session, func(current *Session) {
		current.ConfigOptions = append([]acp.SessionConfigOption(nil), options...)
		if currentModelID != "" {
			if current.Models == nil {
				current.Models = &acp.SessionModelState{}
			}
			current.Models.CurrentModelID = currentModelID
		}
	})
	return value, modelOptionName(modelOpt, value), nil
}

func (s *Service) selectionSession(msg feishu.Message, acpSessionID string, expiredMessage string) (Session, error) {
	store := s.storeForMessage(msg)
	if store == nil {
		return Session{}, fmt.Errorf("会话持久化未初始化")
	}
	for _, key := range callbackSessionKeys(msg) {
		session, ok := store.Get(key)
		if ok && session.ACPSessionID == acpSessionID {
			return session, nil
		}
	}
	return Session{}, errors.New(expiredMessage)
}

func callbackSessionKeys(msg feishu.Message) []SessionKey {
	keys := make([]SessionKey, 0, 2)
	if strings.TrimSpace(msg.ThreadID) != "" && strings.TrimSpace(msg.ChatID) != "" {
		keys = append(keys, imSessionKey(msg.BotID, msg.ChatID, msg.ThreadID))
	}
	for _, key := range sessionKeysFromMessage(msg) {
		duplicate := false
		for _, existing := range keys {
			if existing == key {
				duplicate = true
				break
			}
		}
		if !duplicate {
			keys = append(keys, key)
		}
	}
	return keys
}

func validateSelectionRequester(requesterID string, operatorID string, label string, command string, action string) error {
	requesterID = strings.TrimSpace(requesterID)
	operatorID = strings.TrimSpace(operatorID)
	if requesterID == "" || operatorID == "" {
		return fmt.Errorf("%s选择缺少发起人或操作者信息，请重新发送 /%s", label, command)
	}
	if requesterID != operatorID {
		return fmt.Errorf("只有发起该命令的用户可以%s", action)
	}
	return nil
}
