package otelx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// otlpJSONExporter implements sdktrace.SpanExporter by POSTing spans to an OTLP/HTTP
// collector encoded as OTLP-JSON (Content-Type: application/json). It depends only on
// net/http + the OTel SDK — no grpc, no protobuf, no exporter module.
type otlpJSONExporter struct {
	endpoint string // full traces URL, e.g. http://collector:4318/v1/traces
	client   *http.Client
}

func newOTLPJSONExporter(tracesURL string) *otlpJSONExporter {
	return &otlpJSONExporter{endpoint: tracesURL, client: &http.Client{Timeout: 10 * time.Second}}
}

func (e *otlpJSONExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}
	body, err := json.Marshal(buildTracesPayload(spans))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("otlp-json export: %s", resp.Status)
	}
	return nil
}

func (e *otlpJSONExporter) Shutdown(context.Context) error { return nil }

// --- OTLP-JSON wire structs (the subset we emit; camelCase = proto3 JSON) ---

type otlpPayload struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}
type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}
type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes,omitempty"`
}
type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}
type otlpScope struct {
	Name string `json:"name,omitempty"`
}
type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Status            otlpStatus     `json:"status"`
}
type otlpStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}
type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}
type otlpAnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"` // int64 as decimal string
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

// buildTracesPayload assumes all spans share a single resource (the provider's) and groups them by instrumentation scope.
func buildTracesPayload(spans []sdktrace.ReadOnlySpan) otlpPayload {
	res := otlpResource{Attributes: kvList(spans[0].Resource().Attributes())}
	byScope := map[string][]otlpSpan{}
	var order []string
	for _, s := range spans {
		name := s.InstrumentationScope().Name
		if _, ok := byScope[name]; !ok {
			order = append(order, name)
		}
		byScope[name] = append(byScope[name], toOTLPSpan(s))
	}
	scopeSpans := make([]otlpScopeSpans, 0, len(order))
	for _, name := range order {
		scopeSpans = append(scopeSpans, otlpScopeSpans{Scope: otlpScope{Name: name}, Spans: byScope[name]})
	}
	return otlpPayload{ResourceSpans: []otlpResourceSpans{{Resource: res, ScopeSpans: scopeSpans}}}
}

func toOTLPSpan(s sdktrace.ReadOnlySpan) otlpSpan {
	sc := s.SpanContext()
	out := otlpSpan{
		TraceID:           sc.TraceID().String(), // SDK emits lowercase hex
		SpanID:            sc.SpanID().String(),
		Name:              s.Name(),
		Kind:              int(s.SpanKind()), // OTel SpanKind values match the OTLP enum
		StartTimeUnixNano: strconv.FormatInt(s.StartTime().UnixNano(), 10),
		EndTimeUnixNano:   strconv.FormatInt(s.EndTime().UnixNano(), 10),
		Attributes:        kvList(s.Attributes()),
		Status:            otlpStatus{Code: otlpStatusCode(s.Status().Code), Message: s.Status().Description},
	}
	if p := s.Parent(); p.HasSpanID() {
		out.ParentSpanID = p.SpanID().String()
	}
	return out
}

// otlpStatusCode remaps OTel SDK status codes to the OTLP proto enum. They DIFFER:
// SDK codes are Unset=0, Error=1, Ok=2; OTLP is Unset=0, Ok=1, Error=2. A direct cast
// would turn every error span into "OK".
func otlpStatusCode(c codes.Code) int {
	switch c {
	case codes.Ok:
		return 1
	case codes.Error:
		return 2
	default:
		return 0
	}
}

func kvList(attrs []attribute.KeyValue) []otlpKeyValue {
	out := make([]otlpKeyValue, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, otlpKeyValue{Key: string(a.Key), Value: anyValue(a.Value)})
	}
	return out
}

func anyValue(v attribute.Value) otlpAnyValue {
	switch v.Type() {
	case attribute.BOOL:
		b := v.AsBool()
		return otlpAnyValue{BoolValue: &b}
	case attribute.INT64:
		s := strconv.FormatInt(v.AsInt64(), 10)
		return otlpAnyValue{IntValue: &s}
	case attribute.FLOAT64:
		f := v.AsFloat64()
		return otlpAnyValue{DoubleValue: &f}
	default:
		s := v.AsString()
		return otlpAnyValue{StringValue: &s}
	}
}
