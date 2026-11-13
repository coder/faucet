DROP TABLE faucet_deliveries;

ALTER TABLE faucet_subscriptions
  DROP COLUMN failed_since,
  DROP COLUMN last_error,
  DROP COLUMN last_delivery_at,
  DROP COLUMN scanned_through;
