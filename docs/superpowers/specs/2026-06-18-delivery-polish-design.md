# Delivery polish — `cm ship` + carried cleanups (design)

**Date:** 2026-06-18
**Status:** approved (design)
**Predecessors:** external-repo Slices 1–3 (provision / push / pr) are merged; the full-delivery north star is complete and verified live (mock PR #2, real codex PR #3 merged on `jeremN/site_test`).

## Goal

One sentence: add `cm ship <run>` — a single command that pushes a succeeded external-repo run's result **and** opens its PR (push + pr in one, idempotent) — and clear three small carried follow-ups along the way.

## Scope

**In:**
- `cm ship <run> [flags]` → `POST /v1/runs/{id}/ship` → `Supervisor.Ship` composing the existing `Push` then the PR path.
- Idempotent ship: an already-existing PR is **success** (returns the existing URL), not a 409.
- `ParseRemote` `:port` fix (a github origin with an explicit port resolves instead of failing closed).
- push/pr/ship get a **longer (120s) request-timeout bound** instead of the global 30s — not a blanket exemption.
- `internal/executor/gemini.go` gofmt (clears the pre-existing dirty-on-main file).

**Out (deferred to their own follow-ups):**
- **scratch-GC** (reclaiming `<runsRoot>/<runID>/` after delivery) — a real lifecycle subsystem; its own slice.
- **`GetRun`→404 sentinel** — needs a typed `store.ErrNotFound`; a genuine storage error still reads as 404 today (documented TODO in `Push`/`PR`, unchanged here).
- Other hosts (GitLab/Bitbucket), cross-repo/fork PRs, productionization (API auth) — separate threads.

## Architecture

`cm ship` is a **server-side composition**, honoring `cmd/cm/main.go`'s stated contract ("the thin CLI client for magisterd: pure HTTP calls, no orchestration logic"). The CLI sends one request; the daemon orchestrates push-then-pr.

```
cm ship <run> [--remote --as --step --base --title --body --draft --force]
   → POST /v1/runs/{id}/ship   (JSON body, mirroring /pr)
   → Supervisor.Ship: Push(...) then prCore(...)
   → ShipResult{Push, PR, PRExisted}
```

Rejected alternatives: **client-side** `cm ship` calling `/push` then `/pr` (puts orchestration + 409-string-parsing into the thin client — violates its contract); **a `cm push --pr` flag** (conflates two verbs, less discoverable).

**Flag union, single source of truth.** Ship accepts the union of push's and pr's flags. The shared ones (`--remote`, `--as`, `--step`) feed *both* operations from one value, so the push destination and the PR head branch can never disagree (a real footgun when running the two commands by hand). `--force` is push-only; `--base/--title/--body/--draft` are pr-only.

## Components

### 1. `Supervisor.prCore` (refactor — `internal/supervisor/pr.go`)

Extract the body of the current `PR` into a helper that reports whether the PR was newly created or already existed, so `cm pr` can stay strict (409) while `cm ship` is idempotent — without either string-parsing an error message.

```go
// prCore does the PR work and reports whether an open PR already existed.
// On an already-existing PR it returns (PRResult{URL:…}, true, nil); on a
// newly-created PR (PRResult{URL:…}, false, nil); on failure (PRResult{}, false, *PRError).
func (s *Supervisor) prCore(ctx context.Context, runID core.RunID, opts PROpts) (PRResult, bool, error)
```

`prCore` holds everything `PR` does today: validation (external-repo, succeeded), head/base ref guards, `ResolveRemote`, `ParseRemote`, flow parse, `pickResultStep`, title/body generation, the `ExistingOpenPR` pre-check, and `CreatePR` (with the `BranchExists`-diagnose-on-failure). The only change is the existing-PR branch returns `(PRResult{URL: existingURL, Repo, Head: head, Base, Draft}, true, nil)` instead of a 409 error.

`PR` becomes a thin wrapper preserving today's behavior exactly:

```go
func (s *Supervisor) PR(ctx context.Context, runID core.RunID, opts PROpts) (PRResult, error) {
    res, existed, err := s.prCore(ctx, runID, opts)
    if err != nil {
        return PRResult{}, err
    }
    if existed {
        return PRResult{}, prErr(http.StatusConflict, "PR already exists for %s: %s", res.Head, res.URL)
    }
    return res, nil
}
```

### 2. `Supervisor.Ship` (new — `internal/supervisor/ship.go`)

```go
// ShipOpts is the union of PushOpts and PROpts (shared fields feed both).
type ShipOpts struct {
    Remote, As, Step, Base, Title, Body string
    Draft, Force                        bool
}

// ShipResult bundles the push outcome, the PR outcome, and whether the PR
// already existed (idempotent re-run).
type ShipResult struct {
    Push      PushResult
    PR        PRResult
    PRExisted bool
}

// Ship pushes a succeeded external-repo run's result branch, then ensures a PR
// exists for it. Push runs first (it needs the scratch clone). An already-open
// PR is success (PRExisted=true), so ship is safe to re-run and converges.
// Errors are the underlying *PushError or *PRError (handler maps via errors.As).
func (s *Supervisor) Ship(ctx context.Context, runID core.RunID, opts ShipOpts) (ShipResult, error) {
    pushRes, err := s.Push(ctx, runID, PushOpts{
        Remote: opts.Remote, As: opts.As, Step: opts.Step, Force: opts.Force,
    })
    if err != nil {
        return ShipResult{}, err // *PushError; no PR attempted
    }
    prRes, existed, err := s.prCore(ctx, runID, PROpts{
        Remote: opts.Remote, As: opts.As, Step: opts.Step, Base: opts.Base,
        Title: opts.Title, Body: opts.Body, Draft: opts.Draft,
    })
    if err != nil {
        return ShipResult{}, err // *PRError; push already happened
    }
    return ShipResult{Push: pushRes, PR: prRes, PRExisted: existed}, nil
}
```

Ship composes the two public/refactored methods rather than re-implementing validation; each still owns its own checks (a couple of extra cheap loopback `GetRun` reads — acceptable).

### 3. API (`internal/api/{router,handlers,dto}.go`)

- Route: `v1.HandleFunc("POST /v1/runs/{id}/ship", s.handleShip)`.
- `shipRequest` = `prRequest` plus `Force bool json:"force,omitempty"`.
- `shipResponse` nests the two outcomes:
  ```go
  type shipResponse struct {
      Pushed    pushResponse `json:"pushed"`
      PR        prResponse   `json:"pr"`
      PRExisted bool         `json:"pr_existed"`
  }
  ```
- `handleShip` mirrors `handlePR` (decode JSON body) → `s.Sup.Ship(...)` → on error map **both** `*supervisor.PushError` and `*supervisor.PRError` via `errors.As` (else 500) → on success write `shipResponse` (reusing the `pushResponse`/`prResponse` field mapping).

### 4. `cm ship` (`cmd/cm/main.go`)

- `dispatch` gains a `case "ship": return c.ship(args[1:], out)` and the usage line lists `ship`.
- `c.ship` parses the value flags (`--remote --as --step --base --title --body`) and boolean flags (`--draft --force`) into a JSON body (same pattern as `c.pr`, extended with `--force`), POSTs `/v1/runs/{run}/ship`, decodes `shipResponse`, and prints two lines:
  ```
  pushed <source_branch> → <branch> on <remote>
  opened <url>            # or: exists <url>   when pr_existed
  ```
- `--watch`-style streaming is not relevant (ship is post-run). Non-200 → `printErr` (carries the server's status + message, e.g. a 502 from gh).

### 5. `ParseRemote` `:port` fix (`internal/host/gh.go`)

After parsing the host segment, strip a trailing `:port` before the `host != "github.com"` comparison, so `ssh://git@github.com:22/owner/repo` (and scp-form `git@github.com:owner/repo` already handled) resolve to `github.com`. A genuinely-different host with a port is still rejected (fail-closed preserved). Owner/repo parsing and `safeSeg` guards unchanged.

### 6. push/pr/ship timeout (`internal/api/middleware.go`)

`timeoutMiddleware` currently applies one duration and exempts `/events`. Add a longer bound for the delivery routes: after the SSE exemption, if the path ends in `/push`, `/pr`, or `/ship`, use **120s**; otherwise the passed-in duration (30s). This keeps a hung `git`/`gh` from wedging a handler forever (unlike a blanket exemption) while giving a real network push + two `gh` calls room to finish. The router call site is unchanged (`timeoutMiddleware(30*time.Second)`).

### 7. `gemini.go` gofmt

`gofmt -w internal/executor/gemini.go`. No behavior change; makes `gofmt -l .` clean repo-wide for the first time since the file was introduced.

## Data flow (ship happy path)

1. `cm ship <run> --title "…"` → `POST /v1/runs/<run>/ship` with JSON body.
2. `handleShip` decodes → `ShipOpts` → `Supervisor.Ship`.
3. `Ship` → `Push`: reads run, validates (external/succeeded), picks result step, reads its persisted `step/<id>` branch, resolves the remote, `git push` from the scratch clone → `magister/<run>` (or `--as`). (`*PushError` on failure ⇒ no PR.)
4. `Ship` → `prCore`: derives `owner/repo` from the source origin, builds title/body, `ExistingOpenPR` pre-check, else `CreatePR`. Returns `(PRResult, existed, nil)`.
5. `handleShip` writes `shipResponse{pushed, pr, pr_existed}` (200). `cm` prints the two lines.

## Error handling & status mapping

`handleShip` maps the propagated error by type:
- `*supervisor.PushError` → its `Status` (404 unknown/scratch-reclaimed, 400 not-external/ambiguous/no-branch/bad-step, 409 not-succeeded, 502 push-failed).
- `*supervisor.PRError` → its `Status` (400 unsupported-host/unsafe-ref/bad-remote, 409 branch-not-pushed, 500 corrupt-flow, 502 gh-failed).
- anything else → 500.

Idempotency: an already-open PR is **not** an error from `Ship` (it's `PRExisted=true`, 200). `cm pr` standalone still returns 409 for the same condition (unchanged), because only the thin `PR` wrapper converts `existed`→409.

Re-run convergence: `cm ship` twice in a row — the second push is a git no-op ("everything up-to-date", exit 0), and `prCore` finds the existing PR → `exists <url>`, exit 0.

## Testing

- **`internal/supervisor` (`ship_test.go`)** — reuse `pr_test.go`'s harness (fabricated succeeded external-repo `RunState`, source repo with a github origin, env-driven stub `gh`) plus push's bare-repo fixture:
  - ship happy: push delivers the branch **and** a PR is created; `ShipResult` carries both, `PRExisted=false`.
  - ship idempotent: a pre-existing open PR ⇒ `Ship` succeeds, `PRExisted=true`, `PR.URL` = the existing URL, exit-equivalent success (no error).
  - ship push-fails: `Push` error (e.g. not-succeeded 409 / scratch-missing 404) propagates as `*PushError`; **no** `gh` create is attempted (assert via the stub argv log).
  - ship pr-fails: `prCore` 502 (stub gh create-fail + branch-exists) propagates as `*PRError` after the push happened.
  - shared-flag passthrough: a single `--as`/`--step`/`--remote` reaches both push and the gh head (assert through the bare-repo dest branch + the stub `gh` argv).
  - `PR` wrapper unchanged: existing-PR still returns a 409 `*PRError` with the URL (guard the refactor).
- **`internal/api` (`handlers_test.go`)** — `handleShip` happy (200 `shipResponse`) and error mapping for a `*PushError` and a `*PRError`, reusing the `fake-gh` + bare-repo fixtures; assert `pr_existed` surfaces in the response.
- **`internal/host` (`gh_test.go`)** — `ParseRemote` table gains `:port` cases: `ssh://git@github.com:22/o/r` → `github.com, o, r`; a non-github host with a port still errors.
- **`internal/api` (`middleware_test.go`)** — a delivery path (`…/ship`) gets the 120s bound; a normal path keeps 30s; `/events` stays exempt.
- **`cmd/cm`** — `ship` dispatch builds the expected JSON body (value + boolean flags incl. `--force`) and prints `opened` vs `exists` by `pr_existed`.

## Global constraints

Go 1.22; **stdlib-only, no new dependencies**; zero token handling (ambient `git`/`gh`); read-only source repo; GitHub-only host; engine lifecycle untouched (ship is post-run, store-driven); single conventional-commit subjects (no body, no `Co-Authored-By`); never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.

## Out of scope / carried follow-ups (unchanged or newly noted)

- scratch-GC (its own slice); `GetRun`→404 sentinel; other hosts; fork PRs; API auth/observability.
- `cm ship` does not add a `--no-pr` / push-only mode (that's just `cm push`); YAGNI.
