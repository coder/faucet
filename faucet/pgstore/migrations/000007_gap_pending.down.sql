DROP INDEX faucet_subscriptions_gap;

ALTER TABLE faucet_subscriptions
  DROP COLUMN gap_lease_until,
  DROP COLUMN gap_through;
