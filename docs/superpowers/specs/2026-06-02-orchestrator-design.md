# concentus-magister — Agent Orchestrator Design

- **Date:** 2026-06-02
- **Status:** Approved (design); ready for implementation planning
- **Author:** Jérémie Nehlil (with Claude)

## 1. Context & goals

`concentus-magister` ("conductor / master of harmony") is a single-node, personal
**agent orchestrator**: it routes tasks to AI coding-agent CLIs (Claude, Codex,
Gemini, …) according to declarative *flows*, executing them as a DAG with
approval/verification **gates** between steps.

It is the production-grade successor to the phase-1 `orch` prototype. That
prototype established good seams (an `Agent` interface, mode-as-injected-`Approver`,
filesystem handoff, DAG-derived-from-`needs`, an append-only journal) but was
CLI-invoked, in-memory, single-run, with unbounded fan-out and an aspirational
(unimplemented) resume story. This design keeps the proven ideas and rebuilds
around them as a long-running service.

### Scope decision

- **Build now:** a **long-running service (daemon)** that orchestrates local
  agent CLIs, exposes an HTTP/SSE API, persists state, resumes after crash, and
  runs multiple flows concurrently.
- **Design for, don't build:** generalization into a pluggable *workflow engine*
  (executors other than CLIs). The `Executor` port is the seam that makes this
  additive later — to be pursued only once the service is proven.
- **v1 feature target:** the **full phase-2 vision** — core engine + service +
  robustness (retries/on_fail, timeouts, real-agent cost parsing, worktrees) +
  conditional gates + agent-arbitrated joins. Built incrementally (M0→M5 below),
  but all in scope.

### Non-goals (explicit YAGNI)

- No distributed / multi-node scheduling. Single binary, single machine.
- No multi-tenancy / user accounts. Trust boundary is the loopback interface.
- No web UI. CLI client + API only.
- No `/metrics` Prometheus endpoint in v1 (seam left, not built).
- No exactly-once step execution (see §7 resume caveat).

## 2. Locked decisions

| Area | Decision |
|---|---|
| Deployment | Single node, personal/local. API bound to `127.0.0.1` by default. |
| Persistence | **SQLite** (state tables + append-only `events` table). Pure-Go driver. |
| API transport | **HTTP/JSON + SSE** (stdlib `net/http`), thin CLI client, gate approval via POST. |
| Concurrency | Goroutine-per-step DAG executor + **global weighted semaphore** + per-run cap. |
| Internal architecture | **Approach A** — ports & adapters core + in-process event bus, refined so the `Store` is a synchronous port (persist-then-publish). |
| Condition language | **`expr-lang/expr`** (sandboxed, compile-ahead). |
| Escalation | `on_fail: escalate` converts a failed automated gate into a human approval. |
| Retry model | One unified attempt budget (`Retry.Max`) covers both execution and gate retries. |
| Resume | At-least-once (re-run in-flight steps on restart); documented limitation. |

## 3. Architecture

### Two binaries

- **`magisterd`** — the daemon: owns engine, store, HTTP/SSE API; holds all run state.
- **`cm`** — thin CLI client: pure HTTP calls, no orchestration logic.

(Binary names are placeholders, easily renamed.)

### Package layout

```
concentus-magister/
├── cmd/
│   ├── magisterd/main.go      # server: config, wiring, lifecycle, graceful shutdown
│   └── cm/main.go             # client: subcommands over HTTP
├── internal/
│   ├── flow/                  # DOMAIN: schema, Parse, Validate (DAG invariants, cycle detection)
│   ├── core/                  # DOMAIN CORE: engine, supervisor, run/step state, ports.go
│   ├── event/                 # domain Event types + in-process Bus (publish/subscribe)
│   ├── gate/                  # Approver + Verifier + condition evaluator
│   ├── join/                  # JoinStrategy: merge / select / synthesize
│   ├── executor/              # ADAPTER → core.Executor: CLIAgent (+stream-json), MockAgent
│   ├── workspace/             # ADAPTER → core.Workspace: shared dir / git worktree (+teardown)
│   ├── store/                 # ADAPTER → core.Store: SQLite, migrations, events table
│   ├── api/                   # ADAPTER: HTTP/SSE handlers, SSE hub, DTOs
│   └── config/                # flags/env/file loading + validation
├── flows/                     # example *.yaml
├── migrations/                # SQL
└── docs/superpowers/specs/
```

