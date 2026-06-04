package main

import (
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
			// addr callback: hit healthz, then signal stop
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				resp, err := http.Get("http://" + addr + "/healthz")
				if err == nil {
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
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
}
