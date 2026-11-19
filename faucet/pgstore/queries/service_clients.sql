-- name: InsertServiceClient :execrows
INSERT INTO faucet_service_clients (client_id, secret_hash, resource, grants, scopes, created_at)
VALUES (@client_id, nullif(@secret_hash::bytea, ''), @resource, @grants::text[], @scopes::text[], @created_at)
ON CONFLICT (client_id) DO NOTHING;

-- name: GetServiceClient :one
SELECT client_id, secret_hash, resource, grants, scopes, created_at, disabled_at
FROM faucet_service_clients
WHERE client_id = @client_id;

-- name: ListServiceClients :many
SELECT client_id, secret_hash, resource, grants, scopes, created_at, disabled_at
FROM faucet_service_clients
ORDER BY created_at, client_id;

-- name: DisableServiceClient :one
UPDATE faucet_service_clients
SET disabled_at = coalesce(disabled_at, @now::timestamptz)
WHERE client_id = @client_id
RETURNING client_id, secret_hash, resource, grants, scopes, created_at, disabled_at;

-- name: UpdateServiceClient :one
UPDATE faucet_service_clients
SET grants = CASE WHEN @set_grants::boolean THEN @grants::text[] ELSE grants END,
    scopes = CASE WHEN @set_scopes::boolean THEN @scopes::text[] ELSE scopes END
WHERE client_id = @client_id AND disabled_at IS NULL
RETURNING client_id, secret_hash, resource, grants, scopes, created_at, disabled_at;
