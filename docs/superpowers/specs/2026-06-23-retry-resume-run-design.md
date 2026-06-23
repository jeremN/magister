# Retry / Resume a Failed Run — Design

**Date:** 2026-06-23
**Status:** Approved (brainstorm)
**Feature:** `cm retry <run>` + `POST /v1/runs/{id}/retry` — resume a `failed` or `canceled` run in place, skipping work that already succeeded.

## Summary

Add a user-triggered **in-place resume** for terminal-but-unfinished runs. `cm retry <run>` revives a `failed` or `canceled` run **under its own run id**, reusing the run's preserved scratch repository, re-running only the steps that did not succeed (the failed step and everything downstream of it), and leaving already-succeeded agent work untouched (no re-paying for `opus`/`sonnet` calls).

The orchestrator already contains the entire resume engine — it is what `Supervisor.ResumeAll` runs against every incomplete run on daemon startup. This feature exposes that same machinery on demand for a single terminal run, behind two new guards (terminal-state check, scratch-existence check) and a small amount of new surface (one supervisor method, one HTTP endpoint, one `cm` subcommand).

## Goals

- `cm retry <run>` resumes a `failed` or `canceled` run from its failed step, skipping succeeded steps.
- Succeeded steps are **not** re-executed; their persisted artifacts seed the resumed run's downstream inputs.
- The resumed run keeps its **own ULID**; new events append to its existing event stream.
- Reject retry cleanly when the run is `succeeded` (nothing to do), still in progress, unknown, or when the scratch has been GC-reclaimed.
- Reuse the existing `Resume` / `resetIncompleteSteps` / `Provision` machinery; do not fork a second resume code path.

## Non-goals (out of scope for this slice)

