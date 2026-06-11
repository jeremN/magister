-- +goose Up
-- +goose StatementBegin
ALTER TABLE artifacts ADD COLUMN branch TEXT NOT NULL DEFAULT '';
ALTER TABLE artifacts ADD COLUMN commit_sha TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE artifacts DROP COLUMN commit_sha;
ALTER TABLE artifacts DROP COLUMN branch;
-- +goose StatementEnd
