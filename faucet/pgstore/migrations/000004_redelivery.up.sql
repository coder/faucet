-- One row per delivery that GitHub could not deliver to faucet and that faucet
-- would store. It ends in one final state: recovered (faucet stored the event
-- or a later attempt succeeded), or gave_up with a reason (limit: every ask was
-- spent, expired: GitHub's retention ended). A final row stays until its
-- attempts have left GitHub's list, so reading them again cannot revive it.
CREATE TABLE faucet_redeliveries (
  guid             text PRIMARY KEY CHECK (guid <> ''),
  state            text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'recovered', 'gave_up')),
  reason           text CHECK (reason IN ('limit', 'expired')),
  -- The newest failed attempt known. GitHub's redelivery endpoint takes its ID.
  last_attempt_id  bigint NOT NULL CHECK (last_attempt_id > 0),
  last_attempt_at  timestamptz NOT NULL,
  -- The earliest attempt known. GitHub's retention counts from it.
  first_attempt_at timestamptz NOT NULL,
  asks             integer NOT NULL DEFAULT 0 CHECK (asks >= 0),
  -- When the next ask may go out, or when a delivery with no asks left may be given up.
  next_ask_at      timestamptz NOT NULL,
  settled_at       timestamptz,
  CHECK ((state = 'gave_up') = (reason IS NOT NULL)),
  CHECK ((state = 'pending') = (settled_at IS NULL))
);

CREATE INDEX faucet_redeliveries_due ON faucet_redeliveries (next_ask_at) WHERE state = 'pending';

-- One row. The lease keeps two runs apart, and epoch fences a run whose lease
-- ran out. top_at is the time of the newest attempt that an earlier run read.
-- hook names the webhook the position belongs to.
CREATE TABLE faucet_redelivery_scan (
  id          boolean PRIMARY KEY DEFAULT true CHECK (id),
  hook        text NOT NULL CHECK (hook <> ''),
  epoch       bigint NOT NULL DEFAULT 0 CHECK (epoch >= 0),
  lease_until timestamptz,
  top_at      timestamptz
);

-- A stretch of GitHub's delivery list that no run has read yet (state open),
-- or a stretch that expired unread (state gap, kept as a record).
CREATE TABLE faucet_redelivery_ranges (
  id        bigserial PRIMARY KEY,
  state     text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'gap')),
  newer_at  timestamptz NOT NULL,
  older_at  timestamptz NOT NULL,
  cursor    text NOT NULL,
  closed_at timestamptz,
  CHECK ((state = 'gap') = (closed_at IS NOT NULL)),
  CHECK (state = 'gap' OR cursor <> '')
);

CREATE INDEX faucet_redelivery_ranges_newer ON faucet_redelivery_ranges (newer_at DESC) WHERE state = 'open';
