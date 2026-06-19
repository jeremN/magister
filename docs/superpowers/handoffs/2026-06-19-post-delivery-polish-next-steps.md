# Handoff — delivery-polish slice (`cm ship` + cleanups) COMPLETE (2026-06-19)

**Pick up next session (fresh context).** The **delivery-polish slice** is merged to `main` and complete. `cm ship <run>` now pushes a succeeded external-repo run's result AND opens its PR in **one idempotent command**, plus three carried cleanups landed. The flow → real-repo → PR north star (Slices 1–3) was already done; this slice is the ergonomics layer on top. **No slice is in flight.**

## State (verified)

- `main` at `316beb5`, working tree clean. **Local-only — `main` has NOT been pushed to `origin` (github.com/jeremN/magister) yet** (the previous push left `origin/main` at `59900f2`; this slice's 7 commits + the merge are local). Push when ready: `git push origin main`.
- `go test -race ./...` → **275 passed across 16 packages** (264 baseline + 11 new). `go vet ./...` clean. **`gofmt -l .` is now repo-wide clean** (this slice fixed the one long-standing dirty file, `gemini.go`). `go 1.22`. **No new deps** (stdlib-only held).
- 7 feature commits `e4f78d3`..`316beb5` on top of the plan commit `59900f2`. Spec: `docs/superpowers/specs/2026-06-18-delivery-polish-design.md`; plan: `docs/superpowers/plans/2026-06-18-delivery-polish.md`.
- Built via brainstorm → writing-plans → subagent-driven-development in a worktree (`.worktrees/delivery-polish`, now removed): fresh implementer per task (sonnet for the cm-ship chain Tasks 1–4, haiku for the cleanups Tasks 5–7) → spec+quality review each → **Opus final whole-branch review: Ready-to-merge = Yes, zero Critical/Important, all minors accept-as-is**.

## What was built (per task → commit)

1. **`prCore` refactor** (`e4f78d3`): `PR`'s body extracted into `prCore(ctx, runID, PROpts) (PRResult, existed bool, err)` — existing-open-PR → `(res, true, nil)`, newly-created → `(res, false, nil)`, failure → `(PRResult{}, false, *PRError)`. The thin `PR` wrapper converts `existed → 409` with the existing URL, so **`cm pr` is byte-for-byte unchanged** (its tests are the regression guard). `internal/supervisor/pr.go`.
2. **`Supervisor.Ship`** (`9483018`): `internal/supervisor/ship.go`. `ShipOpts` = the `PushOpts ∪ PROpts` union; `ShipResult{Push, PR, PRExisted}`. `Ship` calls `Push` FIRST (it needs the scratch clone), short-circuits on `*PushError` (no PR attempted), then `prCore`; surfaces `PRExisted` from `prCore`'s bool. Shared `Remote/As/Step` feed both halves (push-dest ≡ PR-head can't disagree); `Force`→push-only; `Base/Title/Body/Draft`→pr-only.
3. **`POST /v1/runs/{id}/ship`** (`c0f3ae8`): `handleShip` decodes `shipRequest`, calls `Ship`, maps `*PushError` **THEN** `*PRError` via `errors.As` (else 500), returns `shipResponse{pushed, pr, pr_existed}`. `internal/api/{dto,handlers,router}.go`.
4. **`cm ship`** (`fddc90b`): `cm ship <run> [--remote --as --step --base --title --body --draft --force]` — thin client; builds the JSON body, POSTs, prints `pushed <src> → <dest> on <remote>` then `opened <url>` (or `exists <url>` when `pr_existed`). `cmd/cm/main.go`.
5. **`ParseRemote` `:port` fix** (`e6d9463`): strips an explicit `:port` in the `://` branch BEFORE the github host check (`ssh://git@github.com:22/o/r` now resolves; non-github + port still rejected). The scp-form branch is untouched. `internal/host/gh.go`. (Closes a Slice-3 carried follow-up.)
6. **120s delivery-route timeout** (`8f78d7e`): `timeoutMiddleware` gives `/push`, `/pr`, `/ship` a 120s bound (was the blanket 30s); SSE (`/events`) stays fully exempt; everything else keeps 30s. `internal/api/middleware.go`. (Closes a Slice-2/3 carried follow-up about a slow real network push/pr 503-ing.)
7. **`gemini.go` gofmt** (`316beb5`): pure whitespace-only reformat of the one long-standing gofmt-dirty file → `gofmt -l .` repo-wide clean. (Closes a follow-up carried since Slice 1.)

## Key design facts to remember

