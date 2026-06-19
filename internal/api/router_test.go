package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouterMethodNotAllowed(t *testing.T) {
	hs, _, _ := testServer(t)
	// DELETE on /v1/runs (no {id}) isn't a route → 404/405 from ServeMux
	req, _ := http.NewRequest(http.MethodPut, hs.URL+"/v1/runs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("PUT /v1/runs should not be 200")
	}
}

func TestRouterAuthAppliesToV1(t *testing.T) {
	// a server with a bearer token rejects unauthenticated /v1 calls
	srv, _, _ := newServerOnly(t)
	const token = "secret"
	hs := httptest.NewServer(srv.Router(token))
	defer hs.Close()
	resp, _ := http.Get(hs.URL + "/v1/runs")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated /v1/runs = %d, want 401", resp.StatusCode)
	}
	// healthz is exempt
	hresp, _ := http.Get(hs.URL + "/healthz")
	hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		t.Errorf("healthz should be exempt from auth, got %d", hresp.StatusCode)
	}
}