**Dependency rule:** `core` and `flow` import nothing outward. Adapters import
`core` to implement its interfaces. `event` is self-contained; `core` publishes
to it, adapters subscribe. No import cycles.

### Ports (`internal/core/ports.go`)

```go
type Executor  interface { Run(ctx context.Context, t Task) (Result, error) }
type Workspace interface { For(s *flow.Step) (dir string, cleanup func() error, err error) }
type Publisher interface { Publish(e event.Event) }
type Store     interface { /* see §8 */ }
```

The engine takes these as struct fields (constructor injection). It never imports
`store`, `api`, `os/exec`, or `git`.

### Runtime data flow

1. `cm run flows/feature.yaml` → **POST /v1/runs** → api `flow.Validate`s, persists
   `Run{status: pending}`, hands it to the supervisor, returns the run ID.
2. The engine starts the goroutine-per-step DAG executor. Each step waits on its
   deps' channels, acquires the **global semaphore + per-run cap**, calls
   `Executor.Run`, and emits events.
3. The event bus fans each event to independent subscribers: **SSE hub** (live
   clients) and **cost/metrics aggregator**. Durable state is written separately
   and synchronously through the `Store` port (§7).
4. `cm watch <run>` → **GET /v1/runs/{id}/events** (SSE) streams the live journal.
5. A manual/escalated gate emits `gate.awaiting` and blocks on an approval channel;
   `cm approve <run> <step>` → **POST .../approve** resolves it.
6. **Resume:** on startup `magisterd` loads incomplete runs and continues them.

## 4. Domain model & flow schema

### Flow YAML (evolved from phase-1)

```yaml
name: feature-flow
concurrency: 4                      # per-run cap (0 = bounded only by the global semaphore)

steps:
  - id: plan
    agent: opus
    role: planner
    timeout: 5m
    gate: { policy: manual, on_fail: abort }

  - id: impl-api
    needs: [plan]
    agent: sonnet
    role: implementer
    workspace: isolated             # own git worktree, torn down after
    retry: { max: 3, backoff: 2s }  # exponential + jitter
    gate:
      policy: auto
      verifier: { command: "go test ./..." }
      on_fail: retry

  - id: pick-impl
    needs: [impl-api, impl-ui, impl-db]
    join:
      strategy: select              # merge | select | synthesize
      agent: opus                   # arbiter (required for select/synthesize)
      on_conflict: escalate
    gate:
      policy: conditional
      condition: { expr: 'result.cost_usd < 1.0 && result.summary contains "OK"' }
      on_fail: escalate
```

### Go types (`internal/flow`)

```go
type Flow struct {
    Name        string
    Concurrency int
    Steps       []*Step
}

type Step struct {
    ID        string
    Needs     []string
    Agent     string        // executor registry key; empty iff Join != nil
    Role      string
    Prompt    string        // optional inline template; default built from role+inputs
    Workspace WSMode        // "shared" | "isolated"
    Timeout   Duration
    Retry     *RetryPolicy  // nil = no retry
    Join      *Join         // non-nil => fan-in step
    Gate      Gate
}

type RetryPolicy struct { Max int; Backoff Duration }      // exponential w/ jitter
type Gate struct {
    Policy    GatePolicy    // "manual" | "auto" | "conditional"
    Verifier  *Verifier     // required iff Policy == auto
    Condition *Condition    // required iff Policy == conditional
    OnFail    FailPolicy    // "abort" | "retry" | "escalate" (default abort)
}
type Condition struct { Expr string; program *vm.Program } // compiled at Validate time
type Join struct {
    Strategy   JoinStrategy  // "merge" | "select" | "synthesize"
    Agent      string        // arbiter; required for select/synthesize
    OnConflict FailPolicy
}
```

### State machines (typed enums, persisted as strings)

