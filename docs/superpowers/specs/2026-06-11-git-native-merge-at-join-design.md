# Design — git-native merge-at-join (2026-06-11)

**Status:** approved (brainstorm), pre-plan.
**Milestone:** the external-repo / git-native follow-on carried since M4-C. Closes the path-based→git-native gap that M5b deliberately deferred.

## Problem

M4-C already runs each `isolated` step in a real `git worktree` on a branch `step/<stepID>` off a per-run scratch repo — **but the work is never committed**, and the artifact handoff is **path-based**: `discoverGit` (`internal/executor/discover.go`) reports the *dirty* worktree via `git status --porcelain`, downstream steps receive file *paths*, and the `merge` join writes a *manifest* (`internal/join/join.go` `Merge`) instead of doing a real `git merge`. M5b's `select`/`synthesize` stayed on that path-based model on purpose (`.candidates/` staging). `join.go:76` always flagged the manifest as a placeholder: *"With real worktrees (M4) this becomes a git merge."*

This slice makes the handoff **git-native**: isolated steps **commit** their work to their branch, downstream steps receive **branch refs** (alongside paths), and joins perform real **`git merge`** of those branches — where `on_conflict` becomes a true merge-conflict policy, not just the arbiter-failure policy it is today.

## Goals

- Isolated steps commit their worktree to `step/<stepID>` on success; their artifacts carry the branch + commit.
- `merge` does a real `git merge` of upstream branches; conflicts are dispositioned by `on_conflict`.
- `select`/`synthesize` go git-native internally (no `.candidates/` staging).
- `on_conflict: escalate` on a merge conflict runs an **arbiter-resolves → human-approves** ladder.
- The path-based join model (manifest `Merge`, `stageCandidates`) is **replaced**, not kept as a parallel path.

## Non-goals

- **External-repo:** running flows against a real pre-existing git repo instead of the synthetic empty-base scratch repo. Separate axis; this slice keeps the per-run scratch repo.
- Multi-turn agents, token-delta streaming, or any executor change.

## Decisions

Five chosen in brainstorm + three derived (all approved):

1. **Scope — full branch-ref handoff.** Isolated steps hand downstream a ref; all three joins go git-native.
2. **Artifact shape — superset.** `core.Artifact` keeps `Path` (always set) and gains `Branch`/`Commit` (set when a step commits; empty for shared steps and the mock executor). Joins prefer refs; gates keep working on paths.
3. **Commit site — engine + workspace method.** `core.Workspace` gains `Commit(...)`; the engine calls it in `runStep` on success for isolated non-join steps and stamps the artifacts. Plain `Manager.Commit` is a no-op. Joins self-commit (the strategy does the git work).
4. **select/synthesize — git-native internals.** `synthesize` = sequential `git merge` with the arbiter resolving only true conflicts; `select` = forward the winner's branch by ref. `.candidates/` staging removed.
5. **Merge-conflict escalate — arbiter resolves, human always approves.** A ladder: arbiter rewrites the conflict markers, then every resolution goes to a human via `gate.awaiting`.

Derived (approved):

6. **Replace path-based joins entirely** (vs. a fallback path). The manifest `Merge` and `.candidates/` staging were always placeholders.
7. **A join step and all its `Needs` must be `workspace: isolated`** — guarantees every input carries a branch; enforced by the validator.
8. **Escalate-ladder orchestration lives in the engine** (`escalateJoin`), keeping strategies focused on combining inputs.

## Architecture

### Data model — `internal/core/ports.go`

```go
type Artifact struct {
    StepID string
    Path   string  // always set (discoverGit, mock)
    Branch string  // "step/<id>" — set once the producing step commits
    Commit string  // sha — set on commit
}

// Workspace gains:
Commit(runID RunID, s *flow.Step, workDir string) (branch, commit string, err error)
```

Additive: existing `.Path` readers (gates, prompts, SSE) are unaffected.

### Workspace — `internal/workspace/`

- **`GitManager.Commit`** (`gitmanager.go`): under the per-run lock, in the worktree → `git -c user.name=… -c user.email=… add -A` then `commit --allow-empty -m "step/<id>"`, then `rev-parse HEAD`. `--allow-empty` so every isolated step advances its branch deterministically (even a step that wrote no files). Returns `"step/"+s.ID`, sha. Reuses the existing `name()`/`email()`/`run()`/`runLock` helpers.
- **`Manager.Commit`** (`workspace.go`): `return "", "", nil`.

