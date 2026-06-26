# Handoff — `cm tui` driver hardening: DONE + REVIEWED locally, PUSH/PR BLOCKED on offline GitHub (2026-06-26)

**Start here next session.** The **`cm tui` driver-hardening slice is code-complete, reviewed clean (Opus: ready-to-merge YES), and fully `-race` green locally — but NOT merged and NOT pushed.** The push to `origin` failed: `github.com` / `api.github.com` do not resolve from this environment (DNS timeout — GitHub/network down on 2026-06-26, confirmed via `curl`, `ping`, and `gh api /rate_limit`). Everything is parked safely on disk. Resume by pushing when connectivity returns, then opening the PR, then running the still-unrun **manual TTY smoke** (the user's stated next step: "driver slice first, then drive the tty smoke").

## Exact resume commands (run when GitHub is reachable)

The user already chose a **code-only PR** (the 2 code commits; spec/plan stay on local `main` and get pushed separately later). Remote branch name: `tui-driver-hardening` (drops the `worktree-` prefix).

```
# 1. push the feature branch (local name -> clean remote name)
git push -u origin worktree-tui-driver-hardening:tui-driver-hardening

# 2. open the PR (base = main; origin/main == the branch base 15eee70, so the
#    PR diff is exactly the 2 code commits)
gh pr create --base main --head tui-driver-hardening \
  --title "fix(tui): driver hardening — redraw message + bounded SSE reconnect" \
  --body "Two isolated fixes in internal/tui/ (no daemon changes, no new deps). (1) A terminal resize sends a dedicated no-op redrawMsg instead of connMsg(true), so resizing while disconnected no longer briefly fakes 'connected'. (2) StreamEvents now returns a typed non-2xx error and streamLoop stops reconnecting on it, ending the ~2/s reconnect storm against a 404/5xx events endpoint; transport errors and clean EOF stay retryable. Spec/plan: docs/superpowers/{specs,plans}/2026-06-26-tui-driver-hardening*. Opus whole-branch review: ready-to-merge. Full go test -race ./... green."
```

**Alternative if the user prefers no-network now:** local merge instead of a PR — `git -C <main-checkout> merge worktree-tui-driver-hardening` (clean: disjoint files vs main's doc commits), then `git worktree remove`/`ExitWorktree` to clean up, then push `main` later. The user was offered this (Option 1) and chose the PR path; only fall back if they change their mind.

## Branch / commit state (all local, nothing on origin yet)

- **Feature branch:** `worktree-tui-driver-hardening` @ **`6e04c52`**, base **`15eee70`** (= current `origin/main`). Worktree **PRESERVED** at `.claude/worktrees/tui-driver-hardening` (do not remove — needed for PR iteration / the smoke).
  - `e487fc2` `fix(tui): use a dedicated redraw message on resize, not connMsg(true)` (Task 1)
  - `6e04c52` `fix(tui): stop SSE reconnect storm on a non-2xx events response` (Task 2)
- **Local `main`** is ahead of `origin/main` (15eee70) by the slice docs (NOT on the branch, NOT on origin):
  - `55d3d71` `docs(tui): spec for driver hardening …`
  - `284e11e` `docs(tui): implementation plan for driver hardening`
  - + this handoff commit.
- After the PR merges to origin/main, local `main` (docs) and origin/main (code) reconcile with a normal `git pull` merge — no data loss, no duplication.

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

## STILL UNRUN — the manual TTY smoke (the user's next step)

A subagent/headless agent cannot drive raw mode (`enterRaw` needs a real TTY), so the live path is human-only. Carry over the base smoke from the cm-tui handoff, PLUS two new behaviors this slice introduces:

```
# start the daemon, submit a run with a manual gate, then:
cm tui
# base (from cm-tui slice): runs list appears; 'enter' opens a run; events stream live;
#   a gate-awaiting step highlights; 'a' approves; 'r' opens the reason editor + rejects;
#   'c' then 'y' cancels an active run; 'R' retries a failed run; 'q' restores the terminal.
# NEW #1 (redraw): with the daemon STOPPED so the bar shows "disconnected", resize the
#   terminal — the indicator must STAY "disconnected" (previously it flipped to
#   "connected" for ~1.5s until the next poll).
# NEW #2 (bounded reconnect): focus a run, then make its events endpoint 404 (e.g. point
#   at a deleted/unknown run id) — the TUI must NOT spin reconnecting; the stream just
#   goes quiet (re-enter the run to re-arm). Harder to stage by hand; the unit test
#   TestStreamLoopStopsOnNon2xx already proves the logic.
```

## Open follow-ups (the 3 review Minors — all triaged NON-blocking by the Opus final review)

1. `model_test.go TestRedrawMsgPreservesConnAndEmitsNoCommands` covers only `conn:false`; a symmetric `conn:true`→stays-true case is structurally redundant (`return m, nil`) — skip or add for completeness.
2. `driver_test.go TestStreamLoopStopsOnNon2xx` exercises only 404 at the loop level (500 is covered at the `StreamEvents` layer; the guard is status-agnostic). Parameterizing 404+500 adds no real path coverage.
3. `sse_test.go` doesn't directly assert `streamStatusError.Error()` string contents (type + `Status` are checked). Cosmetic.

Plus the pre-existing cm-tui follow-ups still open: the README has no `cm tui` section yet; and the multi-host GitLab slice (`.worktrees/multi-host` @ `36eb9fa`) is still blocked on a live gitlab.com MR proof.

## SDD process notes (this run)

- Native `.claude/worktrees/tui-driver-hardening` via `EnterWorktree` (base `fresh` = origin/main 15eee70). Ledger at `.claude/worktrees/tui-driver-hardening/.superpowers/sdd/progress.md` (git-ignored scratch — lost on `git clean -fdx`; recover from `git log`).
- Implementers haiku (both tasks are transcription — plan carried complete code); per-task reviewers sonnet; final whole-branch reviewer Opus. All clean, no fix loops needed.
- The push needs the sandbox disabled for network (`dangerouslyDisableSandbox`) — but it failed on DNS regardless: the environment was fully offline at handoff time, not a sandbox restriction.
