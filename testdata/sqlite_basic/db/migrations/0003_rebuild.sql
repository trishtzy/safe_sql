-- +goose NO TRANSACTION
-- +goose Up
PRAGMA foreign_keys = OFF;
BEGIN;
CREATE TABLE users_new (id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE, name TEXT NOT NULL DEFAULT '', code TEXT, created TEXT);
INSERT INTO users_new (id, email, name, code, created) SELECT id, email, coalesce(name, ''), code, created FROM users;
DROP TABLE users;
ALTER TABLE users_new RENAME TO users;
CREATE INDEX users_code_idx ON users (code);
COMMIT;
PRAGMA foreign_keys = ON;
-- +goose Down
SELECT 1;
