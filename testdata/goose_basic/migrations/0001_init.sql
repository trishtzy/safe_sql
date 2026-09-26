-- +goose Up
CREATE TABLE users (
  id bigserial PRIMARY KEY,
  email varchar(255) NOT NULL,
  name text,
  settings json
);
CREATE INDEX users_email_idx ON users (email);
-- +goose Down
DROP TABLE users;
