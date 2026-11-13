-- One row per webhook subscription. key_hash is the SHA-256 of (principal,
-- callback_url, event_type, arguments), which is the subscription key of
-- SEP-3415. secret_enc is the subscriber's signing secret sealed with the
-- host's key. The row holds no GitHub token.
CREATE TABLE faucet_subscriptions (
  key_hash     bytea PRIMARY KEY CHECK (octet_length(key_hash) = 32),
  principal    text NOT NULL CHECK (principal <> ''),
  callback_url text NOT NULL CHECK (callback_url <> ''),
  event_type   text NOT NULL CHECK (event_type <> ''),
  arguments    jsonb NOT NULL,
  repo_id      bigint NOT NULL CHECK (repo_id > 0),
  secret_enc   bytea NOT NULL CHECK (octet_length(secret_enc) > 0),
  cursor       bigint NOT NULL CHECK (cursor >= 0),
  created_at   timestamptz NOT NULL,
  updated_at   timestamptz NOT NULL,
  expires_at   timestamptz NOT NULL
);

CREATE INDEX faucet_subscriptions_principal ON faucet_subscriptions (principal);
CREATE INDEX faucet_subscriptions_expires ON faucet_subscriptions (expires_at);
