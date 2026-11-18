-- name: GetSession :one
SELECT id, client_id, client_name, connection_id, resource, scopes, refreshed_at, ended_at
FROM faucet_user_sessions
WHERE id = @id;

-- name: GetSessionByRefreshHash :one
SELECT sqlc.embed(faucet_user_sessions), sqlc.embed(faucet_refresh_tokens)
FROM faucet_refresh_tokens
JOIN faucet_user_sessions ON faucet_user_sessions.id = faucet_refresh_tokens.session_id
WHERE faucet_refresh_tokens.token_hash = @refresh_hash::bytea;

-- name: RotateSession :one
-- RotateSession trades the presented refresh token of a live session for
-- the next one, forgets the session's tokens that no client still holds,
-- and issues the next access token, in one statement. The presented token
-- is unused, or its first use came after used_after. The access token holds
-- no scope that the session lacks.
WITH rotated AS (
  UPDATE faucet_user_sessions s
  SET refreshed_at = @now::timestamptz
  FROM faucet_refresh_tokens t
  WHERE t.token_hash = @presented_hash::bytea AND s.id = t.session_id
    AND (t.used_at IS NULL OR t.used_at > @used_after::timestamptz)
    AND s.ended_at IS NULL AND @scopes::text[] <@ s.scopes
  RETURNING s.id, s.client_id, s.client_name, s.connection_id, s.resource, s.scopes, s.refreshed_at, s.ended_at
), used AS (
  UPDATE faucet_refresh_tokens t
  SET used_at = coalesce(t.used_at, @now::timestamptz)
  FROM rotated r
  WHERE t.token_hash = @presented_hash::bytea AND t.session_id = r.id
), forgotten AS (
  DELETE FROM faucet_refresh_tokens t
  USING rotated r
  WHERE t.session_id = r.id AND t.token_hash <> @presented_hash::bytea
    AND coalesce(t.used_at, t.issued_at) <= @used_after::timestamptz
), next_refresh AS (
  INSERT INTO faucet_refresh_tokens (token_hash, session_id, issued_at)
  SELECT @next_hash::bytea, r.id, @now::timestamptz
  FROM rotated r
), next_access AS (
  INSERT INTO faucet_access_tokens (token_hash, expires_at, scopes, resource, subject_kind, subject)
  SELECT @access_hash::bytea, @access_expires_at::timestamptz, @scopes::text[], r.resource, 'user', r.id
  FROM rotated r
)
SELECT id, client_id, client_name, connection_id, resource, scopes, refreshed_at, ended_at
FROM rotated;

-- name: EndSession :exec
UPDATE faucet_user_sessions
SET ended_at = @now::timestamptz
WHERE id = @id AND ended_at IS NULL;

-- name: EndConnectionSessions :exec
UPDATE faucet_user_sessions
SET ended_at = @now::timestamptz
WHERE connection_id = @connection_id AND ended_at IS NULL;
