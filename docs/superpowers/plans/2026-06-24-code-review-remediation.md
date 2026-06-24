# Code-Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the 1 Critical + 21 Important findings from the 2026-06-24 whole-codebase review, in priority order, each with TDD and an independently-committable deliverable.

**Architecture:** Go 1.22 agent-orchestrator (ports-and-adapters, persist-then-publish, goroutine-per-step DAG). Fixes span `join`, `engine`, `workspace`, `core`, `cmd/cm`, `cmd/magisterd`, `config`, `store`, `api`, `flow`.

**Tech Stack:** Go 1.22, stdlib-favored. No new deps.

## Global Constraints

- **No new dependencies**, no `go.mod`/`go.sum` change, `go 1.22` unchanged; pinned deps untouched (modernc.org/sqlite v1.36.1, pressly/goose/v3 v3.24.1, oklog/ulid/v2 v2.1.1, expr-lang/expr v1.17.8, OTel v1.32.0).
- **TDD throughout:** every behavior change gets a test that fails first, then the fix.
- **Persist-then-publish** and **ports-and-adapters** boundaries preserved.
- Commit hygiene: single conventional-commit subject lines, no body, no `Co-Authored-By` trailer, never `--no-verify`. `gofmt -l` clean. Stage explicit files (never `git add -A`; an untracked `.superpowers/` scratch dir exists).
- Real-process/real-git tests run **sandbox-disabled** if the sandbox blocks exec.
- Whole suite after each task: `go test ./<touched-pkgs>/`; full `go test -race ./...` at the end.

---

## Task 1: [C1, Critical] Multi-branch join resumes remaining branches after a resolved conflict

**Problem:** `Merge.Join` (`internal/join/join.go:48-59`) stops at the first conflicting branch and returns `*ConflictError`. The engine's `resolveConflictEscalation` (`internal/engine/engine.go`) resolves *that* conflict, commits, and returns `StepSucceeded` — it never merges the branches after the conflicting one. With ≥3 branch-backed inputs under `on_conflict: escalate`, branches after the conflict are silently dropped and the result looks successful.

**Fix approach:** After `resolveConflictEscalation` commits the resolved conflict, **resume the merge of the remaining branches** before declaring success: re-run the merge strategy (`Merge.Join` is idempotent — `git merge` of an already-merged branch is a clean no-op "Already up to date"), and if a *later* branch also conflicts, re-enter the same escalation ladder (recurse). Terminates because each escalation merges at least one more branch (finite branch set).

**Files:**
- Modify: `internal/engine/engine.go` (`resolveConflictEscalation` — after the approved `WS.Commit`, re-run the join `attempt` for remaining branches instead of returning success directly; recurse on a further `*ConflictError`)
- Test: `internal/engine/engine_test.go` (new 3-branch conflict test, real-git via the existing `newGitEngine` helper)
- Possibly: `internal/join/join.go` (only if a cleaner resume requires it — prefer leaving `Merge.Join` idempotent and re-running it)

**Interfaces:**
- Consumes: `join.Merge.Join`, `join.ConflictError{Branch,Paths,WorkDir}`, `engine.escalateJoin`/`resolveConflictEscalation`, `e.WS.Commit`, `gate.AutoApprover`.
- Produces: a `resolveConflictEscalation` that, on approval, concludes by re-running the join to merge all remaining branches (recursing through the ladder for any subsequent conflict).

