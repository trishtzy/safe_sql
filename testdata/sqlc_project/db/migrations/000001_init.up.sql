CREATE TABLE authors (
  id bigserial PRIMARY KEY,
  name text NOT NULL,
  bio json
);
