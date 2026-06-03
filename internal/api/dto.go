// Package api is the HTTP/SSE adapter: stdlib net/http handlers, a middleware
// chain, and the SSE hub. Trust boundary is the loopback interface (§9).
package api

import "concentus/internal/core"

// runResponse is returned from POST /v1/runs.
type runResponse struct {
	ID core.RunID `json:"id"`
}

// stepDTO is one step in a run snapshot.
type stepDTO struct {
	ID        string   `json:"id"`
	Status    string   `json:"status"`
	Attempt   int      `json:"attempt"`
	Summary   string   `json:"summary,omitempty"`
	CostUSD   float64  `json:"cost_usd,omitempty"`
	WorkDir   string   `json:"workdir,omitempty"`
	Error     string   `json:"error,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
}

// runSnapshot is returned from GET /v1/runs/{id}.
type runSnapshot struct {
	ID          core.RunID `json:"id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Concurrency int        `json:"concurrency"`
	Error       string     `json:"error,omitempty"`
	Steps       []stepDTO  `json:"steps"`
}

// runSummaryDTO is one row in GET /v1/runs.
type runSummaryDTO struct {
	ID     core.RunID `json:"id"`
	Name   string     `json:"name"`
	Status string     `json:"status"`
}

// approveRequest is the body of POST .../approve.
type approveRequest struct {
	Approve bool   `json:"approve"`
	Reason  string `json:"reason,omitempty"`
}

// errorResponse is the uniform error envelope.
type errorResponse struct {
	Error string `json:"error"`
}

func snapshotFromState(rs core.RunState) runSnapshot {
	out := runSnapshot{
		ID: rs.ID, Name: rs.Name, Status: string(rs.Status),
		Concurrency: rs.Concurrency, Error: rs.Err,
		Steps: make([]stepDTO, 0, len(rs.Steps)),
	}
	for _, st := range rs.Steps {
		d := stepDTO{
			ID: st.StepID, Status: string(st.Status), Attempt: st.Attempt,
			Summary: st.Summary, CostUSD: st.CostUSD, WorkDir: st.WorkDir, Error: st.Err,
		}
		for _, a := range st.Artifacts {
			d.Artifacts = append(d.Artifacts, a.Path)
		}
		out.Steps = append(out.Steps, d)
	}
	return out
}
