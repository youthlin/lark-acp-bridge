package bridge

import (
	"fmt"
	"strings"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func formatModelStatus(session Session) string {
	lines := []string{"当前会话模型："}
	current := currentModelDisplay(session)
	if current == "" {
		current = "未知"
	}
	lines = append(lines, current)
	modelOpt, hasModelOpt := findModelConfigOption(session)
	if hasModelOpt && len(modelOpt.Options) > 0 {
		lines = append(lines, "", "可用模型：")
		for _, opt := range modelOpt.Options {
			if strings.TrimSpace(opt.Value) == "" {
				continue
			}
			marker := ""
			if opt.Value == modelValueString(modelOpt.CurrentValue) {
				marker = " *"
			}
			label := opt.Value
			if strings.TrimSpace(opt.Name) != "" && opt.Name != opt.Value {
				label += " - " + strings.TrimSpace(opt.Name)
			}
			lines = append(lines, marker+" "+label)
		}
	} else if session.Models != nil && len(session.Models.AvailableModels) > 0 {
		lines = append(lines, "", "可用模型：")
		for _, model := range session.Models.AvailableModels {
			if strings.TrimSpace(model.ModelID) == "" {
				continue
			}
			marker := ""
			if model.ModelID == session.Models.CurrentModelID {
				marker = " *"
			}
			label := model.ModelID
			if strings.TrimSpace(model.Name) != "" && model.Name != model.ModelID {
				label += " - " + strings.TrimSpace(model.Name)
			}
			lines = append(lines, marker+" "+label)
		}
	} else {
		lines = append(lines, "", "当前 ACP server 还没有上报可用模型。")
	}
	lines = append(lines, "", "设置模型：/model <model>")
	return strings.Join(lines, "\n")
}

func formatModeStatus(session Session) string {
	lines := []string{"当前会话模式："}
	current := currentModeDisplay(session)
	if current == "" {
		current = "未知"
	}
	lines = append(lines, current)
	modeOpt, hasModeOpt := findModeConfigOption(session)
	if hasModeOpt && len(modeOpt.Options) > 0 {
		lines = append(lines, "", "可用模式：")
		for _, opt := range modeOpt.Options {
			if strings.TrimSpace(opt.Value) == "" {
				continue
			}
			marker := ""
			if opt.Value == configOptionValueString(modeOpt.CurrentValue) {
				marker = " *"
			}
			label := opt.Value
			if strings.TrimSpace(opt.Name) != "" && opt.Name != opt.Value {
				label += " - " + strings.TrimSpace(opt.Name)
			}
			lines = append(lines, marker+" "+label)
		}
	} else if session.Mode != nil && len(session.Mode.AvailableModes) > 0 {
		lines = append(lines, "", "可用模式：")
		for _, mode := range session.Mode.AvailableModes {
			if strings.TrimSpace(mode.ModeID) == "" {
				continue
			}
			marker := ""
			if mode.ModeID == session.Mode.CurrentModeID {
				marker = " *"
			}
			label := mode.ModeID
			if strings.TrimSpace(mode.Name) != "" && mode.Name != mode.ModeID {
				label += " - " + strings.TrimSpace(mode.Name)
			}
			lines = append(lines, marker+" "+label)
		}
	} else {
		lines = append(lines, "", "当前 ACP server 还没有上报可用模式。")
	}
	lines = append(lines, "", "设置模式：/mode <mode>")
	return strings.Join(lines, "\n")
}

func formatConfigStatus(session Session) string {
	lines := []string{"当前 ACP 配置项："}
	if len(session.ConfigOptions) == 0 {
		lines = append(lines, "当前 ACP server 还没有上报可配置项。")
		return strings.Join(lines, "\n")
	}
	for _, opt := range session.ConfigOptions {
		if strings.TrimSpace(opt.ID) == "" {
			continue
		}
		lines = append(lines, " "+formatConfigOptionSummary(opt))
	}
	if len(lines) == 1 {
		lines = append(lines, "当前 ACP server 还没有上报可配置项。")
		return strings.Join(lines, "\n")
	}
	lines = append(lines, "", "查看配置项：/config <id>", "设置配置项：/config <id> <value>")
	return strings.Join(lines, "\n")
}

func formatConfigOptionSummary(opt acp.SessionConfigOption) string {
	label := strings.TrimSpace(opt.ID)
	name := strings.TrimSpace(opt.Name)
	if name != "" && name != label {
		label += " - " + name
	}
	current := configOptionValueString(opt.CurrentValue)
	if current == "" {
		current = "未知"
	}
	optionType := strings.TrimSpace(opt.Type)
	if optionType == "" {
		optionType = "unknown"
	}
	return fmt.Sprintf("%s [%s] = %s", label, optionType, current)
}

func formatConfigOptionDetail(opt acp.SessionConfigOption) string {
	id := strings.TrimSpace(opt.ID)
	lines := []string{"ACP 配置项：" + id}
	if name := strings.TrimSpace(opt.Name); name != "" && name != id {
		lines = append(lines, "名称："+name)
	}
	if category := strings.TrimSpace(opt.Category); category != "" {
		lines = append(lines, "分类："+category)
	}
	if description := strings.TrimSpace(opt.Description); description != "" {
		lines = append(lines, "说明："+description)
	}
	optionType := strings.TrimSpace(opt.Type)
	if optionType == "" {
		optionType = "unknown"
	}
	lines = append(lines, "类型："+optionType)
	current := configOptionValueString(opt.CurrentValue)
	if current == "" {
		current = "未知"
	}
	lines = append(lines, "当前值："+current)
	if len(opt.Options) > 0 {
		lines = append(lines, "", "可选值：")
		for _, option := range opt.Options {
			value := strings.TrimSpace(option.Value)
			if value == "" {
				continue
			}
			marker := "[ ]"
			if value == configOptionValueString(opt.CurrentValue) {
				marker = "[x]"
			}
			label := configOptionDisplayName(opt, value)
			if label == "" {
				label = value
			}
			line := "- " + marker + " " + label
			if description := cleanConfigOptionDescription(option.Description); description != "" {
				line += " - " + description
			}
			lines = append(lines, line)
		}
	}
	lines = append(lines, "", "设置配置项：/config "+id+" <value>")
	return strings.Join(lines, "\n")
}

func configDetailCard(opt acp.SessionConfigOption) feishu.ConfigDetailCard {
	id := strings.TrimSpace(opt.ID)
	optionType := strings.TrimSpace(opt.Type)
	if optionType == "" {
		optionType = "unknown"
	}
	current := configOptionValueString(opt.CurrentValue)
	if current == "" {
		current = "未知"
	}
	options := make([]feishu.ConfigOptionValue, 0, len(opt.Options))
	for _, option := range opt.Options {
		value := strings.TrimSpace(option.Value)
		if value == "" {
			continue
		}
		options = append(options, feishu.ConfigOptionValue{
			Value:       value,
			Name:        strings.TrimSpace(option.Name),
			Description: cleanConfigOptionDescription(option.Description),
			Current:     value == configOptionValueString(opt.CurrentValue),
		})
	}
	return feishu.ConfigDetailCard{
		ID:           id,
		Name:         strings.TrimSpace(opt.Name),
		Category:     strings.TrimSpace(opt.Category),
		Description:  strings.TrimSpace(opt.Description),
		Type:         optionType,
		CurrentValue: current,
		Options:      options,
		SetCommand:   "/config " + id + " <value>",
	}
}

func cleanConfigOptionDescription(description string) string {
	description = strings.TrimSpace(description)
	switch description {
	case "", ".", "。":
		return ""
	default:
		return description
	}
}

func currentModeDisplay(session Session) string {
	if modeOpt, ok := findModeConfigOption(session); ok {
		current := configOptionValueString(modeOpt.CurrentValue)
		if current != "" {
			return current
		}
	}
	if session.Mode != nil {
		return strings.TrimSpace(session.Mode.CurrentModeID)
	}
	return ""
}

func currentModelDisplay(session Session) string {
	if modelOpt, ok := findModelConfigOption(session); ok {
		current := configOptionValueString(modelOpt.CurrentValue)
		if current != "" {
			return current
		}
	}
	if session.Models != nil {
		return strings.TrimSpace(session.Models.CurrentModelID)
	}
	return ""
}

func findConfigOption(session Session, id string) (acp.SessionConfigOption, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return acp.SessionConfigOption{}, false
	}
	for _, opt := range session.ConfigOptions {
		if opt.ID == id || strings.EqualFold(opt.ID, id) {
			return opt, true
		}
	}
	for _, opt := range session.ConfigOptions {
		if opt.Category == id || strings.EqualFold(opt.Category, id) {
			return opt, true
		}
	}
	for _, opt := range session.ConfigOptions {
		if opt.Name == id || strings.EqualFold(opt.Name, id) {
			return opt, true
		}
	}
	return acp.SessionConfigOption{}, false
}