- **Fresh restart as a new run** (re-clone + re-run everything under a new id). Considered and rejected for this slice — it cannot skip succeeded work because a fresh scratch holds none of the prior `step/<id>` branches.
- **Hybrid fallback** (resume in place when scratch is present, else fresh-restart). Deferred; retry simply rejects when the scratch is gone.
- **Scratch reconstruction** (rebuilding succeeded steps' branches in a fresh scratch from persisted commits). Not needed; in-place reuse avoids it.
- **`parent_run_id` lineage links.** In-place resume keeps the same id, so there is no parent/child relationship to record.
- Changing the flow on retry, partial step-selection, or re-running succeeded steps.

## Background — the machinery that already exists

These are verified facts about the current code that the design builds on:

- **`Engine.Resume(parent, rs, f)`** (`internal/engine/engine.go:86-99`) builds a `seed` map of every `StepSucceeded` step's `core.Result` (summary, cost, artifacts) and calls `runDAG(parent, rs.ID, f, seed)`.
- **`runDAG`** (`internal/engine/engine.go:101-284`) pre-loads `seed` into its `results` map (`:143-145`), and each step goroutine does `if _, ok := seed[s.ID]; ok { return }` to skip execution (`:160-163`). Downstream steps gather inputs from `results[dep].Artifacts` (`:178-182`), so a seeded step's artifacts flow forward without re-running it.
- `runDAG` itself **sets the run to `running`** (`:110`), **emits a fresh `run.started`** (`:113-120`, with an existing comment noting a resumed run records a second `run.started` — consistent with at-least-once), and emits the terminal `run.done` at the end. **No new status/event plumbing is required for an in-place resume.**
- **`Supervisor.resetIncompleteSteps(ctx, rs)`** (`internal/supervisor/supervisor.go:143-155`) rebuilds every non-succeeded step as a fresh `core.StepState{RunID, StepID, Status: StepPending}` (so `Attempt` resets to 0 — the failed step gets a clean retry budget) and leaves succeeded steps intact.
- **`Supervisor.ResumeAll`** (`internal/supervisor/supervisor.go:159-183`) loops over `LoadIncompleteRuns` and, per run: parse+validate `FlowYAML` → `resetIncompleteSteps` → `Provision(id, repo, base)` → `start(ctx, id, engine.Resume)`. This is exactly the sequence Retry needs after its guards.
- **Run record** (`internal/core/store.go:16-27`) persists everything required to resume: `FlowYAML`, `Repo`, `Base`, and per-step `Status`/`Summary`/`CostUSD`/`Artifacts`.
- **Scratch lifecycle:** per-run scratch lives at `{runs}/{RunID}/` (base repo at `…/base`, worktrees at `…/wt/<step>`), created lazily by `GitManager` (`internal/workspace/gitmanager.go:90-92`). The GC janitor `SweepScratch` reclaims runs whose status is in `('succeeded','failed','canceled')` and whose `updated_at` is older than a caller-supplied cutoff (`internal/supervisor/gc.go`, `internal/store/query.sql:51-52`); reclaim removes `{runs}/{RunID}/` entirely (`GitManager.Reclaim`, `:114-132`). So a failed run's scratch — with its `step/<id>` branches and partial work — persists until the TTL sweep removes it.
- **Run-id generation:** `NewRunID()` returns a ULID (`internal/supervisor/supervisor.go:56-57`). Runs are immutable records today; `ResumeAll` is the one precedent for continuing a run under its existing id.

## Design

### Retry model: in-place resume

Retry operates on the **same run record and the same scratch directory** as the original run. It does not allocate a new id. This is the only model that can skip succeeded work, because skipping a step requires that step's `step/<id>` git branch to still exist in the scratch (downstream steps merge those branches); a fresh scratch would have none of them.

### `Supervisor.Retry(ctx, id) (core.RunID, error)`

The ordering of the guards is load-bearing — the status flip must precede the scratch check so the GC janitor (which only selects terminal-status runs) cannot reclaim the scratch mid-retry.

1. **Reject if active.** Consult the supervisor's in-memory `runs` map under its mutex; if the id is present (running or still unwinding), return `RetryError{409, "run still in progress"}`.
2. **Load.** `store.GetRun(ctx, id)`; not found → `RetryError{404, "run not found"}`.
3. **Guard terminal status.** Allowed: `failed`, `canceled`. `succeeded` → `RetryError{409, "run succeeded; nothing to retry"}`. `pending`/`running` (persisted, but not in the active map — e.g. a stale row) → `RetryError{409, "run still in progress"}`.
4. **Re-parse + validate** the flow from `rs.FlowYAML` (`flow.ParseBytes` + `flow.Validate`). It validated at submit, so a failure here is a `500`-class internal error (corrupt persisted YAML); return `RetryError{500, "stored flow no longer parses: …"}`.
5. **Flip status → `pending`** via `store.SetRunStatus(ctx, id, RunPending, "")`. This removes the run from the GC selection set before the scratch check, closing the reclaim race.
6. **Pre-flight scratch check.** `engine.ScratchExists(id)` (new — see below). If the scratch is gone, **fully revert** the run to its original terminal state (`SetRunStatus(ctx, id, rs.Status, rs.Err)` — so a `canceled` run stays `canceled`, not silently turned into `failed`, and its original error text is preserved) and return `RetryError{409, "scratch reclaimed; resubmit the flow"}`. The reclaimed-scratch reason is reported in the HTTP response, not persisted onto the run.
7. **Resume.** Call the shared helper `resumeRun(ctx, rs, f)` (extracted from `ResumeAll`): `resetIncompleteSteps` → `Provision(id, rs.Repo, rs.Base)` → `start(ctx, id, func(runCtx) error { return engine.Resume(runCtx, rs, f) })`. Return `id, nil`.

### Shared helper: `resumeRun`

Extract the reset → provision → start(Resume) sequence — currently inline in `ResumeAll`'s loop body — into one private method:

```go
// resumeRun resets the run's non-succeeded steps, re-provisions its scratch
// spec, and starts engine.Resume under id. Shared by ResumeAll (startup) and
// Retry (on demand) so the two cannot drift.
func (s *Supervisor) resumeRun(ctx context.Context, rs core.RunState, f *flow.Flow) error {
    s.resetIncompleteSteps(ctx, rs)
    if err := s.engine.Provision(rs.ID, rs.Repo, rs.Base); err != nil {
        return fmt.Errorf("provision run: %w", err)
    }
    rs := rs
    s.start(ctx, rs.ID, func(runCtx context.Context) error { return s.engine.Resume(runCtx, rs, f) })
    return nil
}
```

`ResumeAll` calls `resumeRun(context.Background(), rs, f)` inside its loop (logging+continue on error, preserving today's per-run isolation). `Retry` calls `resumeRun(ctx, rs, f)` after its guards. The startup path keeps passing `context.Background()` (a resumed run must outlive any request); the on-demand path passes the request ctx, matching how `Submit` already threads ctx into `start` (and, with the OTel slice, links the submit span to the run).

### New method: `engine.ScratchExists` / `workspace.Exists`

A thin existence check on the run directory, so Retry can pre-flight before resuming:

```go
// workspace (GitManager)
// Exists reports whether the run's scratch directory is still on disk (not yet
// GC-reclaimed). Used by Retry to fail fast before attempting an in-place resume.
func (m *GitManager) Exists(runID core.RunID) bool {
    if !safeRunID(runID) { return false }
    _, err := os.Stat(m.runDir(runID))
    return err == nil
}

// engine
func (e *Engine) ScratchExists(id core.RunID) bool { return e.Workspace.Exists(id) }
```

(The exact engine→workspace delegation name follows the existing `ReclaimScratch`/`Provision` wiring; `ScratchExists` mirrors them.)

### HTTP: `POST /v1/runs/{id}/retry`

Handler mirrors `handlePush`'s error dispatch (`internal/api/handlers.go:140-164`):

```go
func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
    id, err := s.Sup.Retry(r.Context(), core.RunID(r.PathValue("id")))
    if err != nil {
        var re *supervisor.RetryError
        if errors.As(err, &re) {
            writeError(w, re.Status, re.Msg)
            return
        }
        writeError(w, http.StatusInternalServerError, err.Error())
        return
    }
    writeJSON(w, http.StatusAccepted, retryResponse{ID: string(id)})
}
```

Registered in `internal/api/router.go` alongside the other run actions:
`v1.HandleFunc("POST /v1/runs/{id}/retry", s.handleRetry)`.

`RetryError{Status int, Msg string}` is a new error type mirroring `PushError` (with an `Error()` method), so `errors.As` dispatch is identical.

### Client: `cm retry <run> [--watch]`

A new dispatch case + parser following `cm push`/`cm cancel` (`cmd/cm/main.go`):

- POST `/v1/runs/{run}/retry`; on `202`, decode `{"id":…}` and print `resuming <id>`.
- On `--watch`, after a successful POST, stream the run's events via the existing watch path (the same code `cm watch <run>` / `cm run --watch` use), so the user sees `run.started → … → run.done` live.
- Usage: `cm retry <run> [--watch]`. Errors print the server message and return non-zero (matching `printErr`).

The `cm` command-surface line and the `running-the-orchestrator` skill gain a short "Retry / resume a run" entry.

## Status & event semantics

In-place resume reuses `runDAG`'s existing behavior, so the observable timeline of a retried run is:

```
run.started → step.* (original) → run.done [failed/canceled]   ← original execution
run.started → step.* (only non-succeeded steps) → run.done [succeeded/failed]  ← retry
```

- The second `run.started` is expected and already documented in `runDAG` as consistent with at-least-once resume.
- Succeeded steps emit **no** new `step.started` on retry (they are seeded/skipped) — this is the observable proof that work was reused.
- The run's `error` field is cleared when `runDAG` sets `RunRunning` (`SetRunStatus(…, "")`) and re-written only if the retry itself ends in failure.

## Error handling

| Condition | Status | Message |
|---|---|---|
| Unknown run id | `404` | `run not found` |
| Run active (running / unwinding) | `409` | `run still in progress` |
| Status `succeeded` | `409` | `run succeeded; nothing to retry` |
| Status `pending`/`running` (persisted, not active) | `409` | `run still in progress` |
| Scratch GC-reclaimed | `409` | `scratch reclaimed; resubmit the flow` |
| Stored flow no longer parses/validates | `500` | `stored flow no longer parses: …` |

**Decision — `409` (not `410 Gone`) for reclaimed scratch:** stays within the small status-code vocabulary the existing handlers use (`404`/`409`/`502`); the message is explicit about cause and remedy.

## Edge cases & concurrency

- **GC reclaim race:** mitigated by step 5 (flip to `pending`) running before step 6 (scratch check). Once `pending`, the run leaves the GC selection set (`succeeded/failed/canceled`). If the scratch is found missing in step 6, the run is fully reverted to its original terminal status + error (step 6) so it is GC-eligible again and unchanged from the caller's view.
- **Double retry / retry while retrying:** the second call hits the active-map guard (step 1) → `409`. Once `runDAG` flips to `running` and registers in the active map, concurrent retries are rejected.
- **Retry of a retry that failed again:** allowed — the run is `failed` again and the scratch is still present.
- **Deterministically-failing flows** (e.g. a gate verifier that always fails): retry re-runs the failed step and fails again. This is correct — retry is for recoverable/transient failures or externally-changed conditions, not a flow fix.
- **`canceled` runs:** resumed identically to `failed`; their non-succeeded steps (some reset from mid-flight at cancel) re-run.

## Testing

The central challenge is proving — deterministically and offline — that resume **skips succeeded work** and **re-runs the failed step (and it now passes)**.

**Deterministic fail-then-pass:** give the failing step an **auto gate** whose verifier is an external-file probe, e.g. `test -f <tmp>/ok`. First run: file absent → gate fails → step fails → run fails. The test then creates the file and calls `Retry` → the same verifier passes → step succeeds → run succeeds. Real components, no mocks.

**Proof that the succeeded sibling was skipped:** count `step.started` events per step across the whole (replayed) event stream — the seeded step shows **1** (original only), the resumed step shows **2** (original + retry).

Test inventory:

- **Supervisor** (real engine + `mock`/file-gated steps):
  - `TestRetryResumesSkippingSucceeded` — 2-step flow (A succeeds, B file-gated fails); after `touch ok`, `Retry` → final `succeeded`; assert `step.started` counts A=1, B=2.
  - `TestRetryCanceledRunResumes` — cancel mid-run, then `Retry` resumes to completion.
  - `TestRetryRejectsSucceeded` → `409`.
  - `TestRetryRejectsActive` → `409` for an in-flight run.
  - `TestRetryUnknownRun` → `404`.
  - `TestRetryScratchReclaimedRejects` — `Reclaim` the scratch, then `Retry` → `409 "scratch reclaimed; resubmit the flow"` **and** persisted status + error fully reverted to the original terminal values (e.g. a reclaimed `canceled` run stays `canceled`, not `failed`).
- **`resumeRun` refactor:** existing `ResumeAll` tests must stay green (they now exercise the shared helper); the retry tests exercise the same helper via the on-demand path.
- **API:** `TestRetryEndpoint` — `202` + `{"id":…}` on success; each `RetryError` maps to its status (httptest, fake/stub supervisor as the existing handler tests do).
- **`cm` client:** `TestRetrySubcommandPostsRetry` (httptest asserts `POST /v1/runs/<id>/retry`, prints `resuming <id>`); `TestRetryWatchStreamsEvents` for the `--watch` path; usage/error-path coverage mirroring the push/cancel client tests.

Real-git/`gh` integration tests and any daemon child processes run sandbox-disabled, per project convention.

## Global constraints (carried into the plan)

- **No new dependencies.** Stdlib only (`net/http`, `os`, `errors`). `RetryError` mirrors `PushError`; no new modules.
- **`go 1.22`**; pinned deps untouched (modernc.org/sqlite v1.36.1, pressly/goose/v3 v3.24.1, oklog/ulid/v2 v2.1.1, expr-lang/expr v1.17.8, OTel v1.32.0).
- **Persist-then-publish** preserved — retry mutates persisted run/step status through the existing store methods before `runDAG` publishes events.
- **One blessed `core.Store` deviation** (`AppendEvents`) unchanged; no new store-port deviations.
- Single conventional-commit subjects, no body, no `Co-Authored-By`, never `--no-verify`; run `gofmt -l` yourself (not hook-enforced).

## Decisions log

1. **In-place resume** (reuse id + scratch, skip succeeded) over fresh-restart or hybrid — only model that reuses succeeded work; reuses proven machinery.
2. **`failed` and `canceled`** are both retryable; `succeeded` and in-progress are rejected.
3. **Extract a shared `resumeRun` helper** used by both `ResumeAll` and `Retry` (DRY; no drift).
4. **Pre-flight scratch-existence guard** with a status-flip-first ordering to close the GC race; reclaimed scratch → reject (no silent re-clone).
5. **`409`** for reclaimed scratch (consistency over `410` precision).
6. **`cm retry --watch`** included for parity with `cm run --watch`.
