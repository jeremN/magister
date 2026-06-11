# Design — external-repo (Slice 1: provision from a real repo) (2026-06-11)

**Status:** approved (brainstorm), pre-plan.
**Milestone:** first slice of the **external-repo** axis carried since M4-C. North star = *full delivery* (a flow runs against a real repo and ends in a PR). This slice is the foundation; slices 2 (push result back) and 3 (open PR) are deferred to their own spec→plan cycles.

## Problem

Every run today provisions a **synthetic empty-base scratch repo**: `GitManager.ensureRepo` (`internal/workspace/gitmanager.go`) does `git init` + one `--allow-empty` "base" commit, and isolated steps' `step/<id>` branches fork from that empty tree. The git-native merge-at-join slice (merged `0b05953`) made the fan-in handoff real git — but it all happens on an empty base with no pre-existing files or history. That makes the orchestrator a closed demo system: it can't operate on the code a user actually has.

This slice lets a run provision its per-run scratch repo as a **clone of a real, pre-existing git repo**, with `step/<id>` branches forking from a real **base ref**, so the existing git-native join machinery produces real, mergeable history over real code. The source repo is treated as **read-only**: the run only ever *reads* it (via `git clone`); it never writes the source's working tree, branches, or refs.

## Goals

- A run can target a real local git repo, specified **at submit time** (not baked into the flow YAML), so flows stay repo-agnostic and reusable.
- The per-run scratch base repo is a **full local clone** of the source, checked out at a pinned base commit; `step/<id>` branches fork from there.
- The source repo is **never mutated** — read-only access only.
- **Back-compat:** a run with no repo specified behaves exactly as today (synthetic empty-base scratch repo). External-repo is purely opt-in.
- Resumed external-repo runs **re-provision** correctly (repo + pinned base persisted).
- Invalid repo/base **fails at submit** (`400`), not mid-run.

## Non-goals

- **Push the result back** to the real repo (a new ref) — slice 2.
- **Open a PR / push to a remote** — slice 3 (needs remote auth + host API).
- **Uncommitted/untracked working-tree state** — chose committed-only; the clone captures committed history. "Run against my current edits" is a possible later option.
- **Shallow / shared-object clones** (`--depth`, `--reference`) — chose a full local clone; these stay future levers if huge-repo cost ever bites.
- Any executor, agent, gate, or join-strategy behavior change. Joins/merge/select/synthesize run **unchanged** on the cloned base.

## Decisions

Five chosen in brainstorm (all approved):

1. **Decompose; build the foundation first.** North star is full delivery (PR). This slice = provision-from-real-repo only; result stays in the scratch clone (inspectable). Slices 2 & 3 are additive and out of scope.
2. **Repo + base specified at submit time** — `POST /v1/runs?repo=<abs-path>&base=<ref>` (flow YAML unchanged in the body); `cm run flow.yaml --repo <path> --base <ref>`. Repo paths are machine-specific and don't belong committed in a flow.
3. **Committed base only.** `step/<id>` branches fork from a committed ref; the source's uncommitted/untracked working-tree state is ignored. Reproducible, and a clone gives this for free.
4. **Full local clone** (not shared-object or shallow). `git clone <source> <scratch>/base`; the clone is a fully independent repo (new objects/branches never touch the source). Smallest, safest diff; reuses all existing worktree/commit/merge/teardown machinery.
5. **Pin base to a SHA at submit.** Resolve `base` (default = source `HEAD`) to a concrete commit SHA at submit and store the SHA, so a resume re-clones to the **same** commit even if the source branch advanced. Persist repo + pinned base (migration `0003`) for resume correctness.

## Architecture

### Submit path — `internal/api/handlers.go`, `cmd/cm/main.go`

`handleCreateRun` reads optional query params `repo` and `base`:
- Both absent → today's path (empty `RepoSpec`), behavior unchanged.
- `repo` present → **validate at submit** (read-only on the source): the repo is a git repo (`git -C <repo> rev-parse --git-dir`) and `base` (or `HEAD` if omitted) resolves to a commit (`git -C <repo> rev-parse --verify <base>^{commit}`). Resolve to the concrete **SHA**. Any failure → `400` with a clear message, before `Submit`. A relative `repo` is rejected (require absolute) or resolved against the daemon's CWD — **decision deferred to the plan**, default: require absolute.
- `cm run <flow.yaml> [--repo <path>] [--base <ref>]` appends the query params to the POST.

The trust boundary is unchanged (loopback by default, §9): the caller is local and trusted to name a path. `git` is still invoked without a shell, args orchestrator-controlled.

### Data model — `internal/core/state.go`, `internal/core/ports.go`

- `core.RunState` gains `Repo string` and `Base string` (the pinned SHA). Empty `Repo` = synthetic-base run.
- `core.Workspace` gains a provisioning seam:
  ```go
  // Provision records the run's source repo + pinned base SHA before any step
  // runs. Empty repo = synthetic empty-base scratch (today's behavior). No-op
  // for the plain Manager.
  Provision(runID RunID, repo, base string) error
  ```

### Workspace — `internal/workspace/`

