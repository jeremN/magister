# Delivery — cross-repo / fork pull requests (`cm pr --head-repo`)

## Summary

Let `cm pr` open a pull request **from a fork into an upstream repo** — the standard open-source contribution flow — by adding an optional `--head-repo` input that supplies the fork owner so the PR head becomes `forkowner:branch`. Today `cm pr` can only open a same-repo PR (head is a plain branch on the repo the PR targets). With this slice, an operator clones upstream, runs a flow, `cm push`es the result branch to their own fork, and `cm pr --head-repo <fork>` opens the PR on upstream with a cross-fork head.

The base repo (where the PR opens) is unchanged — it stays the run's source origin (or `--remote`). Only the head changes. This touches three files (`internal/api/dto.go`, `internal/supervisor/pr.go`, `cmd/cm/main.go`) plus tests. **No change to `internal/host`** (its `CreatePR` already passes the head verbatim to `gh --head=`), **no change to push**, and **no new dependency, package, migration, schema, or SSE event.** Go 1.22, stdlib only.

## Motivation

The external-repo north star delivers a flow's result to a real GitHub repo: `cm push` → `cm pr` → `cm ship`. But `cm pr` assumes you can push the branch to, and open the PR on, the **same** repo — i.e. you have write access to the target. The dominant real-world contribution pattern is the opposite: you do **not** have write access to upstream, so you push to **your fork** and open a PR from `yourfork:branch` into `upstream:main`. GitHub models this as a cross-fork head (`gh pr create --repo UPSTREAM --head FORKOWNER:branch`). This slice closes that gap, making magister usable for the common "contribute to someone else's repo" workflow, not just "deliver to a repo I own".

The push side already supports it: `cm push <run> --remote <fork-url>` pushes `magister/<runID>` to any remote, including a fork. The only missing piece is telling `cm pr` that the head lives on a fork.

## Design

### Surface

A new optional input names the fork:

- CLI: `cm pr <run> [existing flags…] [--head-repo <url-or-remote-name>]`
- API: a new optional JSON field `head_repo` on `POST /v1/runs/{id}/pr`.

When `--head-repo` is **omitted**, behavior is byte-for-byte today's same-repo PR. When **set**, the PR is opened on the base repo (unchanged) with a cross-fork head.

`--head-repo` accepts the same forms as `--remote` and is resolved the same way (`workspace.ResolveRemote(rs.Repo, value)`): a git URL is passed through; a bare name is resolved to a URL via `git remote get-url <name>` on the source repo (read-only). The resolved URL is parsed by `host.ParseRemote` to extract the **fork owner**. This is symmetric with `cm push`: the user passes the same fork URL to both (`cm push --remote <fork>` then `cm pr --head-repo <fork>`). Only the owner is used — the fork's repo name is ignored, because a GitHub fork keeps the base repo's name and `gh --head OWNER:branch` resolves the fork by owner against the base repo.

### `Supervisor.prCore` changes

`PROpts` gains `HeadRepo string`. In `prCore` (the shared core of `PR` and `Ship`):

