-- name: InsertAuthRequest :execrows
-- InsertAuthRequest stores a pending request unless the pending requests
-- that have not expired at its creation reach a limit: those of its client
-- host, or all of them.
INSERT INTO faucet_auth_requests (
  id, state, client_id, client_host, client_name, redirect_uri, client_state, resource, scopes, code_challenge, browser_hash, created_at, expires_at
)
SELECT @id::text, 'pending', @client_id::text, @client_host::text, @client_name::text, @redirect_uri::text, @client_state::text,
  @resource::text, @scopes::text[], @code_challenge::text, @browser_hash::bytea, @created_at::timestamptz, @expires_at::timestamptz
WHERE (
    SELECT count(*) FROM faucet_auth_requests
    WHERE state = 'pending' AND client_host = @client_host::text AND expires_at > @created_at::timestamptz
  ) < @per_client_host::bigint
  AND (
    SELECT count(*) FROM faucet_auth_requests
    WHERE state = 'pending' AND expires_at > @created_at::timestamptz
  ) < @total::bigint;

-- name: ConsentAuthRequest :execrows
UPDATE faucet_auth_requests
SET github_state_hash = @github_state_hash::bytea
WHERE id = @id
  AND state = 'pending'
  AND browser_hash = @browser_hash
  AND expires_at > @now::timestamptz;

-- name: ExpireAuthRequest :exec
UPDATE faucet_auth_requests
SET state = 'expired'
WHERE id = @id AND state = 'pending';

-- name: GetAuthRequestByGitHubState :one
SELECT id, state, client_id, client_host, client_name, redirect_uri, client_state, resource, scopes, code_challenge,
  browser_hash, github_state_hash, code_hash, connection_id, created_at, expires_at
FROM faucet_auth_requests
WHERE github_state_hash = @github_state_hash;

-- name: GetAuthRequestByCode :one
SELECT id, state, client_id, client_host, client_name, redirect_uri, client_state, resource, scopes, code_challenge,
  browser_hash, github_state_hash, code_hash, connection_id, created_at, expires_at
FROM faucet_auth_requests
WHERE code_hash = @code_hash;

-- name: ApproveAuthRequest :execrows
UPDATE faucet_auth_requests
SET state = 'approved',
  code_hash = @code_hash::bytea,
  connection_id = @connection_id::text,
  expires_at = @code_expires_at::timestamptz
WHERE id = @id
  AND state = 'pending'
  AND github_state_hash = @github_state_hash::bytea
  AND expires_at > @now::timestamptz;

-- name: ExchangeCode :one
-- ExchangeCode redeems the code, makes the session from the request's row
-- and issues the session's first refresh token and first access token, in
-- one statement.
WITH redeemed AS (
  UPDATE faucet_auth_requests
  SET state = 'redeemed'
  WHERE code_hash = @code_hash::bytea
    AND state = 'approved'
    AND expires_at > @now::timestamptz
  RETURNING id, client_id, client_name, connection_id, resource, scopes
), new_session AS (
  INSERT INTO faucet_user_sessions (id, client_id, client_name, connection_id, resource, scopes, refreshed_at)
  SELECT r.id, r.client_id, r.client_name, r.connection_id, r.resource, r.scopes, @now::timestamptz
  FROM redeemed r
  RETURNING id, client_id, client_name, connection_id, resource, scopes, refreshed_at, ended_at
), new_refresh AS (
  INSERT INTO faucet_refresh_tokens (token_hash, session_id, issued_at)
  SELECT @refresh_hash::bytea, s.id, @now::timestamptz
  FROM new_session s
), new_token AS (
  INSERT INTO faucet_access_tokens (token_hash, expires_at, scopes, resource, subject_kind, subject)
  SELECT @access_hash::bytea, @access_expires_at::timestamptz, s.scopes, s.resource, 'user', s.id
  FROM new_session s
)
SELECT id, client_id, client_name, connection_id, resource, scopes, refreshed_at, ended_at
FROM new_session;

-- name: DeleteAuthRequestsExpiredBefore :execrows
DELETE FROM faucet_auth_requests
WHERE expires_at < @before::timestamptz;