- `Manager.Provision` — no-op (path-only manager has no git backing).
- `GitManager`:
  - A per-run spec map (`map[core.RunID]repoSpec{repo, base}`), populated by `Provision`, guarded like `locks`. Empty/absent spec ⇒ synthetic base.
  - `ensureRepo` branches on the spec:
    - **spec.Repo set, scratch `.git` absent:** `git clone <repo> <base>` → `git checkout <base-SHA>` (detached or a local `base` branch) → set `user.name`/`user.email` (clone doesn't copy local identity; needed for merge commits). `step/<id>` worktrees then fork from `HEAD` = the base commit.
    - **spec.Repo empty:** today's `init` + `--allow-empty` base commit (unchanged).
    - **scratch `.git` present** (resume / repeat): reuse, don't re-clone (today's `os.Stat` guard) — idempotent.
  - `freshWorktree`, `Commit`, `For`, `TeardownRun` are **unchanged**: they operate on whatever base `ensureRepo` produced. `TeardownRun` keeps the base (with the result history) and removes worktrees.

### Engine — `internal/engine/engine.go`

`Run` and `Resume` call `e.WS.Provision(runID, rs.Repo, rs.Base)` once, before scheduling steps. Both already have access to the run's repo/base (`Resume` via `rs core.RunState`; `Run` gains them — passed through from `Submit`, exact threading per the plan, e.g. `Run(ctx, rs, f)` or explicit params). No other engine change — `commitIsolated`, the join dispositions, and teardown are agnostic to how the base was provisioned.

### Supervisor — `internal/supervisor/supervisor.go`

`Submit` gains `repo, base` params, stores them in `RunState`, and starts the run. `ResumeAll` already replays `RunState`, so resumed runs carry repo/base into `Resume` → `Provision`.

### Store — `internal/store/`

- goose migration `0003_run_repo.sql`, matching `0002`'s shape exactly — two `ALTER TABLE` statements in one `StatementBegin/End` block, with a reversing `Down`:
  ```sql
  -- +goose Up
  -- +goose StatementBegin
  ALTER TABLE runs ADD COLUMN repo TEXT NOT NULL DEFAULT '';
  ALTER TABLE runs ADD COLUMN base TEXT NOT NULL DEFAULT '';
  -- +goose StatementEnd

  -- +goose Down
  -- +goose StatementBegin
  ALTER TABLE runs DROP COLUMN base;
  ALTER TABLE runs DROP COLUMN repo;
  -- +goose StatementEnd
  ```
- Hand-edit sqlc output (`models.go`, `query.sql.go`) — sqlc isn't installed, same as `0002`. `CreateRun`/`GetRun`/`ListRuns`/resume-load carry the two columns.

### Result retrieval — `internal/api/` (run GET)

Surface the scratch base path (`<dbdir>/runs/<runID>/base`) in the run's GET response so the user can find the result history (`git -C <path> log`). Small, and the natural hook slices 2/3 build on. (The integrate commit already persists on its branch in the scratch base after teardown.)

## Testing

- **`GitManager` clone** (`internal/workspace/gitmanager_test.go`): a test-created fixture repo (`git init` + a commit with a file) is cloned; assert (a) the scratch base has the fixture's file at the base commit, (b) an isolated step's worktree forks from it, (c) `Commit` advances `step/<id>`, (d) a merge of two step branches yields history containing **both** the base file and each step's additions.
- **Provision idempotence / resume**: `Provision` then `ensureRepo` twice (scratch present second time) → no re-clone, same base SHA. A spec with a pinned SHA whose source branch later moved still checks out the pinned SHA.
- **Back-compat**: empty `Repo` ⇒ today's empty-base path; existing engine/join tests (synthetic base) stay green unchanged.
- **Submit validation** (`internal/api`): non-git `repo` → 400; unresolvable `base` → 400; valid repo+base → 201 and a run whose `RunState.Repo/Base` are set (SHA pinned).
- **Store**: migration round-trips repo/base; `GetRun`/resume-load return them.
- **Demo**: `flows/external-repo.yaml` (isolated steps + a merge join, auto gates) run against a throwaway fixture repo via the real daemon; manual SSE proof that the integrate commit contains the base's files plus the steps' work (slice convention).

## Touchpoints

- `internal/api/handlers.go` (query params + submit validation), `internal/api/dto.go` (run GET adds scratch path), `cmd/cm/main.go` (`--repo`/`--base`).
- `internal/core/state.go` (`RunState.Repo/Base`), `internal/core/ports.go` (`Workspace.Provision`).
- `internal/workspace/gitmanager.go` (clone-or-init; spec map), `internal/workspace/workspace.go` (no-op `Provision`).
- `internal/engine/engine.go` (`Provision` call in `Run`/`Resume`), `internal/supervisor/supervisor.go` (`Submit`/resume thread repo/base).
- `internal/store/` (migration `0003` + hand-edited sqlc), `internal/store/sqlite.go`.
- `flows/external-repo.yaml` (new demo), run-skill doc note.

## Risks / notes

- **Hardlink sharing:** `git clone` from a same-filesystem path hardlinks existing objects. Safe — git objects are immutable; the clone only *adds* new objects, never rewrites the source's. A `git gc --prune` on the source mid-run can't corrupt the clone (hardlinks keep inodes alive). Document; no mitigation needed for the foundation.
- **Clone cost:** a full clone per run walks the source's history. Cheap via hardlinks on same-fs; if a huge real repo makes this bite, the shared-object (`--reference`) or shallow (`--depth`) levers are the follow-up (deliberately out of scope here).
- **Relative `repo` path:** default to requiring an absolute path (reject relative) to avoid CWD ambiguity between `cm` and the daemon; finalize in the plan.
- **Result lifetime:** the result lives only in the scratch base, which persists after teardown but is **not** garbage-collected by this slice. Slice 2 (push back) is the intended durable-delivery path; until then, retrieval is manual from the surfaced scratch path.
- **Carried (unchanged by this slice):** `select`-on-resume stale paths (pre-existing M5b); `merge`+`escalate` non-`ConflictError` re-run path untested.
