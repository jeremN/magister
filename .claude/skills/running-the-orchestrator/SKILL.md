---
name: running-the-orchestrator
description: Use when launching or driving the concentus-magister app (the magisterd daemon + cm CLI) to run a flow end-to-end — e.g. confirming a change works in the running app, watching live SSE events, or reproducing agent.tool milestone streaming against a real (claude) or mock agent.
---

# Running the concentus-magister orchestrator

## Overview

The app is two binaries: **`magisterd`** (daemon: engine + SQLite store + HTTP/SSE API) and **`cm`** (thin HTTP client). You start the daemon, POST a flow YAML, and watch events stream over SSE. "Running the app" = a flow reaching `run.done` with the events you expected on the wire.

## Prerequisites

- **`git` on PATH** — the daemon's `GitManager` makes a scratch git repo + worktree per run. No git ⇒ runs fail.
- **For `opus`/`sonnet` agents:** `claude` on PATH **and authenticated**. The daemon passes `os.Environ()` to the child, so either `ANTHROPIC_API_KEY` is set, or `claude` is logged in (macOS stores the token in the **Keychain** — no env var needed). Verify: `claude -p hi --model sonnet --output-format json`.
- **For a zero-cost / no-network smoke:** use `agent: mock` instead — runs with no keys, no network. Caveat: `mock` never emits `agent.tool` milestones; only real CLI agents do.

## Run it (verified recipe)

```bash
# 1. Build the two binaries
go build -o /tmp/magisterd ./cmd/magisterd
go build -o /tmp/cm ./cmd/cm

# 2. Start the daemon on a throwaway DB + non-default port.
#    Defaults if omitted: addr 127.0.0.1:8080, db ./magister.db. runs/ lands next to -db.
mkdir -p /tmp/cm-demo
/tmp/magisterd -addr 127.0.0.1:8137 -db /tmp/cm-demo/magister.db >/tmp/cm-demo/magisterd.log 2>&1 &

# 3. Wait until it is listening
until curl -sf http://127.0.0.1:8137/v1/runs >/dev/null; do sleep 0.3; done

# 4. Write a minimal flow: one sonnet step, auto-gate (no manual approval), and an
#    explicit prompt that makes the agent use exactly one tool (Write).
tee /tmp/cm-demo/stream-demo.yaml >/dev/null <<'EOF'
name: stream-demo
concurrency: 1
steps:
  - id: greet
    agent: sonnet                 # or `mock` for a no-cost, no-network smoke (emits no agent.tool)
    role: implementer
    prompt: "Create a file named notes.txt in the current directory containing exactly this single line: concentus streamed this. Do nothing else afterward, then stop."
    gate: { policy: auto, verifier: { command: "true" } }   # auto-passes, no manual approval
EOF

# 5. Submit (prints the run id) and capture it
export MAGISTER_ADDR=http://127.0.0.1:8137
RID=$(/tmp/cm run /tmp/cm-demo/stream-demo.yaml)
echo "run: $RID"

# 6. Follow it: replays persisted events from the store, then streams live until run.done
/tmp/cm watch "$RID"
```

(Interactive one-shot: `cm run <flow> --watch` submits AND streams in one command — but use the capture form above when later steps need `$RID`.)

Expected SSE (a `sonnet` run): `run.started → step.started → agent.tool (Write: …/notes.txt) → step.done → run.done`. The `agent.tool` frame arrives **mid-step**, seconds before `step.done` — that is the live-streaming feature working.

## Verify it actually worked

`$RID` and `$MAGISTER_ADDR` are already set from the Run-it block.

```bash
/tmp/cm watch "$RID"                                          # replays ALL events from the STORE (proves persistence)
curl -s -H "Last-Event-ID: 2" "$MAGISTER_ADDR/v1/runs/$RID/events"   # resume: replays seq>2 only
/tmp/cm get "$RID"                                            # status + per-step artifacts
cat /tmp/cm-demo/runs/$RID/base/notes.txt                    # the agent's real edit
```

`cm get` should show `"status":"succeeded"` and `notes.txt` under the step's `artifacts` (discovered via `git status --porcelain`).

## Teardown

```bash
pkill -TERM -f /tmp/magisterd    # SIGTERM = graceful shutdown (drains, then exits 143/144)
rm -rf /tmp/cm-demo
```

## Git-native joins (merge / select / synthesize)

The `merge` join now does a **real `git merge`** of its upstream branches (no longer a manifest). Requirement: a join step **and every step in its `Needs`** must be `workspace: isolated` — that gives each one a `step/<id>` branch to merge. The validator rejects a non-isolated join or upstream, and rejects `merge` + `on_conflict: escalate` without a `join.agent` (the conflict arbiter). Give fan-in upstreams **auto gates** or they default to manual and block.

Zero-cost runnable demo: **`flows/git-native-merge.yaml`** (two isolated `mock` upstreams → `merge` join). Expected SSE: `run.started → step.started×2 → step.done×2 → step.started(integrate) → step.done "merged 2 branch(es)" → run.done`. Confirm a *real* merge (not a manifest): `cm get <run>` shows `integrate`'s artifacts are the merged files of both upstreams, and `git -C <runs>/<run>/base log --graph --all` shows `step/integrate` as a 2-parent merge commit.

## Gotchas (each cost real time to learn)

- **`flows/feature-flow.yaml` does NOT run standalone.** It references the unregistered `gemini` agent, `manual` gates (would block on `cm approve`), and pricey `opus`. (Its `integrate` step is now a valid git-native `merge`+`escalate` join — isolated, with a `join.agent` — but the surrounding agents still make it a poor quick smoke.) Use `flows/git-native-merge.yaml` for a mock merge smoke, or write a minimal flow as above.
- **A nested `claude` inherits your global Claude Code SessionStart hooks** — adds startup latency and floods the raw stream with `system`/`hook_started`/`thinking`/`rate_limit_event` lines. Harmless: the daemon's parser ignores every line type except `assistant` (tool_use) and `result`; only milestone + lifecycle events reach SSE.
- **Inside the Claude Code sandbox:** the daemon's `claude` child needs network egress + Keychain access. Launch the daemon (and any direct `claude` smoke) with the sandbox disabled, or it fails on the API call.
- **Port/addr:** daemon defaults to `127.0.0.1:8080`; `cm` targets `$MAGISTER_ADDR` (default `http://127.0.0.1:8080`). Use `-addr` + `MAGISTER_ADDR` to avoid collisions, as above.
- **Multi-line shell here:** `cm`/`curl` chains with `===` banners and parentheses can trip zsh quoting — run the verification commands one per invocation if a heredoc-style block errors with `unmatched "`.

## cm command surface

`cm run <flow.yaml> [--watch]` · `cm ls` · `cm get <run>` · `cm watch <run>` · `cm approve|reject <run> <step> [reason]` · `cm cancel <run>`. All target `$MAGISTER_ADDR`.
