package config

import (
	"fmt"
	"log/slog"
	"strings"
)

// ParseLogLevel maps a lowercase level name to a slog.Level. It deliberately
// rejects uppercase and slog's offset syntax (e.g. "INFO+2") so the surface is
// tight and predictable; an unknown value fails fast. Shared by the daemon's
// startup -log-level handling and the runtime POST /v1/loglevel endpoint.
func ParseLogLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log-level %q: want debug|info|warn|error", s)
	}
}

// LevelString is the inverse of ParseLogLevel for the four canonical levels,
// rendering them as their lowercase names. Any other value (never produced by
// ParseLogLevel) falls back to slog's own lowercased label.
func LevelString(l slog.Level) string {
	switch l {
	case slog.LevelDebug:
		return "debug"
	case slog.LevelInfo:
		return "info"
	case slog.LevelWarn:
		return "warn"
	case slog.LevelError:
		return "error"
	default:
		return strings.ToLower(l.String())
	}
}
