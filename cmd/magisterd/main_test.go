package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
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
