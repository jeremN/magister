package config

import (
	"log/slog"
	"strings"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	valid := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for s, want := range valid {
		got, err := ParseLogLevel(s)
		if err != nil {
			t.Errorf("ParseLogLevel(%q) unexpected error: %v", s, err)
		}
		if got != want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", s, got, want)
		}
	}
	for _, bad := range []string{"trace", "INFO", "info+2", ""} {
		_, err := ParseLogLevel(bad)
		if err == nil {
			t.Errorf("ParseLogLevel(%q) should return an error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "invalid log-level") {
			t.Errorf("ParseLogLevel(%q) error = %q, want it to mention invalid log-level", bad, err.Error())
		}
	}
}

func TestLevelString(t *testing.T) {
	for _, name := range []string{"debug", "info", "warn", "error"} {
		lvl, err := ParseLogLevel(name)
		if err != nil {
			t.Fatalf("ParseLogLevel(%q): %v", name, err)
		}
		if got := LevelString(lvl); got != name {
			t.Errorf("LevelString(%v) = %q, want %q", lvl, got, name)
		}
	}
}
