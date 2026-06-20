# Handoff — run-scoped logging + request→run bridge: MERGED to main (2026-06-20)

**Start here next session.** The **request-scoped-logging slice is DONE and MERGED to `main`** (fast-forward, `main` at **`df4c99a`**, 4 commits off `f73ecc8`). Full suite **326 passed / 18 packages `-race`** on the merged result, vet + gofmt clean. Final Opus whole-branch review **Ready-to-merge=Yes, zero Critical/Important** (3 Minors, all design-consistent / non-blocking — see below). Worktree + branch cleaned up. **LIVE smoke PASSED** (real daemon, mock run — proved the bridge line + the request→run pivot in real stderr). NOT yet pushed to origin — push when ready (`git push origin main`).

## What shipped

A small observability slice (stdlib only, **no new dep**, no migration, no schema change, Go 1.22) that makes a run's logs correlatable end to end. Spec `…/specs/2026-06-20-request-scoped-logging-design.md`, plan `…/plans/2026-06-20-request-scoped-logging.md`.

Two pieces, four commits (one per SDD task):

**Piece 1 — run-scoped logger into agent runs (Tasks 1–3).**
- `d4eccf6` `feat(logctx): context-carried slog.Logger` — new tiny package `internal/logctx`: `With(ctx, *slog.Logger) context.Context` + `From(ctx) *slog.Logger`. `From` NEVER returns nil (package-level `io.Discard` logger when none set); unexported `ctxKey struct{}` ⇒ no collision. Stdlib only.
- `7a0a7b8` `feat(executor): CLIAgent logs via context logger` — `CLIAgent.logger()` → `logger(ctx context.Context)`, resolving explicit `a.Log` → `logctx.From(ctx)`. Deleted the now-unused package-level `discardLogger` var (kept `io` import — still used by `CLISpec.Parse(stdout io.Reader, …)`). The one call site (the artifact-discovery `Warn`, cli.go) now passes `ctx`, so that previously-**discarded** warning now emits correlated by run-id. (`CLIAgent.Log` is still never wired in main — the context logger is the path now.)
- `c4a6ac3` `feat(engine): inject run-scoped logger at the agent seam` — in `runAgent`, a NEW local `agentCtx := logctx.With(ctx, e.logger().With("run", string(runID), "step", stepID, "agent", agentName))` is passed ONLY to `ag.Run(agentCtx, …)`. The original `ctx` is untouched — the `emit` closure captures it and calls `context.WithoutCancel(ctx)` for event persistence, so persistence + cancellation/timeout are byte-for-byte unchanged. `agentCtx` is a `context.WithValue` child, so it preserves the parent's Done/Deadline (verified by Opus: no timeout regression).

**Piece 2 — request→run bridge line (Task 4).**
- `df4c99a` `feat(api): log run-submitted bridging request-id and run-id` — `handleCreateRun`, after a successful `Submit` and before the response, emits `s.Log.Info("run submitted", "req", reqID, "run", string(id))` where `reqID, _ := r.Context().Value(requestIDKey).(string)` (in-package unexported key; comma-ok ⇒ panic-proof). This is the ONLY place the request-id and run-id meet, so an operator can pivot from an access-log line to every log/event for the resulting run.

## The correlation model (why run-id, not request-id)
The HTTP request context is deliberately severed at `supervisor.start()` (`context.WithCancel(context.Background())` — runs outlive their request, and resumed runs have no request at all). So the **run-id** is the durable correlation key for engine/agent logs (already in every engine log line); the **request-id** correlates the HTTP request; the bridge line links the two. Engine logs already carried `run`/`step` — this slice did NOT touch them.

## Deliberate non-changes (all honored, Opus-verified)
`event.Event` untouched (events already correlate by run-id); `supervisor.start()` severance preserved; root handler stays `TextHandler` (logs are already greppable `key=value`); engine's existing `e.logger().Error(...)` sites unchanged.

## The 3 Minors (all accepted, non-blocking)
1. The engine + executor tests assert on `slog.TextHandler` `key=value` output — format-coupled, but the spec pins `TextHandler` as a deliberate non-change, so the format is contractually stable. (Contradicting it would fight the spec.)
2. The engine test (`logctx_inject_test.go`) builds `Engine` with nil `Bus`/`Store`/`Metrics`; safe only because the stub executor never emits and `e.logger()`/metrics accessors nil-guard. Pre-existing engine property, not introduced here.
3. (cosmetic, optional) cli.go's `Log` field comment now says "nil ⇒ context logger"; could append "… else discard" for completeness. Reviewer said no action needed. Left as-is.

## LIVE smoke (PASSED 2026-06-20)
Real `magisterd` (sandbox not needed — mock agent, no network), POST a one-step mock flow. Daemon stderr showed BOTH:
- `level=INFO msg="run submitted" req=01KVJX6PK2K9KW1CP06J78BV27 run=01KVJX6PK3SSTMHG73BJNSBR0T`
- `level=INFO msg=request id=01KVJX6PK2K9KW1CP06J78BV27 method=POST path=/v1/runs status=201`

Bridge `req=` == access-log `id=` (request pivot); bridge `run=` == the run-id `cm run` printed (run pivot). This proved the one seam unit tests can't: `main.go` wiring `Server.Log` → the root stderr `TextHandler`. (The executor-side `agent.tool`-correlated warning wasn't triggered — mock agents don't emit and artifact-discovery only warns on failure; that path is covered deterministically by the executor unit tests.) Process note: the `rtk`/proxy `grep` TRUNCATED long log lines, producing a false "NO MATCH" — confirmed the real match by reading the raw daemon.log with the file tool. Don't trust a proxied `grep -q` over ULID-length values; read the raw file.

## Open follow-ups (carried)
- **(still unmerged) `multi-host` GitLab slice** — CODE-COMPLETE at `36eb9fa` (worktree `.worktrees/multi-host` still present), awaiting a live gitlab.com proof (no account yet). See `2026-06-19-multi-host-gitlab-next-steps.md`. **DO NOT merge until that proof passes.**
- **(observability backlog):** JSON log format / a `-log-format` flag (deliberately out of scope here); OTel tracing (needs a dep). The per-agent metric triad, `/metrics`, liveness/readiness, the run-not-found sentinel, and now request/run-scoped logging are all done.
- **(delivery axis):** cross-repo/fork PRs (`owner:branch` head).

## Process notes
- Subagent-driven: haiku implementers for all 4 tasks (the plan carried complete code → transcription); haiku reviewers for Tasks 1/2/4, sonnet for Task 3 (the closure-capture seam); Opus final whole-branch. Every per-task + final review returned zero Critical/Important.
- The plan was tightened before execution: confirmed `runAgent`'s metrics calls (`ObserveAgentRun`/`AddCost`) nil-guard their receiver, so the engine test drops the `metrics` dependency and leaves `Metrics` nil.
- Live smoke earns its keep even for a "pure logging" slice: it's the only thing exercising the real `main.go` → root-stderr wiring (unit tests inject their own buffer loggers). State that rationale rather than skipping.
- Commit hygiene held: single conventional subject, no body, no `Co-Authored-By`, never `--no-verify`. zsh trips on multi-line `$(...)`/`export`+`cd` in the Bash tool — one command per invocation, absolute paths, write flows as files.
