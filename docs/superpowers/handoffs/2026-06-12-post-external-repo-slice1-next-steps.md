# Handoff — external-repo Slice 1 COMPLETE; next steps (2026-06-12)

**Pick up next session (fresh context).** The **external-repo Slice 1 (provision from a real repo)** is merged to `main` and complete. A run can now clone a real, pre-existing git repo as its per-run scratch base; `step/<id>` branches fork from a pinned base commit, and the existing git-native join machinery produces real mergeable history over real code. This is the foundation of the **full-delivery north star** (flow → real repo → PR); slices 2 (push result back) and 3 (open PR) remain.

## State (verified)

- `main` at `2daba16`, clean. `go test -race ./...` → **all 13 test packages green**; `go vet ./...` clean; `go 1.22`. **No new deps.**
- 8 feature commits `3da8612..2daba16` on top of the spec+plan commit `2c409c8`. Spec: `docs/superpowers/specs/2026-06-11-external-repo-design.md`; plan: `docs/superpowers/plans/2026-06-11-external-repo-slice1.md`.
- `gofmt -l .` clean EXCEPT a **pre-existing** unformatted `internal/executor/gemini.go` (predates this slice, untouched by it — left a prior gofmt run; `gofmt -w internal/executor/gemini.go` whenever someone touches that pkg).
- Run recipe: project skill `.claude/skills/running-the-orchestrator/SKILL.md` (now documents external-repo). Zero-cost mock demo: **`flows/external-repo.yaml`** — run with `cm run flows/external-repo.yaml --repo <abs> --base <ref>`.

## What was built (per task → commit)

1. **Store** (`3da8612`): `core.RunState` +`Repo`/`Base`; goose migration `0003_run_repo.sql` (`repo`,`base` cols) + hand-edited sqlc output (sqlc not installed; 0002 precedent). SQLite `CreateRun`/`GetRun`/`LoadIncompleteRuns` carry them. Mem store needs no change (whole-struct copy).
2. **Workspace clone + Provision seam** (`e0102bf`): `core.Workspace` +`Provision(runID, repo, base)`; `Manager` no-op; `GitManager` records a per-run `repoSpec` (guarded by `m.mu`) and `ensureRepo` branches **clone-or-init** — `cloneBase` does `git clone <src> <baseDir>` + `checkout --detach <sha>` + sets identity. Empty repo ⇒ today's empty-base path byte-for-byte. **Security hardening (from a flagged HIGH):** `isHexSHA(base)` guard, `clone -- <repo> <dir>`, `-c protocol.ext.allow=never`; checkout-failure cleans up the half-clone (`os.RemoveAll(baseDir)`).
3. **Engine+Supervisor wiring** (`012695a`): `Engine.Provision` delegator → `WS.Provision`; `Submit(ctx, f, yaml, repo, base)` persists + provisions before start; `ResumeAll` re-provisions from persisted `RunState.Repo/Base` (log+continue on error). All `Submit` call sites updated.
4. **ResolveBase** (`f73d5f4`): `workspace.ResolveBase(repoDir, ref) (sha, err)` — submit-time, **read-only** on the source; requires absolute path, validates it's a git repo, defaults ref→HEAD, pins to a concrete SHA via `rev-parse --verify --quiet --end-of-options <ref>^{commit}` (`--end-of-options` = explicit argv-flag-smuggling guard).
5. **API** (`9550ec2`): `POST /v1/runs?repo=&base=` — when `repo` set, validate via `ResolveBase` (400 on bad repo/ref), pass repo + **pinned SHA** to Submit; absent repo ⇒ unchanged.
6. **cm** (`74302e9`): `cm run <flow> --repo <abs> --base <ref>` → query params; bare `--repo`/`--base` (no value) errors.
7. **Scratch path in GET** (`d4386aa`): `Server.ScratchRoot` (wired in `main` from the SAME `runsRoot` as `GitManager.Root`); `runSnapshot.Scratch` (omitempty) = `<root>/<id>/base`, only for external-repo runs.
8. **Demo + docs** (`2daba16`): `flows/external-repo.yaml` + run-skill note. **Manual SSE proof DONE** (real daemon, real fixture repo): scratch is a clone of the source (HEAD detached at the source SHA); `step/build-api`+`step/build-ui` fork from the real base (merge-base == source HEAD); `step/integrate` is a real **2-parent merge commit** whose tree has the cloned `README.md` + both step outputs; run GET surfaced the `scratch` path.

## Key design facts to remember

