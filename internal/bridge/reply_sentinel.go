package bridge

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const silentReplySentinel = "SILENT"

func isSilentReplySentinel(reply string) bool {
	return hasTrailingReplySentinel(reply, silentReplySentinel)
}

func hasTrailingReplySentinel(reply string, sentinel string) bool {
	reply = strings.TrimSpace(reply)
	sentinel = strings.TrimSpace(sentinel)
	if reply == "" || sentinel == "" || len(reply) < len(sentinel) {
		return false
	}
	if !strings.EqualFold(reply[len(reply)-len(sentinel):], sentinel) {
		return false
	}
	if len(reply) == len(sentinel) {
		return true
	}
	before, _ := utf8.DecodeLastRuneInString(reply[:len(reply)-len(sentinel)])
	return !isReplySentinelWordRune(before)
}

func isReplySentinelWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