### Engine — `internal/engine/engine.go`

- **`runStep` success path** (currently `engine.go:250-254`): after `execErr == nil`, before the `StepSucceeded` transition, for `s.Workspace == flow.WSIsolated && s.Join == nil`:

  ```go
  br, sha, cerr := e.WS.Commit(runID, s, workDir)
  if cerr != nil { /* treat as a step failure: lastErr = cerr; fall through to the failure disposition */ }
  for i := range res.Artifacts {
      res.Artifacts[i].Branch = br
      res.Artifacts[i].Commit = sha
  }
  ```

  Joins are skipped here — the strategy self-commits and returns `Branch`/`Commit` in its result.

- **`escalateJoin`** (`engine.go:484`) gains a conflict branch. The `merge` strategy reports a conflict as a typed `*join.ConflictError{Paths, WorkDir}`. On `on_conflict=escalate`:
  - **ConflictError** → run the ladder: `e.runAgent(role="arbiter", join.ResolveConflictPrompt(paths))` on the conflicted worktree → `Gate.Escalate` (human) → on approve, `e.WS.Commit` finalizes the resolved tree (concludes the in-progress merge) and the engine builds the result from the committed worktree; on reject, fail (the worktree is reclaimed by `TeardownRun`).
  - **arbiter-failure** (not a ConflictError) → existing re-run-once semantics, unchanged.

  Distinguished with `errors.As(err, &conflictErr)`.

### Join strategies — `internal/join/`

All operate in the join's own isolated worktree (`step/<joinID>` off empty base). A new `internal/join/git.go` provides a `gitCmd(workDir, args...) ([]byte, error)` helper that shells out to git directly (mirrors `executor/discover.go`; no shell, orchestrator-controlled args). The `Strategy.Join` signature is unchanged — strategies use `gitCmd` + the existing injected `run RunAgent`. `stageCandidates` and the `.candidates/` directory are removed; arbiters read candidates at their original upstream-worktree paths (worktrees stay alive until run-end).

