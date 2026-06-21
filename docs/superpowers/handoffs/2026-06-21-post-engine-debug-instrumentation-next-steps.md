# Handoff — engine Debug/Warn instrumentation: MERGED to main (2026-06-21)

**Start here next session.** The **engine-debug-instrumentation slice is DONE and MERGED to `main`** (fast-forward, `main` at **`aa9dbf1`**, 3 task commits off `7965d79`). Full suite **344 passed / 18 packages `-race`**, vet + gofmt clean. Final Opus whole-branch review **Ready-to-merge=Yes, zero Critical/Important** (2 Minors, spec-sanctioned — accepted). Worktree + branch cleaned up. **LIVE smoke PASSED** (real daemon: 5 Debug line types stream at `-log-level debug`, all suppressed at default `info`). **NOT yet pushed to origin** — `git push origin main` when ready (origin is at `99f09bc` from the log-level slice; behind by spec/plan + 3 task commits + this handoff).

## What shipped

Seven additive `slog` log points in `internal/engine/engine.go` so `-log-level debug` finally reveals the engine's internal decisions (it logged only at `Error` before, so `debug` showed nothing extra). Stdlib `slog` only — **no new dep/package/migration/schema/SSE event**, Go 1.22. Spec `…/specs/2026-06-21-engine-debug-instrumentation-design.md`, plan `…/plans/2026-06-21-engine-debug-instrumentation.md`.

Emitted **entirely from the engine** (which holds the only logger); `gate`/`join` stay logger-free — merge conflicts are logged from `execute()` via `errors.As(*join.ConflictError)`. The one structural change: `backoff` gained a `runID core.RunID` parameter (propagated to both callers — `runStep` and the `TestBackoffJitterAndCap` test).

- `22d886c` `feat(engine): log backoff delay and retry-budget-exhausted` (Task 1) — **B** Debug `step backoff` (run/step/attempt/delay/base, after the jitter) + **E** Warn `retry budget exhausted` (run/step/attempts/last_err/escalating), placed *after* the join-conflict-escalation early-return so `escalating = gateFailed && gateEscalates(s)` is exact. Created `internal/engine/instrumentation_test.go` with the shared helpers `debugLogger(*bytes.Buffer) *slog.Logger` and `hasLine(out, subs...) bool`.
- `b6ffd64` `feat(engine): log agent timing, slot waits, and gate verdicts` (Task 2) — **A** Debug `agent starting`/`agent finished` in `runAgent` (run/step/agent/role/attempt; finish adds dur/cost_usd, + err only when non-nil; `dur` hoisted into one `Clock.Now()` read shared with `ObserveAgentRun`) + **C** Debug `step slot acquired` in the runDAG goroutine (run/step/waited; `queueStart` before token acquisition, log after the ctx.Err guard) + **D** Debug `gate evaluated` in `attempt` (run/step/attempt/policy/pass, + err only on infra error).
- `aa9dbf1` `feat(engine): log join execution and merge-conflict detection` (Task 3) — **F** Debug `join starting`/`join finished` in `execute` (run/step/strategy/inputs/attempt; finish + err only when non-nil) + **G** Warn `merge conflict detected` via `errors.As(*join.ConflictError)` (run/step/branch/paths/attempt). The join branch now captures `res, err := strat.Join(...)` and returns it byte-for-byte.

## Key properties (Opus-verified)
- **Purely additive**: no control-flow / result / event / run-lifecycle change on any touched path. `backoff`'s only functional change is the param; `execute`'s join branch returns `strat.Join`'s value verbatim; `runAgent`'s hoisted `dur` feeds metrics the identical single-clock-read value; the slot-acquired log adds no token-holding.
- **Default-level output change is exactly the two Warn lines (E, G) and nothing else.** A/B/C/D/F are `.Debug` (invisible at `info`); E/G are `.Warn` (default-visible) — deliberate, since retry-exhaustion and merge-conflicts are rare and notable (not per-step). This is the first default-level engine output beyond `Error` (the executor already had one `Warn`).
- **Conditional `err`**: finish/verdict lines (A-finish, D, F-finish) append `"err"` only when non-nil — no `err=<nil>` noise. E's `last_err` is inherently always-present (correct).
- **Field keys** reuse the existing convention: `run`=string(runID), `step`, `agent`, `err`. No `Enabled()` guards (all args are cheap in-hand values; coarse-grained sites).