- **Idempotency is structural, not a flag.** It falls out of `prCore` returning `(result, existed, err)` and giving `PR` and `Ship` different *policies* over the same core: `PR` treats `existed` as a 409, `Ship` treats it as success. One mechanism, two callers, zero duplicated logic — which is *why* `cm pr` is provably unchanged.
- **`Ship` runs push-then-pr; a PR-phase failure leaves the branch already pushed.** That's intended: the recovery path is just re-running `cm ship` (the push becomes "everything up to date" and the existing PR returns `exists`). The HTTP response gives no "push already landed" signal — acceptable because re-run converges.
- **The fully-happy path (push AND pr both succeed) is NOT offline-unit-testable.** A single shared remote can't be both an offline-pushable local bare repo AND a github.com-parseable URL, and `Ship` uses one remote for both halves. So it's a **manual proof**; offline tests cover every *separable* mechanism (push-fails-skips-pr, push-ok-then-pr-error with the verified push side-effect, prCore idempotency, the PR 409 wrapper, cm output formatting).
- **`:port` strip introduces no host-spoofing** (Opus-verified): `github.com:evil` → host `github.com` (bogus port discarded) but owner/repo still come from the `safeSeg`-guarded PATH, so a spoof can't redirect the PR. `github.com.evil.com`, `evil.com:github.com`, `gitlab.com:22` all still correctly rejected.
- **Status mapping for `/ship`:** inherits push's then pr's. 404 unknown run; 400 not-external / unsupported-host / unsafe-ref / ambiguous-step / bad-remote; 409 not-succeeded / branch-not-pushed (push half) — note an existing PR is NOT a 409 here (it's success); 502 git-push or gh failed; 500 corrupt flow.

## Manual proof — DONE (2026-06-19), PASSED

Ran live against the throwaway repo `jeremN/site_test` (`gh` authed `jeremN`, daemon launched **sandbox-disabled** — git/gh children need network + gh keyring), driven by the `running-the-orchestrator` skill:

1. Cloned `site_test` (origin = github.com/jeremN/site_test, HEAD `092f7ff`) as the source → `cm run flows/external-repo.yaml --repo <clone> --base HEAD` → `integrate` = a real 2-parent merge over site_test's HEAD (tree = cloned files + both mock step outputs), `status: succeeded`.
2. **`cm ship <run> --title "…"`** → printed `pushed step/integrate → magister/<run> on https://github.com/jeremN/site_test.git` then `opened https://github.com/jeremN/site_test/pull/4` — push AND PR in one command. The pushed `magister/<run>` branch was commit `5ce1254` with **two parents** (a real merge).
3. **`cm ship <run>` again** → `pushed …` then `exists …/pull/4`, exit 0 — idempotent: existing PR is success, NOT a 409, and **no duplicate PR** was created (`gh pr list` showed exactly one magister PR).
4. PR #4: base `main` (repo default), head `magister/<run>`, not draft, my title. **Source `main` UNTOUCHED at `092f7ff`.**
5. Cleaned up: closed PR #4, deleted its remote branch, stopped the daemon (SIGTERM exit 144 = graceful), removed the temp dir.

## Carried follow-ups (non-blocking, none in flight)

- **Push `main` to origin** — the slice is merged locally; `origin/main` is still at `59900f2`. `git push origin main` when ready.
- **Pre-existing flaky `TestMockHonorsContextCancel`** (`internal/executor`, NOT a slice regression) — context-cancel timing test, fails ~once under a contended `go test ./...`, passes 20/20 on re-run. Worth a `-count`/timing hardening someday. (Did not surface during this slice's runs.)
- **`scratch-GC` after push** — per-run scratch clones still accumulate; push (and now ship) depend on them persisting, PR does not. A GC pass must run *after* push/ship.
- **`GetRun` storage errors masquerade as 404** (documented TODO, no store not-found sentinel) — carried verbatim in `prCore`/`Push`.
- **Logged Minors (all triaged accept-as-is by the final review):** `ship_test.go` inner `sha` shadow; a test's `json.Decode` error unchecked (pre-existing pattern); `handleShip` lacks a doc comment (peers do too); `cm ship` pushed-line test asserts substrings not the exact line; `json.Marshal(body)` error swallowed (can't fail for a `map[string]any` of strings/bools); `middleware_test` bounds loose (60s/31s, from the plan skeleton — can't mask the hardcoded 120s).

## What's next (the north star + ship ergonomics are done — pick a new direction)

No slice is in flight. Natural follow-on threads, none started:
- **Other hosts:** GitLab/Bitbucket `ParseRemote` + a host abstraction (today: GitHub-only, hard 400 on other hosts).
- **Cross-repo / fork PRs:** an `owner:branch` head (today the foundation assumes the pushed branch is on the same repo the PR targets).
- **Scratch GC:** reclaim per-run scratch clones (order after push/ship).
- **Productionize:** API auth (the dead `Server.BearerToken` footgun), observability/metrics, the still-open small follow-ups above.

## Process notes that held this slice

- **Idempotency via a return-value split beats a pre-check flag** — extracting `prCore` to report created-vs-existing let two callers apply different policies with zero duplicated logic, and made "`cm pr` unchanged" *provable* rather than asserted.
- **Name the offline-untestable path honestly and cover its pieces** — the shared-remote impossibility is real; don't fake it with a mock that proves nothing. Unit-test every separable mechanism and gate "done" on a live manual proof.
- **A subagent can write its report to the wrong dir** — Task 6's implementer wrote to a literal `sdd/` at the worktree root instead of the git-path `sdd/`. It was untracked and not committed (the task commit only `git add`ed its source files), but the controller had to relocate the report and `rmdir` the stray dir to keep `git status` clean. Pass implementers the exact absolute report path (with the `.git/` segment) and verify the commit scope.
- **A pure gofmt task warrants a controller-side check, not a full reviewer subagent** — prove it's whitespace-only with `git diff --ignore-all-space <base> <head> -- <file>` (empty == yes) + `gofmt -l` clean + commit scope, scaling the review to a zero-logic diff.
- **Commit conventions:** single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. `rtk` is not reliably present — run `go`/`git`/`gofmt`/`gh` directly. `cm` reads `$MAGISTER_ADDR` **verbatim** as the base URL — needs the `http://` scheme. Launch the daemon **sandbox-disabled** for any network/gh proof; SIGTERM exit 143/144 = graceful.
