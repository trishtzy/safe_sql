ALTER TABLE users DROP COLUMN name;
ALTER TABLE users DROP COLUMN nick;
CREATE INDEX posts_user_id_idx ON posts (user_id);
