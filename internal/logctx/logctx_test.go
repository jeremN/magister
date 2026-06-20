package logctx

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

func TestWithFromRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := slog.New(slog.NewTextHandler(&buf, nil))
	if got := From(With(context.Background(), want)); got != want {
		t.Fatal("From did not return the logger stored by With")
	}
}

func TestFromBareContextIsUsableNotNil(t *testing.T) {
	got := From(context.Background())
	if got == nil {
		t.Fatal("From(bare ctx) returned nil; must return a usable discard logger")
	}
	got.Info("must not panic") // writes to the discard handler
}
