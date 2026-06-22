package otelx

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestOTLPJSONExporterEncodesSpan(t *testing.T) {
	var body []byte
	var contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp := newOTLPJSONExporter(srv.URL)
	res, _ := resource.New(context.Background(), resource.WithAttributes(attribute.String("service.name", "test")))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithResource(res)) // WithSyncer = export on End
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	_, span := tp.Tracer("concentus").Start(context.Background(), "run x",
		trace.WithAttributes(attribute.Int("magister.attempt", 3)))
	span.SetStatus(codes.Error, "boom")
	span.End() // exports synchronously; the POST completes before End returns

	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("payload not JSON: %v\n%s", err, body)
	}
	sp := drillToSpan(t, p)

	// (1) traceId: 32 lowercase hex, NOT base64
	if tid, _ := sp["traceId"].(string); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(tid) {
		t.Errorf("traceId = %q, want 32-hex", tid)
	}
	// (2) startTimeUnixNano: a JSON STRING, not a number
	if _, ok := sp["startTimeUnixNano"].(string); !ok {
		t.Errorf("startTimeUnixNano = %v (%T), want string", sp["startTimeUnixNano"], sp["startTimeUnixNano"])
	}
	// (3) status.code: remapped to OTLP ERROR=2 (NOT the SDK's Error=1)
	if st := sp["status"].(map[string]any); st["code"].(float64) != 2 {
		t.Errorf("status.code = %v, want 2 (OTLP ERROR)", st["code"])
	}
	// (4) int attribute: encoded as a STRING intValue
	found := false
	for _, a := range sp["attributes"].([]any) {
		kv := a.(map[string]any)
		if kv["key"] == "magister.attempt" {
			found = true
			if iv, ok := kv["value"].(map[string]any)["intValue"].(string); !ok || iv != "3" {
				t.Errorf("magister.attempt intValue = %v, want string \"3\"", kv["value"])
			}
		}
	}
	if !found {
		t.Error("magister.attempt attribute missing from payload")
	}
}

func drillToSpan(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	rs := p["resourceSpans"].([]any)[0].(map[string]any)
	ss := rs["scopeSpans"].([]any)[0].(map[string]any)
	return ss["spans"].([]any)[0].(map[string]any)
}
