# Handoff — M5 COMPLETE; next: git-native merge-at-join (2026-06-11)

**Pick up next session (fresh context).** This is the clean bridge after finishing M5. M5a (conditional gates) + M5b (select/synthesize joins) are both merged to `main`; **M5 is done.** The remaining orchestrator follow-on is the **external-repo / git-native merge-at-join** slice. It has **no design doc yet** — start at brainstorming.

## State (verified)

- `main` at `894df6b`, clean. `go test -race ./...` → **177 passed / 15 pkgs**; `go vet ./...` clean; `go 1.22`.
- Deps: only third-party logic dep is `github.com/expr-lang/expr v1.17.8` (added by M5a; M5b added none). Pinned (do NOT bump past): `modernc.org/sqlite v1.36.1`, `pressly/goose/v3 v3.24.1` (next patch needs go 1.23).
- Run recipe: project skill `.claude/skills/running-the-orchestrator/SKILL.md` (build `magisterd`+`cm` → daemon on throwaway db/port → `cm run --watch` → SSE/Last-Event-ID verify → SIGTERM teardown). Inside the sandbox, launch the daemon with the sandbox disabled so a real CLI child reaches the network; for zero-cost smokes use `agent: mock`.
- Full M5b detail (what was built, manual-proof evidence, carried follow-ups) is in **`docs/superpowers/handoffs/2026-06-10-post-m5b-next-steps.md`** — read that for the joins internals. M5a detail in `…/2026-06-10-post-m5a-next-steps.md`.

## What's done (one-line recap)

- **M0–M3:** engine + SQLite store + resume + `magisterd`/`cm` HTTP/SSE service + blocking manual gates.
- **M4:** A=engine robustness; C=git-worktree workspaces (`workspace.GitManager`: each `isolated` step → a real `git worktree` off a per-run scratch repo); B1/B-stream/B2/B3=real CLIAgents (claude/gemini/codex) streaming `agent.tool` milestones over SSE. **M4 complete except the git-native handoff (this doc).**
- **M5a:** `gate.policy: conditional` — compile+eval an `expr` at submit, resolve synchronously (expr-lang).
- **M5b:** `select`/`synthesize` agent-arbitrated joins on the **path-based** model + `on_conflict` engine disposition (`escalateJoin` approve=re-run-once). Deliberately deferred git-native merge to this slice.

## Next: the git-native merge-at-join slice

**Why it's next:** M4-C already runs `isolated` upstream steps in real **git worktrees** off a per-run scratch repo, but the artifact HANDOFF is still **path-based** — downstream steps receive artifact file *paths*, not branch refs, and the `merge` join just writes a *manifest* (`internal/join/join.go` `Merge`), never an actual `git merge`. M5b's select/synthesize stayed on that path-based model on purpose. The git-native slice closes this: make upstream `isolated` work land on real git **branches**, and make the join perform a real **`git merge`**, where **`on_conflict` becomes a true merge-conflict policy** (not just the arbiter-failure policy it is today).

**This is creative/architectural work → it MUST start with `superpowers:brainstorming`** (no spec/plan exists). Do NOT jump to coding. The brainstorm should resolve (at least) these design questions — surface them one at a time, propose 2-3 approaches each:

