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

## Distributed tracing (optional, OTLP/HTTP-JSON)

Tracing is **off by default** — without `-otel-endpoint` the daemon emits no spans, opens no socket, and the runtime is byte-for-byte unchanged. To enable it, point the daemon at any OTLP collector's HTTP endpoint:

```bash
/tmp/magisterd -addr 127.0.0.1:8137 -db /tmp/cm-demo/magister.db \
  -otel-endpoint http://127.0.0.1:4318 -otel-service-name magisterd
```

`-otel-endpoint` is the collector base URL (`/v1/traces` is appended if absent); the standard `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_SERVICE_NAME` env vars are honored when the flags are unset (flags win). Spans are POSTed as **OTLP-JSON over net/http** (a hand-rolled exporter — no grpc/protobuf dependency, matching the dependency-free Prometheus `/metrics` endpoint). A run produces one connected trace: `POST /v1/runs` → `run <id>` → `step <id>` → `agent <name>` (+ `gate`/`join`, and `push`/`pr`/`ship` under their delivery requests). Run-scoped log lines carry the matching `trace_id`/`span_id`. Point it at a Jaeger/Tempo/Grafana/Honeycomb collector that accepts OTLP-JSON on `:4318`.

## Git-native joins (merge / select / synthesize)

The `merge` join now does a **real `git merge`** of its upstream branches (no longer a manifest). Requirement: a join step **and every step in its `Needs`** must be `workspace: isolated` — that gives each one a `step/<id>` branch to merge. The validator rejects a non-isolated join or upstream, and rejects `merge` + `on_conflict: escalate` without a `join.agent` (the conflict arbiter). Give fan-in upstreams **auto gates** or they default to manual and block.

Zero-cost runnable demo: **`flows/git-native-merge.yaml`** (two isolated `mock` upstreams → `merge` join). Expected SSE: `run.started → step.started×2 → step.done×2 → step.started(integrate) → step.done "merged 2 branch(es)" → run.done`. Confirm a *real* merge (not a manifest): `cm get <run>` shows `integrate`'s artifacts are the merged files of both upstreams, and `git -C <runs>/<run>/base log --graph --all` shows `step/integrate` as a 2-parent merge commit.

## External repo (run against a real git repo)

By default the per-run scratch repo is a synthetic empty base. To run a flow against a **real, pre-existing git repo**, pass `--repo` (and optionally `--base`) at submit:

```
cm run flows/external-repo.yaml --repo /abs/path/to/repo --base main
```

The daemon **clones** `<repo>` read-only into the per-run scratch (it never writes the source), checks out the pinned base commit, and `step/<id>` branches fork from there — so joins produce real, mergeable history over real code. `--base` defaults to the source repo's `HEAD`; it is validated and pinned to a concrete SHA at submit (a bad repo path or unresolvable ref → `400`). The result lives in the scratch clone: `cm get <run>` surfaces its path as the `scratch` field (`<runs>/<run>/base`). Inspect with `git -C <scratch> log --graph --all` — `step/integrate` will be a 2-parent merge commit whose tree contains the cloned base's files **plus** each upstream's work. Zero-cost demo: **`flows/external-repo.yaml`** (same shape as the git-native merge demo; the flow stays repo-agnostic — provisioning comes from `--repo`/`--base`, not the YAML).

### Push the result to a remote (`cm push`)

After an external-repo run succeeds, deliver its result branch to a git **remote**:

```
cm push <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--force]
```

The daemon pushes the run's **result branch** (the terminal step's `step/<id>`, e.g. `step/integrate`; `--step` disambiguates a multi-leaf flow) from the scratch clone to the remote — **default = the source repo's own `origin`** (`--remote` takes a remote name resolved against the source, or a URL) — as **`magister/<run>`** by default (`--as` to rename). It refuses to clobber an existing remote branch unless `--force`. The source repo is never written. **Credentials are the daemon's ambient git environment** (SSH agent / credential helper / cached HTTPS) — none are stored; an auth/network failure returns `502` with git's message. Other errors: `409` if the run hasn't succeeded, `400` not-external-repo / ambiguous result / chosen step has no branch, `404` unknown run / scratch reclaimed. The run lifecycle is untouched — push is an explicit, deliberate post-run action (`POST /v1/runs/{id}/push`). Demo against a local **bare** repo as the remote: `git init --bare /tmp/bare && git -C <src> remote add origin /tmp/bare`, run the flow with `--repo <src>`, then `cm push <run>` and `git -C /tmp/bare log magister/<run>`.

