# Design — external-repo Slice 3 (open a PR) (2026-06-18)

**Status:** approved (brainstorm), pre-plan.
**Milestone:** third and final slice of the **external-repo** full-delivery north star (flow → real repo → PR). Slices 1 (provision from a real repo) and 2 (push the result to a remote) are merged. This slice opens a **Pull Request** on the pushed branch via the git host (GitHub first). With it, the north star is complete.

## Problem

After Slice 2, a finished external-repo run's result branch lands on a real remote (`magister/<runID>` by default), but turning it into a reviewable Pull Request is still a manual step the operator does in the GitHub UI or by hand. There is no first-class way to go from "the branch is pushed" to "a PR is open."

This slice adds an explicit, post-push **PR**: `cm pr <run>` (→ `POST /v1/runs/{id}/pr`) opens a Pull Request on the host repo whose head is the pushed branch. Like push, it is a pure post-run, store-driven operation: the engine run lifecycle is untouched.

## Goals

- An explicit `cm pr <run>` / `POST /v1/runs/{id}/pr` opens a PR on the host repo for a **succeeded** external-repo run.
- The host client is the **`gh` CLI** (`gh pr create`), with an **injectable command name** (default `"gh"`) so tests substitute a stub — mirroring the executor's `CLIAgent.Bin` pattern.
- **Ambient auth, zero token handling** — `gh` authenticates via its own keyring/`GH_TOKEN`; we never read, store, or pass a token (the same posture as Slice 2's `git push`).
- Head branch = the **pushed destination** (`magister/<runID>` by default; `--as <branch>` to match a non-default push). Base branch defaults to the repo's own default branch (we omit `--base`); `--base <branch>` overrides.
- PR title defaults to the flow name + short run id; PR **body** defaults to a generated run summary built from store data; `--title`/`--body` override; `--draft` opens a draft PR.
- `owner/repo` + host are parsed from the **source repo's `origin` URL** (the same URL Slice 2's `ResolveRemote` returns); `--remote <url-or-name>` overrides, symmetric with push.
- **Re-run safety:** if an open PR already exists for the head branch, return it as a **409 with the existing PR URL** (no mutation).
- Clear errors (wrong run state, unsupported host, branch not pushed yet, gh failure) surfaced over HTTP via a typed `*PRError{Status, Msg}`.

## Non-goals

