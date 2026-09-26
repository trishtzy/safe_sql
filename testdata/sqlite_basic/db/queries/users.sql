-- name: GetUser :one
SELECT id, email, name FROM users WHERE id = ?;