## The 2 Minors (accepted, spec-sanctioned, non-blocking)
1. E uses the field key `attempts` (plural = total tried) while A/B/D/F use `attempt` (current number) — intentional, spec §E mandates it; the plural key signals the different meaning. Don't "normalize" it.
2. D (`gate evaluated`) fires for a manual gate *after* the human decision returns (not at the await moment); `gate.awaiting` already covers the wait. Spec §D calls this out. Correct as designed.

## LIVE smoke (PASSED 2026-06-21)
Real branch binary + the zero-cost mock flow `flows/git-native-merge.yaml` (2 isolated mock upstreams → merge join):
1. `-log-level debug`: stderr showed all 5 Debug line types with exact fields — `agent finished … dur=118µs cost_usd=0.01`, `step slot acquired … waited=500ns`, `gate evaluated … policy=auto pass=true`, `join starting … strategy=merge inputs=2`, `join finished`. The run succeeded.
2. Default `info` (same flow): **zero** Debug lines (`level=DEBUG` count = 0) while `level=INFO` lines (listening/request) remained. Proves the new instrumentation is correctly gated by the `-log-level` knob — the user-facing payoff of pairing this slice with the prior level slice.

The two Warn lines (E retry-exhausted, G merge-conflict) need a failing / conflicting flow to stage live; they are covered by unit tests (`TestRetryBackoffAndExhaustionLogs`; the G assertion rides the existing `TestMergeConflictEscalateApproveCommits`, which stages a real git conflict).

## Out of scope (deliberate, carried)
- The 8th candidate point — per-dependency-satisfied lines for tracing stuck DAG chains — dropped (`step.started` already implies unblocking).
- Threading a logger into `gate`/`join` (those stay logger-free; conflicts logged at the engine boundary).
- Any new SSE event / metric / config flag / schema change.
- Runtime-adjustable level, per-component levels, OTel tracing.

## Open follow-ups (carried)
- **(deferred) `git push origin main`** — origin at `99f09bc`; this slice (spec, plan, commits 22d886c/b6ffd64/aa9dbf1, handoff) is local-only.
- **(still unmerged) `multi-host` GitLab slice** — CODE-COMPLETE at `36eb9fa` (worktree `.worktrees/multi-host`), awaiting a live gitlab.com proof. See `2026-06-19-multi-host-gitlab-next-steps.md`. **DO NOT merge until that proof passes.**
- **(housekeeping) `MEMORY.md` over budget** (~26KB vs ~24.4KB) — index line is a full changelog; move detail to the topic file, shrink the index to a one-line hook.
- **(observability backlog):** runtime-adjustable level (`slog.LevelVar` + a `/loglevel` control endpoint — now genuinely useful since debug lines exist); OTel tracing (needs a dep). Done: per-agent metric triad, `/metrics`, liveness/readiness, run-not-found sentinel, request/run-scoped logging, JSON log format, log level, **engine debug/warn instrumentation**.
- **(delivery axis):** cross-repo/fork PRs (`owner:branch` head).

## Process notes
- Subagent-driven: haiku implementers + haiku reviewers for all 3 tasks (the plan carried complete code → transcription); Opus final whole-branch. Every per-task + final review returned zero Critical/Important.
- The plan's pre-reading of the actual engine source paid off: code blocks matched verbatim, and the Task 1 implementer caught a *second* `backoff` caller (`TestBackoffJitterAndCap`) the plan hadn't named — a good sign the implementers reason rather than blindly transcribe.
- Live smoke earns its keep: the debug-vs-info contrast on the real binary is the user-facing proof and the only thing exercising the full `cfg → handler → engine seam → os.Stderr` path at a non-default threshold. The unit tests prove the lines fire; the smoke proves the level knob gates them end-to-end.
- Commit hygiene held: single conventional subject, no body, no `Co-Authored-By`, never `--no-verify`. zsh in this env trips on Bash-tool commands combining `printf '…\n'` with `===` banners / parens / `$(...)` — run such commands one per invocation, absolute paths; a proxied/`rtk` grep can truncate long lines (read the raw file / grep short `msg=` patterns).
