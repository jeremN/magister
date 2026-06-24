package tui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListRunsParsesAndSendsAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodGet || r.URL.Path != "/v1/runs" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"id": "a1", "name": "feature", "status": "running"},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "sekret")
	runs, err := c.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != "a1" || runs[0].Status != "running" {
		t.Fatalf("got %+v", runs)
	}
	if gotAuth != "Bearer sekret" {
		t.Fatalf("auth header = %q, want %q", gotAuth, "Bearer sekret")
	}
}

func TestNoAuthHeaderWhenTokenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Fatalf("unexpected Authorization header")
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{})
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "").ListRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGetRunParsesSteps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/a1" {
			t.Fatalf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"a1","name":"feature","status":"running","steps":[{"id":"plan","status":"succeeded","attempt":1}]}`))
	}))
	defer srv.Close()
	rd, err := NewClient(srv.URL, "").GetRun(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if rd.Name != "feature" || len(rd.Steps) != 1 || rd.Steps[0].ID != "plan" {
		t.Fatalf("got %+v", rd)
	}
}

func TestApproveSendsBody(t *testing.T) {
	var body struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/runs/a1/steps/plan/approve" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := NewClient(srv.URL, "").Approve(context.Background(), "a1", "plan", false, "nope"); err != nil {
		t.Fatal(err)
	}
	if body.Approve != false || body.Reason != "nope" {
		t.Fatalf("got %+v", body)
	}
}

func TestCancelSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method %s", r.Method)
		}
		http.Error(w, "run not active", http.StatusConflict)
	}))
	defer srv.Close()
	err := NewClient(srv.URL, "").Cancel(context.Background(), "a1")
	if err == nil {
		t.Fatal("want error on 409, got nil")
	}
}
