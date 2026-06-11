-- +goose Up
-- +goose StatementBegin
ALTER TABLE runs ADD COLUMN repo TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN base TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE runs DROP COLUMN base;
ALTER TABLE runs DROP COLUMN repo;
-- +goose StatementEnd
