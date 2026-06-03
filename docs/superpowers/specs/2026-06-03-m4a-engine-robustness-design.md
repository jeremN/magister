# Design — M4 Slice A: Engine Robustness

**Written:** 2026-06-03, after M3 merged to `main`.
**Parent spec:** `docs/superpowers/specs/2026-06-02-orchestrator-design.md` (§5 failure model, §7 resume).
**Status:** approved (brainstorming), ready for `writing-plans`.

## TL;DR

M4 is really three independent subsystems; this spec covers **Slice A — engine
robustness** only. Slices B (real CLIAgents) and C (git-worktree workspaces) get
their own spec→plan→implement cycles.

Slice A makes the existing attempt loop correct and complete: it gives `on_fail`
real behavior under a **unified attempt budget**, adds **jitter + a backoff cap**,
hardens the **per-step timeout** to cover the auto-verifier, and fixes the **#1 M3
follow-up** (resume-staleness → spurious 409 on approve) on both the server and the
client. It runs entirely on the `mock` executor and `CommandVerifier` — **no new
external dependencies, no migration, no new YAML fields** — so it is fully testable
under `-race`.

## 1. Scope

**In scope**

1. Retry: add **jitter** (full-jitter) and a **max-backoff cap**; firm up the policy.
2. `on_fail`: implement real **abort / retry / escalate** semantics (today the field
   is validated but never consumed by the engine).
3. Per-step timeout: verify interaction with retries and the awaiting-gate path, and
   **extend the timeout to cover the automated gate (verifier)**; add coverage.
