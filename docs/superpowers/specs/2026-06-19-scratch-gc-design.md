# Scratch GC + dead-field cleanup — Design

**Date:** 2026-06-19
**Status:** Approved (design); ready for implementation plan.

## Problem

The daemon never reclaims per-run scratch clones. Layout is `<runsRoot>/<runID>/{base, wt/<step>}`. At run end the engine calls `Workspace.TeardownRun`, which removes only the `wt/` worktrees — **`<runID>/base` persists forever**, and for external-repo runs that is a full clone of the source repo. Nothing reclaims `<runID>/`, so a long-running daemon's disk grows unbounded. Scratch cannot simply be freed at run-end because `cm push`/`cm ship` operate on that clone *after* the run succeeds.

Secondary: `api.Server` carries two **dead struct fields** (`BearerToken`, `ShutdownTimeout`) — set in `cmd/magisterd/main.go` but never read (auth enforcement goes through the `Router(token)` parameter → `authMiddleware`; shutdown uses `cfg.ShutdownTimeout` directly). They are redundant and misleading.

**Non-goal / already done:** API auth is *not* a gap. `authMiddleware` (`internal/api/middleware.go`) does a constant-time `Bearer` compare and 401s on mismatch, enabled whenever `MAGISTER_BEARER_TOKEN` is set (empty token = documented loopback-open default). This slice does not change auth behavior.

## Goal

Reclaim a run's scratch directory on a **TTL-sweep-after-terminal** policy: a background janitor in the daemon periodically deletes the scratch of terminal runs whose terminal time is older than a configurable retention (default 24h). Remove the two dead struct fields. No DB migration, no new HTTP route, no new dependency.

## Architecture — store-driven TTL janitor

A daemon goroutine periodically asks the store for terminal runs older than the retention TTL and deletes each run's scratch directory, staying within the existing ports & adapters structure:

```
daemon janitor goroutine (boot sweep + ticker)
  └─ Supervisor.SweepScratch(ctx, olderThan)
       ├─ Store.ReclaimableRuns(ctx, olderThan)  → []RunID  (terminal & aged)
       └─ for each id: Engine.ReclaimScratch(id) → Workspace.Reclaim(id) → rm <Root>/<id>/
```

`updated_at` is the terminal-time source: `SetRunStatus` (the terminal-status write) executes `UPDATE runs SET status=?, error=?, updated_at=datetime('now')`, so a terminal run's `updated_at` equals the moment it went terminal. `cm push`/`cm ship` are read-only on the store, so they never reset it.

## Components

### 1. `core.Workspace.Reclaim(runID RunID) error` (new port method)

Removes a run's entire scratch directory. Added to the `core.Workspace` interface (same pattern as the prior `Provision`/`BasePath`/`TeardownRun` additions).

- **`GitManager.Reclaim`** (`internal/workspace/gitmanager.go`): under the per-run lock (`m.runLock(runID)`, like `TeardownRun`/`Commit`), `os.RemoveAll(m.runDir(runID))` where `runDir(id) = filepath.Join(m.Root, string(id))` (new helper; `baseDir`/`wtDir` are `<runDir>/base` and `<runDir>/wt`). Idempotent: a missing directory returns nil (`os.RemoveAll` already does).
  - **Safety guard (critical):** before removing, verify `runID` is a clean, non-empty single path segment — reject if it is empty, contains a path separator, or is `.`/`..` (e.g. via a `safeRunID` check). This prevents an empty or `..` id from turning `filepath.Join(Root, id)` into `Root` (or a parent) and `RemoveAll`-ing the whole runs root. On an unsafe id, return an error and touch nothing.
- **`Manager.Reclaim`** (`internal/workspace/workspace.go`): symmetric — removes its own per-run scratch directory under `m.Root` with the same `safeRunID` guard. (The plain `Manager` is test-only, but the port contract is honored faithfully.)

### 2. `core.Store.ReclaimableRuns(ctx, before time.Time) ([]RunID, error)` (new port method)

Returns the IDs of runs in a terminal status (`succeeded`, `failed`, `canceled`) whose `updated_at` is strictly before `before`. Added to the `core.Store` interface.

- **SQLite** (`internal/store`): a new sqlc query
  `SELECT id FROM runs WHERE status IN ('succeeded','failed','canceled') AND updated_at < ? ORDER BY updated_at`,
  with `before` formatted as `before.UTC().Format("2006-01-02 15:04:05")` to match the TEXT form `datetime('now')` writes. (TEXT comparison is correct for this fixed-width UTC format.) Regenerate `query.sql.go` via sqlc; the generated method is wrapped by the store's `ReclaimableRuns`.
- **Mem** (`internal/store/mem.go`): add `updatedAt map[RunID]time.Time`, set to `time.Now()` on `CreateRun` and `SetRunStatus` — mirroring exactly which SQL writes touch `runs.updated_at` (step transitions and `AppendEvents` do **not**). `ReclaimableRuns` returns terminal runs whose `updatedAt` is before `before`.

### 3. `Engine.ReclaimScratch(runID RunID) error`

A thin delegator to `e.WS.Reclaim(runID)` (mirrors the existing `Engine.BasePath`/`Engine.Provision` delegators). Keeps the supervisor talking to the engine, not the workspace directly.

### 4. `Supervisor.SweepScratch(ctx, olderThan time.Time) (int, error)`

