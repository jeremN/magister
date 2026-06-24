# Handoff — `cm tui` live terminal dashboard: MERGED to LOCAL main, NOT PUSHED (2026-06-24)

**Start here next session.** The **`cm tui` slice is DONE and MERGED to LOCAL `main`** — `main` at **`4426f13`** (merge commit `merge: cm tui terminal dashboard`). **NOT pushed** — `origin/main` is still **`30dfa51`**; local `main` is ahead by the 7-commit TUI slice **plus 3 unpushed doc commits** (README `3143baa`, tui spec `cc7fd68`, tui plan `8cafe9f`). Full `go test -race ./...` = **480 passed / 20 packages** on merged main, `gofmt` clean, **`go.mod` still `go 1.22`**, only-new-dep `golang.org/x/term v0.27.0` (direct, tidy-clean). Final Opus whole-branch review **Ready-to-merge = Yes, 0 Critical / 0 Important / 5 Minor** (1 added as a zero-risk hardening `da7339d`; the other 4 are recorded follow-ups below). Worktree (`.claude/worktrees/tui`) removed, branch `worktree-tui` deleted (its commits are reachable from `4426f13` — verified `merge-base --is-ancestor da7339d main`). Only the `multi-host` worktree remains (untouched — still blocked on a gitlab.com MR proof).

## What shipped

The **first interaction surface beyond the `cm` CLI**: a full-screen terminal dashboard to watch runs live and act on the interactive essentials. Pure REST+SSE client, **ZERO daemon changes**. Spec `docs/superpowers/specs/2026-06-24-cm-tui-dashboard-design.md`, plan `docs/superpowers/plans/2026-06-24-cm-tui-dashboard.md`.

- New `cm tui` subcommand (no third binary; reuses `MAGISTER_ADDR`).
- **Left pane:** runs list (polled `GET /v1/runs` ~1.5s). **Right pane:** focused run's steps + a live SSE event log. **Bottom bar:** contextual keys + connection indicator.
- **Keys:** `j/k` select · `enter` open · `a` approve · `r` reject (one-line reason editor) · `c` cancel (`y/n` confirm) · `R` retry · `q` quit. Approve/reject enabled only on a gate-awaiting step; retry only on a terminal run; cancel only on an active run.
- **Auth:** reads `MAGISTER_BEARER_TOKEN` and sends `Authorization: Bearer …` on every request **including the SSE GET** — closes the gap `cm` itself has (cm sends no token).

## Architecture — the Elm split, hand-rolled (no Bubble Tea)

Pure core, each its own file + TDD; thin imperative shell on top. `internal/tui/`:

- `term.go` — `golang.org/x/term` wrappers (`size`, `enterRaw`+restore). Build-verified only (thin OS wrappers).
- `client.go` — typed REST client: `ListRuns`/`GetRun`/`Approve`/`Cancel`/`Retry` + bearer auth. httptest-tested. DTOs decode only the rendered subset (`RunSummary{id,name,status}`, `StepView{id,status,attempt}`, `RunDetail`).
- `sse.go` — `parseEvents` (`id:/event:/data:` frames → `internal/event.Event`, reused not redefined) + `StreamEvents` (reuses `Client.newReq` for auth; `Last-Event-ID` resume).
- `model.go` — the `model` + **PURE** `update(model, msg) (model, []cmd)` reducer. Messages: `runsLoaded`/`runSnapshot`/`sseEvent`/`keyMsg`/`actionResult`/`connMsg`. Commands: `cmdFocus`/`cmdRefresh`/`cmdApprove`/`cmdCancel`/`cmdRetry`/`cmdQuit`. Modal state machine: `modeNormal`/`modeConfirmCancel`/`modeReason` (modal keys checked BEFORE normal keys).
- `view.go` — pure `view(m, w, h) string`, exactly `h` rows ≤ `w` runes. `clip` is rune-aware (the key bar's `·` is multi-byte U+00B7) and total for any width.
- `driver.go` — raw mode, input/poll/SIGWINCH goroutines fanning into one `chan any`, the `runLoop(ctx, msgs, exec, render)` core (testable with injected channels — no TTY) + `Run` (the terminal wiring). `streamLoop` reconnects SSE with a 500ms backoff. Manually-verified only.
- `cmd/cm/main.go` — `case "tui"` → `tui.Run(base, os.Getenv("MAGISTER_BEARER_TOKEN"))`; usage line updated.

**The load-bearing design decision:** two distinct commands for "re-read run X". `cmdFocus` (on `enter`) cancels the prior subscription and REOPENS the SSE stream; `cmdRefresh` (emitted on every lifecycle SSE event) is **snapshot-only** and must NOT touch the stream. Collapsing them would make each event tear down + reconnect the very stream delivering it — a self-amplifying storm. `TestSseEventAppendsToLogAndRefreshes` asserts `cmdRefresh` AND `!cmdFocus` on a lifecycle event, guarding the regression.

## THE DURABLE LESSON — pre-resolve wire contracts against the REAL source, not the plan

The plan's reducer/view/**tests** all guarded the gate-blocked step status as `"awaiting"`/`"gate.awaiting"`. The daemon actually emits **`"awaiting_gate"`** (`core.StepAwaitingGate`, `internal/core/state.go:27`; the run-detail DTO ships it raw via `string(st.Status)`). `"gate.awaiting"` is an event KIND, never a step status. Because the plan's TEST and IMPL were transcribed from the SAME plan, they encoded the SAME wrong literal — so TDD's "watch it fail" failed for the right reason (undefined symbol) then passed against a fixture that was itself wrong, and the suite goes 100% green **while approve/reject + the gate-highlight are silently dead against the real daemon**. A shared-fixture blind spot; only an external oracle (the daemon's `core.StepStatus` constants) breaks the tie. Caught by the controller grepping `internal/core` BEFORE dispatching the reducer task, then injecting a correction (a `statusAwaitingGate = "awaiting_gate"` const reused by model+view; tests arrange that exact string). Generalized into `sdd-process-lessons` memory lesson 4. (Terminal `RunStatus` `succeeded/failed/canceled` and the `approveRequest{approve,reason}` tags DID match — verified, not assumed.)

