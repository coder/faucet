-- One row. purged_through is the highest event ID any purge has deleted. A
-- poller whose cursor is below it has lost events and must be told.
CREATE TABLE faucet_purge_mark (
  id             boolean PRIMARY KEY DEFAULT true CHECK (id),
  purged_through bigint NOT NULL CHECK (purged_through >= 0)
);

INSERT INTO faucet_purge_mark (id, purged_through) VALUES (true, 0);