- **Run:** `pending → running → {succeeded | failed | canceled}`
- **Step:** `pending → ready → running → [awaiting_gate] → {succeeded | failed | skipped | canceled}`,
  with a `retrying` sub-state looping `running → retrying → running` until `Retry.Max`.

`skipped` is reserved for future conditional routing; in v1 it is produced only by
cancellation. It lives in the schema now so adding routing later is not a migration.

### Validation (`flow.Validate`) — fail fast at parse time

Everything phase-1 checked (unique IDs, `needs` resolve, agent-or-join, auto needs
verifier, cycle detection via white/gray/black DFS) **plus**:

- `select`/`synthesize` joins **require** an arbiter `agent`.
- `conditional` gates **require** a `condition`, **compiled now** via `expr-lang/expr`
  (a bad expression fails the submit, never a running step).
- `retry.max ≥ 1` when set; `timeout ≥ 0`; `concurrency ≥ 0`.
- **Strict YAML decode** (unknown keys error) — fixes a phase-1 wart.

By the time a flow reaches the engine, every invariant the engine relies on is
guaranteed. The engine is total.

## 5. Execution engine & concurrency

### Structure

```go
type Supervisor struct {                 // one per daemon; manages all active runs
    engine    *Engine
    runs      map[RunID]*runExec          // mu-guarded
    approvals *ApprovalRegistry           // pending gates, keyed by run/step
}

type Engine struct {                      // stateless config; shared across runs
    execs  map[string]core.Executor       // "opus"→CLIAgent, …, "mock"→MockAgent
    ws     core.Workspace
    gate   *gate.Evaluator
    joins  map[flow.JoinStrategy]join.Strategy
    store  core.Store                     // SYNCHRONOUS, transactional — durable truth
    pub    core.Publisher                 // in-memory event bus — live fan-out only
    sem    *semaphore.Weighted            // GLOBAL cap, shared across all runs
    clock  core.Clock                     // injected; tests don't sleep
}
```

### Step lifecycle (one goroutine per step, per run)

```
wait on dep channels  ─▶  acquire per-run cap ─▶ acquire global sem ─▶ [execute + gate] ─▶ release both ─▶ close done
   (no token held)         (buffered chan)        (semaphore.Weighted)   (the attempt loop)
```

**Deadlock avoidance is structural:** a step waits on its dependency channels
*first*, holding no token, and acquires the semaphore only around the executor
call. No step ever holds a slot while waiting for another step (no hold-and-wait).
A wide-fan-in-under-tiny-semaphore test guards this.

An **attempt** = execute + gate:

1. `ctx, cancel = context.WithTimeout(runCtx, step.Timeout)` (if set).
2. `executor.Run(ctx, task)` — task carries upstream artifacts gathered under mutex.
3. **persist-then-publish** the `running → done` provisional result.
4. `gate.Evaluate(...)` → manual / auto(command) / conditional(`expr`).
5. Decide outcome per the failure model.

### Failure model — `step.Retry` × `gate.OnFail`, unified as attempts

| Failure | Policy | Behaviour |
|---|---|---|
| executor errors, or gate fails | `retry` (attempts < Max) | back off (exponential + jitter via injected `Clock`), loop a fresh attempt |
| retries exhausted, or policy is `abort` | `abort` | return error → **first-error-cancels** the run's context → siblings unwind to `canceled` |
| gate fails | `escalate` | emit `gate.awaiting{escalated:true}`, register pending approval, block until `cm approve/reject` → approve continues, reject aborts |

Manual gates use the same `ApprovalRegistry` block-on-channel mechanism as escalation.

### Cancellation & shutdown

- `cm cancel <run>` cancels that run's context; goroutines bail at `select`/`ctx.Err()`
  checks; steps → `canceled`, run → `canceled`.
- Daemon `SIGTERM` cancels all run contexts, flushes the store, drains the SSE hub,
  and exits within a shutdown deadline.

## 6. Persist-then-publish (Approach A refinement)

We chose SQLite *state* (not event-sourcing), so the durable truth is written
**synchronously** by the engine through the `Store` port; the live event bus is
in-memory and lossy. The discipline:

