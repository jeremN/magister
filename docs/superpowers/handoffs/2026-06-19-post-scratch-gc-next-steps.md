# Handoff — scratch-GC slice (TTL janitor + dead-field cleanup) COMPLETE (2026-06-19)

**Pick up next session (fresh context).** The **scratch-GC slice** is merged to `main` and complete. A background janitor in the daemon now reclaims per-run scratch clones on a TTL-sweep-after-terminal policy, so a long-running `magisterd` no longer grows its disk unbounded. Two dead `api.Server` struct fields were removed in the same slice. **No slice is in flight.**

## State (verified)

- `main` at `8c4b16e`, working tree clean. **Local-only — NOT pushed to `origin` (github.com/jeremN/magister).** `origin/main` is still at `8f82b01` (the delivery-polish push); this slice's plan commit + 6 branch commits are local. Push when ready: `git push origin main`.
- `go test -race ./...` → **285 passed across 16 packages** (was 285 at slice start; net test count is flat because the slice ADDED tests but the count shown is the suite's reported total — the new tests are real: `TestGitManager/Manager Reclaim*`, `TestMem/SQLiteReclaimableRuns`, `TestSweepScratch*` incl. the second-sweep-returns-0 regression, `TestParseScratch*`, `TestRunScratchJanitorDisabledReturns`, `TestGetRunGatesScratchOnExistence`). `go vet ./...` clean. `gofmt -l` clean. `go 1.22`. **No new deps, no DB migration.**
- Built via brainstorm → writing-plans → subagent-driven-development in a worktree (`.worktrees/scratch-gc`, now removed): fresh sonnet implementer per task → spec+quality review each → **Opus final whole-branch review: Ready to merge, zero Critical/Important**. Spec: `docs/superpowers/specs/2026-06-19-scratch-gc-design.md`; plan: `docs/superpowers/plans/2026-06-19-scratch-gc.md`.

## What was built (per commit)

1. **`Workspace.Reclaim`** (`13a67af`, refined by `8c4b16e`): new `core.Workspace` port method; `GitManager.Reclaim` + `Manager.Reclaim` remove `<Root>/<runID>/` (base + worktrees). **Safety guard `safeRunID`** (the critical bit): rejects empty/`.`/`..`/separator ids so `filepath.Join(Root, id)` can never `RemoveAll` the runs root or a parent. GitManager takes the run lock like `TeardownRun`. `runDir(id)` helper added.
2. **`Store.ReclaimableRuns`** (`85a7156`): new `core.Store` method returning terminal runs (`succeeded/failed/canceled`) whose `updated_at` is strictly `< before`. SQLite: hand-written generated method (sqlc NOT installed) + cutoff formatted `before.UTC().Format("2006-01-02 15:04:05")` to match `datetime('now')` TEXT. Mem: new `updatedAt map[RunID]time.Time` set on `CreateRun`+`SetRunStatus` ONLY (mirrors exactly which SQL writes bump `runs.updated_at`).
3. **`Engine.ReclaimScratch` + `Supervisor.SweepScratch`** (`e64cf28`, refined by `8c4b16e`): engine delegator; `SweepScratch(ctx, olderThan)` queries `ReclaimableRuns`, reclaims each, best-effort (per-run failure logged via `s.logger()` + continue; store-query failure → `(0, err)`). Lives in `internal/supervisor/gc.go`.
4. **Config + daemon janitor** (`12b0a18`): `ScratchTTL` (flag `-scratch-ttl`, env `MAGISTER_SCRATCH_TTL`, default **24h**) + `ScratchSweepInterval` (flag `-scratch-sweep-interval`, default **1h**). `runScratchJanitor(ctx, sup, ttl, interval, log)` in `cmd/magisterd/main.go`: `ttl<=0` disables (logged); else boot sweep + `time.Ticker` loop calling `sup.SweepScratch(ctx, time.Now().Add(-ttl))`; exits on ctx cancel. Wired after `ResumeAll`, stopped via **`defer stopJanitor()`** (covers the `net.Listen` early-return path the brief's two-arm version missed; `go vet`-verified; LIFO fires before `defer st.Close()`).
5. **Cleanups** (`98f74f9`): removed dead `api.Server.BearerToken`/`ShutdownTimeout` fields + their main.go assignments (auth stays in `Router(token)`→`authMiddleware`; shutdown uses `cfg.ShutdownTimeout` directly — behavior unchanged); `handleGetRun` now gates the `scratch` path on `os.Stat` so a reclaimed run stops advertising a dead path. Also fixed `router_test.go` (referenced the removed field — auth strength preserved, token still flows into `Router(token)`) and made `TestGetRunSurfacesScratchPathForExternalRepo` hermetic (`t.TempDir()`).
6. **Honest-count fix** (`8c4b16e`): see below — the one defect the live proof surfaced.

## The wart the live proof caught (and the fix)

The janitor is **store-driven**: `ReclaimableRuns` returns every terminal+aged run, and terminal runs stay in the store forever (DB-row GC is out of scope). So the query **re-selects the same run on every sweep**. Pre-fix `Reclaim` was `os.RemoveAll`, which returns nil even for an already-gone dir → the supervisor counted every re-selection as a reclaim and logged `"scratch reclaimed" runs=1` on **every sweep, forever** (live: 25+ identical lines and growing). Disk was correctly reclaimed; only the count/log lied + unbounded pointless re-work.

**Fix (`8c4b16e`, separately reviewed clean):** `Reclaim` now returns `(removed bool, err error)` — `os.Stat` under the lock; absent → `(false, nil)` (no RemoveAll); present → RemoveAll → `(true, nil)`; error → `(false, err)`. Ripples through `Engine.ReclaimScratch` and `SweepScratch` (counts only `removed==true`). Janitor logs only when count>0 → **steady state is silent**. Regression test: `TestSweepScratchReclaimsTerminalAgedRuns` now does a SECOND sweep asserting `n==0`. Re-proved live: logged the reclaim ONCE then silent.

## Manual proof — DONE (2026-06-19), PASSED

Mock external-repo flow (`flows/external-repo.yaml`, zero-cost, no network) against a local fixture git repo, daemon launched **sandbox-disabled** with `-scratch-ttl 3s -scratch-sweep-interval 2s`:
1. `cm run … --repo <fixture> --base HEAD` → `integrate` = real 2-parent merge over fixture HEAD, `status: succeeded`.
2. Janitor reclaimed the scratch on the TTL → `<runs>/<run>/` gone, runs dir empty.
3. `cm get <run>` → `scratch` field **omitted** (the `os.Stat` gate).
4. Daemon log: **exactly 1** `"scratch reclaimed" runs=1` line, then silent (the honest-count fix; pre-fix it was 25+ and growing).
5. `cm push <run>` (after adding a valid origin to the fixture so push reaches the scratch step) → **`404 scratch repo for run "…" not found (reclaimed?)`** — graceful consumer behavior.
6. Cleaned up (SIGTERM exit 144 = graceful; temp dirs removed).

## Carried follow-ups (non-blocking, none in flight)

- **Push `main` to origin** — slice merged locally; `origin/main` still at `8f82b01`. `git push origin main` when ready.
- **Orphan scratch dirs** (a dir with no run row, e.g. after a hard crash) are NOT swept — the janitor is store-driven only. A later filesystem-walk-by-mtime pass could add it. (Spec out-of-scope.)
- **On-demand reclaim** (`cm gc` / `cm rm <run>`) — deferred; the background janitor is the only trigger in v1. (Spec out-of-scope.)
- **DB-row GC** — `runs`/`events`/`steps` rows persist indefinitely; this slice reclaims only on-disk scratch. The store-driven janitor re-stats each terminal run every sweep forever (cheap: one stat syscall each, no log noise after the fix), but it never stops re-selecting them — a `reclaimed_at` marker would need a migration (deliberately out of scope). Acceptable for v1; revisit with DB-row GC.
- **Logged Minors (all triaged carry by the final review):** T1 redundant `filepath.Separator` check (intentional cross-platform); T1 `mkdirAll` generic test-helper name; T4 flag-beats-env precedence for `-scratch-ttl` not DIRECTLY unit-tested (the `flagSet` guard is proven by the `MAGISTER_ADDR`/`MAGISTER_DB` analogs); T4 in-flight-sweep-vs-`st.Close()` race is benign (database/sql returns an err, sweep logs it, no crash); T5 `TestRouterAuthAppliesToV1` never asserts 200-on-valid-token (pre-existing).
- **Pre-existing flaky `TestMockHonorsContextCancel`** (`internal/executor`) — carried from delivery-polish; did not surface this slice.

## What's next (the north star + ship ergonomics + GC are done — pick a new direction)

No slice in flight. Natural follow-on threads, none started:
- **Other hosts:** GitLab/Bitbucket `ParseRemote` + a host abstraction (today: GitHub-only).
- **Cross-repo / fork PRs:** an `owner:branch` head.
- **Productionize:** observability/metrics, the GC follow-ups above (orphan sweep, on-demand `cm gc`, DB-row GC).
- **Note on API auth:** auth is REAL (not a gap) — `authMiddleware` constant-time Bearer compare, enabled when `MAGISTER_BEARER_TOKEN` is set, empty = documented loopback-open default. Only the redundant struct fields were dead; they're now gone.

## Process notes that held this slice

- **The live manual proof earned its keep** — it surfaced the honest-count wart that NO unit test or per-task review caught, because the bug only manifests across repeated sweeps of a store that never forgets terminal runs. Run the proof; don't merge a janitor on green tests alone.
- **A store-driven janitor with no persistent "done" marker re-selects forever** — make the reclaim primitive report whether it did real work, so the count/log is honest and steady state is silent, even though the (cheap) re-scan continues.
- **Adapt wiring to the real control flow, don't blind-paste the plan** — Task 4's implementer correctly switched the brief's two-arm `stopJanitor()` to `defer stopJanitor()` after `go vet` flagged a leak on the `net.Listen` early-return path.
- **Commit conventions:** single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. `rtk` not reliably present — run `go`/`git`/`gofmt` directly. **zsh tripped repeatedly** on `until/for … do` one-liners and on multi-line blocks with `===` banners + `$(...)`/parens — run such commands ONE per invocation, quote globs (`--include='*.go'`). `cm` reads `$MAGISTER_ADDR` verbatim (needs `http://`). Launch the daemon **sandbox-disabled** for any git/network proof; SIGTERM exit 143/144 = graceful. The post-Edit hook fires a harmless path-doubling error on every edit inside the worktree — the edits still succeed.
