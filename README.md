# concentus-magister

An **agent orchestrator**: it routes software tasks to AI coding-agent CLIs
(`claude`, `gemini`, `codex`) along a declarative **DAG flow**, with human or
automated **gates** between steps, isolated git workspaces for parallel work,
fan-in **joins** that merge branches, and one-command **delivery** (push → PR →
ship) to GitHub.

You describe *what* should happen as a YAML pipeline; the daemon runs it,
streams live progress, pauses at gates for approval, and (optionally) opens the
pull request at the end.

```
   flow.yaml ─┐
              ▼
        ┌───────────┐   HTTP + SSE   ┌──────────┐
        │  cm (CLI) │ ─────────────► │ magisterd │  engine ─► agent CLIs (claude/gemini/codex)
        │  client   │ ◄───────────── │  daemon   │  SQLite (runs/events)
        └───────────┘   live events  └──────────┘  git workspaces (per-step branches)
```

- **`magisterd`** — the daemon: DAG engine + SQLite persistence + HTTP/SSE API.
- **`cm`** — the command-line client; every verb is a thin HTTP call.
- **flows** — YAML files describing the pipeline (`flows/` has examples).

**Design:** ports-and-adapters (engine owns policy, adapters own mechanism),
persist-then-publish (state is durably saved before any event is emitted), and a
goroutine-per-step DAG derived from each step's `needs`.

---

## Contents

