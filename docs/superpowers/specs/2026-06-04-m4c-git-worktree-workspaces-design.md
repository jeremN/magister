# Design — M4 Slice C: Git-Worktree Workspaces

**Written:** 2026-06-04, after M4 Slice A merged to `main`.
**Parent spec:** `docs/superpowers/specs/2026-06-02-orchestrator-design.md` (§5 attempt model, §7 resume; the `workspace: isolated` / "torn down after" / "fresh worktree per attempt" intent).
**Status:** approved (brainstorming), ready for `writing-plans`.
**Sequence note:** Slice C is being built **before** Slice B (real CLIAgents), at the user's direction — C builds the isolation substrate B's real agents will run inside.

## TL;DR

Give each `isolated` step a **real `git worktree`** instead of a plain `mkdir`'d subdir, with teardown and resume-safe (idempotent) creation. This is the **"foundation"** model (Model A): a **scratch per-run git repo**, worktrees branched off its base, **path-based handoff unchanged**, **run-end teardown**. It is self-contained, fully testable with the `mock` executor today, needs **no external repo, no executor/join changes, no DB migration, no new YAML fields, and no new dependencies** (`git` via `os/exec`). The git-native handoff (branch-from-deps, commit step output, merge-at-join) is explicitly **deferred to Slice B** (or later), where real agent diffs make a `git merge` meaningful.

## 1. Scope

**In scope**

1. A scratch **per-run git repo** (`git init` + one empty `base` commit), created lazily.
2. `isolated` step → a **linked `git worktree`** on its own branch off `base`.
3. `shared` step → the run's **base working tree** (current shared semantics preserved).
4. **Run-end teardown** of the run's isolated worktrees (the base repo persists).
5. **Resume-idempotent** worktree creation (remove-stale-then-add).
6. A new **`workspace.GitManager`** implementation; the daemon wires it.
7. `stepID` **slug validation** (it now becomes a path segment + git ref).

**Non-goals (deferred)**

- External / configured target repo (the codebase real agents edit) — arrives with Slice B.
- Git-native handoff: branch-from-deps, committing step output, `git merge` at fan-in joins — deferred (most valuable once Slice B produces real diffs).
- Per-attempt worktree **reset** (`git reset --hard && git clean`) — deferred; Slice C is worktree-per-**step** (fresh on resume; in-run retries reuse the worktree).
- Any change to the executor, the join package, conditional gates, or the API.

## 2. Layout & the per-run repo

```
<Root>/<runID>/base/          # the per-run git repo; ALSO the SHARED working tree
<Root>/<runID>/wt/<stepID>/   # an isolated step's linked worktree (branch step/<stepID> off base)
```

- On the **first** `For(runID, …)` of a run, `GitManager` lazily initialises the repo at
  `<Root>/<runID>/base/`: `git init`, then an **empty base commit**:
  `git -C base commit --allow-empty -m "base"`. Commits use an **injected orchestrator identity**
  (`-c user.name=… -c user.email=…`, default e.g. `magisterd` / `magisterd@localhost`) so the
  commit never fails on a missing git user. The default branch name is left to git (`main` /
  `master` / `init.defaultBranch`) and never referenced by name — worktrees branch off **`HEAD`**
  (the base commit), so the design is robust to git version / config.
- `shared` step → `<Root>/<runID>/base/` (steps reuse it; concurrent shared steps may race on
  files — operator's choice, unchanged from M1).
- `isolated` step → `git -C base worktree add <abs wt/<stepID>> -b step/<stepID> HEAD`, returning
  `<Root>/<runID>/wt/<stepID>` as the step's working directory. (Since Model A never commits step
  output, `HEAD` stays at the base commit for the whole run.)
- The base repo is detected by an existing `<Root>/<runID>/base/.git`, so re-entry (resume)
  does not re-init.

## 3. Interface change & engine integration

`core.Workspace` gains exactly one method:

```go
// TeardownRun removes the run's isolated worktrees and prunes admin state. The base
// repo persists. A no-op for a run with no worktrees (or a non-git Workspace).
TeardownRun(runID RunID) error
```

- The engine's `runDAG` adds `defer e.WS.TeardownRun(runID)` immediately after the run starts.
  Because `defer` runs after the function body completes — i.e. **after `wg.Wait()`** and the
  terminal status writes — every downstream step has already read its upstream artifact paths
  before any worktree is removed. Errors are logged (best-effort), never fatal. Teardown fires
  on **all** terminal outcomes (succeeded / failed / canceled) — uniform disk reclamation; the
  base repo remains for inspection.
- `For` stays **once per step** (unchanged call site). Its returned `cleanup` becomes a **no-op**
  for `GitManager` (the worktree must outlive the step for path-based handoff; run-end teardown
  does the real work).
- **Handoff is unchanged.** `runDAG` still gathers a downstream step's inputs as its upstream
  steps' `Artifact` paths; since all worktrees live under `<Root>/<runID>/`, cross-worktree path
  reads work for the whole run.

### 3.1 Post-run artifact paths (accepted consequence)

After `TeardownRun`, `Artifact` paths into isolated worktrees are dangling on disk (the store
still records them as metadata). This is acceptable for v1: by run-end all in-run consumers have
read them, and the API serves step **metadata**, not file contents. Documented, not hidden.

## 4. Two implementations (blast-radius control)

Many engine / supervisor / e2e tests construct `&workspace.Manager{Root: …}` expecting plain
`mkdir`. To avoid forcing `git` into all of them:

- **Keep `workspace.Manager`** as the simple `mkdir` implementation, adding a **no-op
  `TeardownRun`** so it still satisfies the extended interface. Fast unit tests keep using it.
