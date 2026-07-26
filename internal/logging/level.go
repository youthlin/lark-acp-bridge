package logging

import (
	"log/slog"
	"os"
	"strings"
)

const LevelEnv = "LARK_ACP_BRIDGE_LOG_LEVEL"

var programLevel = newProgramLevel()

func LevelFromEnv() slog.Leveler {
	return parseLevel(os.Getenv(LevelEnv))
}

func ProgramLevel() *slog.LevelVar {
	return programLevel
}

func SetDebug(enabled bool) {
	if enabled {
		programLevel.Set(slog.LevelDebug)
		return
	}
	level := LevelFromEnv().Level()
	if level <= slog.LevelDebug {
		level = slog.LevelInfo
	}
	programLevel.Set(level)
}

func DebugEnabled() bool {
	return programLevel.Level() <= slog.LevelDebug
}

func newProgramLevel() *slog.LevelVar {
	level := &slog.LevelVar{}
	level.Set(parseLevel(os.Getenv(LevelEnv)).Level())
	return level
}

func parseLevel(value string) slog.Leveler {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
