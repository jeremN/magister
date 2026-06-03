# Handoff — executing M3 (the plan is written; run it)

**Written:** 2026-06-03, immediately after the M3 plan was written and reviewed. Context is being cleared so a fresh session can execute it.
**Audience:** a fresh Claude Code context with no memory of the planning session.
**TL;DR:** The M3 implementation plan is complete, self-reviewed, and **committed on branch `feat/m3-api`**. Your job is to **execute it via `superpowers:subagent-driven-development`, starting at Task 1** (Task 0 — the branch — is already done). Do not re-plan; do not re-brainstorm.

---

## 1. Where things stand

- **You are on `feat/m3-api`** (already branched off `main`). The plan is committed there as `docs/superpowers/plans/2026-06-03-orchestrator-m3-api.md` (the first commit on the branch). Working tree clean.
- **M0+M1+M2 are merged to `main`** and green: `go test -race -count=1 ./...` passes **51 tests across 10 packages**; `go vet ./...` clean. Module `concentus`, `go 1.22`.
- The project is a single-node Go **agent orchestrator** (routes tasks to AI coding-agent CLIs via declarative DAG flows). M0–M2 built the `flow` schema, `core` ports, `event` bus, mock executor, dir workspace, gate/join, the engine (goroutine-per-step DAG + Run + Resume), and both stores (in-memory `Mem` + SQLite). M3 turns it into a running **service**.

**Authoritative docs:**
- The plan to execute: `docs/superpowers/plans/2026-06-03-orchestrator-m3-api.md` (17 tasks, ~2640 lines, complete code per task).
- Design spec: `docs/superpowers/specs/2026-06-02-orchestrator-design.md` — §3 (two binaries/runtime flow), §5 (Supervisor/ApprovalRegistry), §9 (API + security), §10 (CLI), §11 (errors/slog).
- Project memory summarizes status + dependency pins.

---

## 2. What M3 delivers (so you understand what you're building)

A `magisterd` daemon (HTTP/JSON + SSE API) + a `cm` CLI client, with real blocking manual-gate approvals and resume-on-startup. The runnable proof: `cm run flows/feature-flow.yaml --watch` drives a flow against the daemon, a manual gate blocks until `cm approve`, and killing+restarting the daemon resumes incomplete runs.

**Two scoping decisions were already made by the user (do not revisit):**
1. **One comprehensive plan** (server + `cm` client together), not split.
2. **Full gate-blocking:** manual gates persist `StepAwaitingGate` + emit a `gate.awaiting` event; resume re-blocks a still-pending gate (it re-runs the awaiting step under the existing at-least-once model — no special resume path).

**New packages M3 creates:** `internal/supervisor` (runs map + `ApprovalRegistry` + the blocking `RegistryApprover`), `internal/api` (handlers + middleware + SSE hub + router), `internal/config`, and two binaries `cmd/magisterd` + `cmd/cm`. **Small change to existing code:** `gate.Approver`/`Evaluator` gain a `runID` param (Task 1), the engine emits `awaiting_gate` before a blocking gate (Task 2), and `event` gains a `GateAwaiting` kind.

---

## 3. The exact next step

1. Invoke **`superpowers:subagent-driven-development`**.
2. **Start at Task 1** of the committed plan. (Task 0 = "branch off main" is already done — you are on `feat/m3-api`. Do NOT run `git switch -c feat/m3-api` again; it will fail.)
3. Work through Tasks 1→16, then a final holistic review, then **`superpowers:finishing-a-development-branch`**.

---

## 4. Execution policy (learned the hard way executing M2 — follow it)

**Per-task loop:** fresh implementer subagent → verify yourself → (for risky units) spec-compliance review + code-quality review subagents → fix loop → mark complete → next task.

