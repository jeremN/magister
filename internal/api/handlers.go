package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"concentus/internal/core"
	"concentus/internal/event"
	"concentus/internal/flow"
	"concentus/internal/supervisor"
	"concentus/internal/workspace"
)

const maxBodyBytes = 1 << 20 // 1 MiB flow uploads

// Server holds the dependencies the HTTP handlers need.
type Server struct {
	Sup             *supervisor.Supervisor
	Store           core.Store
	Bus             *event.Bus
	Log             *slog.Logger
	BearerToken     string
	ShutdownTimeout time.Duration
	// ScratchRoot is the per-run scratch repo root (= GitManager.Root). When set and
	// a run targets a real repo, GET /v1/runs/{id} surfaces <root>/<id>/base so the
	// caller can find the result history. Empty disables the field.
	ScratchRoot string
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "flow too large")
		return
	}
	f, err := flow.ParseBytes(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "parse flow: "+err.Error())
		return
	}
	if err := flow.Validate(f); err != nil {
		writeError(w, http.StatusBadRequest, "invalid flow: "+err.Error())
		return
	}
	q := r.URL.Query()
	repo := q.Get("repo")
	base := ""
	if repo != "" {
		sha, err := workspace.ResolveBase(repo, q.Get("base"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "repo: "+err.Error())
			return
		}
		base = sha
	}
	id, err := s.Sup.Submit(r.Context(), f, string(body), repo, base)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, runResponse{ID: id})
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	f := core.Filter{Status: core.RunStatus(r.URL.Query().Get("status"))}
	rows, err := s.Store.ListRuns(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]runSummaryDTO, 0, len(rows))
	for _, rw := range rows {
		out = append(out, runSummaryDTO{ID: rw.ID, Name: rw.Name, Status: string(rw.Status)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	rs, err := s.Store.GetRun(r.Context(), core.RunID(r.PathValue("id")))
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown run")
		return
	}
	scratch := ""
	if rs.Repo != "" && s.ScratchRoot != "" {
		scratch = filepath.Join(s.ScratchRoot, string(rs.ID), "base")
	}
	writeJSON(w, http.StatusOK, snapshotFromState(rs, scratch))
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if !s.Sup.Cancel(core.RunID(r.PathValue("id"))) {
		writeError(w, http.StatusNotFound, "run not active")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	var req approveRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := core.RunID(r.PathValue("id"))
	step := r.PathValue("step")
	if !s.Sup.Approve(id, step, req.Approve, req.Reason) {
		writeError(w, http.StatusConflict, "no gate awaiting approval for this step")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := supervisor.PushOpts{
		Remote: q.Get("remote"),
		As:     q.Get("as"),
		Step:   q.Get("step"),
		Force:  q.Get("force") == "true",
	}
	res, err := s.Sup.Push(r.Context(), core.RunID(r.PathValue("id")), opts)
	if err != nil {
		var pe *supervisor.PushError
		if errors.As(err, &pe) {
			writeError(w, pe.Status, pe.Msg)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pushResponse{
		Remote:       res.Remote,
		Branch:       res.Branch,
		SourceBranch: res.SourceBranch,
		Commit:       res.Commit,
	})
}

func (s *Server) handlePR(w http.ResponseWriter, r *http.Request) {
	var req prRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.Sup.PR(r.Context(), core.RunID(r.PathValue("id")), supervisor.PROpts{
		Remote: req.Remote, As: req.As, Step: req.Step, Base: req.Base,
		Title: req.Title, Body: req.Body, Draft: req.Draft,
	})
	if err != nil {
		var pe *supervisor.PRError
		if errors.As(err, &pe) {
			writeError(w, pe.Status, pe.Msg)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, prResponse{
		URL: res.URL, Repo: res.Repo, Head: res.Head, Base: res.Base, Draft: res.Draft,
	})
}

func (s *Server) handleShip(w http.ResponseWriter, r *http.Request) {
	var req shipRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.Sup.Ship(r.Context(), core.RunID(r.PathValue("id")), supervisor.ShipOpts{
		Remote: req.Remote, As: req.As, Step: req.Step, Base: req.Base,
		Title: req.Title, Body: req.Body, Draft: req.Draft, Force: req.Force,
	})
	if err != nil {
		var pushE *supervisor.PushError
		if errors.As(err, &pushE) {
			writeError(w, pushE.Status, pushE.Msg)
			return
		}
		var prE *supervisor.PRError
		if errors.As(err, &prE) {
			writeError(w, prE.Status, prE.Msg)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, shipResponse{
		Pushed: pushResponse{
			Remote:       res.Push.Remote,
			Branch:       res.Push.Branch,
			SourceBranch: res.Push.SourceBranch,
			Commit:       res.Push.Commit,
		},
		PR:        prResponse{URL: res.PR.URL, Repo: res.PR.Repo, Head: res.PR.Head, Base: res.PR.Base, Draft: res.PR.Draft},
		PRExisted: res.PRExisted,
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return errors.New("body too large")
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}
