# Handoff — post-M5b (select/synthesize joins DONE; next: git-native merge-at-join) (2026-06-10)

**M5 Slice B (select/synthesize agent-arbitrated joins) is COMPLETE and merged to `main`.** With M5a (conditional gates) + M5b (arbitrated joins), **M5 is now fully done.** This handoff records the finished state, the manual-proof evidence, the carried follow-ups, and the next-step menu.

## State

- `main` at `6684608`, clean. M5b merged via fast-forward (6 feature commits over the M5b plan commit `10c05a5`). Worktree removed, branch `m5b-joins` deleted.
- `go test -race ./...` → **177 passed / 15 packages**; `go vet ./...` clean; `go 1.22` unchanged. **No new dependency** (`git diff main -- go.mod` was empty — M5b is pure stdlib).
- The six feature commits:
  - `9528051` refactor(engine): extract runAgent for reuse by join arbiters
  - `9001ebf` feat(join): inject RunAgent callback into Strategy.Join
  - `d6370df` feat(join): add select strategy (arbiter picks a winner)
  - `f20ece5` feat(join): add synthesize strategy (arbiter merges candidates)
  - `80c3ce5` test(engine): cover select/synthesize fan-in joins end-to-end
  - `6684608` feat(engine): on_conflict disposition for failed joins (abort/retry/escalate)

## What M5b built

Two agent-arbitrated join strategies on the existing **path-based** workspace model (git-native merge stays a separate handoff — see below). The YAML surface (`flow.Join{Strategy, Agent, OnConflict}`, the `select`/`synthesize` constants) and `validateJoin` already existed; M5b filled in behavior + one new seam.

- **The seam — `RunAgent`:** `Strategy.Join` gained an injected `run RunAgent` callback (`func(ctx, agentName, prompt, workDir, inputs) (core.Result, error)`). The engine extracted its executor path into a reusable `runAgent` method and binds a `run` closure (role `"arbiter"`) for join steps; `Merge` ignores it. So an arbiter runs exactly like a normal step's agent — **its `agent.tool` milestones stream over the existing SSE for free**, and synthesize's output is discovered via the same `discoverGit`.
- **`select`** (`internal/join/select.go`): stages candidates into `<workDir>/.candidates/<stepID>/`, prompts the arbiter to end with `SELECTED: <step-id>`, parses the **last** such token, validates the winner ∈ `s.Needs`, and forwards that winner's **original** artifacts **by reference** (provenance via `Artifact.StepID` preserved). `stageCandidates` disambiguates basename collisions.
- **`synthesize`** (`internal/join/synthesize.go`): stages candidates, prompts the arbiter to write one merged result, returns the arbiter's written artifacts **excluding** anything under `.candidates/` (the staged inputs), re-tagged `StepID = s.ID`; zero output → error.
- **`on_conflict` engine disposition** (`internal/engine/engine.go`): a failed join (strategy error) is dispositioned by `on_conflict` — **abort** (default/`""`) fails the run (suppresses retry even if `s.Retry` set); **retry** re-runs within `s.Retry.Max`; **escalate** → new `escalateJoin` (sibling of the gate `escalate`, but **approve = re-run the join once** since a failed join has no result to accept; reject → abort). Non-join behavior is byte-for-byte unchanged (guarded by `s.Join != nil`). No store/SSE/schema/YAML/validation change.

User-approved design decisions honored: agent-arbitration only (git-native merge deferred); select = name-a-winner→forward-by-reference (`SELECTED:` token convention); seam = injected `RunAgent` callback; on_conflict full (abort/retry/escalate, approve=re-run-once).

## Manual proof (Task 7) — observed, zero-cost (mock arbiter, no network/keys)

Built `magisterd`+`cm` from the worktree, ran fan-in flows over the live daemon + SSE:

| Scenario | Config | Observed |
|---|---|---|
| **synthesize happy** | 2 mock steps → `synthesize` join (mock arbiter) | `a/b done → merge step.started→step.done → run.done`, `succeeded`; join artifact = `merge.out.md` (arbiter's output; staged `.candidates/` excluded) |
| **select conflict→escalate** | `select` join, mock arbiter (no `SELECTED:` token), `on_conflict: escalate` | `pick` → `gate.awaiting` Err=`select: no SELECTED token in arbiter output` |
| **escalate approve→re-run** | `cm approve <run> pick` | join **re-runs** (`step.failed` at **attempt 2**) → fails again (mock still no token) → `run.done` `failed` — proves approve = exactly-one re-run, no second escalation |

**Gotcha proven the hard way (note for future flows):** fan-in upstream steps with NO `gate` default to the **manual** gate, which **blocks on a human in the real daemon** (`cm approve`) — unlike `gate.AutoApprover{}` in unit tests. The manual-proof flows give the upstream `a`/`b` steps an explicit `gate: {policy: auto, verifier: {command: "true"}}` so only the intended join routes through approval. (This also surfaced as a plan-test defect in Task 6's reject test, fixed the same way.)

## Carried follow-ups (from the final holistic review — none blocking)

- **`escalateJoin` re-run emits no `step.started` for attempt N+1** — a minor SSE-observability asymmetry, but **consistent with the existing gate `escalate` path** (`engine.go` ~458), so it's an established pattern, not a regression. If ever polished, fix BOTH escalate paths together.
- **synthesize retry/escalate re-run re-stages into the same `.candidates/`** — harmless on the path-based model (arbiter overwrites; `underCandidates` still excludes staged copies), but worth revisiting when the git-native handoff reworks workdir semantics. No synthesize-retry test exists.
- **Validator asymmetry:** `validateJoin` does NOT require `s.Retry` when `on_conflict=retry` (the gate path DOES require it for `on_fail=retry`). The engine handles it safely (retry-without-`s.Retry` degrades to abort), but a `retry` join with a forgotten retry policy silently no-ops at submit instead of erroring. Consider mirroring the gate check in a later pass.

## After M5b — next-step menu

**M5 is complete.** The remaining orchestrator follow-on is:

1. **The external-repo / git-native merge-at-join handoff** — the recurring carry-over since M4. M5b deliberately kept joins on the **path-based** model (select forwards by reference, synthesize writes into the shared base tree). The git-native slice would: run `isolated` upstream steps on real git **branches** off the per-run scratch repo (M4-C's `GitManager`), and make `merge` (and possibly synthesize) perform an actual `git merge` at the join — surfacing conflicts (which `on_conflict` would then disposition). This is where `on_conflict` becomes a true *merge*-conflict policy rather than an arbiter-failure policy. Start with brainstorm → spec → plan, same as M5a/M5b.

No design doc exists yet for the git-native slice. Spec/plan/handoff for M5b: `docs/superpowers/{specs,plans,handoffs}/2026-06-10-m5b-*`.
