# Handoff — git-native merge-at-join COMPLETE; next steps (2026-06-11)

**Pick up next session (fresh context).** The **git-native merge-at-join** slice is merged to `main` and complete. This closes the last M4-era follow-on (the external-repo / git-native handoff that M4-C and M5b deferred). The fan-in handoff is now genuinely git-native: isolated steps commit to branches, joins do real `git merge`, and `on_conflict` is a true merge-conflict policy.

## State (verified)

- `main` at `bf0f449`, clean. `go test -race ./...` → **all 13 test-bearing packages green**; `go vet ./...` clean; `go 1.22`.
- 10 feature commits (`486ed2f`..`0b05953`) + a flake fix (`bf0f449`). Spec: `docs/superpowers/specs/2026-06-11-git-native-merge-at-join-design.md`; plan: `docs/superpowers/plans/2026-06-11-git-native-merge-at-join.md`.
- Deps unchanged (no new dep). Same pins as before (`modernc.org/sqlite v1.36.1`, `pressly/goose/v3 v3.24.1`).
- Run recipe: project skill `.claude/skills/running-the-orchestrator/SKILL.md` (now documents git-native joins). Zero-cost mock merge demo: **`flows/git-native-merge.yaml`**.

## What was built (per task → commit)

1. **Artifact superset + store** (`486ed2f`): `core.Artifact` gains `Branch`/`Commit`; persisted via goose migration `0002_artifact_refs.sql` (`commit_sha` column — `commit` is reserved) + hand-edited sqlc output (sqlc isn't installed). Empty for shared/mock steps.
2. **`Workspace.Commit`** (`2caa10d`): `GitManager.Commit` stages+commits a worktree to `step/<id>` (`--allow-empty`), returns branch+sha; `ensureRepo` now persists `user.name`/`user.email` so linked-worktree merges have an identity; plain `Manager.Commit` is a no-op.
3. **Engine commit-on-success** (`e9b0948`): `commitIsolated` commits a successful **isolated non-join** step and stamps Branch/Commit onto its artifacts (joins self-commit; shared steps skipped; a commit error becomes a step failure).
4. **Join git foundation** (`1633336`): `internal/join/git.go` — `gitCmd`, `ConflictError`, `upstreamBranches`, `conflictedPaths`, `CommittedResult` (the single artifact-enumeration point), `ResolveConflictPrompt`, `EnsureResolved` (added in task 8). Uses `-z` NUL-delimited git output for path safety. Shared test fixture `gitfixture_test.go` (`setupJoinRepo`).
5. **merge git-native** (`5c81b37`): sequential `git merge` of upstream branches; clean → `CommittedResult`; conflict → `escalate` returns `*ConflictError` (worktree left mid-merge), else `git merge --abort` + fail. Manifest gone.
6. **synthesize git-native** (`629f81b`): merge each branch, arbiter resolves **only** conflicts, `EnsureResolved`-guarded, commit. `.candidates/` staging gone.
7. **select by-ref** (`447c8ff`): arbiter reads upstream paths, forwards the **winner's** branch by reference (no new commit). `stageCandidates` deleted.
8. **engine escalate ladder** (`30f86bb`): a `*ConflictError` routes to `resolveConflictEscalation` — arbiter resolves in place → `EnsureResolved` guard → human `Gate.Escalate` → `WS.Commit` **concludes** the merge (2-parent commit) → `CommittedResult`. Never aborts/re-merges. Arbiter cost carried. (Non-conflict join failures keep the old re-run path.)
9. **validation + fixture migration** (`c23efec`): a join step **and all its `Needs`** must be `workspace: isolated`; `merge`+`on_conflict=escalate` needs a `join.agent`; `on_conflict=retry` needs a retry policy. Migrated all join fixtures (engine tests, `feature-flow.yaml`) to git-native.
10. **demo + skill doc** (`0b05953`): `flows/git-native-merge.yaml` + run-skill update. **Manual SSE proof DONE** (mock): real daemon ran the merge join; verified `step/integrate` is a **2-parent merge commit** with both upstreams' files (not a manifest).

**+ flake fix** (`bf0f449`): `TestEngineWideFanInNoDeadlock`'s deadlock timeout widened 5s→30s — making it git-native swapped 20 mock steps for 20 real worktrees + a 20-branch merge (~4s, longer under load), so the old fixed bound flaked under CPU contention. A hang-detector timeout should scale with the work it bounds.

## Key design facts to remember

- **The merge-conclusion contract:** `Merge.Join` returns `ConflictError` WITHOUT aborting → the worktree stays mid-merge (`MERGE_HEAD` set) → the engine resolves in place and `WS.Commit` (`git add -A && git commit`) concludes it into a real 2-parent merge commit. NEVER `--abort` or re-run `Merge.Join` on a `ConflictError`.
- **Two arbiter-resolution sites** (synthesize inline; engine ladder) share `join.EnsureResolved` (stage + `diff --cached --check` with whitespace rules disabled, so an arbiter's trailing whitespace isn't misread as a marker). Neither commits an unresolved tree.
- **Defense in depth:** the validator rejects non-isolated join upstreams at submit; the engine also guards (`upstreamBranches` empty → error) at runtime.
- **GOTCHA (unchanged):** fan-in upstreams with no `gate` default to **manual** → block in the real daemon. Give demo upstreams `gate: {policy: auto, verifier: {command: "true"}}`.

## Carried follow-ups (non-blocking; from the final holistic review)

- **`select` on resume reads stale upstream paths** — a resumed run's seeded upstreams have no live worktree, so `selectPrompt` lists non-existent paths to the arbiter. **Pre-existing from M5b, not a regression** (merge/synthesize actually improved: they're branch-based and branches persist). Fix when reworking resume.
- **`merge`+`escalate` non-`ConflictError` re-run path is untested** (`engine.go` `escalateJoin` generic branch) — a transient git error after the first branch committed takes the re-run path (the re-`merge` of the already-merged branch is a no-op). Reasonable but add a test.
- **`ConflictError.WorkDir`** is informational/unused on the resolution path (the engine uses its own `workDir`). Keep for diagnostics or drop.
- **Validator asymmetry (still open from M5b):** `validateJoin` mirrors the gate's `on_fail=retry`-requires-retry now for `on_conflict=retry` — done. No remaining asymmetry there.