- **Add `workspace.GitManager`** — the production worktree implementation (this slice's new
  code). The daemon wires `GitManager`.
- Worktree behaviour is tested directly in `workspace_test` (`GitManager`) plus one focused e2e
  that wires `GitManager` through the daemon.

(Alternative considered: make `Manager` itself git-aware and update every test — rejected: more
churn and it couples all unit tests to `git` on PATH.)

`GitManager` carries `Root string` plus an optional commit identity (`Name`, `Email`, defaulted).
It is essentially **stateless** (it derives a run's state from disk), with a **per-run lock**
(a `map[RunID]*sync.Mutex` guarded by a small meta-mutex) serialising that run's `git` invocations
so concurrent `isolated` steps don't race on the repo index. Lazy repo init happens under that
lock (init iff `base/.git` is absent).

## 5. Resume idempotency

On resume the run dir, base repo, and any pre-crash worktrees survive on disk (the crash skipped
teardown). For a **non-succeeded** step that re-runs, `GitManager.For` must yield a *clean*
worktree:

1. `git worktree prune` (clear admin entries left by the dead process);
2. if `wt/<stepID>` or branch `step/<stepID>` exists → `git worktree remove --force <path>` and
   delete the branch;
3. `git worktree add <abs wt/<stepID>> -b step/<stepID> HEAD`.

This is idempotent and yields **fresh-on-resume**. The base repo is detected (not re-initialised).
**Succeeded** steps are not re-run (their results are seeded from the store), so their worktrees
are left intact until run-end teardown — their artifact paths still resolve for re-running
dependents.

### 5.1 Worktree-per-step vs the spec's "per attempt"

The parent spec says "fresh worktree per attempt," but the engine calls `For` **once per step**
(before the retry loop), so in-run retries reuse the worktree. Slice C is **worktree-per-step**:
fresh on resume, reused across in-run retries. A per-attempt `git reset --hard && git clean -fdx`
is **deferred to Slice B** — with `mock` it is moot, and a dirty-tree reset matters most once real
agents leave partial edits. This is a deliberate, documented divergence from that spec line.

## 6. `git` invocation & safety

- Shell out to `git` via **`os/exec` with argument slices (no shell string)** — **no new
  dependency** (no go-git), `go 1.22` preserved.
- `git` must be on PATH. Workspace tests `t.Skip` when `exec.LookPath("git")` fails, keeping CI
  portable.
- Paths/branches embed `runID` (ulid — safe) and `stepID`.

### 6.1 `stepID` slug validation

`stepID` now becomes a **filesystem path segment** and a **git branch name**, but `flow.Validate`
only checks non-empty / unique today. Add a rule: `stepID` must match `^[A-Za-z0-9._-]+$` (reject
path- and ref-hostile characters, and a leading `-`). This is a small, justified validation
addition that also hardens the existing dir-based `Manager`.

## 7. Testing

- **`workspace_test` (`GitManager`):**
  - isolated `For` → a real linked worktree at `wt/<id>` on branch `step/<id>` (assert via
    `git -C base worktree list` and the worktree's `.git` file);
  - shared `For` → the `base/` tree (two shared steps share it);
  - `TeardownRun` → the worktrees are gone, `base/` remains;
  - **resume idempotency:** `For` the same isolated step twice (simulating re-run) → the second
    succeeds (stale worktree+branch removed first);
  - `t.Skip` when `git` is absent.
- **Engine / e2e:** a flow with **fan-out isolated** steps wired through `GitManager` runs
  end-to-end on the daemon — each isolated step gets its own worktree, a downstream step reads
  upstream artifact paths across worktrees, and run-end teardown removes them. Plus a
  **kill-and-resume** test exercising idempotent re-creation. Stress the resume path
  (`GOMAXPROCS=8 -race -count=N`) per M3 discipline.
- **`flow` validation:** `stepID` slug accepted/rejected cases.
- Unit engine / supervisor tests keep the lightweight `Manager` (unchanged, fast, git-free).

## 8. Files touched (rough)

- `internal/core/ports.go` — extend `Workspace` with `TeardownRun`.
- `internal/workspace/gitmanager.go` — new `GitManager` (init/worktree-add/remove/prune,
  per-run lock, `For` + `TeardownRun`).
- `internal/workspace/workspace.go` — add a no-op `Manager.TeardownRun`.
- `internal/engine/engine.go` — `defer e.WS.TeardownRun(runID)` in `runDAG`.
- `internal/flow/validate.go` — `stepID` slug rule.
- `cmd/magisterd/main.go` — wire `workspace.GitManager` (with the orchestrator identity default).
- Tests across `internal/workspace`, `internal/flow`, and the e2e suite.

**No migration. No new YAML fields. No new dependencies.**

## 9. Conventions (carried from M0–M4 Slice A)

- Commits: single conventional-commit subject, no body, no `Co-Authored-By`, never `--no-verify`;
  commit with the explicit git identity (M4 kickoff handoff §5).
- Dep pins: `go 1.22` must not move; no `go get` is expected for this slice.
- RTK hook reformats `go`/`git` output; use `rtk proxy` for raw PASS/FAIL.
- Semgrep hook runs on edits; `os/exec` of `git` with operator-controlled args is the intended
  capability (annotate `#nosec G204` with the trust-boundary rationale, mirroring
  `gate.CommandVerifier`).
- Engine invariants: persist-then-publish; SQLite single-writer; deadlock-freedom; nil-safe
  loggers; the new `TeardownRun` is best-effort and must not break the run's terminal status.
