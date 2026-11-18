-- name: FreshAccessCheck :one
-- FreshAccessCheck takes the pass of a repository by the connection of a
-- session when the pass started at or after the session's last refresh.
-- It moves the check's last use forward to now only when the last use came
-- before used_before, so a use that moves nothing writes no row.
WITH fresh AS (
  SELECT a.connection_id, a.repo, a.repo_id
  FROM faucet_access_checks a
  JOIN faucet_user_sessions s ON s.connection_id = a.connection_id
  WHERE s.id = @session_id AND a.repo = @repo AND a.repo_id IS NOT NULL
    AND a.checked_at >= s.refreshed_at
), used AS (
  UPDATE faucet_access_checks a
  SET last_used_at = greatest(a.last_used_at, @now::timestamptz)
  FROM fresh f
  WHERE a.connection_id = f.connection_id AND a.repo = f.repo
    AND a.last_used_at < @used_before::timestamptz
)
SELECT repo_id::bigint FROM fresh;

-- name: StoreAccessCheck :execrows
-- StoreAccessCheck stores a check of a repository by a connection, a pass or
-- a denial, whose repo_id 0 is stored as NULL, in place of the stored one
-- unless that one started later. A conflict always updates the row, so no
-- row means no connection. A denial also deletes the connection's checks of
-- the repository that the stored check held under its other names, a
-- repository renamed since, whose lookup started at or before the denial's.
WITH aliased AS (
  SELECT a.repo_id
  FROM faucet_access_checks a
  WHERE @repo_id::bigint = 0 AND a.connection_id = @connection_id AND a.repo = @repo::text
    AND a.repo_id IS NOT NULL
), aliases AS (
  DELETE FROM faucet_access_checks a
  USING aliased d
  WHERE a.connection_id = @connection_id AND a.repo <> @repo::text AND a.repo_id = d.repo_id
    AND a.checked_at <= @checked_at::timestamptz
)
INSERT INTO faucet_access_checks (connection_id, repo, repo_id, checked_at, last_used_at)
SELECT c.id, @repo::text, nullif(@repo_id::bigint, 0), @checked_at::timestamptz, @last_used_at::timestamptz
FROM faucet_connections c
WHERE c.id = @connection_id
ON CONFLICT (connection_id, repo) DO UPDATE SET
  repo_id = CASE WHEN excluded.checked_at >= faucet_access_checks.checked_at THEN excluded.repo_id ELSE faucet_access_checks.repo_id END,
  checked_at = greatest(faucet_access_checks.checked_at, excluded.checked_at),
  last_used_at = greatest(faucet_access_checks.last_used_at, excluded.last_used_at);

-- name: RecentAccessChecks :many
SELECT connection_id, repo, repo_id::bigint AS repo_id, checked_at, last_used_at
FROM faucet_access_checks
WHERE connection_id = @connection_id AND repo_id IS NOT NULL
  AND last_used_at >= @used_since::timestamptz
ORDER BY last_used_at DESC, repo
LIMIT @max_checks::bigint;

-- name: DeleteUnusedAccessChecks :execrows
DELETE FROM faucet_access_checks
WHERE last_used_at < @used_before::timestamptz
  OR (repo_id IS NULL AND checked_at < @denied_before::timestamptz);