func findModeConfigOption(session Session) (acp.SessionConfigOption, bool) {
	for _, opt := range session.ConfigOptions {
		if opt.ID == "mode" || opt.Category == "mode" {
			return opt, true
		}
	}
	return acp.SessionConfigOption{}, false
}

func findModelConfigOption(session Session) (acp.SessionConfigOption, bool) {
	for _, opt := range session.ConfigOptions {
		if opt.ID == "model" || opt.Category == "model" {
			return opt, true
		}
	}
	return acp.SessionConfigOption{}, false
}

func resolveConfigOptionValue(opt acp.SessionConfigOption, target string) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	if len(opt.Options) == 0 {
		return target, true
	}
	for _, value := range opt.Options {
		if value.Value == target || strings.EqualFold(value.Name, target) {
			return value.Value, true
		}
	}
	return "", false
}

func resolveModelValue(opt acp.SessionConfigOption, target string) (string, bool) {
	return resolveConfigOptionValue(opt, target)
}

func resolveModeValue(opt acp.SessionConfigOption, target string) (string, bool) {
	return resolveConfigOptionValue(opt, target)
}

func resolveBooleanConfigOptionValue(target string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "true", "on", "yes", "y", "1", "enable", "enabled":
		return true, true
	case "false", "off", "no", "n", "0", "disable", "disabled":
		return false, true
	default:
		return false, false
	}
}

func resolveLegacyModeValue(state *acp.SessionModeState, target string) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	if state == nil || len(state.AvailableModes) == 0 {
		return "", false
	}
	for _, mode := range state.AvailableModes {
		if mode.ModeID == target || strings.EqualFold(mode.Name, target) {
			return mode.ModeID, true
		}
	}
	return "", false
}

func modelValueString(value any) string {
	return configOptionValueString(value)
}

func configOptionValueString(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func modelOptionName(opt acp.SessionConfigOption, value string) string {
	return configOptionDisplayName(opt, value)
}

func configOptionDisplayName(opt acp.SessionConfigOption, value string) string {
	for _, option := range opt.Options {
		if option.Value != value {
			continue
		}
		name := strings.TrimSpace(option.Name)
		if name != "" && name != value {
			return name + "（" + value + "）"
		}
		break
	}
	return value
}

func legacyModeDisplayName(state *acp.SessionModeState, value string) string {
	if state == nil {
		return value
	}
	for _, mode := range state.AvailableModes {
		if mode.ModeID != value {
			continue
		}
		name := strings.TrimSpace(mode.Name)
		if name != "" && name != value {
			return name + "（" + value + "）"
		}
		break
	}
	return value
}