- **The pinned SHA is the single source of truth.** `ResolveBase` resolves `base`→SHA at submit; that SHA (never the raw ref) is persisted, provisioned, and checked out. A resume re-clones to the same commit even if the source branch moved.
- **Read-only source, airtight:** the source repo is only ever `clone`'d and `rev-parse`'d. Every write (`checkout --detach`, `config`, worktrees/commits/merges) happens in the per-run clone (`<root>/<runID>/base`), never in the source.
- **Two-layer argv defense:** `ResolveBase` (`--end-of-options`, absolute-path) at submit; `cloneBase` (`isHexSHA`, `--`, `protocol.ext.allow=never`) at runtime. Empirically verified: `ext::sh -c …` clone fails `transport 'ext' not allowed`; a `--upload-pack=…` ref is treated as an operand.
- **Back-compat lever:** no `repo` ⇒ `Provision` records an empty spec ⇒ `ensureRepo` takes the synthetic empty-base path, byte-for-byte. Every existing flow/test is unaffected.
- **Path-layout knowledge is duplicated** in two places: `GitManager.baseDir` (`<Root>/<id>/base`) and `handlers.go` (`<ScratchRoot>/<id>/base`), kept consistent by wiring the same `runsRoot` to both in `main`. Fine for now; if it surfaces a third time, add `GitManager.BasePath(id)` and have the API consume it.

## Carried follow-ups (non-blocking)

- **Pre-existing `gemini.go` gofmt** — not this slice's; `gofmt -w` it next time that pkg is touched.
- **`cloneBase` post-checkout config failures don't clean up** the clone (unlike checkout failure). Benign (identity-config failure is near-impossible post-clone, and a retained clone at a valid pinned HEAD is reused correctly). Left as-is per the final review.
- **`ResolveBase` rejects `repoDir==""` internally** though the handler guards `repo != ""` first — harmless dead branch / defense-in-depth for non-HTTP callers. Left.
- **`repo` pointing at a SUBDIR of a git repo:** `ResolveBase` accepts it (rev-parse finds the parent) but `git clone <subdir>` fails at runtime → step fails. Document "pass the repo root," or resolve `--show-toplevel` in a hardening pass if it bites.
- **`select`-on-resume stale paths** (pre-existing from M5b) — unchanged here.

## Next candidates (no design doc yet — start at brainstorming)

- **Slice 2 — push the result back.** After a successful external-repo run, push the final integrate branch to a destination (the real local repo as a new ref, or a configured remote). Introduces the push + credential/remote surface. The result already lives at the surfaced `scratch` path as a real branch, so this is "take that branch and push it."
- **Slice 3 — open a PR.** On top of the pushed branch, call the host API (`gh` CLI or a Git-host MCP). Introduces the PR-API + host-config surface.
- The 3-slice decomposition (full-delivery north star) was agreed in this slice's brainstorm; slices 2 & 3 are additive and each get their own spec→plan→implement cycle.

## Workflow (the loop that built this slice — it works well)

`superpowers:brainstorming` (one question at a time → spec, user-review gate) → `superpowers:writing-plans` (bite-sized TDD tasks, exact code) → `superpowers:subagent-driven-development` in a worktree (`git worktree add .worktrees/<name> -b <name>` — native EnterWorktree is broken here; fresh implementer per task → spec-compliance review → code-quality review → fix-loop; **Opus on the riskiest units + the final holistic review**) → `superpowers:finishing-a-development-branch` (ff-merge to `main`, remove worktree, delete branch) → this handoff.

**Process lessons from this slice:**
- **A background security review runs on each commit and CAN find real issues.** It flagged a genuine HIGH (argv flag-smuggling in `cloneBase`) — fixed with `--`/`isHexSHA`/`protocol.ext.allow=never` + regression test, and the same hardening pattern (`--end-of-options`) was applied proactively to `ResolveBase`. Don't reflexively dismiss it as "internal-only."
- **Verify build/tests BEFORE `--amend`.** A code-quality fix to `testEngine`'s signature missed two callers; I amended a broken build, then had to re-fix. Run `go build ./...` (or the touched test) before amending a reviewed commit.
- **Reject review feedback that fights the design.** A reviewer suggested guarding `Provision` to error on double-provision — but `ResumeAll` re-provisions the same run on restart by design, so last-write-wins is correct; only the doc comment needed fixing. Verify feedback against the design before applying.
- **Run the WHOLE suite (`go test -race ./...`) between tasks**, and **`gofmt -l` yourself** (not hook-enforced).

**Commit conventions:** single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. `rtk` not installed — run `go`/`git`/`gofmt` directly. `cm` reads `$MAGISTER_ADDR` **verbatim as the base URL** — it needs the `http://` scheme (a no-scheme `host:port` errors "first path segment in URL cannot contain colon").
