package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGCSubcommandPostsGC(t *testing.T) {
	var gotMethod, gotPath, gotOlderThan string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotOlderThan = r.URL.Query().Get("older_than")
		json.NewEncoder(w).Encode(map[string]int{"reclaimed": 3})
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"gc"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, want 0; out=%s", code, out.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/gc" {
		t.Errorf("request = %s %s, want POST /v1/gc", gotMethod, gotPath)
	}
	if gotOlderThan != "" {
		t.Errorf("older_than = %q, want empty", gotOlderThan)
	}
	if !strings.Contains(out.String(), "reclaimed 3") {
		t.Errorf("output = %q, want it to contain 'reclaimed 3'", out.String())
	}
}

func TestGCSubcommandForwardsOlderThan(t *testing.T) {
	var gotOlderThan string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOlderThan = r.URL.Query().Get("older_than")
		json.NewEncoder(w).Encode(map[string]int{"reclaimed": 0})
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"gc", "--older-than", "1h"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, want 0; out=%s", code, out.String())
	}
	if gotOlderThan != "1h" {
		t.Errorf("older_than = %q, want %q", gotOlderThan, "1h")
	}
}

func TestRmSubcommandDeletesScratch(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		json.NewEncoder(w).Encode(map[string]bool{"removed": true})
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"rm", "01ABC"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, want 0; out=%s", code, out.String())
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/runs/01ABC/scratch" {
		t.Errorf("request = %s %s, want DELETE /v1/runs/01ABC/scratch", gotMethod, gotPath)
	}
	if !strings.Contains(out.String(), "removed") {
		t.Errorf("output = %q, want it to contain 'removed'", out.String())
	}
}

func TestRmSubcommandAlreadyGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]bool{"removed": false})
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := dispatch([]string{"rm", "01ABC"}, srv.URL, &out); code != 0 {
		t.Fatalf("exit = %d, want 0; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "already gone") {
		t.Errorf("output = %q, want it to contain 'already gone'", out.String())
	}
}

func TestRmSubcommandRequiresRun(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"rm"}, "http://unused", &out); code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
}
