-- clear_at is the Date header of GitHub's answer to the last head read: when,
-- on GitHub's clock, a run last saw the top of the list. A run takes its gap
-- starts from it and never a read bound, because a Date can run ahead of
-- GitHub's list. top_at keeps attempt times only.
ALTER TABLE faucet_redelivery_scan ADD COLUMN clear_at timestamptz;

-- Until now top_at was GitHub's Date when a run read an empty list, and no
-- row says which of the two it holds. So the old top moves to clear_at, and
-- the next run starts as a first run that knows when tracking began: it reads
-- the last 24 hours of the list again, and back to the old top less a margin
-- when that is older, and it records what lies below as a gap.
UPDATE faucet_redelivery_scan SET clear_at = top_at, top_at = NULL;
