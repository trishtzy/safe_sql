ALTER TABLE authors ADD COLUMN email text;
CREATE INDEX authors_email_idx ON authors (email);
INSERT INTO authors (name) SELECT 'x' FROM generate_series(1, 10);
