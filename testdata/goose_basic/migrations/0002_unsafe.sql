-- +goose Up
ALTER TABLE users DROP COLUMN name;
ALTER TABLE users ADD COLUMN token uuid DEFAULT gen_random_uuid();
CREATE INDEX users_token_idx ON users (token);
ALTER TABLE users ALTER COLUMN email SET NOT NULL;
UPDATE users SET token = gen_random_uuid() WHERE token IS NULL;
-- +goose Down
ALTER TABLE users ADD COLUMN name text;