- [Install](#install)
- [Quickstart (no API keys)](#quickstart-no-api-keys)
- [Concepts](#concepts)
- [Flow YAML reference](#flow-yaml-reference)
- [Agents](#agents)
- [`cm` CLI reference](#cm-cli-reference)
- [Delivery: push / PR / ship](#delivery-push--pr--ship)
- [HTTP API](#http-api)
- [Configuration](#configuration)
- [Operations](#operations)
- [Development](#development)

---

## Install

Requires **Go 1.22**. Build the two binaries:

```bash
go build -o bin/magisterd ./cmd/magisterd
go build -o bin/cm        ./cmd/cm
```

(Put `bin/` on your `PATH`, or call the binaries directly.) The only thing the
daemon needs at runtime is a writable directory for its SQLite DB and scratch
workspaces. Running *real* agents additionally needs the corresponding agent CLI
on `PATH` (see [Agents](#agents)); the built-in `mock` agent needs nothing, so
you can try everything below without any API keys.

---

## Quickstart (no API keys)

**1. Start the daemon** (loopback, no auth, SQLite in the current dir):

```bash
./bin/magisterd
# listening on 127.0.0.1:8080
```

**2. Write a flow** using the keyless `mock` agent — `hello.yaml`:

```yaml
name: hello
steps:
  - id: greet
    agent: mock
    prompt: say hello
    gate: { policy: auto, verifier: { command: "true" } }
```

**3. Run it and watch live progress** (in another terminal):

```bash
./bin/cm run hello.yaml --watch
```

`--watch` streams the run's events (step started/succeeded, gate decisions, …)
over Server-Sent Events until the run ends. Without it, `cm run` prints the run
id and returns immediately; you can attach later with `cm watch <id>`.

**4. Inspect:**

```bash
./bin/cm ls            # list runs
./bin/cm get <run-id>  # full run state (steps, statuses, artifacts)
```

To use real coding agents, swap `agent: mock` for `opus`, `sonnet`, `gemini`, or
`codex` and provide that CLI's credentials (see [Agents](#agents)).

---

## Concepts

- **Flow** — a whole pipeline (a `name` + a list of `steps`), loaded from YAML.
- **Step** — a DAG node. Either an **agent step** (runs a coding agent) *or* a
  **join step** (merges upstream branches) — never both. Edges come from
  `needs`: fan-out = several steps sharing a dependency, fan-in = one step
  depending on several. The engine derives parallelism from the graph;
  `concurrency` caps how many run at once.
- **Gate** — sits *after* a step and decides whether the flow proceeds:
  `manual` (a human approves via `cm approve`), `auto` (a shell **verifier**
  command must exit 0), or `conditional` (an expression over the step result).
- **Workspace** — `shared` (all steps work in one directory) or `isolated` (the
  step gets its own git branch, enabling safe parallel work). Join steps and
  their upstreams **must** be `isolated`.
- **Join** — a fan-in step that combines upstream branches: `merge` (git-merge
  them, escalating real conflicts to a human or an arbiter agent), `select`
  (an arbiter picks one), or `synthesize` (an arbiter resolves conflicts).
- **Retry & self-repair** — a step with a `retry` policy re-runs on failure. If
  an `auto` gate's verifier fails and `on_fail: retry` is set, the verifier's
  **output is fed back to the agent** on the next attempt, so it fixes the
  specific failure instead of repeating it (always on; no extra config).

---

## Flow YAML reference

```yaml
name: my-pipeline        # required
concurrency: 4           # max steps running at once; 0 (default) = unlimited

steps:
  - id: plan             # required; unique; [A-Za-z0-9._-], not starting with '-'
    needs: []            # step ids this depends on (no duplicates, no self, no cycles)
    agent: opus          # which agent to run (see Agents); mutually exclusive with `join`
    role: planner        # optional role label folded into the prompt
    prompt: |            # the task text; an agent step needs at least a role or a prompt
      Plan the work.
    workspace: shared    # shared | isolated  (default: shared)
    timeout: 10m         # optional Go duration ("30s","5m"); 0 = no timeout
    retry:               # optional
      max: 3             # >= 1
      backoff: 5s        # >= 0; Go duration
    gate:                # optional; defaults to a manual gate
      policy: manual     # manual | auto | conditional
```

**Field rules** (enforced at submit time by the validator — an invalid flow is
rejected before anything runs):

### Gate

```yaml
gate:
  policy: manual                       # human approval via `cm approve`/`reject`
# policy: auto                         # run a shell verifier; exit 0 = pass
  verifier: { command: "go test ./..." }
# policy: conditional                  # evaluate an expression over the step result
  condition: { expr: 'result.cost_usd < 1.0 && result.summary contains "OK"' }
  on_fail: abort                       # abort (default) | retry | escalate
```

- `auto` requires a non-empty `verifier.command`.
- `conditional` requires a `condition.expr` that compiles to a boolean. The
  expression sees `result` with fields `cost_usd` (float), `summary` (string),
  and `artifacts` (list of paths).
- `on_fail: retry` requires a `retry` policy. `on_fail: escalate` converts a
  failed `auto`/`conditional` gate into a human approval (approve continues,
  reject aborts).

### Join (fan-in step — set `join`, not `agent`)

```yaml
- id: integrate
  needs: [impl-a, impl-b]
  workspace: isolated          # required for joins and their upstreams
  join:
    strategy: merge            # merge | select | synthesize
    agent: opus                # arbiter agent; required for select/synthesize,
                               #   and for merge when on_conflict: escalate
    on_conflict: escalate      # abort | retry | escalate
  gate: { policy: manual }
```

- `merge` git-merges every upstream branch; a true conflict follows
  `on_conflict`. `escalate` resolves it via the arbiter agent + a human gate,
  then continues merging the remaining branches.
- `select` / `synthesize` always require an arbiter `agent`.

### Enum values at a glance

| Field | Values |
|---|---|
| `workspace` | `shared`, `isolated` |
| `gate.policy` | `manual`, `auto`, `conditional` |
| `gate.on_fail` / `join.on_conflict` | `abort`, `retry`, `escalate` |
| `join.strategy` | `merge`, `select`, `synthesize` |

See `flows/feature-flow.yaml` (plan → parallel impl → merge-join), and
`flows/external-repo.yaml` / `flows/git-native-merge.yaml` for worked examples.

---

## Agents

The agent named in a step maps to an entry in the daemon's executor registry
(`cmd/magisterd/main.go`):

| `agent:` | Backed by | Prerequisites |
|---|---|---|
| `mock` | built-in stub | none (deterministic; great for trying flows/tests) |
| `opus` | `claude` CLI, model `opus` | `claude` on `PATH` + `ANTHROPIC_API_KEY` |
| `sonnet` | `claude` CLI, model `sonnet` | `claude` on `PATH` + `ANTHROPIC_API_KEY` |
| `gemini` | `gemini` CLI (`gemini-2.5-pro`) | `gemini` on `PATH` + its auth |
| `codex` | `codex` CLI | `codex` on `PATH` + ChatGPT OAuth **or** `OPENAI_API_KEY` |

The daemon inherits its environment, so export the relevant API keys before
starting `magisterd`. To add or re-map agents, edit the `agents()` registry.

---

## `cm` CLI reference

`cm` talks to the daemon at `$MAGISTER_ADDR` (default `http://127.0.0.1:8080`).

```
cm run <flow.yaml> [--repo <path>] [--base <ref>] [--watch]   submit a flow
cm ls                                                          list runs
cm get <run>                                                   full run state
cm watch <run>                                                 stream live events (SSE)
cm approve <run> <step> [reason]                               release a manual/escalated gate
cm reject  <run> <step> [reason]                               reject a gate (aborts the run)
cm cancel <run>                                                cancel an active run
cm retry  <run> [--watch]                                      resume a failed/canceled run in place
cm push   <run> [--remote <url|name>] [--as <branch>] [--step <id>] [--force]
cm pr     <run> [--remote <url|name>] [--head-repo <url|name>] [--as <branch>] [--step <id>] [--base <branch>] [--title <t>] [--body <b>] [--draft]
cm ship   <run> [--remote ...] [--head-repo ...] [--as ...] [--step ...] [--base ...] [--title ...] [--body ...] [--draft] [--force]
cm gc     [--older-than <dur>]                                 reclaim terminal-run scratch now
cm rm     <run>                                                reclaim one run's scratch now
cm loglevel [debug|info|warn|error]                            get/set the daemon log level at runtime
```

- `cm run --repo <path>` provisions the run's workspace from an existing git
  repo; `--base <ref>` picks the base branch/commit.
- `retry` resumes the **same** run id, reusing its scratch and skipping
  already-succeeded steps.

---

## Delivery: push / PR / ship

After a run produces a branch (an isolated step's committed work), you can
deliver it without leaving the CLI. These need the [`gh`](https://cli.github.com)
CLI authenticated for PR creation.

```bash
cm push <run> --remote git@github.com:you/repo.git --as my-feature   # push the branch
cm pr   <run> --base main --title "My feature"                       # open a PR
cm ship <run> --base main --title "My feature"                       # push + PR in one step
```

**Forks / cross-repo:** `--head-repo <fork>` opens the PR as
`forkowner:branch` into the upstream (and `ship --head-repo` pushes to the fork
and opens the cross-fork PR in one command).

---

## HTTP API

`cm` is a thin wrapper over a small REST surface (auth-protected routes under
`/v1`; operational routes are open):

| Method & path | Purpose |
|---|---|
| `POST /v1/runs` | submit a flow (body = flow YAML; `?repo=&base=` optional) |
| `GET /v1/runs` | list runs |
| `GET /v1/runs/{id}` | full run state |
| `DELETE /v1/runs/{id}` | cancel an active run |
| `GET /v1/runs/{id}/events` | **SSE** live event stream (supports `Last-Event-ID` replay) |
| `POST /v1/runs/{id}/steps/{step}/approve` | release a gate |
| `POST /v1/runs/{id}/push` · `/pr` · `/ship` | delivery |
| `POST /v1/runs/{id}/retry` | resume a failed run |
| `POST /v1/gc` · `DELETE /v1/runs/{id}/scratch` | scratch reclamation |
| `GET`/`POST /v1/loglevel` | read/adjust log level |
| `GET /healthz` · `/readyz` · `/metrics` | liveness · readiness · Prometheus metrics |

The **SSE stream** is the key integration point for any UI: it pushes every run
event as it happens, so a dashboard never polls.

---

## Configuration

`magisterd` flags (all optional; defaults shown):

| Flag | Default | Purpose |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | listen address (loopback by default) |
| `-db` | `magister.db` | SQLite database path |
| `-scratch-ttl` | `24h` | reclaim a terminal run's scratch this long after it ends (`0` disables) |
| `-scratch-sweep-interval` | `1h` | how often the scratch janitor sweeps |
| `-shutdown-timeout` | `10s` | graceful shutdown deadline |
| `-shutdown-drain` | `0` | keep serving (`/readyz`=503) this long after shutdown begins, for LB drain |
| `-log-format` | `text` | `text` or `json` |
| `-log-level` | `info` | `debug`/`info`/`warn`/`error` (also adjustable at runtime via `cm loglevel`) |
| `-otel-endpoint` | `` (off) | OTLP/HTTP collector endpoint for traces, e.g. `http://collector:4318` |
| `-otel-service-name` | `magisterd` | OpenTelemetry `service.name` |

**Environment:**
- `MAGISTER_BEARER_TOKEN` — if set, `/v1` requires `Authorization: Bearer <token>`
  (empty = no auth, relying on the loopback bind as the trust boundary).
- `MAGISTER_ADDR` — the daemon base URL `cm` targets.
- `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_SERVICE_NAME` — env equivalents of the
  `-otel-*` flags (an explicit flag wins over the env).
- agent credentials (`ANTHROPIC_API_KEY`, etc.) are inherited by the daemon.

> **Note:** `cm` does **not** currently send a bearer token, so it only works
> against a token-less (loopback) daemon. If you enable `MAGISTER_BEARER_TOKEN`,
> drive the API with a client that sends the `Authorization` header (e.g. `curl`).

---

## Operations

- **Retry/resume:** `cm retry <run>` resumes a failed/canceled run in place
  (same id, reused scratch, succeeded steps skipped).
- **Scratch GC:** a TTL janitor reclaims terminal runs' workspaces; force it with
  `cm gc [--older-than <dur>]` (all) or `cm rm <run>` (one).
- **Health:** `/healthz` (liveness), `/readyz` (readiness, flips to 503 during
  drain). **Metrics:** Prometheus at `/metrics` (per-agent calls/cost/latency,
  in-flight gauges). **Tracing:** off by default; set `-otel-endpoint` to emit
  one connected trace per run (`POST /v1/runs` → run → step → agent/gate/join).
- **Logging:** structured, request/run-scoped correlation, runtime-adjustable
  level (`cm loglevel debug`).

---

## Development

```bash
go test ./...            # full suite
go test -race ./...      # with the race detector (some tests exec real git)
go build ./...           # build everything
```

Some engine/executor/workspace tests shell out to real `git`; run them outside a
restrictive sandbox if exec is blocked. Internal design specs, plans, and
session handoffs live under `docs/superpowers/`.