The driver. Calls `s.store.ReclaimableRuns(ctx, olderThan)`; for each returned id calls `s.engine.ReclaimScratch(id)`. **Best-effort:** a per-run reclaim failure is logged (via `s.logger()`) and the sweep continues; the method returns the count successfully reclaimed and a non-nil error only if the store query itself failed. The caller supplies the cutoff (`time.Now().Add(-ttl)`); tests pass an explicit `olderThan` so no clock is injected.

Active runs cannot be selected (they are not in a terminal status), so a sweep never deletes an in-flight run's scratch; the 24h default age makes any terminal/cleanup race irrelevant.

### 5. Daemon janitor (`cmd/magisterd/main.go`)

After `ResumeAll`, if `cfg.ScratchTTL > 0`, start a goroutine:
1. Run one sweep immediately at boot (reclaims anything already past TTL from a prior process — self-healing across restarts).
2. Then a `time.Ticker(cfg.ScratchSweepInterval)` loop, each tick calling `sup.SweepScratch(ctx, time.Now().Add(-cfg.ScratchTTL))`.
3. Exit when the daemon's root/shutdown context is canceled.

`cfg.ScratchTTL <= 0` disables the janitor (logged once at boot). Sweep outcomes are logged (count reclaimed, or the query error).

**Config** (`internal/config/config.go`): two new fields, flags, and env:
- `ScratchTTL time.Duration` — flag `-scratch-ttl`, env `MAGISTER_SCRATCH_TTL`, default `24h`.
- `ScratchSweepInterval time.Duration` — flag `-scratch-sweep-interval`, default `1h`.

### 6. Cleanups

- Remove the dead `BearerToken` and `ShutdownTimeout` fields from `api.Server` (`internal/api/handlers.go`) and drop the corresponding assignments in `cmd/magisterd/main.go`'s `&api.Server{…}` literal. Auth and shutdown behavior are unchanged — the `Router(cfg.BearerToken)` parameter and `cfg.ShutdownTimeout` remain the live path.
- `cm get` (the run snapshot handler, `internal/api/handlers.go`) reports the `scratch` path only if the directory still exists (`os.Stat`), so a reclaimed run stops advertising a dead path. Today it reports unconditionally when `rs.Repo != "" && s.ScratchRoot != ""`.

## Data flow

`boot/tick → SweepScratch(now−ttl) → Store.ReclaimableRuns → []RunID → per id → Engine.ReclaimScratch → Workspace.Reclaim → os.RemoveAll(<Root>/<id>/)`.

After reclaim, a later `cm push`/`cm ship` on that run hits the **existing** behavior in `Supervisor.Push`: `base == "" || !dirHasGit(base)` → `404 "scratch repo for run … not found (reclaimed?)"`. `cm get` omits the now-dead `scratch` field. No new error paths are introduced for consumers.

## Error handling

- `Reclaim`: unsafe `runID` → error, runs root untouched; `os.RemoveAll` failure → returned and logged by the driver.
- `SweepScratch`: a single run's reclaim failure is logged and skipped (does not abort the rest of the batch); a `ReclaimableRuns` query failure aborts that cycle (logged) — the next tick retries.
- The janitor goroutine never panics the daemon; it logs and continues to the next tick.

## Testing

- **`GitManager.Reclaim`** (`internal/workspace`): create a run scratch dir with `base/` and `wt/<step>/`; `Reclaim` removes the whole `<Root>/<runID>/`; calling it again is a no-op (idempotent). Unsafe ids (`""`, `"../escape"`, `"a/b"`) → error returned and `Root` and sibling run dirs untouched.
- **`Store.ReclaimableRuns`**: SQLite with rows whose `updated_at` is set to controlled values — assert terminal+aged ids are returned and active/recent/non-terminal are excluded; verify the `<` boundary. Mem: seed runs of each status, assert the status filter and the comparison direction with `before = now+1h` (captures all terminal) vs `before = now−1h` (captures none).
- **`Supervisor.SweepScratch`**: Mem store + a temp-rooted `GitManager` — seed terminal runs with real scratch dirs and a non-terminal run with its own dir; sweep with a cutoff after their `updatedAt`; assert the terminal dirs are gone, the non-terminal dir survives, and the returned count matches.
- **Cleanups**: package builds + `go vet` clean; existing auth (`authMiddleware`) and shutdown tests still pass unchanged; a `cm get` test asserts the `scratch` field is omitted when the dir is absent.

## Out of scope (noted follow-ups)

- **Orphan scratch dirs** with no corresponding run row (e.g. a hard crash that left a dir but lost the row) are not swept in v1 — the janitor is store-driven and only reclaims runs it can see. A later filesystem-walk pass could add orphan-by-mtime cleanup.
- **Explicit on-demand reclaim** (`cm gc` / `cm rm <run>`) — deferred; the background janitor is the only trigger in v1.
- **DB-row GC** — `runs`/`events`/`steps` rows persist indefinitely; this slice reclaims only on-disk scratch. Row retention is a separate concern.

## Global constraints

- Go 1.22; stdlib only, **no new dependency**; no DB migration (the `updated_at` column already exists and is bumped by `SetRunStatus`).
- Two new port methods on `core.Store` (`ReclaimableRuns`) and `core.Workspace` (`Reclaim`), each implemented in every adapter — same blessed pattern as the earlier `AppendEvents`/`Provision`/`BasePath` additions.
- Engine run lifecycle is otherwise untouched; GC is a post-terminal, store-driven background activity.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`, never `--no-verify`. `gofmt`/`go vet`/`go test -race ./...` clean before merge.
