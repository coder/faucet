-- gap_through is the purge mark of a gap envelope that the subscription is owed
-- and has not been sent, or 0. gap_lease_until keeps two runs from sending the
-- same gap: a run that claims it holds the lease, and a run that crashes leaves
-- the gap to the next run after the lease.
ALTER TABLE faucet_subscriptions
  ADD COLUMN gap_through     bigint NOT NULL DEFAULT 0 CHECK (gap_through >= 0),
  ADD COLUMN gap_lease_until timestamptz;

CREATE INDEX faucet_subscriptions_gap ON faucet_subscriptions (gap_through, key_hash) WHERE gap_through > 0;
