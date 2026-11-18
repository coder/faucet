-- name: AddTrustRule :one
-- ON CONFLICT DO NOTHING would return no row, so an update that changes
-- nothing returns the ID of the rule that the client holds already. A client
-- that does not exist gives no row.
INSERT INTO faucet_workload_trusts (client_id, issuer, claims, jwks, rule_hash, created_at)
SELECT
  c.client_id,
  @issuer::text,
  sqlc.arg(claims)::text::jsonb,
  nullif(@jwks::text, '')::jsonb,
  sha256(jsonb_send(jsonb_build_array(@issuer::text, sqlc.arg(claims)::text::jsonb, nullif(@jwks::text, '')::jsonb))),
  @created_at::timestamptz
FROM faucet_service_clients c
WHERE c.client_id = @client_id::text
ON CONFLICT (client_id, rule_hash) DO UPDATE SET rule_hash = EXCLUDED.rule_hash
RETURNING id;

-- name: ListTrustRules :many
SELECT id, issuer, claims::text AS claims, coalesce(jwks::text, '')::text AS jwks, created_at
FROM faucet_workload_trusts
WHERE client_id = @client_id
ORDER BY id;

-- name: CountTrustRules :many
SELECT client_id, count(*)::integer AS rules
FROM faucet_workload_trusts
GROUP BY client_id;

-- name: RemoveTrustRule :execrows
DELETE FROM faucet_workload_trusts
WHERE client_id = @client_id AND id = @id;
