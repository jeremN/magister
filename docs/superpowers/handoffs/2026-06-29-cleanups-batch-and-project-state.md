# Handoff — `cm tui` line fully closed + a maintenance-cleanups batch shipped; only the GitLab slice remains (2026-06-29)

**Start here.** As of 2026-06-29 the entire `cm tui` line of work is **done, merged, pushed, and TTY-verified**, and a follow-on **maintenance-cleanups batch is also merged + pushed**. `origin/main` == local `main` == **`75845ba`**, working tree clean, 0 unpushed. The **only** substantial open work left on the whole project is the **multi-host GitLab slice** (code-complete, unmerged, blocked on a live gitlab.com MR proof — no gitlab.com account yet).

## Repo state (authoritative)

- **`origin/main` == local `main` == `75845ba`**, clean, 0 unpushed (verified via `git push` + `git rev-list --count origin/main..main` = 0).
- Worktrees: the main checkout + `.worktrees/multi-host` (branch `multi-host` @ `36eb9fa`, untouched). No other worktrees/branches.
- Full `go test -race ./...` green across all 20 packages on `75845ba`.

## What landed today (2026-06-29), newest first

### 2. Maintenance-cleanups batch — `c0ac99a..75845ba`, MERGED + PUSHED

A 4-item minor-followup batch. **Not a feature slice** — no spec/plan; done as branch (`followup-cleanups`) → implement → Opus whole-branch review → FF-merge → push, branch deleted.

1. **`Workspace.For` now takes `ctx`** (`refactor(workspace)` `33c18d5`). It was the only `core.Workspace` method without a `ctx`, and used `context.Background()` internally → the lazy clone / worktree creation was uninterruptible. Threaded through: interface `internal/core/ports.go`, both impls (`GitManager.For` uses it; `Manager.For(_ context.Context, …)` ignores it — local `mkdir` only, mirrors the existing `Reclaim(_ context.Context)` precedent), the lone production caller `internal/engine/engine.go:290` passes its live `ctx`, ~20 test call sites, + a new `TestGitManagerForCanceledCtxAborts` asserting `errors.Is(err, context.Canceled)` (a pre-canceled ctx makes `git init` in `ensureRepo` return `Canceled` deterministically — the process never starts).
2. **TUI cursor-hide + drop redundant `\x1b[2J`** (`fix(tui)` `52c297f`). Enter is now `\x1b[?1049h\x1b[?25l` (the exit defer already restored the cursor with `\x1b[?25h`); `frame()` dropped the per-frame screen-clear because `view()` repaints every cell anyway (less flicker). **This made "every rendered row is EXACTLY `w` cells" load-bearing** (a too-narrow row would now leave stale cells), so `TestViewFitsNarrowTerminal` was tightened from `n > w` to `n != w` (Opus review's one meaningful Minor).
3. **`validate_test.go` precision** (`test(flow)` `c0ac99a`). The "agent with role only" positive case left the `Prompt` that `baseFlow()` sets, so it didn't actually test role-without-prompt; added `f.Steps[0].Prompt = ""`.
4. **README `cm tui` section** (`docs` `aa24ff1`) + Contents entry + CLI-block line.

Review-fix commit `75845ba` applied Opus Minors #1/#2/#3/#5 (exact row width; `ctx.Canceled` assert; README "top-bar after the title" wording; a `clip` rune-vs-display-column awareness note). Skipped #4 (the doc's "failed/canceled" is accurate as user intent; the daemon enforces it). **Opus whole-branch review = ready-to-merge YES, 0 Critical / 0 Important, 5 Minor.**

**⚠️ One thing NOT verified:** the cursor-hide / no-flicker change is **unit-tested + Opus-analyzed only — NOT TTY-eyeballed** (the user chose to skip the manual smoke). The next time anyone opens `cm tui` on a real terminal, confirm: the hardware cursor is hidden (not parked/blinking at the key bar), and frames don't visibly flash/clear on each ~1.5s poll. Low risk, but unconfirmed by eye.

### 1. The CRLF render-bug fix + TTY smoke — `8314a7a..ee1a329`, MERGED + PUSHED (see prior handoff)

The manual TTY smoke (the item left open since the driver-hardening slice) was finally driven and **caught a real raw-mode render bug**: `term.MakeRaw` disables `ONLCR`, but the renderer wrote bare `\n`, so rows stair-stepped off-screen (only the bottom bar visible). Fixed with a `frame()` helper translating `\n`→`\r\n` at the write boundary (`view()` stays pure) + regression test `TestFrameUsesCRLFForRawTerminal`. **NEW #1 (resize-stays-`(disconnected)`) — CONFIRMED PASS on a real TTY.** Full detail in `docs/superpowers/handoffs/2026-06-29-tty-smoke-crlf-fix.md`.

## What's open (the whole project)

**The multi-host GitLab slice — the only substantial remaining work.** Code-complete in worktree `.worktrees/multi-host` (branch `multi-host` @ `36eb9fa`), **unmerged**. Blocked on a **live gitlab.com MR proof** — last known status: no gitlab.com account yet. Do NOT merge until a real gitlab.com MR smoke passes (per the older handoff `docs/superpowers/handoffs/2026-06-19-multi-host-gitlab-next-steps.md`). When you have a gitlab.com account, that smoke is the next thing to unblock.

Tiny, truly-optional leftovers (non-blocking, recorded for completeness):
- The cursor-hide TTY eyeball above.
- TUI cosmetics never pursued: none outstanding beyond the eyeball.

## How this work was done (process, for continuity)

- Build slices via brainstorm → writing-plans → subagent-driven-development in a worktree; **Opus ALWAYS does the final whole-branch review** before merge. Small maintenance batches (like today's #2) skip the brainstorm/plan and go branch → implement → Opus review → merge.
- Run recipe for the app: project skill `.claude/skills/running-the-orchestrator/`. Zero-cost smoke uses `agent: mock` (no keys, no network). The TUI manual smoke needs a human at a real TTY (raw mode) — a headless/subagent can't drive it.
- Pinned go-1.22 deps — do not bump: modernc.org/sqlite v1.36.1, pressly/goose/v3 v3.24.1, oklog/ulid/v2 v2.1.1, expr-lang/expr v1.17.8, golang.org/x/term v0.27.0, OTel `otel`/`otel/trace`/`otel/sdk` v1.32.0 (stdlib-only OTLP-JSON exporter; do NOT add any `…/exporters/otlp/…` module).
- Durable SDD process lessons live in memory `sdd-process-lessons` (newest = lesson 5: manual smokes of un-automatable I/O boundaries are load-bearing — the CRLF bug is the worked example).
- A direct push to the default branch is harness-gated and needs an explicit user "push".