### Open a Pull Request on the pushed branch (`cm pr`)

After pushing with `cm push`, open a GitHub Pull Request from the `magister/<run>` branch:

- `cm pr <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft] [--head-repo <url-or-name>]`
  opens a GitHub Pull Request on the pushed `magister/<runID>` branch of a succeeded
  external-repo run (run `cm push <run>` first). Uses the `gh` CLI with ambient auth
  (no token handling); `owner/repo` is parsed from the source's origin remote. A PR
  that already exists is reported as a 409 with its URL. **Cross-fork (contribute to a
  repo you can't write to):** push the branch to your fork (`cm push <run> --remote
  <fork>`), then `cm pr <run> --head-repo <fork>` opens the PR into upstream with a
  `forkowner:branch` head (the base stays upstream/origin; `--head-repo` is resolved
  like `--remote`).

### Deliver in one command (`cm ship`)

`cm ship <run>` = `cm push` then `cm pr` in one idempotent step (an already-open PR is
reported as `exists`, not an error):

- `cm ship <run> [--remote <url-or-name>] [--head-repo <url-or-name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft] [--force]`
  pushes the result branch then opens (or finds) its PR. **Same-repo:** `--remote` feeds
  both the push destination and the PR base. **Fork ship (`--head-repo <fork>`):** the
  push goes to the fork and the PR opens into upstream with a cross-fork head — i.e.
  `cm ship <run> --head-repo <fork>` is the one-command form of `cm push --remote <fork>`
  + `cm pr --head-repo <fork>` (in fork mode `--remote` overrides only the PR base,
  which defaults to the source origin/upstream). `POST /v1/runs/{id}/ship`.

## Gotchas (each cost real time to learn)

- **`flows/feature-flow.yaml` does NOT run standalone.** It references the unregistered `gemini` agent, `manual` gates (would block on `cm approve`), and pricey `opus`. (Its `integrate` step is now a valid git-native `merge`+`escalate` join — isolated, with a `join.agent` — but the surrounding agents still make it a poor quick smoke.) Use `flows/git-native-merge.yaml` for a mock merge smoke, or write a minimal flow as above.
- **A nested `claude` inherits your global Claude Code SessionStart hooks** — adds startup latency and floods the raw stream with `system`/`hook_started`/`thinking`/`rate_limit_event` lines. Harmless: the daemon's parser ignores every line type except `assistant` (tool_use) and `result`; only milestone + lifecycle events reach SSE.
- **Inside the Claude Code sandbox:** the daemon's `claude` child needs network egress + Keychain access. Launch the daemon (and any direct `claude` smoke) with the sandbox disabled, or it fails on the API call.
- **Port/addr:** daemon defaults to `127.0.0.1:8080`; `cm` targets `$MAGISTER_ADDR` (default `http://127.0.0.1:8080`). Use `-addr` + `MAGISTER_ADDR` to avoid collisions, as above.
- **Multi-line shell here:** `cm`/`curl` chains with `===` banners and parentheses can trip zsh quoting — run the verification commands one per invocation if a heredoc-style block errors with `unmatched "`.

## cm command surface

`cm run <flow.yaml> [--repo <abs-path>] [--base <ref>] [--watch]` · `cm ls` · `cm get <run>` · `cm watch <run>` · `cm approve|reject <run> <step> [reason]` · `cm cancel <run>` · `cm push <run> [--remote <url-or-name>] [--as <branch>] [--step <id>] [--force]` · `cm pr <run> [--remote <url-or-name>] [--head-repo <url-or-name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft]` · `cm ship <run> [--remote <url-or-name>] [--head-repo <url-or-name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft] [--force]`. All target `$MAGISTER_ADDR`. `--repo`/`--base` run the flow against a real git repo; `cm push` delivers its result branch to a remote; `cm pr` opens a GitHub PR on that branch; `cm ship` does both in one idempotent step; `--head-repo <fork>` on `cm pr`/`cm ship` opens a cross-fork PR from your fork into upstream (see *External repo* above).