1. **Scope/shape:** Is this primarily about the **`merge`** strategy doing a real `git merge` (and `on_conflict` dispositioning conflicts), with select/synthesize unchanged? Or does it also rework how `isolated` upstreams expose their work (branch refs vs paths)? Recommend scoping tight: `merge`-strategy-goes-git-native first.
2. **Branch model:** M4-C gives each isolated step a worktree off the scratch repo's base commit. Does each isolated step **commit** its work to a per-step branch (so the join has real branches to merge)? Where does that commit happen — engine after a successful isolated step, or the workspace layer? (Touches `workspace.GitManager`.)
3. **The merge itself:** `merge` join does `git merge` of the upstream branches into the join's base. Conflict → surface via `on_conflict` (abort/retry/escalate — escalate already exists from M5b's `escalateJoin`; here "escalate" means a human resolves the conflict). How are conflict markers / the conflicted tree represented to a human or arbiter?
4. **select/synthesize under git-native:** does `select` become a branch fast-forward/checkout (pick a branch, make it the join's tree)? Does `synthesize` become "merge all, then arbiter resolves conflicts"? Or are they out of scope for v1 (keep them path-based, git-native only for `merge`)?
5. **"External-repo":** the carry-over phrase also implies running flows against a **real external git repo** (not just the synthetic per-run scratch repo). Is that in scope here or a separate concern? (Recommend: separate; this slice is git-native *joins*, external-repo is a different axis.)
6. **`on_conflict` reuse:** M5b's `on_conflict` + `escalateJoin` already disposition a *failed* join. A merge conflict is a new kind of join failure — does it slot into the same disposition cleanly (a `git merge` that conflicts returns an error → existing on_conflict path)? Likely yes; confirm the seam.

**Existing seams to ground the brainstorm/spec in (read these):** `internal/workspace/git.go` (`GitManager`: scratch repo, per-run base commit, `freshWorktree`, `TeardownRun`); `internal/join/join.go` (`Merge` manifest, the `Strategy.Join(..., run RunAgent)` signature, `stageCandidates`); `internal/engine/engine.go` (`execute` join dispatch, `runStep` on_conflict disposition, `escalateJoin`); `internal/flow/flow.go` (`Join{Strategy, Agent, OnConflict}`, `Workspace` field / `WSIsolated`). The M4-C spec/plan (`…/specs/2026-06-04-m4c-git-worktree-workspaces-design.md`, `…/plans/2026-06-04-m4c-…`) explain the worktree model and explicitly defer the git-native branch-from-deps/merge-at-join handoff to "Slice B" (now here).

## Workflow (same loop that built M5a + M5b — it works well)

`superpowers:brainstorming` (one question at a time → design → write spec to `docs/superpowers/specs/YYYY-MM-DD-<topic>-design.md`, commit, user-review gate) → `superpowers:writing-plans` (bite-sized TDD tasks, exact code, to `docs/superpowers/plans/…`, commit) → `superpowers:subagent-driven-development` in a worktree (`git worktree add .worktrees/<name> -b <name>` — native EnterWorktree is broken here; fresh implementer subagent per task → spec-compliance review → code-quality review → fix-loop; **Opus for the riskiest units + the final holistic review**; pass each task's full text + scene-setting to the implementer, don't make it read the plan file) → `superpowers:finishing-a-development-branch` (fast-forward merge to `main`, remove worktree, delete branch) → post-slice handoff committed to `main`.

**Commit conventions:** single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`, run hooks normally. `rtk` is NOT installed — run `go`/`git` directly. Committer identity = git's host-based fallback (leave it; matches existing commits). Keep `main..HEAD` to exactly the feature commits (fold review-driven fixes via `git commit --amend`); land each slice's docs as separate `docs:` commits on `main`.

## Carried follow-ups (non-blocking; from M5b's final review — candidates to fold into the git-native slice since it reworks the same areas)

- **`escalateJoin` re-run emits no `step.started` for attempt N+1** — minor SSE-observability gap, but **consistent with the existing gate `escalate`** path, so it's an established pattern. If polished, fix BOTH escalate paths together.
- **synthesize retry/escalate re-runs re-stage into the same `.candidates/`** — harmless on the path-based model; revisit when git-native reworks workdir/branch semantics (no synthesize-retry test exists).
- **Validator asymmetry:** `validateJoin` does NOT require `s.Retry` when `on_conflict=retry` (the gate path DOES require it for `on_fail=retry`); the engine degrades retry-without-`s.Retry` to abort safely, but a forgotten retry policy silently no-ops at submit. Mirror the gate check in a later pass.
- **GOTCHA (bit the M5b manual proof + a plan test):** fan-in upstream steps with NO `gate` default to **manual**, which **blocks on a human in the real daemon** (`cm approve`) — unlike `gate.AutoApprover{}` in unit tests. In demo flows give upstreams an explicit `gate: {policy: auto, verifier: {command: "true"}}`. The git-native demo flows will need this too.

## Blessed deviations on record (don't re-litigate)

- `core.Store.AppendEvents` (M3) — run-level events persisted before bus publish so SSE can replay/terminate.
- M5a's `go mod tidy` moved `oklog/ulid/v2` indirect→direct (a correct tidy fix of a pre-existing untidy go.mod, not over-reach).
