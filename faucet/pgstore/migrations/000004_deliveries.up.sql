-- last_delivery_at, last_error and failed_since are the deliveryStatus of
-- SEP-3415.
ALTER TABLE faucet_subscriptions
  ADD COLUMN scanned_through  bigint NOT NULL DEFAULT 0 CHECK (scanned_through >= 0),
  ADD COLUMN last_delivery_at timestamptz,
  ADD COLUMN last_error       text CHECK (last_error IN ('connection_refused', 'timeout', 'tls_error', 'http_4xx', 'http_5xx', 'challenge_failed')),
  ADD COLUMN failed_since     timestamptz;

UPDATE faucet_subscriptions SET scanned_through = cursor;

-- One row per delivery that has not ended. It holds no secret. event_id has no
-- foreign key, so a purge of old events never waits for a delivery and never
-- deletes one: the worker finds the event gone and ends the delivery with a
-- reason.
CREATE TABLE faucet_deliveries (
  key_hash        bytea NOT NULL REFERENCES faucet_subscriptions (key_hash) ON DELETE CASCADE,
  event_id        bigint NOT NULL CHECK (event_id > 0),
  message_id      text NOT NULL CHECK (message_id <> ''),
  enqueued_at     timestamptz NOT NULL,
  next_attempt_at timestamptz NOT NULL,
  attempts        integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  lease_until     timestamptz,
  PRIMARY KEY (key_hash, event_id)
);

CREATE INDEX faucet_deliveries_due ON faucet_deliveries (next_attempt_at, event_id);
