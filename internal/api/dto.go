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
	Scratch     string     `json:"scratch,omitempty"`
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

// prRequest is the JSON body of POST /v1/runs/{id}/pr. All fields optional.
type prRequest struct {
	Remote   string `json:"remote,omitempty"`
	As       string `json:"as,omitempty"`
	Step     string `json:"step,omitempty"`
	Base     string `json:"base,omitempty"`
	Title    string `json:"title,omitempty"`
	Body     string `json:"body,omitempty"`
	HeadRepo string `json:"head_repo,omitempty"`
	Draft    bool   `json:"draft,omitempty"`
}

// prResponse is returned from POST /v1/runs/{id}/pr.
type prResponse struct {
	URL   string `json:"url"`
	Repo  string `json:"repo"`
	Head  string `json:"head"`
	Base  string `json:"base,omitempty"`
	Draft bool   `json:"draft,omitempty"`
}

// pushResponse is returned from POST /v1/runs/{id}/push.
type pushResponse struct {
	Remote       string `json:"remote"`
	Branch       string `json:"branch"`
	SourceBranch string `json:"source_branch"`
	Commit       string `json:"commit"`
}

// shipRequest is the JSON body of POST /v1/runs/{id}/ship: the union of pr's and
// push's options. All fields optional.
type shipRequest struct {
	Remote   string `json:"remote,omitempty"`
	As       string `json:"as,omitempty"`
	Step     string `json:"step,omitempty"`
	Base     string `json:"base,omitempty"`
	Title    string `json:"title,omitempty"`
	Body     string `json:"body,omitempty"`
	HeadRepo string `json:"head_repo,omitempty"`
	Draft    bool   `json:"draft,omitempty"`
	Force    bool   `json:"force,omitempty"`
}

// shipResponse is returned from POST /v1/runs/{id}/ship.
type shipResponse struct {
	Pushed    pushResponse `json:"pushed"`
	PR        prResponse   `json:"pr"`
	PRExisted bool         `json:"pr_existed"`
}

// errorResponse is the uniform error envelope.
type errorResponse struct {
	Error string `json:"error"`
}

// logLevelRequest is the JSON body of POST /v1/loglevel.
type logLevelRequest struct {
	Level string `json:"level"`
}

// logLevelResponse is returned from GET and POST /v1/loglevel.
type logLevelResponse struct {
	Level string `json:"level"`
}

func snapshotFromState(rs core.RunState, scratch string) runSnapshot {
	out := runSnapshot{
		ID: rs.ID, Name: rs.Name, Status: string(rs.Status),
		Concurrency: rs.Concurrency, Error: rs.Err,
		Steps: make([]stepDTO, 0, len(rs.Steps)), Scratch: scratch,
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
