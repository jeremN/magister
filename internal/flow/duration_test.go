package flow

import (
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

func TestDurationUnmarshal(t *testing.T) {
	var h struct {
		T Duration `yaml:"t"`
	}
	if err := yaml.Unmarshal([]byte("t: 1m30s\n"), &h); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := h.T.Std(); got != 90*time.Second {
		t.Fatalf("got %v, want 90s", got)
	}
}

func TestDurationRejectsUnitless(t *testing.T) {
	var h struct {
		T Duration `yaml:"t"`
	}
	if err := yaml.Unmarshal([]byte("t: 5\n"), &h); err == nil {
		t.Fatal("expected error for unitless duration, got nil")
	}
}
