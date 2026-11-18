-- name: GetSession :one
SELECT id, client_id, client_name, connection_id, resource, scopes, refresh_hash, previous_refresh_hash, refreshed_at, ended_at
FROM faucet_user_sessions
WHERE id = @id;

-- name: GetSessionByRefreshHash :one
-- A hash is the current token of one session at most, and a session that
-- holds it as its current token wins over one that holds it as its previous.
SELECT sqlc.embed(faucet_user_sessions), (refresh_hash = @refresh_hash::bytea)::boolean AS is_current
FROM faucet_user_sessions
WHERE refresh_hash = @refresh_hash::bytea OR previous_refresh_hash = @refresh_hash::bytea
ORDER BY is_current DESC
LIMIT 1;

-- name: RotateSession :one
-- RotateSession swaps the refresh token of the live session that holds the
-- presented one, and issues the next access token, in one statement.
WITH rotated AS (
  UPDATE faucet_user_sessions
  SET previous_refresh_hash = refresh_hash,
    refresh_hash = @next_hash::bytea,
    refreshed_at = @now::timestamptz
  WHERE refresh_hash = @presented_hash::bytea AND ended_at IS NULL
  RETURNING id, client_id, client_name, connection_id, resource, scopes, refresh_hash, previous_refresh_hash, refreshed_at, ended_at
), new_token AS (
  INSERT INTO faucet_access_tokens (token_hash, expires_at, scopes, resource, subject_kind, subject)
  SELECT @access_hash::bytea, @access_expires_at::timestamptz, @scopes::text[], r.resource, 'user', r.id
  FROM rotated r
)
SELECT id, client_id, client_name, connection_id, resource, scopes, refresh_hash, previous_refresh_hash, refreshed_at, ended_at
FROM rotated;

-- name: EndSession :exec
UPDATE faucet_user_sessions
SET ended_at = @now::timestamptz
WHERE id = @id AND ended_at IS NULL;

-- name: EndConnectionSessions :exec
UPDATE faucet_user_sessions
SET ended_at = @now::timestamptz
WHERE connection_id = @connection_id AND ended_at IS NULL;
