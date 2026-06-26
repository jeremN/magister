# Handoff — `cm tui` driver hardening: MERGED + worktree cleaned; ONLY the manual TTY smoke remains (resume next week, 2026-06-26)

**Start here next week.** The **`cm tui` driver-hardening slice is DONE, MERGED, and the worktree is cleaned up.** Code-complete, Opus-reviewed (ready-to-merge YES), full `-race` green, **PR #1 merged into origin/main**, **local `main` reconciled**, and the `worktree-tui-driver-hardening` worktree+branch **removed**. The single remaining item is the **manual TTY smoke** — it needs a human at a real terminal (a headless agent cannot drive raw mode). The smoke was *staged* on 2026-06-26 (daemon + two parked runs) but **the user left before driving it**; that staging is ephemeral and will NOT survive to next week, so **re-stage from scratch using the verified recipe below**.

- **PR #1 MERGED:** https://github.com/jeremN/magister/pull/1 — merge commit `ebd2e9d` on origin/main (`15eee70..ebd2e9d`).
- **Local `main` reconciled** then carried further handoff commits; current local-`main` tip is the "PR #1 merged" handoff commit (`fc90244`) plus this update. Merged `main` re-verified: `go build ./...` clean, `go test ./internal/tui/` green (and `redrawMsg`/`streamStatusError` confirmed present on `main`).
- Local `main` is AHEAD of origin/main by the doc commits (spec `55d3d71`, plan `284e11e`, the handoffs, + the reconcile merge `d0a2801`) — these stay unpushed unless the user explicitly pushes `main` (a direct push to the default branch is harness-gated and needs an explicit user "push").

## Resume (slice merged + worktree cleaned — ONLY the smoke remains)

1. **Re-stage and drive the manual TTY smoke** (verified recipe in "THE TTY SMOKE" below) and record the result here.
2. (Optional) push local `main`'s doc commits to origin — needs an explicit user "push".
3. (Optional, unrelated) the pre-existing cm-tui follow-ups: add a `cm tui` README section; unblock the multi-host GitLab slice (`.worktrees/multi-host` @ `36eb9fa`, still needs a live gitlab.com MR proof).

## Branch / commit state (all landed)

