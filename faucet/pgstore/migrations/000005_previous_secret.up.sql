-- previous_secret_enc is the secret that the last rotation replaced, sealed like
-- secret_enc, and previous_secret_until is when it stops signing. Both are NULL
-- when no rotation window is running.
ALTER TABLE faucet_subscriptions
  ADD COLUMN previous_secret_enc   bytea CHECK (previous_secret_enc IS NULL OR octet_length(previous_secret_enc) > 0),
  ADD COLUMN previous_secret_until timestamptz,
  ADD CONSTRAINT faucet_subscriptions_previous_secret CHECK ((previous_secret_enc IS NULL) = (previous_secret_until IS NULL));

CREATE INDEX faucet_subscriptions_previous_secret_until ON faucet_subscriptions (previous_secret_until) WHERE previous_secret_until IS NOT NULL;
