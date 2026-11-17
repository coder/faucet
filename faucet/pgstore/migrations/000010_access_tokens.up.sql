-- faucet_access_tokens holds the access tokens that faucet mints, by the
-- SHA-256 of the token and never the token. subject is the client ID of a
-- service or the session ID of a user.
CREATE TABLE faucet_access_tokens (
  token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
  expires_at timestamptz NOT NULL,
  scopes text[] NOT NULL CHECK (cardinality(scopes) > 0),
  resource text NOT NULL CHECK (resource <> ''),
  subject_kind text NOT NULL CHECK (subject_kind IN ('service', 'user')),
  subject text NOT NULL CHECK (subject <> '')
);
