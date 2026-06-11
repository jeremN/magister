-- name: CreateRun :exec
INSERT INTO runs (id, name, flow_yaml, status, concurrency, error, repo, base)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: SetRunStatus :exec
UPDATE runs SET status = ?, error = ?, updated_at = datetime('now') WHERE id = ?;

-- name: GetRun :one
SELECT id, name, flow_yaml, status, concurrency, error, repo, base FROM runs WHERE id = ?;

-- name: ListRuns :many
SELECT id, name, status FROM runs ORDER BY created_at DESC, id;

-- name: ListRunsByStatus :many
SELECT id, name, status FROM runs WHERE status = ? ORDER BY created_at DESC, id;

-- name: ListIncompleteRuns :many
SELECT id, name, flow_yaml, status, concurrency, error, repo, base
FROM runs WHERE status IN ('pending', 'running') ORDER BY created_at, id;

-- name: UpsertStep :exec
INSERT INTO steps (run_id, id, status, attempt, summary, cost_usd, workdir, error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, id) DO UPDATE SET
    status = excluded.status, attempt = excluded.attempt, summary = excluded.summary,
    cost_usd = excluded.cost_usd, workdir = excluded.workdir, error = excluded.error;

-- name: ListSteps :many
SELECT run_id, id, status, attempt, summary, cost_usd, workdir, error
FROM steps WHERE run_id = ? ORDER BY id;

-- name: InsertEvent :one
INSERT INTO events (run_id, step_id, kind, summary, cost_usd, attempt, error, at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING seq;

-- name: EventsSince :many
SELECT seq, run_id, step_id, kind, summary, cost_usd, attempt, error, at
FROM events WHERE run_id = ? AND seq > ? ORDER BY seq;

-- name: DeleteArtifactsForStep :exec
DELETE FROM artifacts WHERE run_id = ? AND step_id = ?;

-- name: InsertArtifact :exec
INSERT INTO artifacts (run_id, step_id, path, branch, commit_sha) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (run_id, step_id, path) DO NOTHING;

-- name: ListArtifactsForRun :many
SELECT run_id, step_id, path, branch, commit_sha FROM artifacts WHERE run_id = ? ORDER BY step_id, path;
