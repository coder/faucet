-- A client that faucet clients create -no-secret made has no secret_hash, so
-- the client credentials grant refuses it, and only its trust rules let a
-- workload act as it.
ALTER TABLE faucet_service_clients ALTER COLUMN secret_hash DROP NOT NULL;

-- faucet_workload_trusts holds the trust rules of the service clients. A
-- token that issuer signs for a workload acts as the client when it holds
-- each claim that claims, an object of strings, names, with that value. jwks
-- is the key set of the issuer that an operator pasted, or NULL when faucet
-- fetches the keys that the issuer's OpenID discovery document names.
-- rule_hash is the SHA-256 of issuer, claims and jwks in a jsonb array, which
-- the insert computes, so a rule that holds the same JSON values as one of
-- its client's is that rule. A btree entry cannot hold the values themselves:
-- a pasted key set can be 32 KB. A client is never deleted, so its rules stay
-- until an operator removes them.
CREATE TABLE faucet_workload_trusts (
  id bigserial PRIMARY KEY,
  client_id text NOT NULL REFERENCES faucet_service_clients (client_id),
  issuer text NOT NULL CHECK (issuer <> ''),
  claims jsonb NOT NULL CHECK (
    jsonb_typeof(claims) = 'object' AND claims <> '{}' AND NOT jsonb_path_exists(claims, '$.* ? (@.type() != "string")')
  ),
  jwks jsonb CHECK (jsonb_typeof(jwks) = 'object'),
  rule_hash bytea NOT NULL CHECK (octet_length(rule_hash) = 32),
  created_at timestamptz NOT NULL,
  UNIQUE (client_id, rule_hash)
);
