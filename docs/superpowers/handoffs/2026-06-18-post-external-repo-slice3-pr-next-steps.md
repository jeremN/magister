# Handoff — external-repo Slice 3 (open a PR) COMPLETE; the full-delivery north star is DONE (2026-06-18)

**Pick up next session (fresh context).** **external-repo Slice 3 (open a PR)** is merged to `main` and complete. After a succeeded external-repo run is pushed (`cm push`), `cm pr <run>` opens a GitHub Pull Request on the pushed branch. **This was the third and final slice of the flow → real repo → PR north star — that north star is now COMPLETE.**

## Real-agent capstone — DONE (2026-06-18), the whole system run for real with ZERO new code

After Slice 3 the entire pipeline had only ever run with the `mock` agent. The capstone closed that gap: a **real `codex` agent** (ChatGPT-account auth) ran against a clone of `jeremN/site_test` and its work shipped as a **merged PR** — proving the B-slices (real CLIAgents), Slice C (git-worktree workspaces), and external-repo Slices 1–3 (delivery) compose with **no glue code**.

- Flow: a one-step `agent: codex`, `workspace: isolated` step (`flows`-style, prompt = "add a concise CONTRIBUTING.md, change nothing else"). A single isolated step is its own DAG leaf, so push/PR needed no `--step`.
- Live SSE showed **real `agent.tool` milestones mid-step**: `command_execution: …rg --files…` (codex checking for an existing file) then `file_change: add CONTRIBUTING.md`, both *before* `step.done` — the persist-then-publish streaming working with a real agent, not a mock.
- The engine's `commitIsolated` turned codex's real worktree edit into `step/contribute` (a genuine 24-line CONTRIBUTING.md over base `aa2a2f2`, nothing else touched) → `cm push` → `magister/<run>` on the real remote → `cm pr` → **PR #3**, then **merged to `site_test` `main`** (`092f7ff`, branch deleted).
- Also exercised earlier in the session: the **mock** manual proof (PR #2, since closed). Both mock and real paths now verified live.
- **Process note:** drive this with the `running-the-orchestrator` skill. The daemon must be launched **sandbox-disabled** (its `codex`/`git`/`gh` children need network + `~/.codex` + the gh keyring); `cm` reads `$MAGISTER_ADDR` verbatim (needs `http://`). codex auth check: `codex login status`. Daemon SIGTERM exits 143/144 (graceful, not a failure).

## State (verified)

- `main` at `b08cf39`, clean. `go test ./...` → all packages green; `go test -race ./...` → 264/264 across 16 packages; `go vet ./...` clean; `gofmt -l` clean for the slice's packages; `go 1.22`. **No new deps.**
- 8 feature/doc commits `6da2c4d..b08cf39` on top of the spec+plan commits (`b2f569a` spec, `f8af4ae` plan). Spec: `docs/superpowers/specs/2026-06-18-external-repo-slice3-pr-design.md`; plan: `docs/superpowers/plans/2026-06-18-external-repo-slice3-pr.md`.
- `gofmt -l .` still flags the pre-existing `internal/executor/gemini.go` (predates Slice 1, untouched).
- Built via subagent-driven-development in a worktree (`.worktrees/slice3-open-pr`, now removed): fresh implementer per task → spec+quality review → fix-loop; **Opus on Task 3 (Supervisor.PR) + the final whole-branch review** (verdict: Ready to merge = Yes, zero Critical/Important).

## What was built (per task → commit)

1. **`host.ParseRemote`** (`6da2c4d`): pure parser — `host,owner,repo` from https / scp-like `git@host:o/r` / `ssh://` forms; strips `.git`; `safeSeg` charset-guards owner/repo; non-`github.com` → error. `internal/host/gh.go`.
2. **`host.Runner` + `fake-gh` stub** (`2957c1b`): injectable `Bin` (default `"gh"`); `ExistingOpenPR` (`gh pr list --json=url`), `CreatePR` (`gh pr create`, parses the printed URL), `BranchExists` (`gh api …/branches/…`). Single-token `--flag=value` argv, `exec.CommandContext` no shell, `cmd.Dir=os.TempDir()` (neutral), separate stdout/stderr. Env-driven `internal/host/testdata/fake-gh` (mode 0755) reused offline by the supervisor + api tests.
3. **`Supervisor.PR`** (`a9c405e`): `PR(ctx,runID,PROpts) (PRResult,error)` parallel to `Push` — validate (external-repo + succeeded), head = `--as`/`magister/<runID>`, `owner/repo = host.ParseRemote(workspace.ResolveRemote(...))`, generated title/body from store data, `CreatePR`. `*PRError{Status,Msg}`. Adds `Supervisor.Host *host.Runner` + `hostRunner()` accessor (defaults to `host.New()` — **so the daemon needs no wiring**). Reuses `pickResultStep`/`stepBranch`. **Added beyond the plan (closing a Task-2 review finding + the spec's guarantee): `safePRRef` guard on head/base (== `workspace.safeRef`) → 400** on a user-supplied `--as`/`--base` that could smuggle a flag or traverse the `gh api …/branches/<head>` path.
4. **PR conflicts** (`9c1b9cf`, + test-hardening `98fca42`): existing open PR → **409 with the existing URL** (`ExistingOpenPR` pre-check); on `CreatePR` failure → `BranchExists`? no → **409 "run `cm push <run>` first"**; yes → **502** with gh's output.
5. **`POST /v1/runs/{id}/pr`** (`4bf4cbd`): `handlePR` decodes a JSON body `{remote,as,step,base,title,body,draft}`, maps `*PRError` via `errors.As` (else 500), returns `prResponse{url,repo,head,base,draft}`. Mirrors `handlePush`.
6. **`cm pr`** (`ece1017`): `cm pr <run> [--remote --as --step --base --title --body --draft]` → the endpoint; prints `opened <url>`; surfaces the server error (incl. the 409 URL) on non-200.
7. **Docs** (`b08cf39`): run-skill `cm pr` note.

## Key design facts to remember

- **PR is post-run, store-driven, and touches NO scratch clone — unlike push.** It reads the run from the store, derives `owner/repo` from a read-only `git remote get-url` on the source, and shells `gh`. No `engine.BasePath`, no git exec, no scratch dependency (so push's scratch-GC ordering follow-up does not apply to PR). Verified by the final review.
- **The PR head branch is the push DESTINATION (`magister/<runID>` or `--as`), NOT the local step branch (`step/integrate`).** Push renamed it on the remote. The terminal step (`pickResultStep`) is read ONLY to build the body summary.
- **`gh pr create --repo … --head … --base …` is git-context-free** (empirically probed: from a non-git dir it goes straight to the API, no "not a git repository"). That is *why* PR needs no scratch clone. `cmd.Dir=os.TempDir()` keeps any ambient repo's gh config from leaking in.
- **Base branch:** `--base` empty ⇒ omitted ⇒ gh uses the repo's default branch. (This sidesteps that `rs.Base` is a pinned SHA, which can't be a PR base.) `rs.Base` is never used as the PR base.
- **Credentials = ambient `gh`, zero token handling** (same posture as Slice 2's `git push`). Auth/gh failure → 502 with gh's output.
- **Happy path = 2 gh calls** (`pr list` pre-check, then `pr create`); `BranchExists` fires only to diagnose a create failure into the "push first" message.
- **Status mapping:** 404 unknown run; 400 not-external / unsupported host / unsafe ref / unknown-or-ambiguous `--step` / bad remote; 409 not-succeeded / existing PR / branch-not-pushed; 500 corrupt stored flow; 502 gh create failed.

## Carried follow-ups (non-blocking)

- **MANUAL PROOF — DONE (2026-06-18), PASSED.** Ran end-to-end against a real throwaway GitHub repo, `jeremN/site_test` (`gh` authed as `jeremN`): cloned it as the source → `cm run flows/external-repo.yaml --repo <clone> --base HEAD` → `integrate` is a 2-parent merge over site_test's real HEAD `aa2a2f2`, tree = cloned site files + both step outputs, `status: succeeded` → `cm push <run>` delivered `magister/<run>` (commit `24fd469`) to the real remote, **source repo untouched** → `cm pr <run>` opened a REAL PR (`https://github.com/jeremN/site_test/pull/2`, head `magister/<run>`, base `main` = repo default, generated title `magister: external-repo (<suffix>)` + run-summary body) → `cm pr <run>` AGAIN returned **409 with the existing PR URL** (idempotency). `ParseRemote` correctly derived `jeremN/site_test` from the source origin. The whole flow→real-repo→PR north star is now verified live, not just unit-tested. (PR #2 is a test artifact left on the sandbox repo.)
- **`ParseRemote` rejects a github origin with an explicit `:port`** (e.g. `ssh://git@github.com:22/o/r`) as "unsupported host" — fails closed (400, never a wrong-repo PR). One-liner fix = strip a `:port` suffix before the `host != "github.com"` check. (Final-review Minor; deferred to avoid scope creep post-approval.)
- **30s `timeoutMiddleware` wraps `POST .../pr`** (same as `/push`): a slow real `gh` could 503. If real-remote use bites, exempt these long-ops like SSE is exempted. Pre-existing infra.
- **Pre-existing flaky `TestMockHonorsContextCancel`** (`internal/executor`, NOT Slice 3): a context-cancellation timing test that can miss its deadline under a contended `go test ./...` and fail once, then pass 20/20 on re-run. Unrelated to this slice; worth a `-count`/timing hardening someday.
- **Minors logged during the per-task reviews** (none blocking): `ExistingOpenPR` returns `prs[0]` (single-base assumption, matches the singular spec — add a TODO); a few test-coverage niceties (flag-like repo / empty-segment table cases; `--base`-absent / `--draft` argv-passthrough assertions; assert `base`/`draft` in the endpoint response); `shortSHA` vs `defaultPRTitle` two ad-hoc truncations.

## What's next (the north star is done — pick a new direction)

The flow → real repo → PR north star is COMPLETE. Natural follow-on threads, none started:
- **Run the manual proof** (above) — the one remaining verification.
- **Other hosts:** GitLab/Bitbucket `ParseRemote` + a host abstraction (today: GitHub-only, hard 400 on other hosts).
- **Cross-repo / fork PRs:** a `owner:branch` head (today the foundation assumes the pushed branch is on the same repo the PR targets).
- **Scratch GC:** reclaim per-run scratch clones (push depends on them persisting; PR does not). Order GC after push.
- **One-shot delivery ergonomics:** a combined `cm ship <run>` (push + pr) if the two-step flow proves clunky in practice — deliberately kept separate for now.

## Process notes that held this slice

- **`gh pr create` with explicit `--repo/--head/--base` is git-context-free** — probe an external tool's real behavior before designing around an assumption (saved us from a needless scratch-clone dependency).
- **A clean fast-forward merge can still surface a flaky test on the merged result** — verify, then reproduce-or-refute (`git diff --stat <range> -- <pkg>` to prove the range didn't touch it + `-count=20`) rather than assuming guilt or innocence.
- The per-task background reviews caught a real argv/path-traversal hardening gap (Task-2 finding → `safePRRef` added in Task 3); keep the `--`/single-token/charset-guard discipline for any new tool-exec.
- **Commit conventions:** single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. `rtk` not installed — run `go`/`git`/`gofmt`/`gh` directly. `cm` reads `$MAGISTER_ADDR` **verbatim as the base URL** — needs the `http://` scheme.
