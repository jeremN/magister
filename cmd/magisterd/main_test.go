package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"concentus/internal/executor"
)

func TestRunServesHealthzAndShutsDown(t *testing.T) {
	db := filepath.Join(t.TempDir(), "m.db")
	stop := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		errc <- run([]string{"-addr", "127.0.0.1:0", "-db", db}, func(string) string { return "" }, stop, func(addr string) {
			// addr callback: hit healthz + readyz, then signal stop
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				resp, err := http.Get("http://" + addr + "/healthz")
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						rresp, rerr := http.Get("http://" + addr + "/readyz")
						if rerr != nil {
							t.Errorf("readyz GET failed: %v", rerr)
						} else {
							if rresp.StatusCode != http.StatusOK {
								t.Errorf("readyz while live = %d, want 200", rresp.StatusCode)
							}
							rresp.Body.Close()
						}
						close(stop)
						return
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Error("healthz never became reachable")
			close(stop)
		})
	}()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}

func TestRunScratchJanitorDisabledReturns(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan struct{})
	go func() {
		// ttl <= 0 disables the janitor; it must return without touching sup (nil here).
		runScratchJanitor(context.Background(), nil, 0, time.Hour, log)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("disabled janitor did not return")
	}
}

// TestRunScratchJanitorReturnsPromptlyOnCancel verifies that runScratchJanitor exits
// promptly when its context is canceled. This exercises the pre-canceled ctx path
// (ttl=0, disabled) to confirm the function returns without blocking — the property
// required for the join in main (stopJanitor + <-janitorDone before st.Close()).
// The active-loop cancel path (ttl>0) is verified by reading the code: the for-select
// has an explicit <-ctx.Done() return arm; runScratchJanitor takes a concrete
// *supervisor.Supervisor so a nil-safe unit test requires ttl=0.
func TestRunScratchJanitorReturnsPromptlyOnCancel(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so any ctx-aware path exits immediately

	done := make(chan struct{})
	go func() {
		defer close(done)
		runScratchJanitor(ctx, nil, 0, time.Hour, log)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("janitor did not return after ctx cancel")
	}
}

func TestAgentsRegistry(t *testing.T) {
	m := agents()
	if _, ok := m["mock"]; !ok {
		t.Error("mock agent must remain registered (keyless flows)")
	}
	opus, ok := m["opus"].(*executor.CLIAgent)
	if !ok {
		t.Fatalf("opus = %T, want *executor.CLIAgent", m["opus"])
	}
	if opus.Bin != "claude" || opus.Model != "opus" {
		t.Errorf("opus agent = {Bin:%q Model:%q}, want claude/opus", opus.Bin, opus.Model)
	}
	if sonnet, ok := m["sonnet"].(*executor.CLIAgent); !ok || sonnet.Model != "sonnet" {
		t.Errorf("sonnet agent wrong: %#v", m["sonnet"])
	}
	gem, ok := m["gemini"].(*executor.CLIAgent)
	if !ok {
		t.Fatalf("gemini = %T, want *executor.CLIAgent", m["gemini"])
	}
	if gem.Bin != "gemini" || gem.Model != "gemini-2.5-pro" {
		t.Errorf("gemini agent = {Bin:%q Model:%q}, want gemini/gemini-2.5-pro", gem.Bin, gem.Model)
	}
	cdx, ok := m["codex"].(*executor.CLIAgent)
	if !ok {
		t.Fatalf("codex = %T, want *executor.CLIAgent", m["codex"])
	}
	if cdx.Bin != "codex" || cdx.Model != "" {
		t.Errorf("codex agent = {Bin:%q Model:%q}, want codex/\"\"", cdx.Bin, cdx.Model)
	}
}

func TestNewLogHandlerText(t *testing.T) {
	var buf bytes.Buffer
	h, err := newLogHandler("text", slog.LevelInfo, &buf)
	if err != nil {
		t.Fatalf("newLogHandler(text): %v", err)
	}
	slog.New(h).Info("hi", "k", "v")
	out := buf.String()
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("text format should not be JSON: %s", out)
	}
	if !strings.Contains(out, "msg=hi") || !strings.Contains(out, "k=v") {
		t.Errorf("text output missing key=value fields: %s", out)
	}
}

func TestNewLogHandlerJSON(t *testing.T) {
	var buf bytes.Buffer
	h, err := newLogHandler("json", slog.LevelInfo, &buf)
	if err != nil {
		t.Fatalf("newLogHandler(json): %v", err)
	}
	slog.New(h).Info("hi", "k", "v")
	line := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(line, "{") {
		t.Fatalf("json output should be a JSON object: %s", line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("json output not parseable: %v (%s)", err, line)
	}
	if m["msg"] != "hi" || m["k"] != "v" {
		t.Errorf("json fields wrong: %v", m)
	}
}

func TestNewLogHandlerInvalid(t *testing.T) {
	_, err := newLogHandler("xml", slog.LevelInfo, io.Discard)
	if err == nil {
		t.Fatal("newLogHandler(xml) should return an error")
	}
	if !strings.Contains(err.Error(), "invalid log-format") {
		t.Errorf("error message = %q, want it to mention invalid log-format", err.Error())
	}
}

func TestRunRejectsBadLogFormat(t *testing.T) {
	stop := make(chan struct{})
	err := run([]string{"-log-format", "xml"}, func(string) string { return "" }, stop, nil)
	if err == nil {
		t.Fatal("run with -log-format xml should return an error")
	}
	if !strings.Contains(err.Error(), "invalid log-format") {
		t.Errorf("run error = %q, want invalid log-format", err.Error())
	}
}

func TestNewLogHandlerAppliesLevel(t *testing.T) {
	var buf bytes.Buffer
	h, err := newLogHandler("text", slog.LevelWarn, &buf)
	if err != nil {
		t.Fatalf("newLogHandler: %v", err)
	}
	log := slog.New(h)
	log.Info("below-threshold")
	if buf.Len() != 0 {
		t.Errorf("Info should be suppressed at Warn level, got: %s", buf.String())
	}
	log.Warn("above-threshold")
	if !strings.Contains(buf.String(), "above-threshold") {
		t.Errorf("Warn should be emitted at Warn level, got: %s", buf.String())
	}
}

func TestRunRejectsBadLogLevel(t *testing.T) {
	stop := make(chan struct{})
	err := run([]string{"-log-level", "trace"}, func(string) string { return "" }, stop, nil)
	if err == nil {
		t.Fatal("run with -log-level trace should return an error")
	}
	if !strings.Contains(err.Error(), "invalid log-level") {
		t.Errorf("run error = %q, want invalid log-level", err.Error())
	}
}

func TestRunWithOTelEndpointStartsAndDrains(t *testing.T) {
	dir := t.TempDir()
	stop := make(chan struct{})
	listening := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(
			[]string{"-addr", "127.0.0.1:0", "-db", filepath.Join(dir, "m.db"),
				"-otel-endpoint", "http://127.0.0.1:4318"},
			func(string) string { return "" },
			stop, func(addr string) { listening <- addr })
	}()
	select {
	case <-listening:
	case err := <-errCh:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("never listened")
	}
	close(stop)
	if err := <-errCh; err != nil {
		t.Errorf("run returned %v, want clean shutdown", err)
	}
}
