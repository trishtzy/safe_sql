Golden case for the todo file: `.safe_sql_todo.yaml` is discovered in the
working directory (no safe_sql.yaml) and suppresses the listed findings. One
entry deliberately undercounts (one of two `DROP COLUMN`s), so the extra
finding must be reported along with the unlisted index rule.
