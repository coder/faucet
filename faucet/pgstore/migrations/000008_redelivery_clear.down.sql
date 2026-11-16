ALTER TABLE faucet_redelivery_scan RENAME COLUMN top_attempt_at TO top_at;
UPDATE faucet_redelivery_scan SET top_at = COALESCE(top_at, clear_at), epoch = epoch + 1, lease_until = NULL;

ALTER TABLE faucet_redelivery_scan DROP COLUMN clear_at;
