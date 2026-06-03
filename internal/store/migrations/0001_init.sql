-- +goose Up
-- +goose StatementBegin
CREATE TABLE runs (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    flow_yaml   TEXT NOT NULL,
    status      TEXT NOT NULL,
    concurrency INTEGER NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE steps (
    run_id   TEXT NOT NULL REFERENCES runs(id),
    id       TEXT NOT NULL,
    status   TEXT NOT NULL,
    attempt  INTEGER NOT NULL DEFAULT 0,
    summary  TEXT NOT NULL DEFAULT '',
    cost_usd REAL NOT NULL DEFAULT 0,
    workdir  TEXT NOT NULL DEFAULT '',
    error    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, id)
);

CREATE TABLE artifacts (
    run_id  TEXT NOT NULL,
    step_id TEXT NOT NULL,
    path    TEXT NOT NULL,
    PRIMARY KEY (run_id, step_id, path),
    FOREIGN KEY (run_id, step_id) REFERENCES steps(run_id, id)
);

CREATE TABLE events (
    seq      INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id   TEXT NOT NULL REFERENCES runs(id),
    step_id  TEXT NOT NULL DEFAULT '',
    kind     TEXT NOT NULL,
    summary  TEXT NOT NULL DEFAULT '',
    cost_usd REAL NOT NULL DEFAULT 0,
    attempt  INTEGER NOT NULL DEFAULT 0,
    error    TEXT NOT NULL DEFAULT '',
    at       TEXT NOT NULL
);

CREATE INDEX idx_events_run_seq ON events (run_id, seq);
CREATE INDEX idx_steps_run ON steps (run_id);
CREATE INDEX idx_artifacts_step ON artifacts (run_id, step_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE events;
DROP TABLE artifacts;
DROP TABLE steps;
DROP TABLE runs;
-- +goose StatementEnd
