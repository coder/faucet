-- name: InsertAccessToken :exec
INSERT INTO faucet_access_tokens (token_hash, expires_at, scopes, resource, subject_kind, subject)
VALUES (@token_hash, @expires_at, @scopes::text[], @resource, @subject_kind, @subject);

-- name: DeleteExpiredAccessTokens :execrows
DELETE FROM faucet_access_tokens WHERE expires_at <= @now;

-- name: GetAccessToken :one
SELECT token_hash, expires_at, scopes, resource, subject_kind, subject
FROM faucet_access_tokens
WHERE token_hash = @token_hash;
