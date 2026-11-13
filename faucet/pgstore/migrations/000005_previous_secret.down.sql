DROP INDEX faucet_subscriptions_previous_secret_until;

ALTER TABLE faucet_subscriptions
  DROP CONSTRAINT faucet_subscriptions_previous_secret,
  DROP COLUMN previous_secret_until,
  DROP COLUMN previous_secret_enc;
