# Handoff — TTY smoke DRIVEN: it caught a real raw-mode render bug (CRLF), now fixed (2026-06-29)

**Start here.** The manual TTY smoke for the `cm tui` slice — the single item left open since 2026-06-26 — was finally **driven on a real terminal on 2026-06-29**, and it did exactly what a manual smoke is for: it **caught a real rendering bug that every unit test and all 3 review rounds had missed.** The bug is now **fixed, test-guarded, and committed on a branch.** The base TUI walkthrough renders and the keys work (user confirmed: "good").

## The bug the smoke caught

**Symptom:** launching `cm tui` showed only the bottom key bar — header, RUNS list, and detail/EVENTS pane were all blank/garbled.

**Root cause:** `term.MakeRaw(fd)` (in `internal/tui/term.go`) puts the terminal in raw mode, which disables `OPOST`/`ONLCR` — the kernel's automatic `\n`→`\r\n` translation. But the renderer joined its rows with a bare `\n`:
- `view.go` → `strings.Join(rows, "\n")`
- `driver.go` render closure → `os.Stdout.WriteString(view(...))` wrote that verbatim to the raw terminal.

With `ONLCR` off, a bare `\n` is a pure line-feed: cursor moves **down one line but keeps the same column**. So every row started printing where the previous row ended — a diagonal "staircase" that marched off the right edge. Only the tail near the bottom landed readably → "only the keys at the bottom."

**Why nothing caught it earlier:** `view()` is unit-tested as a *pure string* and correctly uses `\n` (tests even `strings.Split(out, "\n")`). The bug lived only at the *terminal write* in `driver.go`, which has no TTY test. The one thing that exercises the real write is the manual TTY smoke — which had never been run until now. (This is the durable lesson — see the memory note `sdd-process-lessons`.)

## The fix (committed, not merged, not pushed)

- New pure helper `frame(m, w, h)` in `internal/tui/view.go`: returns `"\x1b[H\x1b[2J"` (home + clear) + `view(...)` with `\n`→`\r\n` translated. CRLF translation is a property of *writing to a raw terminal*, so it lives at the write boundary; **`view()` is untouched** (its tests depend on `\n`).
- `driver.go` render closure now writes `frame(m, w, h)` (one line, replacing the two-write `\x1b[H\x1b[2J` + `view`).
- Regression test `TestFrameUsesCRLFForRawTerminal` (RED→GREEN): asserts the frame homes+clears first, contains no bare `\n` once CRLFs are stripped, and has exactly 23 CRLF separators for a 24-row frame.
- **Verification:** `gofmt -l internal/tui/` clean; `go build ./...` clean; **full `go test -race ./...` green across all 20 packages.**
- **Commits:** `8314a7a` `fix(tui): use CRLF line endings when rendering to the raw terminal` (3 files: `view.go`, `driver.go`, `view_test.go`) + `f3cbbe0` `test(tui): derive expected CRLF count from the view's newline count` (the one actionable review Minor).
- **Opus whole-branch review (final gate): Ready-to-merge YES — 0 Critical / 0 Important, 3 Minor.** Confirmed the `\n`→`\r\n` translation covers 100% of rendered rows (view emits only `\n`, no stray `\r` — `reasonBuf` is printable-ASCII-filtered), the seam keeps `view()` pure, and the regression test genuinely fails if `frame` reverts. The 2 non-actionable Minors (cursor not hidden via `\x1b[?25l`; `\x1b[2J` now strictly redundant) are pre-existing and left for the manual-smoke follow-ups.
- **MERGED to local `main` via fast-forward → `main` now `f3cbbe0`** (branch `fix-tui-raw-mode-crlf` deleted; commits preserved). **NOT pushed** — `main` is ahead of `origin/main` (`ebd2e9d`) by 11 commits (4 from this session: `8314a7a`/`6a1e3f9`/`f3cbbe0`/the `3cb3ed9` handoff-update + 7 prior unpushed doc/merge commits); default-branch push is harness-gated and needs an explicit user "push".

## Smoke result

Driven via the re-staged recipe (daemon `:8139`, two `mock` runs parked at a manual gate, fixed `/tmp/cm`):
- **Base walkthrough renders and works** — user confirmed "good" after the CRLF fix. The full layout (header / RUNS list / detail+EVENTS / key bar) draws correctly.
- **NEW #1 (resize stays `(disconnected)`)** — the headline behavior of the driver-hardening slice. Re-confirm explicitly if not already eyeballed: with `cm tui` open, `pkill -TERM -f /tmp/magisterd` → indicator flips `(disconnected)` within ~1.5s → resize the window → it must **stay** `(disconnected)`.
- **NEW #2 (bounded reconnect on non-2xx)** — not hand-stageable; covered by `TestStreamLoopStopsOnNon2xx`.

## What's left

1. ~~Integrate the branch~~ **DONE** — Opus-reviewed (ready-to-merge YES) and merged to local `main` (`f3cbbe0`). Only remaining integration step: **push `main` to origin** when you're ready (harness-gated, needs explicit "push") — `origin/main` is at `ebd2e9d` (PR #1 merge) and is now 11 commits behind local.
2. (If not already done) explicitly confirm **NEW #1** resize-stays-disconnected on the real TTY.
3. **Teardown** the smoke staging when done: `pkill -TERM -f /tmp/magisterd && rm -rf /tmp/cm-tui-smoke`.

## Still-open, unrelated (carried from prior handoffs)

- Local `main` is ahead of `origin/main` by doc commits (spec/plan/handoffs) — unpushed (default-branch push is harness-gated).
- `cm tui` README section: still not written.
- Multi-host GitLab slice: code-complete but **unmerged** at `36eb9fa` (`.worktrees/multi-host`) — blocked on a live gitlab.com MR proof (no gitlab.com account yet).
