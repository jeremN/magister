package otelx

import (
	"context"
	"testing"
)

func TestInitDisabledReturnsNilProvider(t *testing.T) {
	tp, err := Init(context.Background(), Config{Endpoint: ""})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if tp != nil {
		t.Errorf("provider = %v, want nil when endpoint empty", tp)
	}
}

func TestInitEnabledBuildsProvider(t *testing.T) {
	tp, err := Init(context.Background(), Config{Endpoint: "http://127.0.0.1:4318", ServiceName: "test"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if tp == nil {
		t.Fatal("provider = nil, want non-nil when endpoint set")
	}
	if err := tp.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestTracesURL(t *testing.T) {
	cases := map[string]string{
		"http://host:4318":            "http://host:4318/v1/traces",
		"http://host:4318/":           "http://host:4318/v1/traces",
		"http://host:4318/v1/traces":  "http://host:4318/v1/traces",
		"https://collector/otlp/path": "https://collector/otlp/path",
	}
	for in, want := range cases {
		if got := tracesURL(in); got != want {
			t.Errorf("tracesURL(%q) = %q, want %q", in, got, want)
		}
	}
}
