# Handoff — multi-host delivery (GitLab via glab): CODE COMPLETE, awaiting live proof (2026-06-19)

**Pick up next session.** The **multi-host GitLab delivery slice** is fully implemented, fully reviewed, and green — but **NOT merged**. It sits on branch `multi-host` (worktree `.worktrees/multi-host`) pending the **one remaining gate: a live manual proof against a real gitlab.com repo**, which is blocked only because there is no throwaway gitlab.com account/repo to test against yet. The user will create a gitlab.com account later to run it. **Do not merge until the live proof passes** (the user explicitly chose "live proof first").

## State (verified)

- Branch `multi-host` at **`36eb9fa`**, 5 commits off `main` (merge-base `90f1fec`). Worktree `.worktrees/multi-host` preserved.
- `go test -race ./...` → **292 passed across 16 packages** (was 285 on main; +7 real new tests: gitlab `ParseRemote` cases, the 6 `GL` adapter tests, `TestForRoutesGitLab`, `TestPROpensMergeRequestGitLab`). `go vet ./...` clean. `gofmt -l` clean. go 1.22. **No new deps, no migration, no new route, no engine change.**
- Built brainstorm → spec → plan → subagent-driven-development (4 tasks, fresh sonnet/haiku implementer + sonnet spec+quality review each). **Final Opus whole-branch review: Ready to merge, zero Critical/Important** — and it verified the glab argv contract against the **real glab v1.101.0** installed here (e.g. confirmed `--fill` would push, so the adapter's deliberate omission is correctness, not style).
- Spec: `docs/superpowers/specs/2026-06-19-multi-host-gitlab-design.md`. Plan: `docs/superpowers/plans/2026-06-19-multi-host-gitlab.md`. SDD ledger: `.git/worktrees/multi-host/sdd/progress.md`.

## What was built (per commit)

1. **`24a842a` `feat(host): ParseRemote recognizes gitlab.com`** — widened the host gate from hardcoded `github.com` to `supportedHost(host)` = `{github.com, gitlab.com}` (`internal/host/gh.go`). Flipped the `ParseRemote` test cases that asserted gitlab-is-rejected → accepted (https/scp/ssh forms); moved the unsupported-host probe to `bitbucket.org` (both `gh_test.go` table tests + the supervisor's `TestPRUnsupportedHost`).
2. **`53fa9a1` `refactor(host): Host interface + For factory; supervisor picks host by remote`** — new `host.Host` interface (`ExistingOpenPR`/`CreatePR`/`BranchExists`) in `internal/host/host.go` + `host.For(host) (Host, error)` factory (github.com→`New()`); renamed the gh adapter `Runner`→`GH` (byte-for-byte method bodies; `var _ Host = (*GH)(nil)`). Supervisor: replaced `Host *host.Runner` field with injectable **`HostFor func(string) (host.Host, error)`** + `hostFor(hostName)` accessor (nil→`host.For`, so the daemon needs NO wiring); `prCore` now USES the parsed host (was discarded `_`) → `hostFor(hostName)` → 400 on unsupported. Test injections migrated `sup.Host=&host.Runner{}` → `sup.HostFor=stubHostFor(t)` in `pr_test.go`/`ship_test.go` **and** `internal/api/handlers_test.go` (the one file beyond the plan's list — a mechanical compile fix from the field rename, same gh stub/assertions).
3. **`b0218d5` `feat(host): GL adapter opens GitLab merge requests via glab`** — `internal/host/gl.go`: `GL` adapter shelling `glab`, same hardening as gh (neutral `os.TempDir` cwd, separate stdout/stderr, single-token `--flag=value`, `#nosec G204`). `ExistingOpenPR`→`glab mr list --repo=o/r --source-branch=<head> --output=json` parsing `web_url` (NOT `url`); `CreatePR`→`glab mr create --repo= --source-branch= --title= --description= --yes [--target-branch= when Base!=""] [--draft]`, **never `--fill`/`--push`** (branch already pushed), `--yes` required (glab is interactive); `BranchExists`→`glab api projects/<o>%2F<r>/repository/branches/<%2F-encoded-branch>`. New env-driven `internal/host/testdata/fake-glab` stub (mode **100755**), GL unit tests, and `host.For` now routes `gitlab.com→NewGitLab()`.
4. **`b44c94a` `test(supervisor): GitLab run routes prCore to the glab adapter`** — `glabStub(t)` helper + extended `stubHostFor` (github.com→GH-stub, gitlab.com→GL-stub, default→`host.For`); `TestPROpensMergeRequestGitLab` proves a gitlab.com-origin run routes through `prCore` to GL and yields the MR url (non-tautological: GL emits `--source-branch` to `FAKE_GLAB_ARGV_FILE`, GH emits `--head` to `FAKE_GH_ARGV_FILE`, so mis-routing fails deterministically).
5. **`36eb9fa` `docs(host): package + ParseRemote comments reflect gitlab.com support`** — the lone review Minor: `gh.go` package doc + `ParseRemote` doc no longer say "gh CLI only / Only github.com is supported."

## THE REMAINING GATE — live manual proof (blocked on a gitlab.com repo)

The offline suite + Opus review cover everything EXCEPT the spec's **probe-gated assumption**: that `glab mr create -R <o/r> -s <head> [-b <base>] --yes` is **git-context-free** (runs from `os.TempDir`, addresses the project via `-R`, does NOT push or need local git state) and prints the MR URL to stdout for `lastURL` to scrape. This is the GitLab analog of Slice-3's empirically-confirmed `gh pr create` behavior; the project pattern (and the user) require it proven live before merge.

**Prereqs the user must provide (the only blockers):**
1. **A gitlab.com account + a throwaway repo** they own (≥1 commit). (None exists yet; user will create one.)
2. **`glab` authenticated:** `glab auth login` → gitlab.com → PAT with scopes **`api`** + **`write_repository`** (or browser). Git protocol HTTPS (push uses the token) OR SSH if their key is on gitlab.com (this env already has SSH-for-gitlab configured). Verify `glab auth status` shows all ✓ (currently 401, no token). The token lives in glab's config/keychain → the daemon's `glab` child finds it; no env juggling.

**Recipe (zero-cost mock — no agent spend; still a real 2-parent merge + real push + real MR):**
```
# in .worktrees/multi-host
go build -o /tmp/magisterd ./cmd/magisterd && go build -o /tmp/cm ./cmd/cm
git clone <user's gitlab repo URL> /tmp/gl-src          # local clone = source; its origin = gitlab.com/<user>/<repo>
mkdir -p /tmp/gl-demo
# launch daemon SANDBOX-DISABLED (glab/git children need network + keychain):
/tmp/magisterd -addr 127.0.0.1:8139 -db /tmp/gl-demo/magister.db >/tmp/gl-demo/d.log 2>&1 &
export MAGISTER_ADDR=http://127.0.0.1:8139
RID=$(/tmp/cm run flows/external-repo.yaml --repo /tmp/gl-src --base HEAD)   # mock agents → 2-parent merge over the repo's HEAD
/tmp/cm watch "$RID"                                    # expect succeeded
/tmp/cm push "$RID"                                     # pushes magister/<RID> to source origin = gitlab.com (ambient git creds)
/tmp/cm pr "$RID"                                       # → glab mr create → REAL MR url on gitlab.com
/tmp/cm pr "$RID"                                       # → 409 with the existing MR url (idempotency)
# (optional) /tmp/cm ship "$RID2" on a 2nd run → push+MR in one cmd, then idempotent "exists <url>"
```
- The Bash tool must launch `magisterd` with sandbox disabled (network + keychain for the glab/git children).
- **Watch for the probe assumption:** if `cm pr` fails because glab needs git context, the documented fallback is to run the GL adapter from the run's scratch clone via `Engine.BasePath` (interface-preserving — a `gl.go`-local change, no engine change) — record it as a divergence from the gh path.
- **Cleanup after:** close the MR + delete the `magister/<RID>` branch on gitlab.com; `pkill -TERM -f /tmp/magisterd` (SIGTERM exit 143/144 = graceful); `rm -rf /tmp/gl-demo /tmp/gl-src`. The source repo is never written (read-only clone into scratch).

**On a green proof:** run `superpowers:finishing-a-development-branch` from `.worktrees/multi-host` (tests already green) → merge `multi-host` → `main`, push to origin, update memory + a closing handoff. **On a failing proof:** apply the scratch-clone fallback (above) on the branch, re-review that one change, re-prove.

## Carried follow-ups / out of scope (unchanged from the spec)

- **Bitbucket / other hosts** — no ambient-auth CLI; needs REST + a token (different posture). Deferred.
- **Self-hosted GitLab** (`gitlab.example.com`) — needs a host→provider mapping (known-hosts list or `GITLAB_HOST` env); the hostname alone doesn't reveal the provider. Deferred.
- **GitLab nested subgroups** (`group/subgroup/repo`, 3+ segments) — `splitOwnerRepo`/`safeSeg` + the `api` project-path encoding would need multi-segment support. This slice is 2-segment `owner/repo` only.
- **glab-needs-scratch fallback** — only if the live proof forces it (above).
- **Pre-existing flakes (NOT this branch):** `TestPushDeliversResultToRemote` surfaced once during the slice but was PROVEN pre-existing (whole branch touched 0 push code; `-count=20 -race` → 20/20). `TestMockHonorsContextCancel` (`internal/executor`) — long-carried, did not surface. `GetRun→404 sentinel` TODO still open.

## Process notes that held this slice

- **The plan authored its own shadowing trap and caught it pre-dispatch:** a func-type field `HostFor func(host string)...` would shadow the `host` package in the `host.Host` return type → the field has NO param name (`func(string)...`) and the accessor param is `hostName`, import stays unaliased. Fixed in the plan before any code ran.
- **A field rename ripples into test files the plan didn't list** — `Runner`→`GH` + `Host`→`HostFor` broke `internal/api/handlers_test.go`; the implementer correctly made the mechanical injection-migration and reported it (same class as scratch-gc's `router_test.go`). Allow the necessary compile fix; verify it doesn't weaken the test.
- **Prove a flake's provenance, don't assume it** — `git diff --stat <merge-base>..HEAD -- <push files>` (empty = branch innocent) + `-count=20 -race` (20/20 = refuted) before blaming or absolving the branch.
- **`host.For` accepting gitlab BEFORE the GL adapter exists is an intentional mid-plan window** (Task 2 errors for gitlab; Task 3 adds the case) — the per-task reviewer flagged the "supported: …, gitlab.com" message as premature; it self-resolved at Task 3. Sequence abstraction-seam tasks so the suite stays green at each commit even while a capability is half-wired.
- **zsh in this env trips on `$(...)` command substitution inside some multi-line Bash-tool invocations** (`parse error near SDD=$(git ...`) and on `===` banners + parens — run such commands ONE per invocation and prefer explicit absolute paths over `$(git rev-parse --git-path …)`. The post-Edit hook fires a harmless path-doubling error on every worktree edit (edit still succeeds).
- Commit conventions: single conventional-commit subject, **no body, no `Co-Authored-By`**, never `--no-verify`. `gofmt` is NOT hook-enforced — run `gofmt -l` yourself.