1. `store.SaveStepTransition(...)` — one transaction writing the step row **and**
   the corresponding `events` rows. Durable.
2. `pub.Publish(event)` — async fan-out to the SSE hub + metrics.

A live SSE frame therefore can never reference state not yet durable, and a dropped
live frame is harmless: the `events` table is the system of record, and SSE clients
re-sync via REST snapshot + `Last-Event-ID` replay.

## 7. Resume

On daemon startup, `store.LoadIncompleteRuns()` → for each run, rebuild the DAG and
reconcile from persisted step status:

- `succeeded` → channel pre-closed; **result/artifacts loaded from the store** so
  downstream steps still receive their inputs.
- `pending`/`ready` → re-queued normally.
- `awaiting_gate` → re-emit `gate.awaiting`, re-block (the human's pending approval
  is not lost).
- `running`/`retrying` at crash → **re-run from a fresh attempt.**

**Known limitation — at-least-once, not exactly-once.** A step whose agent had
already run and written files gets re-run on resume. True exactly-once is impossible
with opaque external CLIs; v1 accepts re-execution, and `workspace: isolated` (fresh
worktree per attempt) contains the blast radius.

## 8. Persistence (`internal/store`)

Driver: **`modernc.org/sqlite`** (pure-Go, no cgo). Opened with **WAL mode**,
`busy_timeout`, `foreign_keys=on`. SQLite has a single writer, so the writer handle
runs `SetMaxOpenConns(1)` with a separate read pool — a design constraint, not an
afterthought. Queries via **`sqlc`** (compile-time-checked, parameterized → no
injection surface). Migrations embedded with `//go:embed`, applied by **`goose`** on
startup.

```sql
runs(id PK, name, flow_yaml, status, concurrency, created_at, updated_at, error)
steps(run_id FK, id, status, attempt, summary, cost_usd, workdir, started_at, ended_at, error,
      PRIMARY KEY(run_id,id))
artifacts(run_id, step_id, path)                       -- inputs survive resume
events(seq PK AUTOINCREMENT, run_id, step_id, kind, summary, cost_usd, attempt, at)
```

`flow_yaml` stored verbatim = the resume source of truth. `events.seq` doubles as
the **SSE replay cursor**: a reconnecting client sends `Last-Event-ID: <seq>`; we
replay missed rows, then attach to the live bus.

```go
type Store interface {
    CreateRun(ctx, Run) error
    SaveStepTransition(ctx, RunID, StepState, []Event) error   // ONE tx
    SetRunStatus(ctx, RunID, Status, error) error
    LoadIncompleteRuns(ctx) ([]RunState, error)
    GetRun(ctx, RunID) (RunState, error)
    ListRuns(ctx, Filter) ([]RunSummary, error)
    EventsSince(ctx, RunID, seq int64) ([]Event, error)
}
```

## 9. API (`internal/api`) — stdlib `net/http`, Go 1.22 routing

```
POST   /v1/runs                                  submit flow → {id}
GET    /v1/runs                                  list (filter by status)
GET    /v1/runs/{id}                             snapshot (run + steps)
DELETE /v1/runs/{id}                             cancel
GET    /v1/runs/{id}/events                      SSE (Last-Event-ID replay)
POST   /v1/runs/{id}/steps/{step}/approve        {approve:bool, reason?}
GET    /healthz
```

- Routing via stdlib `ServeMux` method+wildcard patterns — **no router dependency**.
- Middleware: request ID → `slog` logging → panic recovery → auth → timeouts.
- **Trust boundary = the loopback interface.** Default bind `127.0.0.1`; optional
  static **bearer token** (constant-time compare) for LAN binds. No cookies → no CSRF
  surface. `http.MaxBytesReader` on bodies; server read/write timeouts; graceful
  shutdown. Sane security headers even absent a browser surface. Binding publicly is
  documented as an explicit "harden here" point, not silently supported.

## 10. CLI (`cmd/cm`)

`cm run <flow.yaml> [--watch]` · `cm ls` · `cm get <run>` · `cm watch <run>` ·
`cm approve|reject <run> <step>` · `cm cancel <run>`.

