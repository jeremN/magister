# Handoff — cross-repo / fork PRs: MERGED to main (2026-06-22)

**Start here next session.** The **cross-repo/fork-PR slice is DONE, MERGED, LIVE-PROVEN, and PUSHED** — `main` at **`642276a`**, pushed to **origin @`642276a`** 2026-06-22. The slice itself = 2 task commits (`4d4fc14` server, `e9d693a` client) off plan `74d073a`/spec `00b43ac`; final Opus whole-branch review Ready=Yes zero Crit/Imp. **The LIVE FORK SMOKE PASSED (2026-06-22) and CAUGHT + FIXED a real bug** — see the "Live smoke + idempotency fix" section below; the fix is `642276a` `fix(pr): cross-fork existing-PR check queries bare branch + filters head owner`. Both worktrees (`.worktrees/cross-repo-pr`, `.worktrees/fork-pr-idempotency`) removed and branches deleted. Full suite green, `vet`+`gofmt` clean.

## Live smoke + idempotency fix (2026-06-22 — PASSED, one bug found & fixed)

Ran the live cross-fork proof against a real GitHub fork: `gh repo fork octocat/Spoon-Knife` → jeremN/Spoon-Knife; cloned octocat/Spoon-Knife as the source; `cm run flows/external-repo.yaml --repo <clone> --base main` (mock, integrate = 2-parent merge over Spoon-Knife `d0dd1f6` + step outputs, succeeded); `cm push <run> --remote https://github.com/jeremN/Spoon-Knife.git` (delivered `magister/<run>` to the fork); `cm pr <run> --head-repo https://github.com/jeremN/Spoon-Knife.git` → **opened a REAL cross-fork PR `octocat/Spoon-Knife#40247`** — verified `isCrossRepository:true`, head `magister/<run>` owned by **jeremN** (the fork), base `main` on octocat/Spoon-Knife. Core feature: **PASSED**. (Cleaned up afterward: PR closed, fork branch deleted; the empty fork repo remains — jeremN's token lacks `delete_repo` scope, delete it via the GitHub UI or `gh auth refresh -h github.com -s delete_repo && gh repo delete jeremN/Spoon-Knife --yes`.)

**Bug the smoke caught (the offline fake-gh stub could NOT model it):** the second `cm pr` returned **502 instead of the idempotent 409**. Root cause, proven empirically: `gh pr list --head` matches only a **BARE** branch name — the `owner:branch` form returns `[]` — whereas `gh pr create --head` accepts `owner:branch`. The slice passed the composed `forkOwner:branch` head to `ExistingOpenPR`, so the existing-PR pre-check found nothing and fell through to `CreatePR`, which gh rejected as a dup → 502. **THE FIX (`642276a`):** `host.Runner.ExistingOpenPR` now takes `(ctx, owner, repo, branch, headOwner)` — queries `--head=<bareBranch> --json=url,headRepositoryOwner` and returns the first PR whose `headRepositoryOwner.login == headOwner` (the base owner for same-repo, the fork owner for cross-fork — also disambiguates a same-named branch on a *different* fork into the same upstream, which matters for custom `--as`). `prCore` passes the bare `branch` + the `headOwner` it already computes (same-repo path unchanged: `headOwner == owner`, and a same-repo PR's head owner == base owner, so the filter still matches). fake-gh stub emits `headRepositoryOwner.login` (new `FAKE_GH_EXISTING_PR_OWNER`, default `o`); tests updated + new `TestPRFromForkExistingReturns409` (asserts 409, the URL, AND that pr-list used the bare `--head=magister/r1`) + a host-level owner-mismatch case. Sonnet implementer-equivalent (controller-implemented) + sonnet review (Correctness APPROVED, MERGE; one Important = a stale stacked doc comment, FIXED via amend). **Fix LIVE-RE-VALIDATED:** rebuilt the daemon on the fix, resumed the same run, re-ran `cm pr --head-repo` → now returns the clean **409** `PR already exists for jeremN:magister/<run>: https://github.com/octocat/Spoon-Knife/pull/40247`. PROCESS LESSON: this is the textbook payoff of a live smoke — the two `gh` subcommands have ASYMMETRIC `--head` grammars (`create` accepts `owner:branch`, `list` needs the bare branch), discoverable only against the real tool; an echoing stub masks it.

### Original merge note (slice, pre-fix)

## What shipped

`cm pr` can now open a GitHub pull request **from a fork into the upstream repo** — the standard OSS-contribution flow — via a new optional `--head-repo <url-or-remote-name>` input (CLI flag + `head_repo` JSON field on `POST /v1/runs/{id}/pr`). The PR still **opens on the upstream/base repo** (origin or `--remote`); only the **head** changes from a bare `branch` to the cross-fork form `forkowner:branch`. Omitting `--head-repo` is **byte-for-byte** today's same-repo PR.

The full fork delivery flow is now: clone upstream as the source → `cm run --repo <upstream-clone>` → `cm push <run> --remote <fork-url>` (already fork-capable — pushes `magister/<run>` to your fork) → `cm pr <run> --head-repo <fork-url>` (opens the cross-fork PR on upstream). Spec `…/specs/2026-06-22-cross-repo-fork-prs-design.md`, plan `…/plans/2026-06-22-cross-repo-fork-prs.md`.

**Mechanism (the core insight):** a fork PR keeps the base repo and changes only the head. `host.CreatePR` already passes `opts.Head` verbatim to `gh --head=`, so the whole feature is **string composition** in `Supervisor.prCore` — no change to `internal/host`, no change to push, no schema/migration/dep/SSE event. Go 1.22 stdlib only.

- `4d4fc14` `feat(pr): cross-fork PRs via PROpts.HeadRepo (head owner:branch)` (Task 1, server) — `PROpts` gains `HeadRepo string`; `prCore` renames the bare ref `head`→`branch`, then composes `head` + tracks `headOwner`: when `HeadRepo != ""` it resolves the fork via the **same** `workspace.ResolveRemote` + `host.ParseRemote` as `--remote` (→ `forkOwner`, fork repo name discarded), sets `head = forkOwner + ":" + branch` and `headOwner = forkOwner`. The PR opens on the base `owner/repo` (unchanged); `ExistingOpenPR`/`CreatePR` get the composed head; the create-failure refinement now calls `BranchExists(headOwner, repo, branch)` — the **fork** owner with the **bare** branch — and a fork-specific 409 hint (`run cm push … --remote <fork> first`). `prRequest` gains `HeadRepo string json:"head_repo,omitempty"`; `handlePR` threads `HeadRepo: req.HeadRepo` into `PROpts` (and **`handleShip` deliberately does not** — ship stays same-repo-only). New tests: `TestPRFromForkComposesCrossForkHead`, `TestPRFromForkChecksBranchOnFork`, `TestPRFromForkRejectsNonGitHubHeadRepo` (supervisor), `TestPREndpointOpensCrossForkPR` (api).
- `e9d693a` `feat(cm): cm pr --head-repo opens a cross-fork PR` (Task 2, client) — `cm pr` gets a **dedicated** `case "--head-repo"` that sets `body["head_repo"]` (the generic `body[flag[2:]]` path would wrongly produce the hyphenated `head-repo`); usage string updated. Test `TestPRHeadRepoSendsHeadRepoJSONField` asserts the underscore key is present and the hyphen key absent.

## Key properties (Opus-verified)

- **Same-repo path byte-for-byte unchanged.** With `HeadRepo == ""` the fork block is skipped, so `head == branch` and `headOwner == owner`; every downstream call (`ExistingOpenPR`, `CreatePR`, `BranchExists`, the 409 text) is identical to the pre-change code. Opus diffed against `74d073a:internal/supervisor/pr.go` to confirm.
- **Argv-safety holds.** `forkOwner` is charset-guarded by `host.ParseRemote`/`safeSeg` (rejects leading `-`, restricts to `[A-Za-z0-9._-]`, no `:`/`=`/space); `branch` by `safePRRef`; `gh.go` builds `"--head=" + head` as a single argv token. The composed `owner:branch` cannot smuggle a flag or a second `:`. The composed head is deliberately **not** re-run through `safePRRef` (which rejects `:`) — each segment is validated independently.
- **`cm ship` sealed out of scope.** `ShipOpts`, `shipRequest`, and `handleShip` carry no `HeadRepo`; `Ship`'s `PROpts` leaves it `""` → same-repo-only.
- **`internal/host` and `cm push` untouched.** Only `dto.go`, `handlers.go` (one-field thread-through), `pr.go`, and `cmd/cm/main.go` (+ tests) changed.
- **Error mapping.** Bad/unresolvable/non-GitHub `--head-repo` → 400 via `ResolveRemote`/`ParseRemote`; no path can 500 or panic on a fork input.

## The 1 Minor (accepted, cosmetic, non-blocking)

`cmd/cm/main.go` (the `--head-repo` missing-value branch) uses `fmt.Fprintln(out, "usage: --head-repo requires a value")` while the sibling value-flags use `fmt.Fprintf(out, "usage: %s requires a value\n", flag)`. Functionally identical output, both exit 2. Pre-existing style; left for consistency with how the prior slice handled cosmetic Minors.

## Live smoke (DONE 2026-06-22 — PASSED, see the top "Live smoke + idempotency fix" section)

This was originally deferred (live proof needs a real GitHub fork + `gh` auth). It has since been **run and PASSED** against an `octocat/Spoon-Knife` fork — and it caught the cross-fork idempotency bug now fixed in `642276a`. Full details in the **"Live smoke + idempotency fix"** section near the top of this handoff.

## Out of scope (deliberate, carried)

- `cm ship` fork support — ship's shared `--remote` feeds both push-dest and PR-base; a fork makes those differ (push→fork, base→upstream), needing a conditional base-resolution change. Clean follow-up if wanted.
- Persisting the push destination on the run (the explicit `--head-repo` is stateless by design).
- Renamed forks whose repo name differs from upstream (the head assumes fork repo name == base repo name, the GitHub default; documented in the `PROpts` doc comment + spec).
- Non-GitHub hosts (`host.ParseRemote` is github.com-only).

## Open follow-ups (carried)

- **(deferred) `git push origin main`** — `main` (`e9d693a`) is local-only and carries BOTH this slice and the prior runtime-loglevel slice (origin is well behind; the loglevel handoff noted origin at `fa17bf5`). Push when ready.
- **(still unmerged) `multi-host` GitLab slice** — CODE-COMPLETE at `36eb9fa` (worktree `.worktrees/multi-host`), awaiting a live gitlab.com MR proof. See `2026-06-19-multi-host-gitlab-next-steps.md`. **DO NOT merge until that proof passes** (user has no gitlab.com account yet).
- **(delivery axis):** `cm ship --head-repo` (fork ship, above); cross-repo PRs is now DONE for `cm pr`.
- **(observability backlog):** OTel tracing (needs a dep → breaks stdlib-only, needs sign-off). Everything else on that axis is done (per-agent metric triad, `/metrics`, liveness/readiness, run-not-found sentinel, request/run-scoped logging, JSON log format, log level, engine debug/warn, runtime log-level endpoint).

## Process notes

- Subagent-driven: **sonnet** implementer for Task 1 (correctness-critical surgical edits to `prCore` across 5 files) + **haiku** implementer for Task 2 (single-file flag transcription); **sonnet** reviewers per task; **Opus** final whole-branch. Every per-task + final review returned zero Critical/Important.
- The plan's pre-reading of the actual source (`pr.go`, `pr_test.go`, `dto.go`, `handlePR`, the `handlers_test.go` PR test, the `cm pr` parser) paid off — every code block matched verbatim and the implementers transcribed cleanly. **Caught in plan self-review:** the `cm` test harness uses `dispatch(args, addr, out)` (server URL as a positional arg) + a `writeBody` helper, NOT `MAGISTER_ADDR` — the first draft of the Task 2 test had the wrong signature; fixed before dispatch.
- **Provenance fix:** the plan doc was initially an uncommitted file in the MAIN tree (branch was cut off the spec commit). Committed it as the branch's first commit (`74d073a`) so the FF merge stayed clean and the plan rode onto `main` with the code — matching the prior slices' spec+plan+commits shape.
- Commit hygiene held: single conventional subject, no body, no `Co-Authored-By`, never `--no-verify`. zsh/rtk in this env still trips on `printf` lines containing `()`/commas/quotes — keep ledger/banner writes plain. The supervisor/api/magisterd real-git e2e tests need the Bash sandbox disabled (`dangerouslyDisableSandbox: true`); `cmd/cm` is httptest-only and fine in-sandbox.