- [ ] **Step 1: Write the failing 3-branch test.** In `internal/engine/engine_test.go`, add a test using `newGitEngine` (skips if git absent; run sandbox-disabled). Build a flow: a base step, then three isolated steps `a`,`b`,`c` (each `Workspace: WSIsolated`) where `a` and `b` both modify a shared file (so `b` conflicts when merged after `a`), and `c` adds an independent file; a join step `integrate` with `Needs:[a,b,c]`, `Join:{Strategy:JoinMerge, OnConflict:FailEscalate}`, gate auto-approved (`gate.AutoApprover`). After `eng.Run`, assert the run succeeds AND the join commit's tree contains **all three** branches' files (read the integrate step's workDir / committed result). Use the existing engine git-test patterns (`TestEngineFanOutFanIn`, `TestJoin*`) as the structural model; the mock executor writes per-step files.

- [ ] **Step 2: Run it — expect FAIL** (c's file missing / run result lacks branch c). `go test ./internal/engine/ -run TestJoinMultiBranch -count=1` (sandbox-disabled). Capture the RED output.

- [ ] **Step 3: Implement the resume.** In `resolveConflictEscalation`, after the approved `e.WS.Commit(runID, s, workDir)` succeeds, do NOT return success immediately. Instead re-run the join for remaining branches by calling `e.attempt(ctx, runID, s, inputs, next+1, workDir, "")` (which re-invokes `Merge.Join`; already-merged branches are clean no-ops). Handle its return exactly like `runStep`'s join disposition: on success → persist `StepSucceeded` + return; on a `*gate.VerifierFailure`/normal error → persist `StepFailed` + return; on a fresh `*join.ConflictError` → recurse into `escalateJoin`/`resolveConflictEscalation`. Keep the arbiter-cost carry (`res.CostUSD = ares.CostUSD`) accumulated across rungs. Preserve persist-then-publish on every branch. Keep the attempt-number monotonic.

- [ ] **Step 4: Run it — expect PASS** + run the existing join/escalation tests (`go test ./internal/engine/ -run 'TestJoin|TestEscalate|TestRetry' -count=1`, sandbox-disabled). All green.

- [ ] **Step 5: gofmt + commit.** `gofmt -w` the two files; `git add internal/engine/engine.go internal/engine/engine_test.go` (+ join.go if touched); commit `fix(join): merge remaining branches after a resolved escalation conflict`.

---

## Task 2: [Theme 1] Thread context into git subprocesses so step timeout/cancel can kill them

**Problem:** 5 of 8 exec sites use `exec.Command` (no context): `internal/join/git.go:19`, `internal/workspace/gitmanager.go:142`, `internal/workspace/provision.go:46`, `internal/workspace/push.go:67`, `internal/executor/discover.go:19`. A hung git op (clone/push are network-bound) ignores step-timeout/run-cancel and blocks the per-step goroutine.

**Fix approach:** Thread `context.Context` from the engine's step context down to every git subprocess and switch `exec.Command`→`exec.CommandContext(ctx, …)`. This requires adding a `ctx context.Context` first parameter to the `core.Workspace` interface methods that shell out (`Commit`, `Provision`, `Reclaim`, `TeardownRun`; `For` does no exec → leave it, or add for symmetry — prefer minimal: only the shelling methods), to the `join` git helper, and to `executor.discoverGit`. Update both `workspace.Manager` (no-op impls) and `workspace.GitManager`, the `engine` wrappers/call sites, the `supervisor` callers, and all tests.

**Files:**
- Modify: `internal/core/ports.go` (add `ctx` to `Workspace.Commit/Provision/Reclaim/TeardownRun`)
- Modify: `internal/workspace/gitmanager.go`, `internal/workspace/workspace.go`, `internal/workspace/provision.go`, `internal/workspace/push.go` (internal `git(...)` helper takes ctx; `CommandContext`)
- Modify: `internal/join/git.go` (`gitCmd` takes ctx; `Merge.Join`/`Synthesize` already receive `ctx` from `Strategy.Join` — thread it), `internal/join/join.go`, `internal/join/synthesize.go`
- Modify: `internal/executor/discover.go` (`discoverGit(ctx, workDir)`), `internal/executor/cli.go` (pass `ctx` to discover)
- Modify: `internal/engine/engine.go` (pass step `ctx`/`attemptCtx` to `WS.Commit`/`Provision`/`Reclaim`/`TeardownRun` and the Engine wrapper methods `ProvisionRun`/`ReclaimScratch` — thread a ctx; the supervisor passes its own ctx)
- Modify: `internal/supervisor/*.go` callers of `Provision`/`Reclaim`/`TeardownRun`/`Push`/`BasePath` (BasePath does no exec — leave)
- Tests: update all callers in `*_test.go` to pass `context.Background()`

**Interfaces:**
- Consumes: the current `core.Workspace` interface + `join.Strategy.Join(ctx,…)` (already has ctx).
- Produces: `Workspace.Commit(ctx, runID, s, workDir)`, `Provision(ctx, runID, repo, base)`, `Reclaim(ctx, runID)`, `TeardownRun(ctx, runID)`; `join.gitCmd(ctx, workDir, args…)`; `executor.discoverGit(ctx, workDir)`. All git subprocesses via `exec.CommandContext`.

- [ ] **Step 1: Write a failing cancellation test.** In `internal/workspace/gitmanager_test.go` (or a new test), prove a canceled context aborts a git op: provision/clone or a `git` helper call with an already-canceled `ctx` returns promptly with `ctx.Err()` (or a context-canceled error), not a hang/success. Model it on how `gate/verifier_test.go` would test cancellation. Keep it fast (a canceled ctx → immediate return).

- [ ] **Step 2: Run it — expect FAIL** (compile error: methods don't take ctx / op ignores cancel). Capture RED.

- [ ] **Step 3: Implement the ctx threading.** Add `ctx` params as above; switch every git `exec.Command`→`exec.CommandContext(ctx, …)`. Keep all existing argv hardening (`safeRunID`, `--end-of-options`, `protocol.ext.allow=never`, etc.) byte-for-byte. The plain `Manager` no-op methods just accept and ignore ctx. In the engine, use the step's `attemptCtx`/`ctx` (the same one bounding the executor) for `Commit`; for `Provision`/`Reclaim`/`TeardownRun` use the relevant lifecycle ctx (the supervisor's run ctx / the GC ctx). Update every test caller to pass `context.Background()`.

- [ ] **Step 4: Run it — expect PASS** + `go test ./internal/workspace/ ./internal/join/ ./internal/executor/ ./internal/engine/ ./internal/supervisor/ -count=1` (sandbox-disabled). All green.

- [ ] **Step 5: gofmt + commit.** Commit `fix(workspace,join,executor): thread context into git subprocesses for cancellation`.

---

## Task 3: [Theme 5] CLI/daemon lifecycle robustness

Four independent localized fixes.

**Files:**
- Modify: `cmd/cm/main.go` (per-request timeout)
- Modify: `cmd/magisterd/main.go` (janitor join-before-close; `ReadHeaderTimeout`)
- Modify: `internal/config/config.go` (OTEL endpoint flag-vs-env precedence)
- Tests: `cmd/cm/*_test.go`, `internal/config/*_test.go`

- [ ] **Step 1 (cm timeout):** `cmd/cm/main.go:36` — `&http.Client{Timeout: 0}` makes every non-streaming subcommand hang forever on a dead daemon. Fix: give the client a sane timeout (e.g. `30 * time.Second`) for all commands EXCEPT the SSE/`watch` path, which needs `Timeout: 0`. Implement by constructing the client per-dispatch with a normal timeout, and have `watch` (the `/events` SSE reader, `main.go:157`) use a separate no-timeout client (or a per-request context with no deadline). **Test first:** a test that a non-watch command returns a non-zero exit (not a hang) when the server never responds — use an `httptest` server that blocks, with a short client timeout injected, asserting the command returns an error promptly. (If injecting the timeout cleanly is hard, make the timeout a `client` field defaulting to 30s and 0 for watch, and unit-test `client` directly.)

- [ ] **Step 2 (janitor join):** `cmd/magisterd/main.go:110,131-133` — `stopJanitor()` cancels the janitor ctx but doesn't wait for the goroutine to exit before `defer st.Close()` fires → possible store use-after-close. Fix: have `runScratchJanitor` signal completion (return/close a done channel or use a `sync.WaitGroup`); after `stopJanitor()`, wait for it before the store closes. Reorder defers / add an explicit join so the janitor goroutine is guaranteed finished before `st.Close()`. **Test:** if hard to unit-test the main wiring, add a focused test on `runScratchJanitor` proving it returns promptly after ctx cancel (so the join can't block). Note in the report if the main-wiring ordering is verified by reading, not a test.

- [ ] **Step 3 (ReadHeaderTimeout):** `cmd/magisterd/main.go:137-141` — only `ReadTimeout` is set. Add an explicit `ReadHeaderTimeout` (e.g. `5 * time.Second`) to the `http.Server` to guard Slowloris header-exhaustion deterministically. (No behavior test needed — a one-line server-config addition; confirm via reading + build.)

- [ ] **Step 4 (OTEL precedence):** `internal/config/config.go:67-69` — `OTEL_EXPORTER_OTLP_ENDPOINT` overrides an explicit `-otel-endpoint ""` because it tests value-equality, not whether the flag was set. The file already has a `flagSet(fs, name)` helper (used for `otel-service-name` at line 70). Fix: gate the env fallback on `!flagSet(fs, "otel-endpoint")`, mirroring the service-name handling. **Test first:** in `config_test.go`, assert that an explicit `-otel-endpoint ""` (or any explicit value) WINS over a set `OTEL_EXPORTER_OTLP_ENDPOINT` env, and that env still applies when the flag is absent.

- [ ] **Step 5:** Run `go test ./cmd/... ./internal/config/ -count=1`; gofmt; commit `fix(cm,magisterd,config): per-request timeout, janitor join-before-close, ReadHeaderTimeout, OTEL flag precedence`.

---

## Task 4: [Theme 2] `transition` propagates persist failures

**Problem:** `internal/engine/engine.go:574-585` — `transition` swallows a `SaveStepTransition` error (logs + publishes a synthetic `StepFailed`) but returns void, so `runStep` proceeds as if the transition (e.g. `StepSucceeded`) was durable. A run can finish "succeeded" while a step's success was never persisted, and the SSE stream shows a contradictory `StepFailed`.

**Fix approach:** Make `transition` return its persist error so callers can react. The minimal correct behavior: when a step's terminal/transition persist fails, the step (and thus the run) must fail coherently rather than silently proceed. Change `transition` to `error`-returning; in `runStep`/`attempt`/the join paths, on a persist error from the success transition, treat the step as failed (set `lastErr`, fall into the failure disposition) instead of returning success.

**Files:**
- Modify: `internal/engine/engine.go` (`transition` returns `error`; update all ~15 call sites — the ones whose failure must abort the step react to it; the already-failing-path transitions can stay best-effort but should at least not mask)
- Test: `internal/engine/engine_test.go` (extend the existing `failingStore` pattern, `engine_test.go:253-259`)

- [ ] **Step 1: Write the failing test.** Using the existing `failingStore` (whose `SaveStepTransition` always errors), assert that a run whose step's success transition fails to persist does NOT report overall success — the run ends failed/errored, and (optionally) no contradictory state. Today `eng.Run` would return nil (success) despite the persist failure. Model on `TestTransitionDoesNotPublishOnStoreFailure` if present, or add a new test.

- [ ] **Step 2: Run it — expect FAIL** (run reports success despite persist failure). Capture RED.

- [ ] **Step 3: Implement.** `transition` returns `error` (still does persist-then-publish; on persist error it logs + publishes the synthetic StepFailed as today, then returns the error). At the success-transition call sites in `runStep` and the join success paths, capture the returned error; if non-nil, set `lastErr` and route into the failure disposition (so the step is recorded failed and the run fails). Keep the failure-path transitions tolerant (a persist error while already failing is logged; don't loop). Preserve persist-then-publish exactly.

- [ ] **Step 4: Run it — expect PASS** + `go test ./internal/engine/ -count=1` (sandbox-disabled).

- [ ] **Step 5:** gofmt; commit `fix(engine): surface step-transition persist failures instead of swallowing them`.

---

## Task 5: [Theme 3] Store double parity: `MarkReclaimed` + `ListRuns` ordering

**Problem:** `internal/store/mem.go:178` errors on a missing run; `internal/store/sqldb/query.sql.go` `MarkReclaimed` no-ops 0 rows. The `Mem` double must mirror SQLite. (Plus a related `ListRuns` ordering parity gap noted as Minor — fold it in.)

**Fix approach:** Make `Mem.MarkReclaimed` no-op on a missing run (mirroring SQLite's 0-row UPDATE), and update the `core.Store` doc comment to state the contract (missing run = no-op, not an error). Align `Mem.ListRuns` ordering with SQLite's (confirm SQLite's `ORDER BY` and match it in Mem).

**Files:**
- Modify: `internal/store/mem.go` (`MarkReclaimed` no-op on missing; `ListRuns` ordering to match SQLite)
- Modify: `internal/core/store.go` (doc the `MarkReclaimed` missing-run contract)
- Test: `internal/store/mem_test.go` (+ cross-check against `sqlite_test.go` expectations)

- [ ] **Step 1: Write/adjust the failing test.** In `mem_test.go`, assert `Mem.MarkReclaimed(ctx, "unknown")` returns nil (no error), matching SQLite. If an existing test asserts the error behavior, update it to the new contract (and note why in the report). Add/confirm a `ListRuns` ordering test that matches SQLite's documented order.

- [ ] **Step 2: Run it — expect FAIL** (Mem returns an error today). Capture RED.

- [ ] **Step 3: Implement.** `Mem.MarkReclaimed`: if the run is absent, return nil (set the marker only if present). Confirm SQLite's `ListRuns` `ORDER BY` (read `query.sql`/`query.sql.go`) and make `Mem.ListRuns` sort the same way. Update the `core.Store.MarkReclaimed` doc comment.

- [ ] **Step 4: Run it — expect PASS** + `go test ./internal/store/... -count=1`.

- [ ] **Step 5:** gofmt; commit `fix(store): Mem mirrors SQLite for MarkReclaimed missing-run and ListRuns ordering`.

---

## Task 6: [Theme 4] HTTP status / step-enum precision

Three localized correctness fixes.

**Files:**
- Modify: `internal/api/handlers.go` (decodeJSON 413; cancel terminal→409)
- Modify: `internal/engine/engine.go` (cancel-while-gated → `StepCanceled` not `StepFailed`)
- Tests: `internal/api/*_test.go`, `internal/engine/engine_test.go`

- [ ] **Step 1 (413):** `internal/api/handlers.go` — `decodeJSON` body-too-large surfaces as 400 via callers (`:174,:215,:239`), but `handleCreateRun` already maps it to 413. Make body-too-large consistently 413 across all decodeJSON callers. **Test first:** a handler test posting an over-limit body to one of the affected routes asserts 413.

- [ ] **Step 2 (cancel 409):** `internal/api/handlers.go:119` — `handleCancelRun` returns 404 for both unknown and known-but-terminal runs. Make a known-but-terminal (non-cancelable) run return **409** (consistent with retry/reclaim), keep unknown = 404. **Test first:** add the missing cancel-endpoint tests (unknown→404, terminal→409, active→202/200 as today). Check whether `Supervisor.Cancel` already distinguishes these (it may return a typed error); thread the distinction to the handler.

- [ ] **Step 3 (StepCanceled):** `internal/engine/engine.go:355-357` — a step canceled while blocked in a manual/escalated gate is persisted `StepFailed("context canceled")` inside a `canceled` run. Use the existing `core.StepCanceled` enum for this path. **Test first:** assert a run canceled while a step awaits a gate records that step as `StepCanceled`, not `StepFailed`.

- [ ] **Step 4:** Run `go test ./internal/api/ ./internal/engine/ -count=1` (sandbox-disabled); gofmt; commit `fix(api,engine): 413 on oversized body, 409 on terminal-run cancel, StepCanceled on gate cancel`.

---

## Task 7: [Theme 6] Flow validation gaps

**Problem:** `internal/flow/validate.go` accepts four malformed flows the engine assumes can't happen.

**Files:**
- Modify: `internal/flow/validate.go`
- Test: `internal/flow/validate_test.go`

- [ ] **Step 1: Write failing tests** for each gap (one assertion each): (a) duplicate entries in a step's `needs:` are rejected; (b) an agent step with BOTH empty `role` and empty `prompt` is rejected; (c) a negative `retry.backoff` is rejected (mirror the existing `retry.max`/`timeout` checks); (d) an unknown `workspace:` value on a non-join step is rejected (not silently treated as shared). Use the existing `validate_test.go` table-test style.

- [ ] **Step 2: Run — expect FAIL** (all four currently pass validation). Capture RED.

- [ ] **Step 3: Implement** the four checks in `validate.go`, with clear error messages matching the existing style (e.g. `step %q: duplicate needs entry %q`). Keep them additive — don't tighten anything the engine/tests legitimately rely on.

- [ ] **Step 4: Run — expect PASS** + `go test ./internal/flow/ -count=1` (ensure no existing valid-flow test now fails).

- [ ] **Step 5:** gofmt; commit `fix(flow): reject duplicate needs, empty role+prompt, negative backoff, unknown workspace`.

---

## Task 8: [Themes 7+8] Coverage tests + synthesize abort-on-commit-fail

**Files:**
- Modify: `internal/join/synthesize.go` (abort on commit failure)
- Test: `internal/api/sse_test.go` (disconnect/cleanup), `internal/api/*_test.go` (cancel endpoint — if not already added in Task 6), `internal/engine/engine_test.go` (artifact-aliasing race regression), `internal/join/synthesize_test.go`

- [ ] **Step 1 (synthesize abort):** `internal/join/synthesize.go:44` — when `git commit --no-edit` fails, the workDir is left in MERGING state, poisoning retries. **Test first:** a synthesize join whose commit fails leaves no MERGING state (or the next attempt isn't poisoned). **Fix:** `git merge --abort` (or equivalent cleanup) on the commit-failure path, mirroring `Merge.Join`'s abort handling.

- [ ] **Step 2 (SSE disconnect test):** `internal/api/sse_test.go` — add a test that a client disconnect (canceled request context) causes the SSE handler to return AND unsubscribe from the bus (no leaked subscription/goroutine). Assert via the bus's subscriber count dropping, or a follow-up publish not blocking. This guards the `defer unsub()` path.

- [ ] **Step 3 (engine race regression):** `internal/engine/engine_test.go` — add a `-race`-meaningful test that exercises concurrent `GetRun`/status-poll against a running step with artifacts (the artifact-aliasing class the prior store fix addressed), at the engine level (not just the store). Keep it bounded/fast.

- [ ] **Step 4:** Run `go test -race ./internal/api/ ./internal/engine/ ./internal/join/ -count=1` (sandbox-disabled); gofmt; commit `test(api,engine,join): SSE-disconnect, engine race-regression, synthesize abort + test`.

---

## Final verification (after all tasks)

- [ ] Full `go test -race ./...` (sandbox-disabled where exec is blocked) — all packages green.
- [ ] `gofmt -l internal/ cmd/` prints nothing.
- [ ] Whole-branch review (Opus) over the full task range.
