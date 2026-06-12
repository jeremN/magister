# Handoff — external-repo Slice 2 (push to remote) COMPLETE; next steps (2026-06-12)

**Pick up next session (fresh context).** **external-repo Slice 2 (push the result to a remote)** is merged to `main` and complete. After a succeeded external-repo run, `cm push <run>` delivers its result branch to a git remote. This is the second of the 3-slice full-delivery north star (flow → real repo → PR); only **Slice 3 (open a PR)** remains.

## State (verified)

- `main` at `28f6dfd`, clean. `go test -race ./...` → **all 13 test packages green**; `go vet ./...` clean; `go 1.22`. **No new deps.**
- 8 feature commits `b05aec3..28f6dfd` on top of the spec+plan commits (`b7ac03d` spec, `7150277` plan). Spec: `docs/superpowers/specs/2026-06-12-external-repo-slice2-push-design.md`; plan: `docs/superpowers/plans/2026-06-12-external-repo-slice2-push.md`.
- `gofmt -l .` clean EXCEPT the pre-existing `internal/executor/gemini.go` (predates Slice 1, untouched; `gofmt -w` it whenever that pkg is next touched).
- Run recipe: `.claude/skills/running-the-orchestrator/SKILL.md` (now documents `cm push`). Demo: `flows/external-repo.yaml` + a local bare repo as the remote.

## What was built (per task → commit)

