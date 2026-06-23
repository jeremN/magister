-- +goose Up
-- +goose StatementBegin
ALTER TABLE runs ADD COLUMN reclaimed_at TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE runs DROP COLUMN reclaimed_at;
-- +goose StatementEnd
