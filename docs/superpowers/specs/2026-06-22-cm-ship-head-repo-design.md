# Delivery — fork ship (`cm ship --head-repo`)

## Summary

Extend the one-command `cm ship` (push + open-PR) to **forks**, completing the open-source-contribution delivery story. Today `cm ship` only ships to the same repo: its single `--remote` feeds both the push destination and the PR base, which can never disagree. A fork ship needs them to differ — push to **your fork**, open the PR into **upstream**. This slice adds an optional `--head-repo` input that names the fork; when set, `cm ship` pushes the result branch to the fork and opens the cross-fork PR (head `forkowner:branch`) into the upstream, in one command.

A fork ship is exactly *push-to-fork* + the *cross-fork PR already built* (`cm pr --head-repo`). The change is minimal: `Supervisor.Ship` routes its push to the fork when `--head-repo` is set, and passes `HeadRepo` through to the shared `prCore` (which already composes the cross-fork head and, since the idempotency fix, detects an existing cross-fork PR). Same-repo ship is byte-for-byte unchanged. Touches `internal/supervisor/ship.go`, `internal/api/dto.go`, `internal/api/handlers.go`, `cmd/cm/main.go` plus tests. **No change to `internal/host`, push, or `prCore` logic; no new dependency, package, migration, schema, or SSE event.** Go 1.22, stdlib only.

## Motivation

The external-repo north star delivers a run's result to a real GitHub repo, and `cm ship` collapses the final two steps (`cm push` then `cm pr`) into one idempotent command — but only for repos you can write to. The cross-fork PR slice (`cm pr --head-repo`) closed the write-access gap for the *PR* step, and `cm push --remote <fork>` already delivers to a fork. What remains is the convenience: an OSS contributor still has to run two commands (`cm push --remote <fork>` then `cm pr --head-repo <fork>`) where same-repo users run one (`cm ship`). Fork ship gives forks the same one-command ergonomics, naturally building on the just-merged cross-fork PR machinery.

## Design

### Surface

A new optional input names the fork:

- CLI: `cm ship <run> [existing flags…] [--head-repo <url-or-remote-name>]`
- API: a new optional JSON field `head_repo` on `POST /v1/runs/{id}/ship`.

When `--head-repo` is **omitted**, behavior is byte-for-byte today's same-repo ship. When **set**, `cm ship` pushes to the fork and opens the cross-fork PR into the upstream.

`--head-repo` accepts the same forms as `--remote` and is resolved the same way (a git URL passed through, or a source-configured remote name resolved via `git remote get-url`) — once by the push half (as its push destination) and once by `prCore` (parsed to the fork owner for the head). The user passes the same fork URL they would to `cm push --remote <fork>` / `cm pr --head-repo <fork>`.

### `--remote` in fork mode (PR-base override, mirrors `cm pr`)

In fork mode, `--head-repo` is the fork (push destination + PR head owner) and `--remote` retains exactly its `cm pr` meaning: it overrides the PR's **base** repo (the upstream the PR targets), defaulting to the source repo's origin. So `cm ship --head-repo <fork>` opens the PR on the source origin; `cm ship --head-repo <fork> --remote <upstream-url>` overrides the base (rare). The push always goes to `--head-repo`. This makes `cm ship` and `cm pr` behave identically with respect to `--remote`/`--head-repo`.

(Note the deliberate, scoped semantic: in **same-repo** ship `--remote` feeds the push destination; in **fork** ship the push destination is `--head-repo` and `--remote` feeds only the PR base. The two never both feed the push.)

### `Supervisor.Ship` changes

`ShipOpts` gains `HeadRepo string`. `Ship` makes two minimal changes:

1. **Push routes to the fork.** Select the push destination before pushing:
   ```
   pushRemote := opts.Remote
   if opts.HeadRepo != "" {
       pushRemote = opts.HeadRepo
   }
   ```
   then `Push(PushOpts{Remote: pushRemote, As: opts.As, Step: opts.Step, Force: opts.Force})`. Same-repo (`HeadRepo==""`): `pushRemote == opts.Remote`, unchanged.
2. **PR stays cross-fork.** `prCore(PROpts{Remote: opts.Remote, As: opts.As, Step: opts.Step, Base: opts.Base, Title: opts.Title, Body: opts.Body, Draft: opts.Draft, HeadRepo: opts.HeadRepo})`. The PR base = `opts.Remote` (default origin = upstream); the head = `forkOwner:branch` when `HeadRepo` is set (composed inside `prCore`). Same-repo: `HeadRepo==""` → byte-for-byte today.

Push runs first (it needs the scratch clone), then `prCore` — the existing order. The result is `ShipResult{Push, PR, PRExisted}`, unchanged.

**Idempotency comes for free.** `prCore` uses the fixed `host.ExistingOpenPR` (queries the bare branch, filters by `headRepositoryOwner.login == headOwner`), so a re-run of a fork ship whose PR already exists returns `PRExisted=true` (success) — exactly like same-repo ship. No 502-on-duplicate.