1. **`flow.TerminalSteps`** (`b05aec3`): pure DAG helper — the leaves (steps nothing else `Needs`), in flow order. `internal/flow/graph.go`.
2. **`Workspace.BasePath` seam** (`6240000`): `core.Workspace` +`BasePath(runID) string` (GitManager → `<Root>/<id>/base`; Manager → run dir); `Engine.BasePath` delegator (mirrors `Provision`). Resolves the Slice-1 path-duplication follow-up.
3. **`workspace.ResolveRemote`** (`d12db7e`): resolves the push destination read-only on the source — `""`→origin URL, a name→`git -C <src> remote get-url <name>`, a URL→passthrough. `safeRemoteName`/`looksLikeURL` hardening; absolute-source guard.
4. **`workspace.PushBranch`** (`46af8b5`): `git push [--force] -- <remoteURL> <srcBranch>:refs/heads/<destBranch>` in the scratch clone; `safeRef` guards both refs (rejects `-`/`:`/`..`/`.lock`/trailing-`.`/non-`[A-Za-z0-9/._-]`); `--` separates operands; combined git output rides on the error. New-branch succeeds; non-fast-forward refused without `--force`.
5. **`Supervisor.Push`** (`a1ab7f5`): `Push(ctx, runID, PushOpts{Remote,As,Step,Force}) (PushResult, error)` — GetRun → validate (external-repo + succeeded) → parse flow → `pickResultStep` (unique terminal or `--step`) → `stepBranch` (the chosen step's persisted `Artifact.Branch`/`Commit`) → `ResolveRemote` → `BasePath`+`.git` check → `PushBranch`. Errors are `*PushError{Status,Msg}` (HTTP statuses, so the API maps without string-matching).
6. **`POST /v1/runs/{id}/push`** (`7d3cee9`): `handlePush` reads `?remote=&as=&step=&force=`, calls `Sup.Push`, maps `*PushError` via `errors.As` (else 500), returns `pushResponse{remote,branch,source_branch,commit}`. `newGitServer` test helper (GitManager-backed httptest).
7. **`cm push`** (`7becdd2`): `cm push <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--force]` → the endpoint; prints `pushed <source> → <dest> on <remote>`; bare-flag-with-no-value errors.
8. **Docs + proof** (`28f6dfd`): run-skill `cm push` note. **Manual proof DONE** (real daemon, fixture source repo whose `origin` is a local bare repo): `cm push <run>` pushed `magister/<run>` to the bare remote — a real 2-parent merge commit whose tree carries the cloned `README.md` + both step outputs; the **source repo stayed untouched** (only `main`, no `magister/*`).

## Key design facts to remember

- **Result branch = persisted, not recomputed.** Each isolated/join step's artifacts carry `Branch`/`Commit` (set by `commitIsolated`/`CommittedResult`). `Push` finds the terminal step (`flow.TerminalSteps`) and reads its `Artifact.Branch` (e.g. `step/integrate`). `--step` disambiguates a multi-leaf flow; a terminal with no branch (shared step) → 400.
- **Push is post-run + store-driven — the engine lifecycle is UNTOUCHED.** It reads the run from the store and acts on the scratch clone (`Engine.BasePath`). The scratch base persists after the run (`TeardownRun` keeps it), so the result branch is still there.
- **Default remote = the SOURCE's own `origin`, NOT the scratch clone's origin.** The scratch was cloned from a local path, so its origin points back at that local path; `ResolveRemote` reads the source repo's origin URL instead. `--remote` overrides (name or URL).
- **Credentials = ambient git, zero token handling.** `PushBranch` just execs `git push`; git authenticates via the daemon's SSH agent / credential helper / cached HTTPS. Auth/network failure → 502 with git's stderr.
- **Two-layer argv hardening (empirically verified by the final review):** `ResolveRemote` (`safeRemoteName`, absolute-source, read-only) + `PushBranch` (`safeRef` on both refs, `--` separator). No `-`-leading or `:`-bearing value can reach git as a flag or corrupt the `src:refs/heads/dest` refspec. **Not SSRF** (loopback; operator pushes to their own remote — git's intended capability).
- **Read-only source, airtight:** only `remote get-url`/`rev-parse` on the source; all writes (the push) run in the scratch clone. Source refs/worktree never touched.
- **Safe by default:** new-branch push always succeeds; re-push of a moved branch is a non-fast-forward git rejects unless `--force`. No force logic of our own.

## Carried follow-ups (non-blocking)

- **30s timeout middleware applies to `POST .../push`** (`internal/api/router.go` `timeoutMiddleware`). Fine for local/bare remotes (<1s); a slow REAL network push could exceed it and return 503. If Slice 3 / real-remote use makes this bite, exempt push from the timeout like SSE is exempted. Pre-existing infra, not introduced here.
- **`GetRun` storage errors masquerade as 404** (`supervisor.go` Push, documented `// TODO`): the store has no not-found sentinel, so a genuine I/O error reads as 404. Fine for a loopback SQLite daemon; revisit if the store grows a typed `ErrNotFound`.
- **Scratch lifetime:** push depends on the scratch base persisting (it does). A future scratch-GC slice must order GC **after** push, or rely on the `404 missing scratch` path.
- **Pre-existing `gemini.go` gofmt** — not this slice's; `gofmt -w` next time that pkg is touched.
- `looksLikeURL` is a heuristic (scheme:// or scp-like `user@host:path`); a Windows `C:\path` remote name would misclassify — irrelevant on darwin/linux.

## Next: Slice 3 — open a PR (no design doc yet → brainstorm)

On top of the pushed branch (`magister/<runID>` now lands on the remote), open a Pull Request via the host API. New surface: a Git-host client (`gh` CLI or a GitHub/GitLab MCP), host detection from the remote URL, PR title/body, and host config/auth. Natural CLI shape: `cm pr <run>` (or `cm push --pr`). Start at `superpowers:brainstorming` — key questions: which host(s), `gh` CLI vs API/MCP, PR metadata (title/body/base branch), and whether push+PR are one command or two. With Slice 3, the full-delivery north star (flow → real repo → PR) is complete.

## Workflow (the loop that built this slice — works well)

`superpowers:brainstorming` (one question at a time → spec, user-review gate) → `superpowers:writing-plans` (bite-sized TDD tasks, exact code) → `superpowers:subagent-driven-development` in a worktree (`git worktree add .worktrees/<name> -b <name>` — native EnterWorktree is broken here; fresh implementer per task → spec-compliance review → code-quality review → fix-loop; **Opus on the riskiest unit (T5 Supervisor.Push) + the final holistic review**) → `superpowers:finishing-a-development-branch` (ff-merge to `main`, remove worktree, delete branch) → this handoff.

**Process notes that held this slice:**
- The integration-heavy happy-path test (T5/T6) runs a REAL GitManager-backed external-repo flow in-process then pushes to a **local bare repo** standing in as the remote — no network, fully deterministic. Reuse this pattern for Slice 3 (a bare repo can't host a PR, but a local `gh`-stub or a fake host server can).
- Verify build/tests BEFORE `git --amend` (a lesson carried from Slice 1; held this time).
- The per-commit background security review stayed quiet this slice (the argv hardening was designed in from the spec) — but keep the `--`/`safeRef`/`safeRemoteName` discipline for any new git-exec.

**Commit conventions:** single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. `rtk` not installed — run `go`/`git`/`gofmt` directly. `cm` reads `$MAGISTER_ADDR` **verbatim as the base URL** — needs the `http://` scheme.
