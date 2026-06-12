# Design — external-repo Slice 2 (push the result to a remote) (2026-06-12)

**Status:** approved (brainstorm), pre-plan.
**Milestone:** second slice of the **external-repo** full-delivery north star (flow → real repo → PR). Slice 1 (provision from a real repo) is merged. This slice pushes a finished run's result branch to a git **remote**. Slice 3 (open a PR) builds on it.

## Problem

After an external-repo run succeeds, its result lives only in the per-run scratch clone at `<runsRoot>/<runID>/base` as a branch (e.g. `step/integrate`) — surfaced by Slice 1 as the run GET's `scratch` field, but reachable only by the user manually inspecting that path. There is no way to get the result out of the scratch repo and onto a real remote where it can be reviewed or turned into a PR.

This slice adds an explicit, post-run **push**: `cm push <run>` (→ `POST /v1/runs/{id}/push`) pushes the run's result branch from the scratch clone to a git remote (the source repo's `origin` by default), as a new branch (`magister/<runID>` by default). The engine run lifecycle is untouched; push is a pure post-run, store-driven operation.

## Goals

- An explicit `cm push <run>` / `POST /v1/runs/{id}/push` pushes a **succeeded** external-repo run's result branch to a remote.
- Default remote = the source repo's `origin` URL; overridable with `--remote <url-or-name>`.
- Default destination branch = `magister/<runID>`; overridable with `--as <branch>`.
- The result branch is identified from persisted state (the terminal step's `Artifact.Branch`); `--step <id>` disambiguates a multi-terminal flow.
- **No credential handling of our own** — `git push` authenticates via the daemon's ambient git setup.
- Safe by default: never clobber an existing remote ref (no `--force`); argv-hardened; the source repo is never written.
- Clear errors (wrong run state, ambiguous result, push failure) surfaced over HTTP.

## Non-goals

- **Opening a PR / host API** (GitHub/GitLab) — Slice 3.
- **Auto-push** on run success — chosen explicit-only (a remote push is outward-facing; the user triggers it deliberately after inspecting).
- **Pushing to the LOCAL source repo as a new branch** — the brainstorm chose remote; local-ref push is not implemented.
- **Credential provisioning / token storage / a credential helper** — we rely on the ambient git environment; configuring it is the operator's job.
- **Pushing a non-succeeded run**, or pushing arbitrary intermediate step branches beyond `--step` selection of a terminal.
- Any engine / join / gate / executor behavior change.

## Decisions

Chosen in brainstorm (all approved):

1. **Push to a remote** (not the local source repo). The scratch clone's own `origin` points at the *local* source path, so the destination is the source repo's **own** remote URL, resolved separately.
2. **Explicit trigger** — `cm push <run>` / `POST /v1/runs/{id}/push`. Engine hot-path unchanged; user inspects first.
3. **Remote = source's `origin` by default, `--remote` override.** Read fresh at push time from `RunState.Repo` (a delivery target, not part of run identity, so not pinned at submit). `--remote` accepts a remote *name* (resolved against the source) or a *URL*.
4. **Ambient credentials.** `PushBranch` execs `git push`; git authenticates via the daemon's SSH agent / credential helper / cached HTTPS. We never read/store/pass a token.
5. **Result = the terminal step's persisted branch.** Terminal = a step nothing else `Needs`. Unique terminal → it; `--step <id>` override; zero/ambiguous → error. Branch read from the step's persisted `Artifact.Branch`.
6. **Destination branch** = `--as <name>` or `magister/<runID>`. New-branch push; git refuses non-fast-forward overwrite unless `--force`.

## Architecture

### CLI — `cmd/cm/main.go`

`cm push <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--force]` → `POST /v1/runs/<run>/push` with the options as query params (mirrors `cm run`'s flag-parsing + `url.Values` style; bare `--remote`/`--as`/`--step` with no value errors, like `--repo`/`--base`). On `200`, prints the resolved remote + pushed source→dest branch; on non-2xx, prints the server error.

### API — `internal/api/handlers.go`, `internal/api/router.go`, `internal/api/dto.go`

- Route: `v1.HandleFunc("POST /v1/runs/{id}/push", s.handlePush)` (Go 1.22 wildcard, like `/approve`).
- `handlePush`: reads `id` (path), `remote`/`as`/`step`/`force` (query), calls `s.Sup.Push(ctx, id, pushOpts{...})`, maps the result/error to JSON + status, returns a `pushResponse` DTO `{remote, branch, source_branch, commit}`.
- Status mapping (from typed supervisor errors): `404` unknown run / missing scratch; `400` not external-repo / ambiguous terminal / chosen step has no branch / unknown `--step`; `409` run not `succeeded`; `502` push exec failed (auth/network/non-fast-forward), git stderr surfaced; `200` success.

### Supervisor — `internal/supervisor/supervisor.go`

New `Push(ctx, runID core.RunID, opts PushOpts) (PushResult, error)` orchestrates:
1. `GetRun(runID)` → `rs` (404 if unknown).
2. Validate: `rs.Repo != ""` (else 400 not-external-repo); `rs.Status == RunSucceeded` (else 409).
3. `flow.ParseBytes(rs.FlowYAML)` → `f`; `flow.TerminalSteps(f)`. Pick: `opts.Step` (must be a real step ID; 400 if not) → that step; else the unique terminal; else 400 ambiguous (error lists the terminal IDs).
4. Find the chosen step's persisted branch: scan `rs.Steps[chosen].Artifacts` for a non-empty `Branch` (all share the step's branch); empty → 400 "step <id> has no branch (not isolated)".
5. `remoteURL = workspace.ResolveRemote(rs.Repo, opts.Remote)` (400/“remote” error on failure).
6. `dest = opts.As`; default `"magister/" + string(runID)`.
7. `base = s.engine.BasePath(runID)`; if `base`/`.git` is absent → 404 "scratch repo for run <id> not found".
8. `workspace.PushBranch(base, remoteURL, branch, dest, opts.Force)` → 502 on git failure (stderr surfaced).
9. Return `PushResult{Remote: remoteURL, Branch: dest, SourceBranch: branch, Commit: <sha from the artifact>}`.

`PushOpts{Remote, As, Step string; Force bool}`. Typed sentinel errors (or an error-with-HTTP-status discriminator) so the handler maps codes without string-matching — e.g. small exported error vars / a `pushError{code,msg}` type in the supervisor.

### Engine — `internal/engine/engine.go`

`func (e *Engine) BasePath(runID core.RunID) string { return e.WS.BasePath(runID) }` — thin delegator (mirrors `Provision`), so the supervisor reaches the scratch path through its existing `engine` dependency.

### Workspace — `internal/core/ports.go`, `internal/workspace/`

- `core.Workspace` gains `BasePath(runID RunID) string` (the per-run base dir). `GitManager.BasePath` returns `m.baseDir(runID)`; `Manager.BasePath` returns its run dir (`filepath.Join(m.Root, string(runID))`) — harmless for non-git runs (push only targets external-repo/GitManager runs). This also realizes the seam the Slice-1 review suggested for the path-layout duplication.
- `internal/workspace/push.go` (new):
  - `ResolveRemote(sourceRepo, remote string) (string, error)`: `remote == ""` → `git -C <sourceRepo> remote get-url origin`; a value containing `://` or matching `scp`-like `user@host:path` → returned as-is (a URL); else a remote *name* → `git -C <sourceRepo> remote get-url <name>`. Read-only on the source; reuses the read-only `gitRead` helper; absolute-source + `--end-of-options` guards as in `ResolveBase`. Errors: no such remote / empty URL.
  - `PushBranch(scratchBase, remoteURL, srcBranch, destBranch string, force bool) error`: runs `git -C <scratchBase> push [--force] -- <remoteURL> <srcBranch>:refs/heads/<destBranch>`. Returns the git error WITH combined output (push failures must surface the remote's message). Validates `srcBranch`/`destBranch` are ref-safe (reject leading `-`, control chars) before exec; `--` separates the `<repository> <refspec>` operands from options.

### Flow — `internal/flow/flow.go` (or a new `internal/flow/graph.go`)

`func TerminalSteps(f *Flow) []*Step` — returns every step whose `ID` appears in no other step's `Needs`. Deterministic order (flow order). Pure, no I/O.

## Testing

- **`flow.TerminalSteps`** (`internal/flow`): single terminal (fan-in), multiple terminals (two independent leaves), linear chain (last is terminal), single step.
- **`workspace.ResolveRemote`** (`internal/workspace`): origin default from a fixture repo with a configured remote; resolve by name; URL pass-through; missing remote → error; relative source rejected; flag-like remote name rejected.
- **`workspace.PushBranch`**: a fixture scratch repo (a branch with a commit) + a **bare** repo as the "remote" (`git init --bare`). New-branch push lands the ref at the right commit; a non-fast-forward re-push is **refused** without `--force` and **succeeds** with it; a `-`-leading branch is rejected pre-exec.
- **`Supervisor.Push` + handler** (`internal/api` and/or `internal/supervisor`): a seeded/real **succeeded external-repo run** (provision from a fixture source repo, run the mock merge flow, scratch base present) pushed to a bare remote → the remote has `magister/<runID>` at the integrate commit; the `pushResponse` echoes remote/branches. Error paths: non-external-repo run → 400; not-succeeded → 409; ambiguous terminal (two leaves) → 400; unknown run → 404; missing scratch → 404; push to an unreachable/bogus remote → 502.
- **`cm push`** (`cmd/cm`): `fakeAPI`-captured request asserts `POST /v1/runs/<id>/push` with the right query params; bare-flag errors.
- **Manual proof:** real daemon; a fixture source repo whose `origin` is a local **bare** remote; `cm run flows/external-repo.yaml --repo <src>` → succeeds; `cm push <run>` → `git -C <bare> log magister/<runID>` shows the 2-parent integrate merge with the cloned base + step outputs.

## Touchpoints

- `cmd/cm/main.go` (push verb), `internal/api/{handlers,router,dto}.go` (endpoint + DTO), `internal/supervisor/supervisor.go` (`Push` + `PushOpts`/`PushResult` + typed errors), `internal/engine/engine.go` (`BasePath` delegator), `internal/core/ports.go` (`Workspace.BasePath`), `internal/workspace/{workspace,gitmanager}.go` (`BasePath` impls), `internal/workspace/push.go` (new: `ResolveRemote`, `PushBranch`), `internal/flow/flow.go`-or-`graph.go` (`TerminalSteps`), `flows/`/run-skill doc note.

## Security / notes

- **Credentials:** none handled by us. The daemon must have push access to the remote via its ambient git environment (SSH agent, credential helper, cached HTTPS). An auth failure surfaces as a `502` with git's stderr.
- **Not SSRF:** the remote URL is user-supplied, but the trust boundary is loopback (§9) and the operator IS the user pushing to their OWN remote — this is git's intended capability, analogous to `ResolveBase` accepting a local path. We do NOT add an SSRF URL filter (those guard servers accepting untrusted URLs from *external* clients; here the client is the trusted local operator). Argv hardening (`--`, ref-safety, `--end-of-options` on read) prevents flag/transport smuggling, mirroring Slice 1's `cloneBase`/`ResolveBase`.
- **Read-only source:** `ResolveRemote` only `rev-parse`/`remote get-url`s the source; `PushBranch` runs in the scratch clone. The source repo's refs/working tree are never touched.
- **No overwrite by default:** a fresh `magister/<runID>` push always succeeds; re-pushing a moved branch is a non-fast-forward that git rejects unless `--force`. We add no force logic of our own.
- **Scratch lifetime:** push depends on the scratch base persisting after the run (it does — `TeardownRun` keeps it). If a future slice GC's scratch repos, push must run before reclamation; for now the `404 missing scratch` path covers a manually-deleted scratch.
- **Carried (unchanged):** Slice-1 path-layout duplication is partially addressed by `Workspace.BasePath` (push uses it; the Slice-1 GET handler still uses its `ScratchRoot` string — consolidating it is optional and out of scope here).