- **The 2 code commits are on origin/main and local `main`** (via PR #1 merge `ebd2e9d`):
  - `e487fc2` `fix(tui): use a dedicated redraw message on resize, not connMsg(true)` (Task 1)
  - `6e04c52` `fix(tui): stop SSE reconnect storm on a non-2xx events response` (Task 2)
- **Worktree REMOVED.** `.claude/worktrees/tui-driver-hardening` and branch `worktree-tui-driver-hardening` were deleted via `ExitWorktree action:remove discard_changes:true` — "discard" only dropped the redundant branch pointer; both commits are ancestors of `main` (verified `git merge-base --is-ancestor 6e04c52 main` = YES). Remaining worktree: only `multi-host` (untouched).
- **Local `main`** is ahead of `origin/main` by the doc commits (spec `55d3d71`, plan `284e11e`, the handoffs, + reconcile merge `d0a2801`) — unpushed (default-branch push is harness-gated).

## What the slice does (spec `docs/superpowers/specs/2026-06-26-tui-driver-hardening-design.md`, plan `…/plans/2026-06-26-tui-driver-hardening.md`)

Closes follow-ups #1 and #2 from the post-cm-tui handoff. `internal/tui/` only.

- **#1 — redraw message.** SIGWINCH (resize) sent `connMsg(true)` purely to force a frame; the reducer reads `connMsg` as daemon-reachability, so a resize while disconnected briefly flipped the indicator to "connected". Fix: new `redrawMsg struct{}` with reducer arm `return m, nil` (pure no-op — `runLoop` renders on every message); SIGWINCH now sends `redrawMsg{}`. `conn` is now owned solely by the ~1.5s poll loop.
- **#2 — bounded SSE reconnect.** `StreamEvents` handed the body to `parseEvents` without checking the HTTP status, so a non-2xx (404 run-gone / 5xx) read as a clean empty stream → `streamLoop` hammer-reconnected at ~2/s. Fix: new typed `streamStatusError{Status int}` in `sse.go`; `StreamEvents` checks `resp.StatusCode/100 != 2` before parsing and returns it (body still closed via the existing `defer`, `emit` never called); `streamLoop` stops reconnecting via a **bare, status-agnostic** `errors.As(err, &se)` guard (404 AND 5xx both stop). Transport errors (`hc.Do` failed) and clean EOF stay retryable — preserving daemon-down recovery.

### The load-bearing rationale (do NOT "fix" this later)

The post-cm-tui handoff's #2 proposed **reducer→driver terminal-status signalling**. That was based on a wrong premise and is **intentionally NOT built**. Traced: the daemon (`internal/api/sse.go:56`) closes the SSE stream on `run.done`, so a *terminal* run ends with a **clean EOF**, not a non-2xx → `streamLoop` reconnects once and the daemon then holds an **idle** stream open. That idle connection is **load-bearing**: it is how pressing `R` (retry) on a focused run streams the *resumed* run's events live (retry reuses the same run id; the daemon drains the new higher-seq events onto the still-open connection). Closing the stream on terminal would regress retry-rewatch. So terminal runs do **not** storm — only a genuinely *refusing* 404/5xx does, which is exactly what this slice fixes. (Stop-policy choice: "any non-2xx", accepted by the user — a transient 5xx stops the focused stream until re-entry, in exchange for a hard no-storm guarantee.)

## Verification done

- `gofmt -l internal/tui/` clean; `go build ./...` clean.
- **`go test -race ./...` = all 20 packages PASS** (controller-run, not just implementer-claimed), incl. 3 new `internal/tui` tests: `TestRedrawMsgPreservesConnAndEmitsNoCommands`, `TestStreamEventsNon2xxReturnsTypedErrorWithoutEmitting` (404 + 500), `TestStreamLoopStopsOnNon2xx` (concurrent: goroutine + 5s safety ctx + atomic hit counter; reviewer confirmed non-flaky).
- Per-task reviews: both Spec ✅ / quality Approved (sonnet). **Final whole-branch review (Opus): Ready-to-merge YES, 0 Critical / 0 Important.** It independently verified the non-obvious seam — dropping SIGWINCH's `connMsg(true)` does NOT strand the indicator, because `conn=true` is independently restored by the poll loop's `runsLoaded` arm + `initialModel`.

## THE TTY SMOKE (the only remaining item) — VERIFIED re-stage recipe

A headless agent cannot drive raw mode (`enterRaw` needs a real TTY), so this is human-only. The staging below was run & verified on 2026-06-26 (binaries built, daemon up on `:8139`, two runs parked at a manual gate) — but it is ephemeral, so re-run it. Uses `agent: mock` (zero-cost, no network, no keys).

**1. Stage it** (run these from the repo root; the daemon start may need the sandbox disabled for process/port/git):

```bash
go build -o /tmp/magisterd ./cmd/magisterd && go build -o /tmp/cm ./cmd/cm
mkdir -p /tmp/cm-tui-smoke
/tmp/magisterd -addr 127.0.0.1:8139 -db /tmp/cm-tui-smoke/magister.db >/tmp/cm-tui-smoke/magisterd.log 2>&1 &
until curl -sf http://127.0.0.1:8139/v1/runs >/dev/null 2>&1; do sleep 0.3; done
cat > /tmp/cm-tui-smoke/tui-smoke.yaml <<'YAML'
name: tui-smoke
concurrency: 1
steps:
  - id: greet
    agent: mock
    role: implementer
    prompt: "Say hello, then stop."
    gate: { policy: manual }       # parks the step at status "awaiting_gate"
YAML
export MAGISTER_ADDR=http://127.0.0.1:8139
/tmp/cm run /tmp/cm-tui-smoke/tui-smoke.yaml   # submit twice -> two parked runs (one to approve, one to cancel+retry)
/tmp/cm run /tmp/cm-tui-smoke/tui-smoke.yaml
/tmp/cm ls                                     # both should show "running" (step greet is awaiting_gate)
```

**2. Drive it — in YOUR OWN terminal** (NOT via the `!` prefix; raw mode needs a real TTY). No bearer token needed (this daemon has no `-auth-token`):

```
MAGISTER_ADDR=http://127.0.0.1:8139 /tmp/cm tui
```

| Action | Key | Expect |
|---|---|---|
| select runs (left pane) | `j`/`k` | `>` cursor moves |
| open selected run | `enter` | right pane: `greet  awaiting_gate  <-- approve?` highlighted + live EVENTS log |
| **approve** (run 1) | `a` | gate releases → `step.done` → `run.done`; run → `succeeded`, events stream live |
| **reject editor** | `r` | bar → `reject reason: _`; type text, `enter` submits / `esc` aborts |
| **cancel** (run 2) | `c` then `y` | bar `cancel run …? (y/n)`; run → `canceled` |
| **retry** (canceled run 2) | `R` | run resumes, re-runs `greet`, parks at the gate again |
| **quit** | `q` | TUI exits, terminal **restored cleanly** (no garbled prompt / stuck raw mode) |

**3. The two behaviors THIS slice changed — the point of the smoke:**

- **NEW #1 (redraw — the headline fix):** with `cm tui` open, stop the daemon from another terminal (`pkill -TERM -f /tmp/magisterd`). Within ~1.5s the top-right indicator flips to `(disconnected)`. **Resize the terminal window — it must STAY `(disconnected)`** (before this fix it briefly flipped back to `(connected)` until the next poll). Restart the daemon (same command as step 1) to reconnect.
- **NEW #2 (bounded reconnect):** hard to stage by hand (runs aren't deletable, so you can't easily make a focused run's `/events` return 404). Left to the unit test `TestStreamLoopStopsOnNon2xx`, which proves `streamLoop` stops reconnecting on a non-2xx. Nothing to do interactively.

**4. Teardown:** `pkill -TERM -f /tmp/magisterd && rm -rf /tmp/cm-tui-smoke`

**Record the result** (esp. NEW #1's resize behavior and the clean `q` terminal-restore) back into this handoff / a memory note once driven.

## Open follow-ups (the 3 review Minors — all triaged NON-blocking by the Opus final review)

1. `model_test.go TestRedrawMsgPreservesConnAndEmitsNoCommands` covers only `conn:false`; a symmetric `conn:true`→stays-true case is structurally redundant (`return m, nil`) — skip or add for completeness.
2. `driver_test.go TestStreamLoopStopsOnNon2xx` exercises only 404 at the loop level (500 is covered at the `StreamEvents` layer; the guard is status-agnostic). Parameterizing 404+500 adds no real path coverage.
3. `sse_test.go` doesn't directly assert `streamStatusError.Error()` string contents (type + `Status` are checked). Cosmetic.

Plus the pre-existing cm-tui follow-ups still open: the README has no `cm tui` section yet; and the multi-host GitLab slice (`.worktrees/multi-host` @ `36eb9fa`) is still blocked on a live gitlab.com MR proof.

## SDD process notes (this run)

- Native `.claude/worktrees/tui-driver-hardening` via `EnterWorktree` (base `fresh` = origin/main 15eee70). Ledger at `.claude/worktrees/tui-driver-hardening/.superpowers/sdd/progress.md` (git-ignored scratch — lost on `git clean -fdx`; recover from `git log`).
- Implementers haiku (both tasks are transcription — plan carried complete code); per-task reviewers sonnet; final whole-branch reviewer Opus. All clean, no fix loops needed.
- The push needs the sandbox disabled for network (`dangerouslyDisableSandbox`) — but it failed on DNS regardless: the environment was fully offline at handoff time, not a sandbox restriction.
