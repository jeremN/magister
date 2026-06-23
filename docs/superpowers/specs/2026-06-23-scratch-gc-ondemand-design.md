# Scratch-GC round-out: `reclaimed_at` marker + `cm gc` / `cm rm` — Design

## Goal

Round out the scratch-GC story with two pieces:

1. A **`reclaimed_at` marker** so a run whose scratch has been reclaimed is never
   re-selected by the sweep again — killing the "re-stat every terminal run on
   every sweep, forever" inefficiency in the background janitor.
2. **On-demand reclaim** from the client: `cm gc` (reclaim all terminal scratch
   now) and `cm rm <run>` (reclaim one run's scratch now), so disk can be freed
   without waiting for the TTL ticker — and so reclaim works at all when the
   background janitor is disabled (`-scratch-ttl 0`).

Everything stays **store-driven** (query terminal runs, reclaim their dirs). It
reuses the existing reclaim chain — `SweepScratch → ReclaimableRuns →
ReclaimScratch → Workspace.Reclaim` — adding a marking step and two entry points.

## Background (current state)

- `Supervisor.SweepScratch(ctx, olderThan)` (internal/supervisor/gc.go) →
  `Store.ReclaimableRuns(ctx, before)` (`status IN ('succeeded','failed','canceled')
  AND updated_at < ?`) → per id `Engine.ReclaimScratch(id)` →
  `Workspace.Reclaim(runID) (removed bool, err error)` (stat-then-`RemoveAll`,
  under the per-run lock, `safeRunID`-guarded).
- The janitor (`cmd/magisterd`) runs a boot sweep + a ticker, calling
  `SweepScratch(ctx, time.Now().Add(-ttl))`.
- **The gap:** nothing records that a run's scratch is gone, so `ReclaimableRuns`
  keeps selecting every terminal run on every sweep forever (cheap stats, but
  unbounded re-selection; steady-state `removed` count is 0 only because the dir
  is already absent). There is no on-demand path — the janitor is the only trigger.
- `cm get` already gates the scratch path on `os.Stat`, so a reclaimed run shows
  no path; `cm push`/`pr`/`ship` on a reclaimed run already 404.

## Global Constraints

- **No new dependencies.** Stdlib only. `ReclaimError` mirrors the existing
  `PushError`/`RetryError`. No `go.mod` change.
- **`go 1.22`** unchanged. Pinned deps untouched: modernc.org/sqlite v1.36.1,
  pressly/goose/v3 v3.24.1, oklog/ulid/v2 v2.1.1, expr-lang/expr v1.17.8,
  OTel v1.32.0.
- **One DB migration** (`0004_run_reclaimed_at.sql`), additive + nullable, with a
  goose down. `sqlc` is not installed — the generated `internal/store/sqldb/
  query.sql.go` is hand-edited (established pattern, e.g. migration 0003).
- **Persist-then-publish** is unaffected (reclaim/mark are post-terminal store
  writes; no events).
- Commit hygiene: single conventional-commit subject lines, no body, no
  `Co-Authored-By` trailer, never `--no-verify`. `gofmt -l` clean.
- Real-git tests guard with `requireGitS`/`exec.LookPath("git")` and run
  sandbox-disabled.

## Design

### 1. Data model — the `reclaimed_at` marker

- **Migration `internal/store/migrations/0004_run_reclaimed_at.sql`** — same
  `-- +goose Up/Down` + `StatementBegin/End` form as 0003 (which does a bare
  `ADD COLUMN` / `DROP COLUMN` round-trip; modernc.org/sqlite v1.36.1 is SQLite
  3.35+, so `DROP COLUMN` works):
  - up: `ALTER TABLE runs ADD COLUMN reclaimed_at TEXT;` (nullable; no default —
    `NULL` means "scratch not yet reclaimed").
  - down: `ALTER TABLE runs DROP COLUMN reclaimed_at;`
- **`ReclaimableRuns` query** gains `AND reclaimed_at IS NULL`:
  `SELECT id FROM runs WHERE status IN ('succeeded','failed','canceled') AND
  reclaimed_at IS NULL AND updated_at < ? ORDER BY updated_at;`
  Once a run is marked, it drops out of every future sweep — steady state selects
  zero rows.
- **New store method `MarkReclaimed(ctx, id) error`** on `core.Store`:
  - SQLite: `UPDATE runs SET reclaimed_at = datetime('now') WHERE id = ?;`
    (new `-- name: MarkReclaimed :exec` query + hand-edited generated code +
    `SQLite.MarkReclaimed` wrapper).
  - `Mem`: a `reclaimedAt map[core.RunID]time.Time` (set on `MarkReclaimed`,
    consulted by `ReclaimableRuns` to skip marked ids). Mirrors how `updatedAt`
    is tracked today.
- No other store method changes. `GetRun`/`RunState` do **not** surface
  `reclaimed_at` (YAGNI — out of scope).

### 2. Reclaim flow — stamp-on-success

The marking rule is the crux:

- After `Engine.ReclaimScratch(id)` returns `err == nil`, call
  `Store.MarkReclaimed`. `err == nil` covers **both** `removed == true` (we
  deleted the dir) and `removed == false` (the dir was already absent) — both
  mean "scratch is gone", so both should stop future re-selection.
- On a reclaim **error** (`RemoveAll` failed), **do not** mark — log and leave it
  selectable so the next sweep retries.
- A `MarkReclaimed` store error is logged but non-fatal: the dir is already gone;
  worst case the run is re-selected next sweep (a no-op reclaim) until a later
  mark succeeds.

`SweepScratch` is refactored to call a shared private helper
`reclaimOne(ctx, id) (removed bool, err error)` that does
`ReclaimScratch` → mark-on-success. `ReclaimRun` (below) calls the same helper, so
the marking rule lives in exactly one place.

`SweepScratch`'s return value is unchanged: the count of runs whose dir was
**actually removed** (`removed == true`) — the meaningful "freed N scratch dirs"
number.

### 3. Surface — HTTP + `cm`

| Client | Endpoint | Behavior |
|---|---|---|
| `cm gc [--older-than <dur>]` | `POST /v1/gc?older_than=<dur>` | Reclaim **all terminal, not-yet-reclaimed** runs whose scratch still exists, **now** (cutoff = `now`, or `now-dur` when `older_than` is given). Returns `200 {"reclaimed": <N>}`. Reuses `SweepScratch`. |
| `cm rm <run>` | `DELETE /v1/runs/{id}/scratch` | Reclaim **one** run's scratch immediately (ignores TTL). Returns `200 {"removed": <bool>}`. |

- Both routes are registered inside the **auth-protected `v1` mux** (mutating
  ops), alongside `push`/`pr`/`ship`/`retry`/`cancel`.
- `cm rm <run>` reclaims the **scratch workspace only, not the run record** — the
  run row and its events/artifacts persist (just the on-disk clone is removed).
  Documented explicitly in the skill + the usage line. (Name per request; not
  `cm reclaim`.)
- `older_than` parsing: `time.ParseDuration`; absent/empty ⇒ `0` ⇒ cutoff `now`;
  unparseable ⇒ **400**.

**Supervisor API:**

- `cm gc` reuses `SweepScratch(ctx, cutoff)`; the handler computes
  `cutoff = time.Now().Add(-olderThan)`.
- New `Supervisor.ReclaimRun(ctx, id core.RunID) (bool, error)` for single-run
  reclaim, returning `removed` and a `*ReclaimError{Status, Msg}` (mirrors
  `PushError`/`RetryError`, mapped by the handler via `errors.As`).

### 4. Guards & edge cases

`ReclaimRun` guard order (mirroring `Retry`/`Push`):

1. **Active check** — if the run is in `s.runs` (running/unwinding/being
   retried), reject **409** ("run in progress; cannot reclaim its scratch"). Don't
   yank a live scratch.
2. **Load** — unknown run → **404**; store error → **500**.
3. **Terminal status** — only `succeeded`/`failed`/`canceled` are reclaimable;
   `pending`/`running` persisted → **409**.
4. **Reclaim** — `reclaimOne(ctx, id)`; reclaim error → **500**. Success →
   `200 {"removed": <bool>}`. An already-reclaimed / already-absent scratch →
   `200 {"removed": false}` (**idempotent** — the run still exists; only its
   scratch is gone).

`cm gc` needs no extra guard: `ReclaimableRuns` selects only terminal,
not-yet-reclaimed runs, and a concurrently-retried run has flipped to `pending`
(excluded by status) — so the bulk path can't grab an active or retrying run.

`Workspace.Reclaim` is unchanged (per-run lock + `safeRunID`).

### Components / files

- `internal/store/migrations/0004_run_reclaimed_at.sql` (create).
- `internal/store/query.sql` (edit `ReclaimableRuns`; add `MarkReclaimed`).
- `internal/store/sqldb/query.sql.go` (hand-edit: regenerated-equivalent for the
  edited + new query).
- `internal/store/sqlite.go` (edit `ReclaimableRuns` if needed; add
  `MarkReclaimed`).
- `internal/store/mem.go` (`reclaimedAt` set; `MarkReclaimed`; `ReclaimableRuns`
  exclusion).
- `internal/core/store.go` (add `MarkReclaimed` to the `Store` interface +
  doc comment).
- `internal/supervisor/gc.go` (refactor `SweepScratch` to use `reclaimOne`; add
  `reclaimOne`, `ReclaimRun`, `ReclaimError`, `reclaimErr`).
- `internal/api/handlers.go` (`handleGC`, `handleReclaimScratch`).
- `internal/api/router.go` (`POST /v1/gc`, `DELETE /v1/runs/{id}/scratch`).
- `cmd/cm/main.go` (`gc`/`rm` dispatch + methods + usage line).
- `.claude/skills/running-the-orchestrator/SKILL.md` (document `cm gc`/`cm rm`).

### Testing

- **Store** (mem_test.go + sqlite_test.go): `MarkReclaimed` stamps; a marked run
  is excluded from `ReclaimableRuns` even when terminal + old; a second
  `MarkReclaimed` is idempotent.
- **Supervisor** (gc_test.go): `SweepScratch` marks reclaimed runs and a **second
  sweep selects zero** (the regression guard for the re-selection fix);
  `ReclaimRun` happy path (`removed=true` then idempotent `removed=false`),
  404 unknown, 409 active (run still in `s.runs` via a blocking gate), 409
  non-terminal. Real-git via `requireGitS`.
- **API** (gc_test.go / reclaim_test.go): `POST /v1/gc` → `{"reclaimed":N}`;
  `?older_than=bad` → 400; `DELETE /v1/runs/{id}/scratch` 200/404/409.
- **cm** (gc_test.go / rm_test.go): `cm gc` POSTs `/v1/gc` (and forwards
  `--older-than`); `cm rm <run>` DELETEs `/v1/runs/<id>/scratch`; usage/exit codes.
- **Migration**: up adds the column; down reverses; `goose` round-trips.
- Full `go test -race ./...` green across all packages; `gofmt -l` clean.

## Out of scope (deferred)

- **Orphan scratch dirs** (a dir under the runs root with no run row) — needs a
  filesystem walk reconciled against the store, a different mechanism with its own
  safety concerns. Its own later slice if ever needed.
- **DB-row deletion** — run rows persist (now marked `reclaimed_at`); no row GC.
- Surfacing `reclaimed_at` in `cm get` / `GetRun`.
- A `cm rm --all` alias (use `cm gc`).
