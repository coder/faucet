-- A build before this migration keeps its position in top_at alone, and after
-- an empty list that position was GitHub's Date. A scan that has read no
-- attempt since the migration gets that position back.
UPDATE faucet_redelivery_scan SET top_at = COALESCE(top_at, clear_at);

ALTER TABLE faucet_redelivery_scan DROP COLUMN clear_at;
