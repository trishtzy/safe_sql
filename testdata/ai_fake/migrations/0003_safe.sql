-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY users_token_idx2 ON users (token);
-- safe_sql:disable ban-drop-column
ALTER TABLE users DROP COLUMN settings;
-- +goose Down
DROP INDEX CONCURRENTLY users_token_idx2;
