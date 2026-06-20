# Handoff — /healthz liveness + /readyz readiness split: MERGED to main, live-proven (2026-06-20)

**Start here next session.** The **readiness slice is DONE and MERGED to `main`** (fast-forward, `main` at **`e9de8ae`**, 3 commits off `06c9a45`). Full suite **313 passed / 17 packages `-race`**, vet + gofmt clean. Final Opus whole-branch review **Ready-to-merge=Yes, zero Critical/Important**. **Live drain proof PASSED.** Worktree + branch cleaned up. **Pushed to origin** (`origin/main` at `e9de8ae`).

## What shipped

A liveness-vs-readiness split for `magisterd` (stdlib only, **no new dep**, no migration, no store-table change, Go 1.22). Spec `…/specs/2026-06-20-healthz-readiness-liveness-design.md`, plan `…/plans/2026-06-20-healthz-readiness-liveness.md`.

- **`/healthz` (liveness)** — UNCHANGED: unconditional `200 {"status":"ok"}`, dependency-free. Decides *restart*.
- **`/readyz` (readiness, new)** — `200 {"status":"ready"}` only when **not draining AND the store pings OK**; `503 {"status":"draining"}` while shutting down; `503 {"status":"store unreachable"}` if the store ping fails. Decides *route*. Auth-exempt (outer mux), labeled in `routeLabel`.
- **Graceful drain** — at shutdown the daemon flips `SetDraining(true)` (so `/readyz`→503 immediately), optionally sleeps `-shutdown-drain`, then runs the existing unchanged `sup.Shutdown`→`httpSrv.Shutdown`.

Per commit (`06c9a45..e9de8ae`):
1. **`e6004dd` `feat(store): Ping for readiness probes`** — `core.Store.Ping(ctx context.Context) error` added to the port + both (only) implementers: `*store.SQLite.Ping` → `s.r.PingContext(ctx)` (the **reader** pool), `*store.Mem.Ping` → `nil`. Store tests for each.
2. **`6f5434e` `feat(api): /readyz readiness probe + draining flag`** — `Server` gains unexported `draining atomic.Bool` + exported `SetDraining(v bool)`; `handleReadyz` (draining-checked-BEFORE-ping; ping under `context.WithTimeout(r.Context(), 2*time.Second)`); route on the **outer** mux (auth-exempt, beside `/healthz`+`/metrics`); `routeLabel` `/readyz` case; new `internal/api/readyz_test.go` (ready/draining/store-unreachable via a `pingErrStore` embedding `*store.Mem`/auth-exempt). `handleHealthz` untouched. Added imports `context`,`sync/atomic`,`time` to handlers.go.
3. **`e9de8ae` `feat(magisterd): shutdown drain + readiness wiring`** — config `ShutdownDrain time.Duration` from `-shutdown-drain` / `MAGISTER_SHUTDOWN_DRAIN`, **default 0** (env override mirrors the `ScratchTTL` flag-set-guard + `ParseDuration` pattern); shutdown path inserts `srv.SetDraining(true)` + `if cfg.ShutdownDrain>0 { log + time.Sleep }` BEFORE the two unchanged shutdown calls; `TestRunServesHealthzAndShutsDown` extended to also assert `/readyz` 200 while live.

## Semantics / design notes
- `/readyz` lives on the outer mux → it bypasses `timeoutMiddleware`; the handler's own 2s `context.WithTimeout` is the sole bound. `database/sql.PingContext` (modernc pure-Go driver) honors the context for both connection acquisition and the ping, so a wedged DB cannot hang the probe past ~2s (Opus verified).
- `draining atomic.Bool` is the right primitive: written once at shutdown, read by concurrent handlers; Server is always `*Server` (never copied), named-field literal needs no change.
- Default `ShutdownDrain==0` ⇒ no sleep ⇒ shutdown timing byte-for-byte today's; the flag still flips so `/readyz` is correct immediately.
- The drain `time.Sleep` is **not** interruptible by a second signal (out of scope; k8s SIGKILL is the hard backstop). Default 0 makes it inert unless opted in.

## Live proof (PASSED 2026-06-20, real binary, `-shutdown-drain 3s`)
Built `magisterd`, started on `127.0.0.1:8147` with a throwaway DB. LIVE: `/healthz`→`200 {"status":"ok"}`, `/readyz`→`200 {"status":"ready"}`. After `kill -TERM`, throughout the 3s drain window: `/healthz` stayed **200** (liveness intact — would NOT be restarted) while `/readyz` flipped to **503 `{"status":"draining"}`** (LB stops routing). Structured log showed `path=/readyz status=503` + `path=/healthz status=200` during drain. Process exited **0** (clean) after the drain elapsed. This is the shutdown→draining→503 transition no unit test exercises end-to-end.

## Open follow-ups (carried)
- **(still unmerged) `multi-host` GitLab slice** — CODE-COMPLETE at `36eb9fa` (worktree `.worktrees/multi-host` still present), awaiting a live gitlab.com proof (no account yet). See `2026-06-19-multi-host-gitlab-next-steps.md`. **DO NOT merge until that proof passes.**
- **(observability backlog):** structured/request-scoped logging (request-ID through HTTP + engine — note the logging middleware already stamps a `request id`); OTel tracing (needs a dep). The per-agent metric triad (calls/cost/duration) + `/metrics` + liveness/readiness are now done.
- **(readiness, optional/future):** make the drain sleep interruptible by a second signal; a per-dependency readiness JSON breakdown; a one-line operator doc that `/readyz` pings the *reader* pool. All explicitly out of scope here.
- **(delivery axis):** cross-repo/fork PRs (`owner:branch` head).
- **(long-carried):** `GetRun→404` sentinel TODO; flaky `TestMockHonorsContextCancel`.

## Process notes
- Subagent-driven: haiku for Task 1 (trivial port method) + its review; sonnet for Tasks 2–3 (multi-file api/daemon) + their reviews; Opus final whole-branch review. All per-task reviews + the final review returned zero Critical/Important.
- The live smoke earned its keep again: the only way to see `/healthz` stay 200 while `/readyz` goes 503 mid-drain through a real OS process + real SIGTERM. Use `-shutdown-drain 3s` to widen the window; with default 0 the drain window is sub-millisecond.
- Commit hygiene held: single conventional subject, no body, no `Co-Authored-By`, never `--no-verify`. zsh trips on `$(...)`/`cd` in multi-line Bash-tool calls — wrote the smoke as a file and ran `bash /tmp/readyz-smoke.sh` to sidestep it entirely. The post-Edit hook path-doubling error on worktree edits is harmless.