- **Dispatch implementers pointing at the committed plan + the specific task number + scene-setting context** (don't re-paste hundreds of lines — the plan is on disk on the branch). Give each the hard rules below.
- **Model choice:** `sonnet` for mechanical, well-specified tasks (most of them — the plan has complete code). Use **`opus`** for the riskiest *reviews* (and implement if you judge it warranted). The riskiest units in M3:
  - **Task 2** — engine emits/persists `awaiting_gate` before a *blocking* gate (touches the proven attempt loop). opus review.
  - **Tasks 5–6** — the blocking `RegistryApprover` + the `Supervisor` run lifecycle (goroutines, cancellation, shutdown). opus review.
  - **Task 11** — the SSE hub (store-as-content + bus-as-wakeup + ticker; concurrency + client-disconnect). opus review.
  - **Task 13** — `magisterd` graceful shutdown + resume-on-startup wiring. opus review.
  - **Task 15** — the e2e tests (real daemon, kill/resume timing). Verify carefully; watch for flakiness.
- **Verify reviewer/implementer claims yourself**: read the diff, run `go test -race -count=1 ./...`. Self-reports are optimistic.
- **`SendMessage` to continue a subagent is NOT available** here. To fix review findings: dispatch a fresh fix subagent (the code on disk is its context), or apply tiny reviewer-specified fixes directly and verify — then `git commit --amend --no-edit` the local task commit to keep history clean (the M2 session did this for small fixes).
- **The API cluster (Tasks 10–12) is mutually referential** — the handler/SSE tests call `srv.Router(...)`, which Task 12 creates. The plan documents this explicitly (an "API cluster" note before Task 10). Dispatch 10→11→12 as a unit and verify the whole `internal/api` package green **after Task 12** (don't expect Task 10/11 tests to pass in isolation). Every other task is strict per-task TDD (failing test → impl → green → commit).
- After all tasks pass: **final holistic opus review** over `git diff main...HEAD`, then `superpowers:finishing-a-development-branch` (the user merged M2 fast-forward to `main` + deleted the branch — they'll likely choose the same, but present the options).

---

## 5. Design decisions baked into the plan (respect them; they're validated)

- **`gate.Approver.Approve` gains `runID`** so the API-backed approver can key the `ApprovalRegistry` by `(runID, stepID)`. The `gate` package is an adapter — this signature change is fine. `core.Store` stays **frozen** (M3 reads through it; don't change it).
- **`awaiting_gate` resume needs no special engine path:** an `awaiting_gate` step is non-`succeeded`, so M2's existing `runDAG` already re-runs it (re-execute + re-block) — which satisfies §7. The only new engine code is the forward path (Task 2).
- **SSE design (validated in a throwaway spike under `-race`):** content always comes from `store.EventsSince` (real seqs), because live `event.Bus` frames carry `Seq=0` (the frozen store can't return the DB seq); the bus is a *wakeup only*, with a 1s ticker backstop for dropped (lossy) wakeups; the stream ends after the `run.done` frame or on client disconnect. `Last-Event-ID` (or `?since=`) resumes the cursor. Don't "simplify" this to streaming live frames directly — it breaks replay.
- **`magisterd` registers only the `mock` executor.** Real `claude`/`codex`/`gemini` CLIAgents are **M4**. M3 proves the *service* (API/SSE/approval/resume) with the keyless loop.
- **Deferred to later milestones (don't build them in M3):** `on_fail: escalate` (escalating a *failed* auto gate) → M4; real CLIAgents + stream-json cost + git-worktree workspaces → M4; `expr`-evaluated conditional gates + select/synthesize joins → M5 (conditional still falls back to manual blocking in M3).

---

## 6. Conventions & gotchas (strict)

**Git / commits (user CLAUDE.md):** single conventional-commit subject line, **no body**, **never** a `Co-Authored-By` trailer, **never** `--no-verify`. Git identity isn't global — commit with:
```bash
git -c user.name="Jérémie Nehlil" -c user.email="jeremie.nehlil.freelance@proton.me" commit -m "<subject>"
```
Each task in the plan gives its exact commit subject.

**Dependency pins (go-1.22 must not move):** M3 adds `github.com/oklog/ulid/v2 v2.1.1` (declares go 1.15 — safe, use latest). Existing pins `modernc.org/sqlite v1.36.1` and `pressly/goose/v3 v3.24.1` must not change (their next patch needs go 1.23). After `go get`, confirm `grep '^go ' go.mod` is still `go 1.22`.

**Environment quirks:**
- **RTK hook** reformats `go`/`git` output (e.g. "Go test: N passed"). If a result looks summarized, re-run with `-v -count=1` (or `rtk proxy go test ...`) for raw lines.
- **Semgrep hook** runs. It **will** flag `fmt.Fprintf(w, ...)` in the SSE handler as CWE-79 (XSS) — **accepted false positive** (SSE is `text/event-stream`, not HTML; content is server-generated event JSON; loopback trust boundary). Document it in-code with a short comment (like the accepted `sh -c` in `gate/verifier.go`); do NOT switch to `html/template`. It may flag the bearer compare — `crypto/subtle.ConstantTimeCompare` is the correct choice; note it if flagged.
- **UserPromptSubmit hook** injects a "secure-by-default libraries" blurb every message — ambient noise (JS/Ruby/Python-focused), ignore it. The real security stance is the spec's §9: loopback trust boundary, optional bearer token (constant-time compare), `MaxBytesReader`, server timeouts, graceful shutdown.
- Output style is **learning/explanatory** — include brief `★ Insight` notes when teaching something codebase-specific.

**Code-level gotchas carried from M0–M2:** the engine lives in `internal/engine` (not `core` — import-cycle); persist-then-publish (durable write, then publish; on store error don't publish the original event); SQLite is single-writer (`SetMaxOpenConns(1)`) + a separate reader pool with pragmas in the DSN; structural deadlock-freedom (a step waits on dep channels before acquiring any token — preserve this). `Engine` already has a `Log *slog.Logger` field (nil = discard) that `magisterd` wires to a real handler.

---

## 7. Quick orientation commands for the fresh session

```bash
cd /Users/jeremienehlil/Documents/Code/Personal/concentus-magister
git branch --show-current          # should be feat/m3-api
git log --oneline -3               # top = "docs: add M3 api plan"
git status --short                 # clean
go test -race -count=1 ./...       # confirm 51 tests green before starting Task 1
# then read the plan and execute it via superpowers:subagent-driven-development, starting at Task 1
sed -n '1,60p' docs/superpowers/plans/2026-06-03-orchestrator-m3-api.md
```
