-- +goose Up
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE, name TEXT, code TEXT);
CREATE INDEX users_code_idx ON users (code);
CREATE VIEW user_names AS SELECT name FROM users;
-- +goose Down
DROP VIEW user_names; DROP TABLE users;