- **`merge`** (`join.go`): sequential `git merge step/A`, `git merge step/B`, … into the join worktree. First merge off the empty base fast-forwards; later ones are real 3-way merges (empty base = common ancestor). Clean → each `git merge` auto-commits → result `{Branch: "step/"+s.ID, Commit: HEAD, Artifacts: changed files diffed base..HEAD}`. On conflict, the strategy inspects `s.Join.OnConflict`:
  - `escalate` → leave the conflicted worktree, return `*ConflictError{Paths, WorkDir}` (engine runs the ladder).
  - `abort`/`retry`/default → `git merge --abort`, return a plain error (engine's existing budget disposition handles it; `retry` re-runs but a deterministic conflict re-conflicts — documented).
- **`select`** (`select.go`): arbiter reads each candidate (upstream paths, listed by step-id in the prompt), ends `SELECTED: <id>` (last-wins, must be a dependency — unchanged parse/validation). Result **forwards the winner's branch by ref**: `Branch`/`Commit` = winner's, `Artifacts` = winner's originals (provenance kept). No new commit, no rewrite. The join's own (empty) worktree is unused.
- **`synthesize`** (`synthesize.go`): sequential `git merge` of each upstream branch; whenever a merge conflicts, the arbiter resolves the markers in-place (`run` with a resolve prompt) and we `git add -A && commit` to conclude that merge, then continue to the next branch. Non-conflicting changes merge automatically. Result `{Branch: "step/"+s.ID, Commit: HEAD, Artifacts: changed files base..HEAD}`. synthesize never fails on a merge conflict; it fails only if the arbiter errors → existing arbiter-failure `on_conflict`.

`ResolveConflictPrompt(paths)` (shared by merge-escalate's rung 1 and synthesize) shows the conflicted files and asks the arbiter to resolve every `<<<<<<`/`======`/`>>>>>>` marker and leave a clean tree.

A shared helper `join.CommittedResult(workDir, s)` builds the result of a committed join worktree — `{Branch: "step/"+s.ID, Commit: rev-parse HEAD, Artifacts: git diff --name-only base..HEAD joined to workDir}` — reused by `merge`'s clean path, `synthesize`, and the engine's escalate-finalize (so all three enumerate artifacts identically).

### Validation — `internal/flow/validate.go`

`validateJoin` (extended; takes `byID` to inspect upstreams):

- A join step must be `workspace: isolated`, and every step in its `Needs` must be `workspace: isolated`.
- `merge` + `on_conflict: escalate` requires `Join.Agent` (the conflict arbiter). `select`/`synthesize` already require an agent.
- `on_conflict: retry` requires a `Retry` policy — mirrors the gate check (`validate.go:95`) and closes the M5b validator-asymmetry follow-up.

### Store — `internal/store/`

Artifacts live in a real table `artifacts(run_id, step_id, path)` (`migrations/0001_init.sql:26`), **not** a JSON blob — so `Branch`/`Commit` need a migration:

- **`migrations/0002_artifact_refs.sql`** (goose): `ALTER TABLE artifacts ADD COLUMN branch TEXT NOT NULL DEFAULT ''; ALTER TABLE artifacts ADD COLUMN commit_sha TEXT NOT NULL DEFAULT '';` (column named `commit_sha`; `commit` is a SQL keyword). PK stays `(run_id, step_id, path)`.
- **`query.sql`**: `InsertArtifact` writes branch/commit_sha; `ListArtifactsForRun` selects them. Regenerate `internal/store/sqldb/` via `sqlc generate` (or hand-edit the generated file if sqlc is unavailable, matching its style).
- **`sqlite.go`**: `loadSteps` (`:179`) and `saveStep` (`:228`) carry `Branch`/`Commit`.
- **`mem.go`**: `cloneRun` already deep-copies the `Artifacts` slice by value (`:125`) — the new string fields ride along for free.

This makes `Artifact.Branch/Commit` round-trip, so a resumed join still finds its upstream branches (which persist in the base repo across the run — `TeardownRun` removes worktrees, not branches/objects).

## Testing

- **Workspace:** `GitManager.Commit` — commits a dirty worktree, advances `step/<id>`, returns a real sha; `--allow-empty` on a no-file step.
- **Join (with `GitManager` + mock executor):**
  - `merge`: clean two-branch merge → committed result; conflict + `abort` → fail (worktree aborted); conflict + `escalate` → `ConflictError`.
  - `synthesize`: auto-merge of non-overlapping branches with no arbiter call; arbiter invoked only on a true conflict.
  - `select`: result carries the winner's branch ref + original artifacts.
- **Engine integration:** end-to-end git-native merge over isolated upstreams; the escalate ladder (arbiter resolves → approve → committed result; reject → fail).
- **Validation:** join/upstream not-isolated rejected; `merge`+`escalate` without agent rejected; `on_conflict: retry` without retry policy rejected.
- **Store:** artifact branch/commit round-trips through sqlite (save → load).
- **Migration:** existing M5b join tests migrate from plain `Manager` to `GitManager` (git-native requires a git-backed workspace).
- **Manual SSE proof** (per the running-the-orchestrator skill, mock agent): a 2-branch isolated fan-out → `merge` join; demonstrate a clean merge, then a conflicting variant with `on_conflict: escalate` showing `gate.awaiting` → `cm approve` → committed result over live SSE.

## Touchpoints

- `internal/core/ports.go` — `Artifact` fields; `Workspace.Commit`.
- `internal/workspace/gitmanager.go`, `workspace.go` — `Commit` impls.
- `internal/engine/engine.go` — commit-on-success stamp; `escalateJoin` conflict ladder.
- `internal/join/git.go` (new), `join.go`, `select.go`, `synthesize.go` — git-native strategies; `ConflictError`; `ResolveConflictPrompt`; remove `stageCandidates`.
- `internal/flow/validate.go` — isolated-join rule; merge+escalate agent rule; on_conflict=retry rule.
- `internal/store/migrations/0002_artifact_refs.sql` (new), `query.sql`, `sqldb/` (regen), `sqlite.go`.
- Demo flow(s) + `running-the-orchestrator` skill — isolated upstreams + a conflict example.

## Risks / notes

- **`merge` auto-commit vs. engine commit:** joins self-commit; the engine's commit-on-success explicitly skips `s.Join != nil` to avoid a redundant empty commit on top.
- **Concurrency:** join `gitCmd` calls operate on the join's own worktree/branch and read already-frozen upstream branches; distinct worktrees have distinct indexes and the object store is concurrency-safe — consistent with `discoverGit` already shelling out without the run lock. If a race surfaces in testing, route join git through the `GitManager` run lock.
- **`retry` for deterministic merge conflicts** re-conflicts; documented as a near-no-op (escalate/abort are the meaningful policies).
- **Carried follow-up folded in:** `escalateJoin` emits no `step.started` for the re-run/finalize attempt — fix here alongside the gate `escalate` path for consistency.