- **Hosts other than GitHub.** A non-`github.com` remote → a clear `unsupported host` 400. GitLab/Bitbucket are future slices.
- **Cross-repo / fork PRs** (a head of the form `owner:branch` on a fork). The foundation assumes the pushed branch lives on the same repo the PR targets (Slice 2's default).
- **Updating / re-targeting / closing an existing PR**, or any draft↔ready toggle beyond the `--draft` flag at creation.
- **Auto-PR** on push or on run success — explicit-only, like push (a PR is outward-facing; the operator triggers it deliberately).
- **Credential provisioning / token storage** — we rely on `gh`'s ambient auth; configuring it is the operator's job.
- **Touching the scratch clone.** Unlike push, PR needs no on-disk git work (see Architecture); it reads `owner/repo` from the source origin and run data from the store, then shells `gh`.
- Any engine / join / gate / executor / push behavior change.

## Decisions

Chosen in brainstorm (all approved):

1. **Host client = `gh` CLI** (`gh pr create`), not the REST API or an MCP. Installed + already authed; ambient auth keeps the zero-token posture; zero new Go deps; matches the established stub-CLI test pattern. (Q1)
2. **Separate, explicit `cm pr <run>`** — not `cm push --pr` and not a combined push+PR verb. Operates on an **already-pushed** branch; if the branch isn't on the remote yet, it fails with an actionable "run `cm push <run>` first." Single-purpose, independently testable, consistent with push being its own deliberate step. (Q2)
3. **`owner/repo` + host parsed from the source `origin` URL** (the same URL `ResolveRemote` returns); `--remote` override; non-github host → 400. (Q3)
4. **Generated run-summary PR body** by default (flow name, run id, terminal step + its merge commit, step list — all from store data); `--body` overrides. Title defaults to flow name + short run id (`--title` override); base omitted → repo default branch (`--base` override); `--draft` flag. (Q4)
5. **Existing open PR → 409 with the existing URL.** Proactively look it up so the caller always learns the PR URL; no PR mutation. (Q5)

Mechanical decisions taken during design presentation (approved):

6. **`POST /v1/runs/{id}/pr` takes a JSON request body** (`{remote, as, step, base, title, body, draft}`), not query params (push's style), because title/body are free-text — cleaner and length-safe.
7. **Head branch = the push destination** (`--as`/default `magister/<runID>`), *not* the local step branch (`step/integrate`); push renamed it on the remote. The terminal step is read only to build the body summary.
8. **Branch-existence is diagnosed on failure**, not pre-checked: the happy path is two `gh` calls (existing-PR lookup, then create); only if create fails do we run a `BranchExists` probe to produce the "run `cm push` first" message. Keeps the happy path lean and fully offline-testable through the `gh` seam.

## Architecture

### New package — `internal/host`

A bounded GitHub-via-`gh` client. Pure parsing + a thin `gh` runner; no engine/store/workspace deps.

- **`ParseRemote(remoteURL string) (host, owner, repo string, err error)`** — pure, no I/O. Accepts:
  - `https://github.com/owner/repo` and `…/repo.git`
  - `git@github.com:owner/repo(.git)` (scp-like)
  - `ssh://git@github.com/owner/repo(.git)`
  Strips a trailing `.git` and any leading `/`; charset-guards `owner` and `repo` (reject empty, a leading `-`, and anything outside `[A-Za-z0-9._-]`) so neither can smuggle a flag into `-R owner/repo`. `host != "github.com"` → `unsupported host %q (only github.com is supported)`.

- **`Runner{ Bin string }`** — default `Bin == "gh"`; tests inject an absolute path to `testdata/fake-gh`. Construct with `New()` (Bin `"gh"`) so production wiring is a one-liner. All argv use the single-token `--flag=value` form so a value can never be parsed as a following flag; `exec.CommandContext`, no shell. Methods:
  - `ExistingOpenPR(ctx, owner, repo, head string) (url string, exists bool, err error)` — `gh pr list -R=<owner/repo> --head=<head> --state=open --json=url`; decode the JSON array, return the first URL (`exists=false` on empty).
  - `CreatePR(ctx, opts CreateOpts) (url string, err error)` — `gh pr create -R=<owner/repo> --head=<head> [--base=<base>] --title=<title> --body=<body> [--draft]`; `gh` prints the new PR URL on stdout — return the trimmed last line. On non-zero exit, return the combined output as the error (the caller decides how to map it).
  - `BranchExists(ctx, owner, repo, branch string) (bool, err error)` — `gh api repos/{owner}/{repo}/branches/{branch} --silent`; exit 0 → exists, a 404 → not; used only to refine a `CreatePR` failure.

  `CreateOpts{ Owner, Repo, Head, Base, Title, Body string; Draft bool }`.

### Supervisor — `internal/supervisor/` (new `pr.go`, parallel to push)

`PR(ctx context.Context, runID core.RunID, opts PROpts) (PRResult, error)`:

1. `GetRun(runID)` → `rs` (404 if unknown — same `// TODO` store-not-found caveat as push).
2. Validate: `rs.Repo != ""` (else 400 not-external-repo); `rs.Status == RunSucceeded` (else 409). (Reuses push's validation shape.)
3. `head = opts.As`; default `"magister/" + string(runID)`.
4. `remoteURL = workspace.ResolveRemote(rs.Repo, opts.Remote)` (400 on failure); `host, owner, repo = host.ParseRemote(remoteURL)` (400 / `unsupported host` on failure).
5. `f = flow.ParseBytes(rs.FlowYAML)` (500 on a corrupt stored flow, as push does); `term = pickResultStep(f, opts.Step)` (reused from push) — the result step the body summarizes. A genuinely ambiguous terminal with no `--step` → **400** (same as push; the typical single-terminal merge flow is unambiguous, and `--step` resolves the rest). Body: `opts.Body`, else `generatePRBody(rs, term)`; title: `opts.Title`, else `defaultPRTitle(rs)` (e.g. `"magister: <name> (<short-id>)"`, short id = the trailing slice of the ULID). Base: `opts.Base` (empty → omitted, gh uses the repo default).
6. `ExistingOpenPR(owner, repo, head)` → if `exists`, **409** `"PR already exists for <head>: <url>"`.
7. `CreatePR(...)` → on success, the PR URL. On failure: `BranchExists(owner, repo, head)`? **no → 409** `"branch \"<head>\" not on remote; run \`cm push <run>\` first"`; **yes → 502** with gh's combined output.
8. Return `PRResult{URL, Repo: owner+"/"+repo, Head: head, Base: base, Draft: opts.Draft}`.

Types: `PROpts{ Remote, As, Step, Base, Title, Body string; Draft bool }`; `PRResult{ URL, Repo, Head, Base string; Draft bool }`; `PRError{ Status int; Msg string }` with a `prErr(status, format, …)` helper — mirrors `PushError`. The `gh` `host.Runner` is a field on the `Supervisor` (wired by the daemon; default `host.New()`), so tests inject a stub-backed runner — analogous to how the engine takes an injected `RunAgent` callback.

`generatePRBody` / `defaultPRTitle` are small pure helpers over `RunState`: the body lists the flow name, run id, the terminal step + its `Artifact.Commit`, and `✓`-marked step ids; the title is the flow name + a short id slice.

### API — `internal/api/{handlers,router,dto}.go`

- Route: `v1.HandleFunc("POST /v1/runs/{id}/pr", s.handlePR)` (Go 1.22 wildcard, inside the authed group like `/push`).
- `handlePR`: `decodeJSON` the body into a `prRequest`, call `s.Sup.PR(ctx, id, supervisor.PROpts{…})`, map `*PRError` via `errors.As` (else 500), return `prResponse`.
- `prRequest{ Remote, As, Step, Base, Title, Body string; Draft bool }` (all optional, JSON).
- `prResponse{ url, repo, head, base, draft }`.
- Status mapping (from `*PRError`): `404` unknown run; `400` not-external-repo / unsupported host / unknown or ambiguous `--step` / bad remote; `409` not-succeeded / existing PR / branch-not-pushed; `500` corrupt stored flow; `502` gh create failed; `200` success.

### CLI — `cmd/cm/main.go`

`cm pr <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft]` → builds a JSON body and `POST`s `/v1/runs/<run>/pr` (mirrors `cm push`'s flag parsing; bare value-flags with no argument error). On `200`, prints `opened <url>`; on non-2xx, prints the server error (the 409 body carries the existing/actionable URL). Add `pr` to the usage line and the top-level command switch.

### Daemon wiring — `cmd/magisterd` (or wherever the Supervisor is constructed)

Set the Supervisor's `host.Runner` to `host.New()` (Bin `"gh"`). No other daemon change.

## Testing

All offline; mirrors Slice 2's "real where cheap, stub the external tool" approach.

- **`host.ParseRemote`** (`internal/host`): table tests — `https`/`https+.git`/`git@`scp/`ssh://` forms all → `github.com,owner,repo`; trailing `.git` and leading `/` stripped; non-github host → error; flag-like / empty owner or repo → error.
- **`host.Runner`** (`internal/host`) against a `testdata/fake-gh` stub (records argv, emits canned output, branches on `$1`/`$2`): `CreatePR` builds the exact `pr create` argv (incl. `--draft`, `--base` present/omitted) and returns the parsed URL; `ExistingOpenPR` parses the `pr list` JSON (hit and empty); `BranchExists` maps exit 0 / non-zero. A `-`-leading title/body is carried safely as a `--flag=value` token.
- **`Supervisor.PR`** (`internal/supervisor/pr_test.go`): a store seeded with a **succeeded external-repo `RunState`** (terminal step with `Artifact.Branch`/`Commit`) + a source repo whose `origin` is a github URL string (never fetched) + a stub-`gh`-backed `host.Runner`. Assert: happy path returns the canned URL + derived `owner/repo` + the generated title/body reaching the stub; error paths — unknown run → 404; non-external-repo → 400; not-succeeded → 409; existing open PR → **409 with URL**; create-fails-and-branch-missing → **409 "run cm push first"**; create-fails-and-branch-exists → **502**; non-github origin → 400 unsupported host.
- **Handler** (`internal/api`): `POST /v1/runs/{id}/pr` with a JSON body → `prResponse` echoes url/repo/head; `*PRError` statuses propagate.
- **`cm pr`** (`cmd/cm`): a `fakeAPI` captures the request → asserts `POST /v1/runs/<id>/pr` with the expected JSON body; `opened <url>` printed on 200; bare value-flag errors.
- **Manual proof:** real daemon + a throwaway **real GitHub repo** (`gh` authed as `jeremN`): `cm run flows/external-repo.yaml --repo <src>` → succeeds; `cm push <run>` → branch on GitHub; `cm pr <run>` → an open PR whose head is `magister/<runID>`, body = the generated summary; a second `cm pr <run>` → 409 with the existing PR URL.

## Touchpoints

- `internal/host/` (new: `gh.go` `ParseRemote`+`Runner`, `gh_test.go`, `testdata/fake-gh`).
- `internal/supervisor/pr.go` (new: `PR`, `PROpts`/`PRResult`/`PRError`, `generatePRBody`/`defaultPRTitle`), `internal/supervisor/supervisor.go` (a `host.Runner` field; reuse `pickResultStep`), `internal/supervisor/pr_test.go`.
- `internal/api/{handlers,router,dto}.go` (`handlePR`, route, `prRequest`/`prResponse`).
- `cmd/cm/main.go` (`pr` verb + usage), daemon construction (wire `host.New()`).
- run-skill doc note (`cm pr`).

## Security / notes

- **Credentials:** none handled by us. `gh` authenticates via its own keyring / `GH_TOKEN`. An auth failure surfaces as a `502` with gh's output. Same zero-token posture as Slice 2's `git push`.
- **Argv hardening:** `owner`/`repo` are charset-guarded in `ParseRemote`; head/base are the push refs (already `safeRef`-shaped from Slice 2) and additionally carried as `--flag=value` single tokens; free-text title/body are passed as `--title=…`/`--body=…` single tokens so a leading `-` can never be parsed as a flag. `exec.CommandContext`, no shell. `// #nosec G204` with the rationale, as in `executor/cli.go`.
- **Not SSRF:** the remote URL is user-supplied but the trust boundary is loopback (§9) and the operator is opening a PR on their **own** repo via their own `gh` auth — the host's intended capability. No URL filter (same reasoning as `ResolveRemote`/`ResolveBase`).
- **Read-only source:** PR only `remote get-url`s the source (via `ResolveRemote`) and reads the store; it never writes the source, the scratch, or any git ref. The PR is created entirely host-side by `gh`.
- **No scratch dependency:** unlike push, PR does no on-disk git, so it is unaffected by scratch lifetime/GC (resolving one of push's carried follow-ups for this verb).
- **Carried (unchanged):** the 30s `timeoutMiddleware` will wrap `POST .../pr`; a slow real `gh` call could 503. Same pre-existing follow-up as push — exempt these long-ops like SSE if it bites. Not addressed here.
