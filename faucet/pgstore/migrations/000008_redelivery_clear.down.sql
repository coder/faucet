UPDATE faucet_redelivery_scan SET top_at = COALESCE(top_at, clear_at);

ALTER TABLE faucet_redelivery_scan DROP COLUMN clear_at;