4. The 409 resume-staleness fix: **server-side reset-to-pending on resume** +
   **`cm` retry-on-409**, plus the cheap **`ResumeAll` log-and-continue** fold-in
   (M3 follow-up #2).

**Non-goals (deferred)**

- Real CLIAgents / stream-json cost parsing (Slice B).
- Git-worktree workspaces + teardown + re-run safety (Slice C).
- Conditional-gate `expr` evaluation, select/synthesize joins (M5).
- New `RetryPolicy` YAML fields (e.g. a configurable `MaxBackoff`) — YAGNI; the cap
  is a constant for now.

## 2. The failure model (unified attempt budget)

An **attempt = execute + gate**. The budget is `Retry.Max` (nil retry ⇒ 1 attempt,
no retry). The model the user chose is the spec-literal **unified budget**: a `retry:`
block retries the whole attempt on *either* an executor error *or* a gate failure, and
`on_fail` selects the **terminal disposition** once the budget is spent.

`runStep` disposition, made explicit:

- A failed attempt (executor error **or** gate failure) consumes one attempt.
- **While budget remains:** emit `step.retrying`, back off (§3), loop a fresh attempt.
  The step **re-executes** (the agent gets another crack) and is then re-gated.
- **When the budget is spent**, apply the terminal disposition:
  - last failure was a **gate failure** and `on_fail == escalate` → **escalate** (§4);
  - otherwise (`abort` / `retry`, or the last failure was an **executor error**)
    → `step.failed` → first-error-cancels the run.

### 2.1 `on_fail` truth table

| Last failure | `on_fail` | Budget remaining | Behaviour |
|---|---|---|---|
| executor error | any | yes | back off + retry (re-execute) |
| executor error | any | no | `step.failed` → abort run |
| gate failure | `abort` / `retry` / unset | yes | back off + retry (re-execute + re-gate) |
| gate failure | `abort` / `retry` / unset | no | `step.failed` → abort run |
| gate failure | `escalate` | yes | back off + retry (re-execute + re-gate) |
| gate failure | `escalate` | no | **escalate**: block on human approval |

### 2.2 Documented consequence: `retry` ≡ `abort`

Because the unified budget already retries gate failures regardless of `on_fail`,
**`on_fail: abort` and `on_fail: retry` are behaviourally identical** — both fail the
run after the budget is spent. `on_fail: retry` survives only as an explicit,
validator-checked synonym (the validator still requires a `retry:` policy with it).
**Only `escalate` is distinct.** This is the deliberate cost of "unified"; it will be
documented loudly in the schema doc-comment on `flow.FailPolicy` so a flow author is
not surprised. (Making the three distinct would require the *orthogonal* model —
`on_fail: abort` meaning "do not spend the budget on gate failures" — which was
considered and rejected in favour of the spec's unified language.)

### 2.3 Manual-gate edge

`on_fail: escalate` on a **manual** gate is nonsensical (escalating a human's
rejection back to a human). A manual-gate rejection therefore **aborts** regardless
of `on_fail`; escalate is meaningful only for **auto** gates. This is documented and
optionally surfaced as a validation note (not a hard error, to avoid breaking
otherwise-valid flows).

## 3. Jitter & backoff cap

- **Backoff cap.** Today `base·2^(attempt-2)` is unbounded. Add a constant
  `maxBackoff = 30s`. The delay before a retry is `min(maxBackoff, base·2^(attempt-2))`
  *before* jitter. Not a new YAML field (YAGNI); a `RetryPolicy.MaxBackoff` can be
  added later if a flow needs it.
- **Jitter algorithm.** Full jitter (AWS): `sleep = Rand()·min(maxBackoff, base·2^(attempt-2))`,
  where `Rand()` ∈ [0, 1). Full jitter is simple and spreads concurrent retries to
  avoid a thundering herd.
- **RNG injection (testability).** Add a field `Rand func() float64` to `engine.Engine`,
  defaulting to `math/rand/v2`'s `rand.Float64` (auto-seeded) when nil — mirroring the
  `Log` "nil = default" convention. Tests inject a deterministic `Rand` so that, with
  the injected `Clock`, backoff durations stay exactly assertable. This preserves M3's
  determinism discipline.
- **Rejected alternatives.** Extending `core.Clock` with a rand method (conflates two
  concerns); deriving jitter from a `(run, step, attempt)` hash (not real randomness;
  no cross-run spread, defeating the anti-herd purpose).
- `backoff(ctx, s, attempt)` keeps using the injected `Clock` for the actual sleep and
  its `ctx.Done()` early-return.

## 4. `on_fail: escalate` plumbing

Reuse the **exact** manual-gate block-on-channel mechanism (parent spec §5: "manual
gates use the same `ApprovalRegistry` mechanism as escalation") — no new registry
machinery.

- **New seam.** `gate.Evaluator.Escalate(ctx, runID, step, res) (bool, error)` delegates
  to its `Approver.Approve(...)`. In the daemon the `Approver` is already the
  `supervisor.RegistryApprover`, so escalation blocks on the same channel `cm approve`
  resolves. In keyless/test wiring the `AutoApprover` passes it.
- **Engine flow.** In `runStep`, on budget-spent + gate-failure + `escalate`:
  1. persist `awaiting_gate` and emit `gate.awaiting` carrying the failure reason
     (see §4.1);
  2. call `e.Gate.Escalate(...)` → blocks until `cm approve/reject` or ctx cancel;
  3. **approve → `succeeded`** (using the `core.Result` already in hand from the last
     attempt); **reject → `step.failed`** → the run aborts.
- **Step status.** An escalated gate reuses `StepAwaitingGate` (identical to manual) —
  the escalation distinction lives in the event, not in a new status enum value.

### 4.1 Escalation marker (no migration)

The `events` table has fixed columns (`seq, run_id, step_id, kind, summary, cost_usd,
attempt, error, at`), so adding an `Escalated bool` to `event.Event` would require a
goose migration + `sqlc` regen. Instead, **carry the verifier's failure reason in the
existing `Err` field of the `gate.awaiting` event**: a normal manual gate emits
`gate.awaiting` with empty `Err`; an escalated gate emits it with `Err` populated
(e.g. `gate failed (policy="auto"): <reason>`). This is zero-schema-change and *more*
informative (the human sees *why* they are being asked). A future explicit boolean is
possible at the cost of a migration if cleaner client-side detection is wanted.

### 4.2 Resume

An escalated step persists as `awaiting_gate` (identical to manual). With §6's
reset-to-pending, on resume it re-runs from a fresh attempt, re-reaches the gate, and
re-escalates if it fails again. **No special resume code** is needed — escalation is
reconstructed by re-execution (consistent with at-least-once, parent spec §7).

## 5. Per-step timeout hardening

- **Verified current behaviour.** `execute` wraps the executor call in
  `context.WithTimeout(ctx, s.Timeout)` when `s.Timeout > 0`, per attempt. ✓
- **Gap.** The auto-verifier (`CommandVerifier` running `go test ./...` etc.) currently
  runs on the run context, **outside** the timeout — a hung verifier is unbounded.
- **Change.** The per-step timeout covers the **automated** portion of an attempt:
  `execute` **and** an **auto gate**. **Manual and escalate gates use the un-timed-out
  run context** (humans take arbitrary time). Concretely, the timeout context is
  created once per attempt and threaded into both the executor call and the auto
  verifier; the approval (manual/escalate) path is invoked with the run context.
- **Semantics.** A timeout expiry (executor or verifier) is an error → retryable under
  the unified budget (consumes an attempt) → eventually aborts if the budget is spent.

## 6. The 409 resume-staleness fix (both halves)

**Bug.** On resume the engine re-runs every non-succeeded step from a fresh attempt
(parent spec §7), but the *persisted* status (e.g. `awaiting_gate`) lingers in the
store until that step re-runs and re-registers its gate. A client watching the run
sees `awaiting_gate`, fires `cm approve`, but no gate is registered yet →
`Resolve()` returns false → **HTTP 409**. The human's in-flight approval was never
persisted (it lived only in the in-memory `ApprovalRegistry`, gone after the crash),
so re-blocking is already correct; the only defect is the stale visible status
provoking a premature approve.

- **Server-side (root cause).** In `supervisor.ResumeAll`, before re-running each
  loaded run, reset **every non-`succeeded` step** → `pending` (attempt 0, cleared
  `Err`) via `Store.SaveStepTransition` with an **empty** event slice (startup
  reconciliation, not a runtime transition → no event emitted; reconnecting clients
  get the corrected REST snapshot, and the append-only `events` history is untouched).
  The rule mirrors `engine.Resume`'s seed logic exactly: only `succeeded` steps are
  seeded (and left intact, since they feed downstream inputs); **everything else
  re-runs from a fresh attempt**, so `pending` is the honest status for all of them
  (`running` / `retrying` / `awaiting_gate`, and any transient `failed` / `canceled` /
  `ready` rows in an incomplete run).
- **Client-side (defense in depth).** `cm approve` / `cm reject` retry on **HTTP 409**
  for a bounded window (~10s, short fixed interval), since 409 means "no gate awaiting
  *yet*" — transient during the re-registration window. Other non-200 statuses
  (404 etc.) do **not** retry. The existing e2e `approveStep` helper already proves the
  pattern.
- **Bonus fold-in (M3 follow-up #2).** `ResumeAll` currently `return`s on the first
  corrupt-`FlowYAML` parse/validate error, stranding every *later* run in the slice.
  Switch to **log-and-continue** per row: skip the corrupt run (logged), resume the
  rest.

### 6.1 Implementation note to verify in planning

Confirm `Store.SaveStepTransition` accepts an empty `[]event.Event` (both `Mem` and
`SQLite`) and UPSERTs the step row. If it requires ≥1 event, either relax it or add a
narrow `Store.ResetStep` method; prefer reusing `SaveStepTransition`.

## 7. Testing strategy

Per M3 discipline, drive flows and **assert on the emitted event stream**, with the
`Clock` **and** `Rand` injected so there are no real sleeps and no real randomness.

- **Failure-model truth table** (fake executor + fake approver + fake `Clock` + fake
  `Rand`): {executor-error, gate-fail} × {abort, retry, escalate} × {budget-remaining,
  exhausted} → assert the `step.retrying` / `step.failed` / `gate.awaiting` /
  `step.done` sequence per row in §2.1.
- **Jitter:** fixed `Rand` + fake `Clock` → assert exact sleep durations and that the
  `maxBackoff` cap clamps high attempts.
- **Escalate:** budget-exhausted gate-fail → `gate.awaiting` (with reason in `Err`) →
  approve → `succeeded`; reject → `failed` → run aborts; plus re-escalation after a
  resume.
- **Timeout:** a timed-out executor retries; a timed-out verifier retries; a blocked
  manual/escalate gate is **not** killed by `step.Timeout` (resolves via approval after
  the timeout would have fired).
- **409 fix:** `ResumeAll` resets non-succeeded steps to `pending` (assert store
  state); `cm approve` retries 409 then succeeds; `ResumeAll` logs-and-continues past a
  corrupt `FlowYAML` row (later runs still resume).
- **Stress:** the escalate-on-resume and reset-to-pending paths get
  `GOMAXPROCS=8 go test -race -count=N` (M3 Task 15 lesson).

## 8. Files touched (rough)

- `internal/engine/engine.go` — `runStep` disposition (escalate branch); `backoff`
  (jitter + cap); per-attempt timeout context covering execute + auto-gate; add
  `Rand func() float64` field + `rand()` helper (nil ⇒ `math/rand/v2`).
- `internal/gate/gate.go` — `Evaluator.Escalate` method.
- `internal/supervisor/supervisor.go` — `ResumeAll` reset-to-pending + log-and-continue.
- `internal/flow/validate.go` — escalate-on-manual-gate note (doc/validation).
- `internal/flow/flow.go` — doc-comment on `FailPolicy` documenting `retry ≡ abort`.
- `cmd/cm/main.go` — `approve`/`reject` retry-on-409.
- `cmd/magisterd/main.go` — leave `Rand` nil (default); no functional change unless a
  seedable source is wanted for ops.
- Tests across `internal/engine`, `internal/supervisor`, `cmd/cm`, and the e2e suite.

**No migration. No new YAML fields. No new external dependencies.**

## 9. Conventions (carried from M0–M3)

- Commits: single conventional-commit subject, no body, no `Co-Authored-By`, never
  `--no-verify`; commit with the explicit git identity (see the M4 kickoff handoff §5).
- Dep pins: `go 1.22` must not move; confirm after any `go get`. `math/rand/v2` is
  stdlib in go 1.22 — no new dependency.
- RTK hook reformats `go`/`git` output; use `rtk proxy` for raw PASS/FAIL lines.
- Semgrep hook runs on edits; the accepted `http.ResponseWriter` false positives stand.
- Engine invariants: persist-then-publish; SQLite single-writer; deadlock-freedom
  (no token held while waiting on deps); `Engine.Log`/`Engine.Rand` nil-safe.
