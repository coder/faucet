-- name: FreshAccessCheck :one
-- FreshAccessCheck takes the check of a repository by the connection of a
-- session when the check started at or after the session's last refresh,
-- and moves its last use forward to now, in one statement.
UPDATE faucet_access_checks a
SET last_used_at = greatest(a.last_used_at, @now::timestamptz)
FROM faucet_user_sessions s
WHERE s.id = @session_id AND a.connection_id = s.connection_id AND a.repo = @repo
  AND a.checked_at >= s.refreshed_at
RETURNING a.repo_id;

-- name: StoreAccessCheck :execrows
-- StoreAccessCheck stores a check of a repository by a connection, in place
-- of the stored one unless that one started later. A conflict always updates
-- the row, so no row means no connection.
INSERT INTO faucet_access_checks (connection_id, repo, repo_id, checked_at, last_used_at)
SELECT c.id, @repo::text, @repo_id::bigint, @checked_at::timestamptz, @last_used_at::timestamptz
FROM faucet_connections c
WHERE c.id = @connection_id
ON CONFLICT (connection_id, repo) DO UPDATE SET
  repo_id = CASE WHEN excluded.checked_at >= faucet_access_checks.checked_at THEN excluded.repo_id ELSE faucet_access_checks.repo_id END,
  checked_at = greatest(faucet_access_checks.checked_at, excluded.checked_at),
  last_used_at = greatest(faucet_access_checks.last_used_at, excluded.last_used_at);

-- name: DeleteAccessCheck :exec
DELETE FROM faucet_access_checks
WHERE connection_id = @connection_id AND repo = @repo;

-- name: RecentAccessChecks :many
SELECT connection_id, repo, repo_id, checked_at, last_used_at
FROM faucet_access_checks
WHERE connection_id = @connection_id
ORDER BY last_used_at DESC, repo
LIMIT @max_checks::bigint;