## Commits (`687ddba..da7339d`, merged via `4426f13`)

- `687ddba` `feat(tui): add golang.org/x/term preflight and terminal wrappers` (Task 1; amended to drop a spurious `// indirect`).
- `0a9d1ba` `feat(tui): REST client for runs, approve, cancel, retry` (Task 2).
- `a843ccc` `feat(tui): SSE reader with Last-Event-ID resume` (Task 3).
- `aa48b1c` `feat(tui): model and pure reducer for runs, gates, actions` (Task 4; carries the `awaiting_gate` correction).
- `b44184b` `feat(tui): pure full-screen view renderer` (Task 5; amended to fix a narrow-terminal width-overflow + regression test).
- `6d618ad` `feat(tui): terminal driver and cm tui subcommand` (Task 6).
- `da7339d` `fix(tui): make clip total for non-positive width` (final-review hardening).

## Review arc

Per-task: implementers haiku (Task 1, transcription) / sonnet (Tasks 2–6); reviewers sonnet. Task 5 review found 1 **Important** — the assembled body row wasn't clipped to `w`, so it overflowed at narrow widths (my own pre-flight clamp had fixed the *panic* but not the ≤w invariant). Fixed via `clip(l+" "+r, w)` + `TestViewFitsNarrowTerminal`, re-reviewed Approved. Two accepted Task-5 deviations from the plan: `clip` made rune-aware, and the left column renders run **Name** (fallback ID) because the plan's own test demanded Name. **Opus whole-branch review = Ready-to-merge Yes** — verified all 6 cross-task seams (command/message protocol completeness — every reducer cmd has a driver handler and vice-versa; status strings vs the daemon; auth through SSE; `cmdFocus`/`cmdRefresh`; concurrency: single-writer `focusCancel`, correct `chan any` fan-in, deferred terminal restore on every exit path). Its one new finding (a `clip` negative-width panic) it PROVED unreachable through `view` via a width/height sweep → applied as the 1-line `da7339d` hardening anyway.

## NOT DONE — needs a human at a real TTY

The **interactive manual smoke was not run.** A subagent / headless agent cannot drive a raw-mode terminal (`enterRaw` needs a real TTY), so the live end-to-end path is unverified. Before relying on it, one human pass:

```
# start the daemon (however you run it), submit a run with a manual gate, then:
cm tui
# verify: runs list appears; 'enter' opens a run; events stream live; a gate-awaiting
# step highlights; 'a' releases the gate; 'r' opens the reason editor and rejects;
# 'c' then 'y' cancels an active run; 'R' retries a failed run; 'q' restores the terminal cleanly.
```

The non-TTY logic IS covered: `internal/tui` = 26 tests (client httptest, SSE framing, the full reducer + modal state machine, view goldens incl. narrow-terminal, the `runLoop` smoke).

## Open follow-ups (non-blocking, recorded — the SDD ledger was discarded with the worktree)

1. **SIGWINCH reuses `connMsg(true)`** purely to force a redraw, but the reducer reads it as "connected" → a resize briefly (≤1.5s, until the next poll) flips the "disconnected" indicator. Cosmetic. Clean fix: a dedicated `redrawMsg` whose reducer arm returns `m, nil` unchanged.
2. **`StreamEvents` doesn't check HTTP status** → a 404/500 parses as an empty stream and returns nil; combined with `streamLoop`'s unconditional 500ms reconnect, a focused terminal/deleted run reconnects ~2/s until the user navigates away (bounded, no data loss). A real fix needs reducer→driver terminal-status signalling (pairs naturally with #1's small message-vocabulary extension).
3. **Minor reducer coverage gaps:** the `runsLoaded` sel-clamp branch, the `canceled`/`succeeded` retry-guard disjuncts, and the `connMsg` arm have no direct test (all correct by inspection). No `Retry` client-level httptest (symmetry gap).
4. **Docs:** the README (`3143baa`) documents the daemon/flows/CLI but predates `cm tui` — add a `cm tui` section when convenient.

## Next-thread menu

- **PUSH** local `main` to origin (`origin/main 30dfa51..4426f13`) — carries the TUI slice **+ the README + tui spec/plan**. Gated on an explicit user "push" (the harness rejects an ambiguous reply for a default-branch push).
- **Run the manual TTY smoke** (above) and record the result.
- **Close follow-up #1+#2 together** (the `redrawMsg` + terminal-status signal) as a small slice.
- **A `cm tui` README section** (follow-up #4).
- **Unblock the multi-host GitLab slice** (`.worktrees/multi-host` @ `36eb9fa`) — still needs a live gitlab.com MR proof (user has no gitlab.com account yet; handoff `2026-06-19-multi-host-gitlab-next-steps.md`).
- Pinned deps unchanged: modernc.org/sqlite v1.36.1, goose/v3 v3.24.1, oklog/ulid/v2 v2.1.1, expr-lang/expr v1.17.8, OTel v1.32.0, **golang.org/x/term v0.27.0** (do not bump past go-1.22-compatible).
