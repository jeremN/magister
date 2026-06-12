# Kickoff — external-repo Slice 3 (open a PR) (prep for next week, 2026-06-12)

**Forward-looking warm-start for next week.** Slices 1 (provision-from-real-repo) and 2 (push-result-to-remote) are merged to `main`; this doc pre-loads the design space for **Slice 3 — open a Pull Request** so the brainstorm starts warm. Nothing here is decided — the questions below are for `superpowers:brainstorming` (one at a time, user-gated). For the full retrospective on what's built, read `2026-06-12-post-external-repo-slice2-next-steps.md` first.

## Where we are

- `main` at `45d8458`, clean. All 13 pkgs `go test -race ./...` green; `go vet` clean; `go 1.22`; no new deps.
- Slice 2 gave us: a pushed branch on the remote. `Supervisor.Push` returns `PushResult{Remote (URL), Branch (dest, e.g. magister/<runID>), SourceBranch, Commit}` — **exactly the inputs a PR needs** (which remote, which head branch).
- **Slice 3 closes the full-delivery north star: flow → real repo → PR.**

## Goal (Slice 3)

On top of the pushed `magister/<runID>` branch, open a Pull Request on the git host (GitHub first), from a finished external-repo run. Likely surface: `cm pr <run>` → `POST /v1/runs/{id}/pr`, parallel to Slice 2's push.

## Environment is ready (verified today)

- **`gh` CLI 2.92.0 on PATH, authenticated to github.com** (account `jeremN`, scopes `repo`+`workflow`, token in keyring). So the daemon process can run `gh pr create` with **ambient auth** — same philosophy as Slice 2 (we never handle tokens; the host tool does).
- Greenfield: no existing host/PR/GitHub code in the repo (the only `github.com` hits are go-module imports).

## Design space — questions for the brainstorm (with preliminary leanings, NOT decided)

1. **Host client — `gh` CLI vs REST API vs MCP.** Leaning: **`gh` CLI** (`gh pr create`). It's installed + already authenticated via keyring (ambient auth, mirrors Slice 2's "delegate creds to the tool" stance — the secure-by-default choice vs. us storing a token); zero new Go deps; and it matches the repo's established **stub-CLI testing** pattern (cf. `internal/executor/testdata/fake-claude*` — a `gh` stub on PATH that records args + returns a canned PR URL keeps tests offline). REST API would reintroduce the token handling we deliberately avoided; an MCP adds a moving part. Open: confirm gh-only is acceptable for the foundation.
2. **Trigger / shape — `cm pr <run>` (separate) vs `cm push --pr` (combined).** Leaning: **separate explicit `cm pr <run>`**, consistent with Slice 2 making push its own deliberate step. Sub-question: does `cm pr` require a prior `cm push`, or push-then-PR itself? (Probably: operate on an already-pushed branch; error clearly if the branch isn't on the remote yet — keeps each step single-purpose.)
3. **Host/repo detection.** The PR needs the host + `owner/repo` + head branch + base branch. `gh pr create` can take `-R <owner/repo>` (parse it from the remote URL — `git@github.com:owner/repo.git` / `https://github.com/owner/repo.git`) and `--head magister/<runID> --base <base>`. Leaning: parse `owner/repo` + host from the remote URL (the same URL `ResolveRemote` already returns), reuse Slice-2 argv hardening; non-github host → clear "unsupported host" error for now.
4. **PR metadata — title / body / base branch.** Leaning: `--title`/`--body` flags with sensible defaults (title from the flow `name` + short run id; body a small run summary — steps, the integrate commit); **base branch** defaults to the repo's default branch (or the run's `--base` ref?) with a `--base` override. Open: where the body comes from (flags vs. a generated summary vs. a flow field).
5. **Scope / non-goals.** Leaning: **GitHub-only via `gh`** for the foundation; GitLab/others later. Out: PR review automation, draft-vs-ready toggles beyond a `--draft` flag, updating an existing PR (open-or-noop is fine — `gh pr create` errors if one exists; surface it).

## Likely architecture (mirrors Slice 2's shape)

- `internal/host` (or extend `internal/workspace`): a `gh`-runner that shells `gh pr create` with an **injectable command name** so tests substitute a stub (`gh` stub on PATH, like the executor stubs). Plus a `ParseRemote(url) (host, owner, repo, error)` helper (hardened, like `ResolveRemote`).
- `internal/supervisor`: a `PR(ctx, runID, PROpts) (PRResult, error)` method parallel to `Push` — load run, validate (succeeded + external-repo + branch already pushed?), derive owner/repo from the remote, run `gh pr create`, return the PR URL. Typed `*PRError{Status,Msg}` (reuse the `PushError` pattern, or generalize).
- `internal/api`: `POST /v1/runs/{id}/pr` + `prResponse{url,...}`.
- `cmd/cm`: `cm pr <run> [--base <branch>] [--title <t>] [--body <b>] [--draft] [--remote <…>]`.
- **Testing:** a stub `gh` (records args, prints a canned `https://github.com/owner/repo/pull/N`) on PATH → assert the right `gh pr create` argv + the surfaced URL, fully offline. The happy path can run a real external-repo flow + push to a bare remote, then `cm pr` against the stub gh. Manual proof against a REAL throwaway GitHub repo at the end (gh is authed).

## Carried follow-ups still open (from Slice 2 — revisit if relevant)

- 30s `timeoutMiddleware` wraps `/push` (and would wrap `/pr`) — a slow network/`gh` call could 503; exempt these long-ops like SSE if it bites.
- `GetRun`-error-as-404 TODO (no store not-found sentinel).
- Pre-existing `internal/executor/gemini.go` gofmt (untouched; `gofmt -w` next time that pkg is edited).

## Workflow to follow next week

`superpowers:brainstorming` (resolve Q1–Q5 one at a time → spec, user-review gate) → `superpowers:writing-plans` (bite-sized TDD tasks) → `superpowers:subagent-driven-development` in a worktree (`git worktree add .worktrees/<name> -b <name>` — native EnterWorktree is broken here; fresh implementer per task → spec-compliance review → code-quality review → fix-loop; **Opus on the riskiest unit + the final holistic review**) → `superpowers:finishing-a-development-branch` (ff-merge, remove worktree, delete branch) → post-slice handoff.

**Conventions:** single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. `rtk` not installed — `go`/`git`/`gofmt`/`gh` directly. `cm` reads `$MAGISTER_ADDR` verbatim (needs `http://`).

## START-HERE pointer

Spec/plan from Slices 1–2: `…/specs/2026-06-11-external-repo-design.md` + `…/plans/2026-06-11-external-repo-slice1.md`; `…/specs/2026-06-12-external-repo-slice2-push-design.md` + `…/plans/2026-06-12-external-repo-slice2-push.md`. Retrospective: `…/handoffs/2026-06-12-post-external-repo-slice2-next-steps.md`. **This doc = the Slice-3 warm start.**
