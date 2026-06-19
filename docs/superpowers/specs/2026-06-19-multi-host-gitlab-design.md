# Multi-host delivery (GitLab via glab) — Design

**Date:** 2026-06-19
**Status:** Approved (design); ready for implementation plan.

## Problem

Delivery is **GitHub-only for the PR step**. `cm pr` / `cm ship` open a Pull Request via the `gh` CLI, and `host.ParseRemote` hard-rejects any host that is not `github.com` (`internal/host/gh.go:51`). A run whose source remote is on GitLab gets a `400 unsupported host`.

**Already host-agnostic (do not touch):** `cm push` is a plain `git push -- <url> <src>:refs/heads/<dest>` (`workspace.PushBranch`) to a URL resolved by `workspace.ResolveRemote`, which never inspects the host. Pushing a result branch to a gitlab.com remote with ambient git credentials already works today. **This slice is purely about the PR/MR-opening step.**

## Goal

Make the PR/MR-opening step **host-pluggable** and add **GitLab (gitlab.com) via the `glab` CLI**, preserving the project's zero-token / ambient-auth posture. No new Go dependency, no DB migration, no new HTTP route, no engine change. `cm push`, `cm pr`, and `cm ship` keep their current command surface; the host is derived from the run's source remote.

**Scope decisions (locked during brainstorming):**
- **GitLab via `glab`** (installed, v1.101.0) — mirrors `gh`'s ambient-auth CLI model. Bitbucket is out of scope (no ubiquitous ambient-auth CLI → would need REST + a token, departing from the zero-token posture).
- **gitlab.com only.** Route strictly on the canonical hostnames `github.com`→gh, `gitlab.com`→glab. Self-hosted GitLab (indistinguishable from any host by name) is a noted follow-up needing a host→provider mapping.
- **2-segment `owner/repo` only.** Covers `gitlab.com/user/repo` and `gitlab.com/group/repo`. GitLab nested subgroups (`group/subgroup/repo`) are a noted follow-up so the existing `safeSeg` single-segment guard stays unchanged.

## Architecture — Host interface + per-host adapter, picked by a factory

The `internal/host` package gains a small `Host` interface — the seam the supervisor depends on — implemented by two adapters: the existing **gh** one (GitHub) and a new **glab** one (GitLab). `ParseRemote` recognizes both canonical hosts and returns the host string. A `host.For(hostname) (Host, error)` factory maps a recognized host to its adapter. The supervisor's PR/Ship path parses the remote → picks the host → calls the interface, with **no GitHub-specific knowledge left in the supervisor**.

```
cm pr/ship <run> → prCore
  → store.GetRun → workspace.ResolveRemote(source origin) → URL
  → host.ParseRemote(URL) → (host, owner, repo)
  → host.For(host) → Host adapter  (github.com→gh, gitlab.com→glab, else 400)
  → adapter.ExistingOpenPR / CreatePR / BranchExists  (shells gh|glab, ambient auth)
  → URL → response
```

This is the project's ports-and-adapters pattern applied once more: the PR step becomes an interface with two CLI-backed adapters, each isolating its tool's dialect and independently testable with its own fake-CLI stub.

## Components

### 1. `host.Host` interface (`internal/host`)

The methods the supervisor's `prCore` needs — exactly the current gh `Runner`'s surface, so no caller logic changes:

```go
type Host interface {
    ExistingOpenPR(ctx context.Context, owner, repo, head string) (url string, exists bool, err error)
    CreatePR(ctx context.Context, o CreateOpts) (url string, err error)
    BranchExists(ctx context.Context, owner, repo, branch string) bool
}
```

`CreateOpts` is reused unchanged (`Owner, Repo, Head, Base, Title, Body string; Draft bool`). Method names stay "PR" (internal vocabulary). **User-facing output stays host-neutral** — `cm pr`/`cm ship` print the returned URL (`opened <url>` / `exists <url>`) with no per-host wording, so no GitHub-vs-GitLab text logic is added. Both adapters satisfy this interface at compile time (`var _ Host = (*GH)(nil)`, `var _ Host = (*GL)(nil)`).

