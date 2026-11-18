-- name: UpsertConnection :exec
-- UpsertConnection leaves id, provider and upstream_user_id alone, which the
-- ID fixes. Setting no key column keeps its row lock at FOR NO KEY UPDATE, so
-- it waits for LockConnection and a session insert can check its foreign key
-- meanwhile.
INSERT INTO faucet_connections (
  id, provider, upstream_user_id, login, access_token_enc, refresh_token_enc, key_id, access_expires_at, refresh_expires_at, updated_at
) VALUES (
  @id, @provider, @upstream_user_id, @login, @access_token_enc, @refresh_token_enc, @key_id, @access_expires_at, @refresh_expires_at, @updated_at
)
ON CONFLICT (id) DO UPDATE SET
  login = excluded.login,
  access_token_enc = excluded.access_token_enc,
  refresh_token_enc = excluded.refresh_token_enc,
  key_id = excluded.key_id,
  access_expires_at = excluded.access_expires_at,
  refresh_expires_at = excluded.refresh_expires_at,
  updated_at = excluded.updated_at;

-- name: GetConnection :one
SELECT id, provider, upstream_user_id, login, access_token_enc, refresh_token_enc, key_id, access_expires_at, refresh_expires_at, updated_at
FROM faucet_connections
WHERE id = @id;

-- name: LockConnection :one
-- LockConnection takes the lock that UpsertConnection and UpdateConnection
-- take, and holds it to the end of the transaction.
SELECT id, provider, upstream_user_id, login, access_token_enc, refresh_token_enc, key_id, access_expires_at, refresh_expires_at, updated_at
FROM faucet_connections
WHERE id = @id
FOR NO KEY UPDATE;

-- name: UpdateConnection :one
UPDATE faucet_connections
SET login = @login,
  access_token_enc = @access_token_enc,
  refresh_token_enc = @refresh_token_enc,
  key_id = @key_id,
  access_expires_at = @access_expires_at,
  refresh_expires_at = @refresh_expires_at,
  updated_at = @updated_at
WHERE id = @id
RETURNING id, provider, upstream_user_id, login, access_token_enc, refresh_token_enc, key_id, access_expires_at, refresh_expires_at, updated_at;

-- name: DeleteUnusedConnections :execrows
-- DeleteUnusedConnections deletes the connections that no session and no
-- authorization request names. A login that stores a connection while this
-- runs sets updated_at, which a deletion that waits for its row lock checks
-- again, so the connection stays.
DELETE FROM faucet_connections c
WHERE c.updated_at < @updated_before::timestamptz
  AND NOT EXISTS (SELECT 1 FROM faucet_user_sessions s WHERE s.connection_id = c.id)
  AND NOT EXISTS (SELECT 1 FROM faucet_auth_requests r WHERE r.connection_id = c.id);
