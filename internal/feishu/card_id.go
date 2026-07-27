package feishu

import "strings"

func normalizedCardID(cardID *string) string {
	if cardID == nil {
		return ""
	}
	return strings.TrimSpace(*cardID)
}