### 2. GitHub adapter (`internal/host/gh.go`)

Today's `Runner` made to satisfy `Host`, **renamed `GH` for symmetry with the new `GL`** (a mechanical rename — the type already implements all three methods; behavior is byte-for-byte unchanged: `gh pr list`/`gh pr create`/`gh api …/branches/…`, neutral `os.TempDir` cwd, separate stdout/stderr, `--flag=value` tokens, `#nosec G204`). `New()` keeps returning the gh adapter (now `*GH`), used by the factory and as the prior public constructor.

### 3. GitLab adapter (`internal/host/gl.go`, new)

Implements `Host` by shelling `glab`. Same hardening posture as the gh adapter (neutral cwd, separate stdout/stderr, single-token `--flag=value`, `#nosec G204`). Auth is ambient (`glab`'s own token store); no token is handled.

- **`ExistingOpenPR`** → `glab mr list -R <owner>/<repo> -s <head> -F json` → parse `[]struct{ WebURL string `json:"web_url"` }` → first element's `web_url` (GitLab uses `web_url`, not `url`). `glab mr list` returns open MRs by default.
- **`CreatePR`** → `glab mr create -R <owner>/<repo> -s <head> -t <title> -d <body> --yes` (+ `-b <base>` when `Base != ""`, + `--draft` when `Draft`). **`--yes`** skips the confirmation prompt (non-interactive — `gh` defaults to this; `glab` needs it explicitly). **Never** pass `--fill` or `--push` (the source branch was already delivered by `cm push`). Parse the MR URL from stdout (`lastURL`-style, shared with gh).
- **`BranchExists`** → `glab api projects/<owner>%2F<repo>/repository/branches/<branch>` (GitLab's REST API addresses a project by its URL-encoded full path; the `/` between owner and repo is encoded as `%2F`). `err == nil` ⇒ exists. As in gh, used only to refine a create failure into "run `cm push` first."

### 4. `ParseRemote` generalization (`internal/host/gh.go` → shared)

Replace the hardcoded `host != "github.com"` reject with a known-hosts check `{github.com, gitlab.com}`; any other host → `unsupported host %q (supported: github.com, gitlab.com)`. The existing parse (https / scp-like / ssh, `:port` strip, credential strip, `safeSeg` owner/repo) is unchanged and applies to both. `splitOwnerRepo` keeps requiring exactly 2 segments (subgroups deferred). `ParseRemote` already returns the host; only the validity gate widens.

### 5. `host.For` factory (`internal/host`, new)

```go
func For(host string) (Host, error)  // github.com→New() (gh), gitlab.com→NewGitLab() (glab), else unsupported-host error
```

Constructs the adapter with its default binary (`gh` / `glab`). The unsupported-host case mirrors `ParseRemote`'s (defense in depth; in practice `ParseRemote` already rejected it).

### 6. Supervisor wiring (`internal/supervisor`)

- `prCore` (`pr.go`): use the `host` value `ParseRemote` returns (currently discarded as `_`) → `hostFor(host)` → call the interface. No other prCore logic changes.
- Replace `Supervisor.Host *host.Runner` (+ `hostRunner()`) with an injectable **`Supervisor.HostFor func(host string) (host.Host, error)`** and a `hostFor(host)` accessor: `if s.HostFor != nil { return s.HostFor(host) }; return host.For(host)`. Default nil ⇒ production factory, so **the daemon needs no new wiring**. Tests set `HostFor` to return fake-CLI-backed adapters per host.
- `cm pr` / `cm ship` (`cmd/cm`) and the HTTP handlers/DTOs are **unchanged** — the host is derived from the remote, not a flag.

## Data flow

`cm pr <run>` → `POST /v1/runs/{id}/pr` → `prCore` → GetRun → ResolveRemote(source origin) → ParseRemote → `(host, owner, repo)` → `hostFor(host)` → `ExistingOpenPR` (→ 409/idempotent if open) else `CreatePR` → URL → `prResponse`. `cm ship` reuses `prCore` via `Ship` (idempotent variant) exactly as today. Shape is identical to the current GitHub flow; only adapter selection is new.

## Error handling

- **Unsupported host** (not github.com/gitlab.com) → `400` (message lists both supported hosts). Raised by `ParseRemote`.
- **Unsafe head/base** (user `--as`/`--base`) → `400` via the existing `safePRRef` in `prCore` (unchanged; guards both hosts).
- **CLI failure** (gh or glab non-zero) → `502` carrying the CLI's stderr. A **glab-not-authenticated** run surfaces here (glab's 401 stderr), pointing the user to `glab auth login`.
- **Existing open MR/PR** → `409` for `cm pr` (carries the URL) / **success** for `cm ship` (idempotent) — unchanged via `prCore`'s `(result, existed, err)`.
- **Branch not pushed** → `409` "run `cm push <run>` first" (the `BranchExists`-refined create failure) — unchanged.
- Status mapping in the handler (`errors.As` on `*PRError`/`*PushError`) is unchanged.

## Testing

- **`host` pkg:**
  - `ParseRemote` table test extended with gitlab.com URLs in all three forms (`https://gitlab.com/o/r.git`, `git@gitlab.com:o/r.git`, `ssh://git@gitlab.com/o/r`) plus an unsupported-host case (e.g. `bitbucket.org`) asserting the error.
  - GitLab adapter tested via a new env-driven **`testdata/fake-glab`** stub (mirrors `testdata/fake-gh`, mode 0755): asserts the exact argv for `mr list` / `mr create` / `api` and returns canned JSON/text so `web_url` parsing and the `--yes`/no-`--fill` invariant are verified offline. gh adapter tests unchanged.
- **`supervisor` pkg:** add a gitlab.com variant of the PR and Ship tests — inject `HostFor` returning fake-CLI-backed adapters for both hosts; assert a gitlab-remote run routes to the glab adapter and yields the MR `web_url`; the existing github tests stay green (regression guard that the refactor is behavior-preserving).
- **Manual proof (live):** requires `glab auth login` and a gitlab.com repo. Clone it as the source → external-repo flow → `cm push` (already host-agnostic) → `cm pr` opens a **real MR** on gitlab.com; a second `cm pr` → 409 with the MR URL (idempotency); `cm ship` opens-or-reports-exists. Mirrors the Slice-3 gh proof. Source repo untouched.

### The one probe-gated assumption

The design assumes `glab mr create -R <owner/repo> -s <head> -b <base> --yes` is **git-context-free** — runs from a neutral `os.TempDir`, addresses the project via `-R`, and does **not** push or require local git state (the GitLab analog of Slice 3's empirically-confirmed `gh pr create --repo/--head/--base` behavior). The **manual proof confirms this**. **Fallback if the proof shows glab needs git context:** run the glab adapter from the run's scratch clone (via `Engine.BasePath`), making the GitLab PR path scratch-dependent — a documented divergence from the gh path (which needs no scratch), recorded as a follow-up. This does not change the interface, only where the glab adapter is invoked.

## Out of scope (noted follow-ups)

- **Bitbucket / other hosts** — no ambient-auth CLI; would need REST + a token (different auth posture). Deferred.
- **Self-hosted GitLab** (`gitlab.example.com`) — needs a host→provider mapping (a configured known-hosts list or a `GITLAB_HOST`-style env) since the hostname doesn't reveal the provider. Deferred.
- **GitLab nested subgroups** (`group/subgroup/repo`, 3+ path segments) — would need `splitOwnerRepo`/`safeSeg` to handle a multi-segment namespace and the `api` project-path encoding to follow. Deferred; this slice handles 2-segment `owner/repo`.
- **glab-needs-scratch fallback** (above) — only if the live proof forces it.

## Global constraints

- Go 1.22; stdlib only, **no new Go dependency** (`glab` is an external binary like `gh`, not a module); **no DB migration; no new HTTP route; no engine change.**
- Zero token handling — ambient `glab`/`gh` auth only.
- The PR step stays **post-run, store-driven** (reads the run + a read-only `git remote get-url` on the source + shells the host CLI). It touches no scratch clone — UNLESS the probe-gated fallback above is needed.
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`, never `--no-verify`. `gofmt`/`go vet`/`go test -race ./...` clean before merge.
