-- clear_at is the Date header of GitHub's answer to the last head read: when,
-- on GitHub's clock, a run last saw the top of the list. A run takes its gap
-- starts from it and never a read bound, because a Date can run ahead of
-- GitHub's list.
ALTER TABLE faucet_redelivery_scan ADD COLUMN clear_at timestamptz;

-- Until now top_at could hold a Date that ran ahead of GitHub's list, and no
-- row says whether it does (see #26).
UPDATE faucet_redelivery_scan SET clear_at = top_at, top_at = NULL, epoch = epoch + 1, lease_until = NULL;
ALTER TABLE faucet_redelivery_scan RENAME COLUMN top_at TO top_attempt_at;