### Wiring

- `internal/api/dto.go`: `shipRequest` gains `HeadRepo string \`json:"head_repo,omitempty"\``.
- `internal/api/handlers.go`: `handleShip` maps `HeadRepo: req.HeadRepo` into `ShipOpts` alongside the existing fields.
- `cmd/cm/main.go`: `cm ship` gains a **dedicated** `case "--head-repo"` that sets `body["head_repo"]` (the generic `body[flag[2:]]` path would produce the hyphenated `head-repo`, which the server would ignore); the usage string adds `[--head-repo <url-or-name>]`.
- `internal/supervisor/ship.go`: the `ShipOpts` doc comment is updated — the current "shared fields (Remote/As/Step) feed both operations, so the push destination and the PR head branch can never disagree" is nuanced for fork mode (with `HeadRepo`, the push destination is the fork and the PR base is `Remote`/origin; the PR head owner is the fork).

### Error handling

All statuses map exactly as today via `errors.As` on `*PushError` (push half) then `*PRError` (pr half). A bad/unresolvable/non-GitHub `--head-repo` surfaces as the push half's error (the push to the fork fails) or, if the push somehow succeeds, the PR half's `400` (from `ResolveRemote`/`ParseRemote` in `prCore`) — the same mapping already exercised by `cm pr --head-repo`.

## Testing

- **`internal/supervisor` (`ship_test.go`):** with the existing ship/`fake-gh` test harness:
  - **Same-repo ship unchanged** — a regression assertion that `HeadRepo==""` pushes to `opts.Remote` and opens a same-repo PR (head = bare branch), identical to today.
  - **Fork ship routes the push to the fork** — a `ShipOpts{HeadRepo: <fork>}` ship sends the push to the fork remote, not `opts.Remote` (provable on the push-failure path: a fork ship with an unreachable/bad fork remote returns a `*PushError` naming the fork, and no PR is attempted).
  - **`HeadRepo` threads into `prCore`** — a fork ship reaching the PR half carries `HeadRepo` into `prCore` (the cross-fork head behavior itself is already covered by the merged `prCore` fork tests; here we assert Ship forwards the field, e.g. via a fork ship whose non-GitHub `--head-repo` makes the PR half 400 only when `HeadRepo` is threaded).
  - The **fully-happy fork path** (push to the fork *and* open the PR with one fork URL) is **not offline-unit-testable** — one URL cannot be both an offline-pushable bare repo (for the push half) and a github.com-parseable head owner (for the PR half). This is the same constraint the original same-repo `cm ship` happy path has; it is covered by the live smoke below.
- **`internal/api`:** a `POST /v1/runs/{id}/ship` body with `head_repo` threads into `ShipOpts.HeadRepo` (mirror the existing ship endpoint test).
- **`cmd/cm` (`main_test.go`):** `cm ship <run> --head-repo <url>` puts `head_repo` (underscore) in the POST JSON body, not the hyphenated key (mirror the `cm pr --head-repo` test).
- Full `go test -race ./...` green; `go vet` and `gofmt -l` clean. (The supervisor/api/magisterd packages contain real-git/`gh` e2e tests that require the sandbox disabled — unrelated to this change.)

### Live smoke (manual proof)

Against a real GitHub fork relationship (`gh` authed; e.g. fork `octocat/Spoon-Knife`): clone the upstream as the source; `cm run flows/external-repo.yaml --repo <clone> --base main`; then a single `cm ship <run> --head-repo <fork-url>` → pushes `magister/<run>` to the fork **and** opens a real cross-fork PR into the upstream (head `forkowner:magister/<run>`, base upstream default) in one command. A second `cm ship <run> --head-repo <fork-url>` → idempotent success (`PRExisted=true`, the existing PR URL, no duplicate). Clean up afterward (close the PR, delete the fork branch).

## Out of scope

- `cm pr`/`cm push` changes (already fork-capable).
- `internal/host` and `prCore` logic changes (Ship only threads the field).
- Persisting the push destination on the run (the explicit `--head-repo` is stateless by design, consistent with the rest of the delivery surface).
- Renamed forks whose repo name differs from upstream (the head assumes the fork repo name == the base name, the GitHub default — inherited from `cm pr --head-repo`).
- Non-GitHub hosts (`host.ParseRemote` is github.com-only).

## Global constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no new package; no migration; no schema change; no new SSE event kind.
- `--head-repo` omitted ⇒ behavior is **byte-for-byte** today's same-repo ship.
- Read-only on the source repo (`ResolveRemote` only runs `git remote get-url`).
- `--head-repo` is resolved/guarded by the existing machinery (`ResolveRemote` + `host.ParseRemote`/`safeSeg`; `safePRRef` on the branch); auth stays ambient `gh`/git (zero token handling).
- Push always goes to `--head-repo` when set (never `--remote`); `--remote` feeds only the PR base in fork mode (its `cm pr` meaning).
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.