Stdlib subcommand dispatch (dependency-light). **Agent-aware:** every command
supports `--json` and uses meaningful exit codes, so it is scriptable by both humans
and other tools.

## 11. Error handling & observability

- Wrapped errors (`%w`) + sentinels via `errors.Is`; typed API errors → HTTP status map.
- **No silent failures** (fixing phase-1's dropped `enc.Encode` error): store/bus
  write failures are logged via `slog`; step errors surface into run state.
- Keep phase-1's good distinction: a gate verifier exiting non-zero is a *result*
  (gate failed), not an *error* (couldn't run the check).
- `slog` structured logging with `run_id`/`step_id` throughout.
- A `/metrics` Prometheus endpoint is left as a seam, not built (YAGNI).

## 12. Testing strategy

- **Engine with fakes:** `FakeExecutor` + in-memory `Store` + **fake `Clock`** →
  drive flows and **assert on the emitted event stream**. Covers fan-out/in, retry,
  on_fail abort/escalate, cancellation.
- **All concurrency tests under `-race`**, including a deadlock-freedom test (wide
  fan-in under a tiny semaphore).
- **`flow.Validate` table tests** — every rejection path.
- **Store tests** against a real temp SQLite file (modernc is fast); migration-apply
  + resume-load.
- **API tests** via `httptest`, including the SSE `Last-Event-ID` replay path.
- **Golden end-to-end with MockAgent** (preserves phase-1's "no API keys needed"
  loop) and a **kill-and-resume** test.

## 13. Build milestones (each independently runnable)

| M | Delivers | Runnable proof |
|---|---|---|
| **M0** | module, packages, `flow` schema + Parse + Validate (strict, expr-compile), state enums, `core` ports, MockExecutor | `go test ./internal/flow` |
| **M1** | engine: goroutine-per-step DAG, global semaphore + per-run cap, event bus, manual/auto gates, merge join, cancellation; in-memory store | mock flow runs end-to-end under `-race` ("phase-1 done right") |
| **M2** | SQLite store (WAL/migrations), persist-then-publish, resume reconciliation, artifacts | kill-and-resume test passes |
| **M3** | `net/http` API + SSE hub (+replay), `cm` client, loopback/bearer, graceful shutdown | `cm run --watch` against `magisterd` |
| **M4** | retry/backoff, on_fail escalate, timeouts, real CLIAgent + stream-json cost, git worktree workspace + teardown + re-run safety | real `claude`/`codex` flow |
| **M5** | conditional gates (`expr`), select/synthesize joins (arbiter agent) | full phase-2 flow |

## 14. Dependency budget (each justified, otherwise stdlib)

| Dependency | Why |
|---|---|
| `modernc.org/sqlite` | pure-Go SQLite (no cgo) |
| `expr-lang/expr` | sandboxed, compile-ahead condition expressions |
| `golang.org/x/sync/semaphore` | weighted global concurrency cap |
| `goccy/go-yaml` | YAML (carried from phase-1), used in **strict mode** |
| `pressly/goose` | embedded SQL migrations |
| `oklog/ulid` | sortable run IDs |
| `sqlc` | **dev-time** codegen for type-safe queries (not a runtime dep) |

Everything else — HTTP, SSE, logging (`slog`), routing, CLI, process exec, context —
is stdlib.

## 15. Known limitations & risks

- **At-least-once resume** (§7): in-flight steps re-run on restart. Mitigated by
  isolated worktrees; documented, not hidden.
- **SQLite single writer**: handled via WAL + single writer connection; acceptable
  for single-node personal scale.
- **No fairness across concurrent runs**: the global semaphore is FIFO-ish, not
  priority-aware. The seam allows swapping in a scheduler later if runs starve.
- **Agent CLIs are not idempotent**: inherent to the domain; the design contains
  rather than eliminates the risk.

## 16. Future (designed-for, not built)

- **Pluggable executors** (the option-3 generalization): non-CLI executors behind
  the same `Executor` port.
- **Scheduler** replacing the global semaphore for fairness/priority.
- **`/metrics`**, webhooks, a web UI — all additive subscribers on the event bus.
