CREATE TABLE users (
  id bigserial PRIMARY KEY,
  email text NOT NULL,
  name text,
  nick text,
  settings json
);
CREATE TABLE posts (
  id bigserial PRIMARY KEY,
  user_id bigint NOT NULL
);
