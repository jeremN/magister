# Cleanup — typed run-not-found sentinel + deterministic Mock cancellation

## Summary

Two small, independent pieces of carried tech debt, shipped as one cleanup slice:

- **Part A — `core.ErrRunNotFound` sentinel.** Today `Store.GetRun` returns an untyped error for *both* "this run does not exist" and "the storage backend failed," and all four callers map *any* `GetRun` error to HTTP 404. A genuine storage failure therefore reads as "not found." Add a typed sentinel, wrap it on the not-found path only, and have callers distinguish: not-found → 404, anything else → 500. This closes the two `// TODO: no store not-found sentinel` comments in the supervisor.
- **Part B — deterministic `Mock.Run` cancellation.** `TestMockHonorsContextCancel` is latently flaky: `Mock.Run` `select`s between a tiny `time.After(Delay)` and `ctx.Done()`, so an already-cancelled context can lose the race and the mock proceeds, returning `nil`. Hoist the existing `ctx.Err()` guard to the top of `Mock.Run` so an already-done context returns deterministically before the select.

Stdlib only, **no new dependency**, no DB migration, no schema change. No behavior change for the happy path or for genuine unknown-run requests (still 404); the only new behavior is that a real storage error now surfaces as 500 instead of a misleading 404.

## Motivation

The masking is a real observability footgun: if SQLite errors (locked, corrupt, disk-full) on a `GET /v1/runs/{id}`, `/v1/runs/{id}/events`, `cm push`, or `cm pr`, the operator sees `404 unknown run` and chases a phantom client bug instead of the storage fault. The supervisor TODOs (`supervisor.go`, `pr.go`) explicitly defer the fix "if the store grows a typed `ErrNotFound`" — this slice grows it.

The flaky test is the long-carried `TestMockHonorsContextCancel`. It rarely fails locally (an already-closed `ctx.Done()` almost always beats a 5 ns timer to readiness), but the `select` over two simultaneously-ready channels is nondeterministic by construction, so it can fail under load/GC/`-race` scheduling. Fixing the root (the mock) rather than the test removes the race for the scenario the test exercises.

## Part A — `core.ErrRunNotFound`

### The sentinel

Add to `internal/core` (the package that owns the `Store` interface; both store implementations and all four callers already import it — putting the sentinel in `internal/store` would force the api/supervisor callers to import the concrete adapter just to match an error):

```go
// ErrRunNotFound is returned (wrapped) by Store.GetRun when no run with the
// given id exists. Callers use errors.Is to distinguish a missing run (HTTP
// 404) from a storage failure (HTTP 500).
var ErrRunNotFound = errors.New("run not found")
```

### Store implementations wrap it on the not-found path only

- `internal/store/sqlite.go` `GetRun`: the existing `if errors.Is(err, sql.ErrNoRows)` branch returns `fmt.Errorf("unknown run %q: %w", id, core.ErrRunNotFound)`. The non-`ErrNoRows` branch (and the `loadSteps` error path) keep returning the raw error unchanged.
- `internal/store/mem.go` `GetRun`: the map-miss branch returns `fmt.Errorf("unknown run %q: %w", id, core.ErrRunNotFound)`.

The user-visible message (`unknown run "<id>"`) is unchanged; the error is now `errors.Is`-matchable.

### Callers distinguish not-found from storage error

All four sites branch on `errors.Is(err, core.ErrRunNotFound)`:

| Site | not-found | other error (new) |
|---|---|---|
| `handleGetRun` (`internal/api/handlers.go`) | `404 "unknown run"` | `500 "internal error"` |
| `handleEvents` / SSE (`internal/api/sse.go`) | `404 "unknown run"` | `500 "internal error"` |
| `Supervisor.Push` (`internal/supervisor/supervisor.go`) | `pushErr(404, "unknown run %q", runID)` | `pushErr(500, "load run %q: %v", runID, err)` |
| `Supervisor.prCore` (`internal/supervisor/pr.go`) | `prErr(404, "unknown run %q", runID)` | `prErr(500, "load run %q: %v", runID, err)` |

The two `// TODO: no store not-found sentinel …` comments (supervisor.go, pr.go) are deleted. `Push`/`prCore` already return `*PushError`/`*PRError` carrying an HTTP status, mapped by the handlers via `errors.As`; the 500 path reuses that mechanism, so the handlers need no change beyond what handleGetRun/handleEvents already get.

## Part B — deterministic `Mock.Run` cancellation

In `internal/executor/mock.go`, hoist the context check to the top of `Run`, before the `Delay` branch:

```go
func (m Mock) Run(ctx context.Context, t core.Task) (core.Result, error) {
	if err := ctx.Err(); err != nil {
		return core.Result{}, err
	}
	if m.Delay > 0 {
		select {
		case <-time.After(m.Delay):
		case <-ctx.Done():
			return core.Result{}, ctx.Err()
		}
	}
	// ... unchanged: write artifact, return result ...
}
```

The previous `else if err := ctx.Err(); err != nil { ... }` on the `Delay==0` path is removed — the top guard now covers it. An already-cancelled context returns deterministically; the `select` is reached only with a live context, so it no longer races an already-closed `ctx.Done()`. The in-flight `ctx.Done()` case inside the select is retained (it handles cancellation that arrives *during* a real delay).

## Testing

- **store** (`internal/store`): for both `Mem` and `SQLite`, `GetRun` on an unknown id returns an error satisfying `errors.Is(err, core.ErrRunNotFound)`.
- **api** (`internal/api`): `handleGetRun` on an unknown id → 404; with a fake `core.Store` whose `GetRun` returns a non-sentinel error → 500 (embed `*store.Mem`, override `GetRun`, as the readiness slice's `pingErrStore` did for `Ping`). Cover the `/events` SSE not-found → 404 path if it is cheap with the existing harness.
- **supervisor** (`internal/supervisor`): `Push` and `prCore` with a fake store whose `GetRun` returns `core.ErrRunNotFound` → status 404; returning a plain error → status 500.
- **executor** (`internal/executor`): the existing `TestMockHonorsContextCancel` now passes deterministically (confirm with `-count`); `TestMockWritesArtifact` still green.
- Full `go test -race ./...` green; `go vet` + `gofmt -l` clean.

## Out of scope

- A broader store-error taxonomy — only "not found" gets a sentinel; other failures stay raw (and now correctly surface as 500).
- The cancel-*during*-delay select ordering — the in-flight `ctx.Done()` case is kept; the pre-check only deterministically handles cancel-*before*-run, which is what the flaky test exercises.
- Any change to the happy path, schema, migrations, or auth.

## Global constraints

- Go 1.22; **stdlib only, no new dependency** (do not touch `go.mod`); no DB migration; no schema change.
- `core.ErrRunNotFound` lives in `internal/core`; `GetRun` wraps it with `%w` on the not-found path only; the message stays `unknown run "<id>"`.
- Genuine unknown-run requests still return 404; only genuine storage errors change (404 → 500).
- Commits: single conventional-commit subject, no body, no `Co-Authored-By`; never `--no-verify`; `gofmt`/`go vet`/`go test -race ./...` clean before merge.