1. **Base repo (unchanged):** `remoteURL = workspace.ResolveRemote(rs.Repo, opts.Remote)` → `host.ParseRemote` → `baseOwner, baseRepo`. This is the repo gh opens the PR on (`--repo=baseOwner/baseRepo`) — the upstream.
2. **Branch (unchanged):** `branch = opts.As` or `"magister/" + runID`, validated by `safePRRef` (which already rejects `:`, a leading `-`, `..`, etc.). Keep `branch` as a distinct variable from the composed head.
3. **Head composition (new):**
   - If `opts.HeadRepo == ""`: `head = branch` (today's same-repo head).
   - Else: `forkURL = workspace.ResolveRemote(rs.Repo, opts.HeadRepo)`; `_, forkOwner, _, err = host.ParseRemote(forkURL)` (the fork repo name is discarded); on error → `400`. `head = forkOwner + ":" + branch`. `forkOwner` is charset-guarded by `ParseRemote`'s `safeSeg` and `branch` by `safePRRef`, so the resulting single-`:` head cannot smuggle a flag into the gh argv.
4. **Existence check + create (unchanged calls, head may now contain `owner:`):** `runner.ExistingOpenPR(baseOwner, baseRepo, head)`; `runner.CreatePR(host.CreateOpts{Owner: baseOwner, Repo: baseRepo, Head: head, Base: opts.Base, …})`.
5. **`BranchExists` refinement (fixed for forks):** today, a `CreatePR` failure is refined by checking `BranchExists(baseOwner, baseRepo, head)` to produce a helpful "run `cm push` first" 409. For a fork PR the branch lives on the **fork**, not the base, so:
   - `HeadRepo == ""`: `BranchExists(baseOwner, baseRepo, branch)` (today; `head == branch` here).
   - `HeadRepo != ""`: `BranchExists(forkOwner, baseRepo, branch)` — the **bare** `branch` on the fork owner (fork repo name == base repo name, the GitHub default). The 409 message hints the fork: ``branch %q not on fork; run `cm push %s --remote <fork>` first``.

`PRResult.Head` naturally carries `forkOwner:branch`, so the API/`cm` response reports the cross-fork head.

`PR` (strict, existing→409) and `Ship` (idempotent) both call `prCore`, so `cm pr` gets fork support and `cm ship` is **unchanged** (it simply never sets `HeadRepo` — same-repo only, as scoped).

### `cm pr` client

Add `--head-repo <value>` to the `cm pr` argument parser and usage string, marshaled into the existing PR JSON body as `head_repo`. No new client machinery.

### API handler + DTO

`internal/api/dto.go`: `prRequest` gains `HeadRepo string \`json:"head_repo,omitempty"\``. The `POST /v1/runs/{id}/pr` handler maps it into `PROpts.HeadRepo` alongside the existing fields.

### Error handling

- A `--head-repo` that fails to resolve (no such remote / not absolute source) or parse (malformed URL) → `400` via `ResolveRemote`/`ParseRemote`.
- A non-GitHub fork URL → `400` (`ParseRemote` is github.com-only, unchanged).
- All other statuses (404 unknown run, 400 not-external / unsafe ref / bad step, 409 not-succeeded / existing PR / branch-not-pushed, 502 gh failed) map exactly as today.

## Testing

- **`internal/supervisor` (`pr_test.go`):** with the existing `testdata/fake-gh` stub (env-driven, offline):
  - A fork PR — `PROpts{HeadRepo: "<fork-url>"}` — drives the stub with `--repo=UPSTREAM/repo` and `--head=forkowner:branch` (assert the stub received the composed `owner:branch` head and the upstream `--repo`).
  - Create-failure path with `HeadRepo` set points `BranchExists` at the **fork owner** with the bare branch (assert the `gh api repos/forkowner/repo/branches/branch` call, and the 409 "run `cm push … --remote <fork>` first" message).
  - The same-repo path (no `HeadRepo`) is unchanged — a regression assertion that `--head` is the bare branch and `--repo`/`BranchExists` target the base.
  - A non-GitHub or unresolvable `--head-repo` → `*PRError` with `Status == 400`.
- **`internal/api`:** a `POST /v1/runs/{id}/pr` body with `head_repo` threads into `PROpts.HeadRepo` (assert via a stubbed supervisor path or the fake-gh end-to-end handler test, mirroring the existing PR handler test); a bad `head_repo` surfaces 400.
- **`cmd/cm` (`main_test.go`):** `cm pr <run> --head-repo <url>` puts `head_repo` in the POST JSON body (mirror `TestPRSendsJSONBody`).
- Full `go test -race ./...` green; `go vet` and `gofmt -l` clean. (The supervisor/api/magisterd packages contain real-git/`gh` e2e tests that require the sandbox disabled — unrelated to this change.)

### Live smoke (manual proof)

Against a throwaway GitHub fork relationship (`gh` authed): fork an upstream repo; clone upstream as the source; `cm run --repo <upstream-clone> --base main`; `cm push <run> --remote <fork-url>` (delivers `magister/<run>` to the fork); `cm pr <run> --head-repo <fork-url>` opens a **real cross-fork PR** on upstream with head `forkowner:magister/<run>`, base upstream default. A second `cm pr` → 409 with the existing URL. A `cm pr` against a not-yet-pushed fork branch → 409 "run `cm push … --remote <fork>` first".

## Out of scope

- Persisting the push destination on the run (the explicit `--head-repo` is stateless by design).
- `cm ship` fork support (ship's shared `--remote` feeds both push-dest and PR-base; a fork makes those differ, needing a conditional base-resolution change — a deliberate follow-up).
- Renamed forks whose repo name differs from upstream (the head assumes the fork repo name == the base repo name, the GitHub default).
- Non-GitHub hosts (`host.ParseRemote` is already github.com-only).
- Any change to `cm push` (already fork-capable) or `internal/host`.

## Global constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no new package; no migration; no schema change; no new SSE event kind.
- `--head-repo` omitted ⇒ behavior is **byte-for-byte** today's same-repo PR.
- Read-only on the source repo (`ResolveRemote` only runs `git remote get-url`).
- Reuse the existing argv-safety guards: `forkOwner` via `host.ParseRemote`/`safeSeg`, `branch` via `safePRRef`; compose the head as a single `owner:branch` token. Auth stays ambient `gh` (zero token handling).
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.
