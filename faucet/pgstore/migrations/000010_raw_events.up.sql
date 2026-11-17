-- One row per GitHub delivery of an event that no typed family stores, and that
-- a live interest wanted when it arrived: a webhook subscription of a raw event
-- name (github.<event>), or a poll lease. body is NULL when no interest could
-- get the event, and once no live interest has it unread. The row then keeps
-- its delivery_id until the retention, so a delivery that GitHub sends again is
-- a duplicate. fields are the field paths that body holds, and NULL with a body
-- means the whole payload.
CREATE TABLE faucet_raw_events (
  id          bigserial PRIMARY KEY,
  delivery_id text NOT NULL UNIQUE CHECK (delivery_id <> ''),
  event       text NOT NULL CHECK (event <> ''),
  action      text NOT NULL,
  repo_id     bigint NOT NULL CHECK (repo_id > 0),
  body        bytea,
  fields      text[],
  received_at timestamptz NOT NULL,
  CHECK (fields IS NULL OR body IS NOT NULL)
);

CREATE INDEX faucet_raw_events_held ON faucet_raw_events (id) WHERE body IS NOT NULL;
CREATE INDEX faucet_raw_events_received ON faucet_raw_events (received_at);

-- One row per raw delivery that has not ended. It has no foreign key, so the
-- insert of a delivery never locks a subscription. incarnation is the
-- created_at of the subscription that the event matched: a row whose
-- subscription is gone, expired or made again is never claimed. give_up ends
-- the delivery with that reason, without a request.
CREATE TABLE faucet_raw_deliveries (
  key_hash        bytea NOT NULL CHECK (octet_length(key_hash) = 32),
  incarnation     timestamptz NOT NULL,
  raw_event_id    bigint NOT NULL CHECK (raw_event_id > 0),
  give_up         text CHECK (give_up IN ('event_too_large')),
  enqueued_at     timestamptz NOT NULL,
  next_attempt_at timestamptz NOT NULL,
  attempts        integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  lease_until     timestamptz,
  PRIMARY KEY (key_hash, raw_event_id)
);

CREATE INDEX faucet_raw_deliveries_due ON faucet_raw_deliveries (next_attempt_at, raw_event_id);
CREATE INDEX faucet_raw_deliveries_event ON faucet_raw_deliveries (raw_event_id);

-- A gap envelope that a raw subscription is owed, because it held the most
-- unsent deliveries when an event arrived. through is the highest raw ID that
-- it missed.
CREATE TABLE faucet_raw_gaps (
  key_hash    bytea NOT NULL CHECK (octet_length(key_hash) = 32),
  incarnation timestamptz NOT NULL,
  through     bigint NOT NULL CHECK (through > 0),
  lease_until timestamptz,
  PRIMARY KEY (key_hash, incarnation)
);

-- One row per poller of a raw event name. key_hash is the SHA-256 of the
-- login, the event name and the arguments. acked_through is the highest cursor
-- that the poller has presented, and lost_through the highest raw ID that the
-- lease missed.
CREATE TABLE faucet_raw_leases (
  id            bytea PRIMARY KEY CHECK (octet_length(id) = 16),
  key_hash      bytea NOT NULL CHECK (octet_length(key_hash) = 32),
  principal     text NOT NULL CHECK (principal <> ''),
  event         text NOT NULL CHECK (event <> ''),
  actions       text[] NOT NULL DEFAULT '{}',
  fields        text[],
  repo_id       bigint NOT NULL CHECK (repo_id > 0),
  acked_through bigint NOT NULL CHECK (acked_through >= 0),
  lost_through  bigint NOT NULL DEFAULT 0 CHECK (lost_through >= 0),
  created_at    timestamptz NOT NULL,
  polled_at     timestamptz NOT NULL,
  expires_at    timestamptz NOT NULL
);

CREATE INDEX faucet_raw_leases_match ON faucet_raw_leases (repo_id, event);
CREATE INDEX faucet_raw_leases_principal ON faucet_raw_leases (principal);

-- One row per raw event that a lease has not acknowledged.
CREATE TABLE faucet_raw_unread (
  lease_id     bytea NOT NULL CHECK (octet_length(lease_id) = 16),
  raw_event_id bigint NOT NULL CHECK (raw_event_id > 0),
  PRIMARY KEY (lease_id, raw_event_id)
);

CREATE INDEX faucet_raw_unread_event ON faucet_raw_unread (raw_event_id);

-- The match of a delivery against the raw subscriptions reads by repository.
CREATE INDEX faucet_subscriptions_raw ON faucet_subscriptions (repo_id, event_type);