## Next candidates (no design doc yet — start at brainstorming)

- **External-repo / real git repo:** run flows against a real pre-existing git repo (a real base commit with history) instead of the synthetic empty-base scratch repo. Explicitly OUT OF SCOPE of this slice (spec §non-goals). This is the natural next axis: the per-run scratch repo would become a clone/worktree of a real repo; `step/<id>` branches would fork from the real base; the merge join would produce real mergeable history. Touches `workspace.GitManager` (repo provisioning) + flow schema (where's the repo?).
- **select/synthesize under resume** (the stale-paths follow-up above).
- Anything M6-ish the user prioritizes.

## Workflow (same loop that built this slice — it works well)

`superpowers:brainstorming` (one question at a time → spec → `docs/superpowers/specs/…`, user-review gate) → `superpowers:writing-plans` (bite-sized TDD tasks, exact code → `docs/superpowers/plans/…`) → `superpowers:subagent-driven-development` in a worktree (`git worktree add .worktrees/<name> -b <name>` — native EnterWorktree is broken here; fresh implementer per task → spec review → code-quality review → fix-loop; **Opus for the riskiest units + the final holistic review**) → `superpowers:finishing-a-development-branch` (ff-merge to `main`, remove worktree, delete branch) → post-slice handoff on `main`.

**Process lessons from this slice:**
- **Run the WHOLE suite (`go test ./...`), not just the touched package, between tasks.** Tasks 5-7's git-native joins broke 3 engine tests (path-based joins on the plain Manager) that per-package `./internal/join/` checks missed; they only surfaced when Task 8's implementer ran the engine suite.
- **gofmt is NOT enforced by the repo hooks** — run `gofmt -l` yourself each task (caught an import-order and a couple of others).
- **Verify reviewer feedback technically before applying** — two reviewer "blockers" were false positives (the `0001` multi-statement migration pattern; the `--abort`-before-`ConflictError` "fix" that would have broken the escalate ladder), while others were real (the `ls-files` quoting bug, the silent-marker-commit gap). Empirical checks (run the git command, read the existing pattern) settled each.

**Commit conventions:** single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. `rtk` NOT installed — run `go`/`git` directly. Committer identity = git host-based fallback (leave it).
