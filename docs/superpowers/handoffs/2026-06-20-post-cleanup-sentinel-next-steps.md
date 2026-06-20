# Handoff — run-not-found sentinel + deterministic Mock cancel: MERGED to main (2026-06-20)

**Start here next session.** The **cleanup slice is DONE and MERGED to `main`** (fast-forward, `main` at **`d37781f`**, 4 commits off `14bfe80`). Full suite **319 passed / 17 packages `-race`**, vet + gofmt clean. Final Opus whole-branch review **Ready-to-merge=Yes, zero Critical/Important** (one Minor, spec-prescribed — see below). Worktree + branch cleaned up. **Pushed to origin** (`origin/main` at `d37781f`). This clears two of the long-carried debt items.

## What shipped

Two small, independent pieces of carried tech debt (stdlib only, **no new dep**, no migration, no schema change, Go 1.22). Spec `…/specs/2026-06-20-cleanup-run-not-found-sentinel-design.md`, plan `…/plans/2026-06-20-cleanup-run-not-found-sentinel.md`.

**Part A — `core.ErrRunNotFound` sentinel (closes the `GetRun→404` masking).** Before, `Store.GetRun` returned an untyped error for both "missing run" and "storage failure," and all four callers mapped *any* `GetRun` error to HTTP 404 — so a real DB fault read as "not found."
- `1aa728c` `feat(core): ErrRunNotFound sentinel for Store.GetRun` — `var core.ErrRunNotFound = errors.New("run not found")`; `*store.SQLite.GetRun` (on `sql.ErrNoRows`) and `*store.Mem.GetRun` (on map-miss) now `return fmt.Errorf("unknown run %q: %w", id, core.ErrRunNotFound)`. Message preserved, now `errors.Is`-matchable. Non-not-found paths (SQLite's raw non-`ErrNoRows` return, the `loadSteps` error) keep returning raw errors → now correctly 500.
- `a59d409` `fix(api): distinguish unknown-run 404 from storage-error 500` — `handleGetRun` (handlers.go) + `handleEvents`/SSE (sse.go) branch `errors.Is(err, core.ErrRunNotFound)` → `404 "unknown run"`, else → `500 "internal error"`. (`sse.go` gained an `errors` import.) New `internal/api/getrun_test.go` (404 via Mem-backed `testServer`; 500 via a `getErrStore` embedding `*store.Mem` overriding `GetRun`).
- `51ab5af` `fix(supervisor): distinguish unknown-run 404 from storage-error 500 in Push/PR` — `Supervisor.Push` (supervisor.go) + `prCore` (pr.go) branch the same way (not-found → `pushErr/prErr(404, "unknown run %q", id)`; other → `…(500, "load run %q: %v", id, err)`), deleting both stale `// TODO: no store not-found sentinel` comments. (`errors` added to both files.) New `TestPushStorageError500`/`TestPRStorageError500` (shared `getErrStore` in push_test.go); existing `TestPushUnknownRun`/`TestPRUnknownRun404` stay green (the now-wrapped error still maps to 404).

**Part B — deterministic `Mock.Run` cancellation (fixes the latently-flaky `TestMockHonorsContextCancel`).**
- `d37781f` `fix(executor): Mock.Run checks context before delay (deterministic cancel)` — hoisted the `ctx.Err()` guard to the TOP of `Mock.Run`, before the `Delay` select, and removed the now-redundant `else if` arm. The in-flight `<-ctx.Done()` case inside the select is retained (handles cancel *during* a real delay). The select previously raced two ready channels (5 ns `time.After` vs an already-closed `ctx.Done()`) → a timer-win returned nil → test failure. The Task-4 characterization actually **reproduced** the flake: **2 failures / 2000 runs** pre-fix, **0 / 2000** post-fix.

## The behavior change (only one, and it's the point)
Genuine unknown-run requests still return **404**. The only new behavior: a genuine **storage error** now surfaces as **500** instead of a misleading 404, at `GET /v1/runs/{id}`, `GET /v1/runs/{id}/events`, `cm push`, `cm pr`.

## The Minor (accepted, spec-prescribed)
Opus flagged an asymmetry: the supervisor 500 body interpolates the raw error (`"load run %q: %v"`, the format the spec table mandated and the same convention every other `*PushError`/`*PRError` already uses — push/pr also return raw git stderr on 502s), while the api 500 body is a generic `"internal error"`. Each layer follows its own established convention (api read-surface hides internals; operator CLI actions surface detail). Authenticated loopback SQLite daemon → low severity. Reviewer: "No change required for this slice." Left as-is deliberately.

## Open follow-ups (carried)
- **(still unmerged) `multi-host` GitLab slice** — CODE-COMPLETE at `36eb9fa` (worktree `.worktrees/multi-host` still present), awaiting a live gitlab.com proof (no account yet). See `2026-06-19-multi-host-gitlab-next-steps.md`. **DO NOT merge until that proof passes.**
- **(observability backlog):** structured/request-scoped logging (request-ID through HTTP + engine — the logging middleware already stamps a `request id`); OTel tracing (needs a dep). Metric triad + `/metrics` + liveness/readiness are done.
- **(delivery axis):** cross-repo/fork PRs (`owner:branch` head).
- **(both long-carried debt items above are now DONE):** `GetRun→404` sentinel ✅ and flaky `TestMockHonorsContextCancel` ✅.

## Process notes
- Subagent-driven: haiku implementers + reviewers for Tasks 1/2/4 (trivial transcription); sonnet review for Task 3 (two call-sites + shared fake); Opus final. Every per-task and the final review returned zero Critical/Important.
- **Stray-edit incident (new process lesson):** the Task-3 implementer applied a *partial* `getErrStore` edit to the **main checkout's** `internal/supervisor/push_test.go` (8 lines) in addition to the worktree where the real work was committed — an uncommitted stray that blocked the ff-merge with "Please commit your changes or stash them." Diagnosed via `git diff`: it duplicated part of the already-committed, reviewed `d37781f` version, so `git restore`-ing main's file was safe (the authoritative full version arrived via the merge). **Lesson:** after a subagent finishes, the MAIN checkout can carry a stray duplicate of worktree work; if the ff-merge complains about uncommitted changes, `git diff` the offending file and confirm it's a duplicate of committed work before discarding. The worktree's `/sdd/` exclude (in `.git/info/exclude`) does NOT cover stray edits to tracked files in the main tree.
- No live daemon smoke this slice (unlike readiness/metrics): the 404/500 paths run through the real handlers via `httptest` and the Mock fix is proven deterministic at `-count=2000` — there's no process/signal/clock interaction a live smoke would add. Stated the rationale rather than silently skipping.
- Commit hygiene held: single conventional subject, no body, no `Co-Authored-By`, never `--no-verify`. zsh trips on `$(...)`/`cd` in multi-line Bash-tool calls — one command per invocation / absolute paths.
