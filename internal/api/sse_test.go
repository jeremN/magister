package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"concentus/internal/core"
)

func TestSSEStreamsRunToCompletion(t *testing.T) {
	hs, _, _ := testServer(t)
	resp, err := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString(oneStepFlow))
	if err != nil {
		t.Fatal(err)
	}
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()

	// stream events; the handler closes after run.done
	ereq, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/runs/"+string(rr.ID)+"/events", nil)
	eresp, err := http.DefaultClient.Do(ereq)
	if err != nil {
		t.Fatal(err)
	}
	defer eresp.Body.Close()

	var kinds []string
	var lastSeq int64
	sc := bufio.NewScanner(eresp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			lastSeq, _ = strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		}
		if strings.HasPrefix(line, "event: ") {
			kinds = append(kinds, strings.TrimPrefix(line, "event: "))
		}
	}
	if len(kinds) == 0 || kinds[0] != "run.started" || kinds[len(kinds)-1] != "run.done" {
		t.Fatalf("expected run.started ... run.done, got %v", kinds)
	}
	if lastSeq == 0 {
		t.Error("expected non-zero final seq for Last-Event-ID")
	}
}

func TestSSEReplayWithLastEventID(t *testing.T) {
	hs, _, st := testServer(t)
	resp, _ := http.Post(hs.URL+"/v1/runs", "application/x-yaml", bytes.NewBufferString(oneStepFlow))
	var rr runResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	resp.Body.Close()
	waitForStatus(t, st, rr.ID, core.RunSucceeded)

	// reconnect asking for events after seq 1 → must not include seq 1
	ereq, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/runs/"+string(rr.ID)+"/events", nil)
	ereq.Header.Set("Last-Event-ID", "1")
	eresp, err := http.DefaultClient.Do(ereq)
	if err != nil {
		t.Fatal(err)
	}
	defer eresp.Body.Close()
	sc := bufio.NewScanner(eresp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			seq, _ := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			if seq <= 1 {
				t.Errorf("replay returned seq %d, want only > 1", seq)
			}
		}
	}
}
