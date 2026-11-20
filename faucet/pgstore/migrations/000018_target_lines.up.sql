-- Every target has a line of its own, so that what a caller of one target
-- reads counts that target's events and no other's. repo_seq numbers the
-- events of a repository 1, 2, 3 in ID order, and owner_seq the events of an
-- owner, for an event that names one. The line of the all target is the
-- event ID. The rows that exist are numbered from the oldest that a purge
-- left.
ALTER TABLE faucet_events ADD COLUMN repo_seq bigint, ADD COLUMN owner_seq bigint;

UPDATE faucet_events e
SET repo_seq = n.repo_seq,
    owner_seq = CASE WHEN e.owner_id > 0 THEN n.owner_seq END
FROM (
  SELECT id,
    row_number() OVER (PARTITION BY repo_id ORDER BY id) AS repo_seq,
    row_number() OVER (PARTITION BY owner_id ORDER BY id) AS owner_seq
  FROM faucet_events
) n
WHERE n.id = e.id;

ALTER TABLE faucet_events
  ALTER COLUMN repo_seq SET NOT NULL,
  ADD CONSTRAINT faucet_events_owner_seq_check CHECK ((owner_id > 0) = (owner_seq IS NOT NULL));
CREATE UNIQUE INDEX faucet_events_repo_line ON faucet_events (repo_id, repo_seq);
CREATE UNIQUE INDEX faucet_events_owner_line ON faucet_events (owner_id, owner_seq) WHERE owner_id > 0;

-- purged_through is the highest position of the line that a purge deleted.
-- A line that no purge reached has no row.
CREATE TABLE faucet_event_lines (
  kind           text NOT NULL CHECK (kind IN ('repo', 'owner')),
  target_id      bigint NOT NULL CHECK (target_id > 0),
  purged_through bigint NOT NULL CHECK (purged_through >= 0),
  PRIMARY KEY (kind, target_id)
);

-- The purge mark of a line, and of the all target the one of faucet_purge_mark.
CREATE FUNCTION faucet_line_purged_through(kind text, repo_id bigint, owner_id bigint) RETURNS bigint
LANGUAGE sql STABLE AS $$
  SELECT CASE $1
    WHEN 'all' THEN (SELECT m.purged_through FROM faucet_purge_mark m)
    ELSE COALESCE((
      SELECT l.purged_through FROM faucet_event_lines l
      WHERE l.kind = $1 AND l.target_id = CASE $1 WHEN 'repo' THEN $2 ELSE $3 END
    ), 0)
  END
$$;

-- Triggers number an event and raise the purge marks, so that an instance of
-- the build before this migration, which inserts and purges during a rolling
-- deploy, keeps the lines whole. Every insert holds the lock of pgstore's
-- eventsLockKey, so the events of a line are numbered in the order they
-- commit. A row trigger sees the rows that its statement inserted before, so
-- a statement that inserts many rows numbers each.
-- The head of a line is the higher of its newest position and its purge
-- mark, which a purge of every event of the line leaves. EXECUTE plans each
-- lookup with the table as it is now: a plan cached while the table was small
-- reads every row, and a statement that inserts many rows would pay that for
-- each of them.
CREATE FUNCTION faucet_events_number() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  newest bigint;
BEGIN
  EXECUTE 'SELECT max(repo_seq) FROM faucet_events WHERE repo_id = $1' INTO newest USING NEW.repo_id;
  NEW.repo_seq := GREATEST(newest, faucet_line_purged_through('repo', NEW.repo_id, 0), 0) + 1;
  NEW.owner_seq := NULL;
  IF NEW.owner_id > 0 THEN
    EXECUTE 'SELECT max(owner_seq) FROM faucet_events WHERE owner_id = $1 AND owner_id > 0' INTO newest USING NEW.owner_id;
    NEW.owner_seq := GREATEST(newest, faucet_line_purged_through('owner', 0, NEW.owner_id), 0) + 1;
  END IF;
  RETURN NEW;
END
$$;

CREATE TRIGGER faucet_events_number
  BEFORE INSERT ON faucet_events
  FOR EACH ROW EXECUTE FUNCTION faucet_events_number();

-- A row of a repository ID that is not positive, which only an older build
-- could store, has no line to mark, and must not fail the purge of every
-- other row.
CREATE FUNCTION faucet_events_mark_purged() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO faucet_event_lines (kind, target_id, purged_through)
  SELECT 'repo', p.repo_id, max(p.repo_seq) FROM purged p WHERE p.repo_id > 0 GROUP BY p.repo_id
  UNION ALL
  SELECT 'owner', p.owner_id, max(p.owner_seq) FROM purged p WHERE p.owner_id > 0 GROUP BY p.owner_id
  ON CONFLICT (kind, target_id) DO UPDATE
    SET purged_through = GREATEST(faucet_event_lines.purged_through, EXCLUDED.purged_through);
  RETURN NULL;
END
$$;

CREATE TRIGGER faucet_events_mark_purged
  AFTER DELETE ON faucet_events
  REFERENCING OLD TABLE AS purged
  FOR EACH STATEMENT EXECUTE FUNCTION faucet_events_mark_purged();

-- position is where delivery starts on the line of the subscription's
-- target, as the subscribe reply names it. For a row that a build before
-- this migration wrote it is the position of the newest event of the target
-- at or below cursor. A cursor below the purge mark may have lost events that
-- no row counts any more, so it gets the newest position still stored, or 0,
-- and a resume from it reports the loss.
CREATE FUNCTION faucet_line_position(kind text, repo_id bigint, owner_id bigint, through bigint) RETURNS bigint
LANGUAGE sql STABLE AS $$
  SELECT GREATEST(
    CASE $1
      WHEN 'repo' THEN (SELECT max(e.repo_seq) FROM faucet_events e WHERE e.repo_id = $2 AND e.id <= $4)
      WHEN 'owner' THEN (SELECT max(e.owner_seq) FROM faucet_events e WHERE e.owner_id = $3 AND e.owner_id > 0 AND e.id <= $4)
      ELSE $4
    END,
    CASE WHEN $4 >= (SELECT m.purged_through FROM faucet_purge_mark m) THEN faucet_line_purged_through($1, $2, $3) END,
    0
  )
$$;

ALTER TABLE faucet_subscriptions ADD COLUMN position bigint;
UPDATE faucet_subscriptions SET position = faucet_line_position(target_kind, repo_id, owner_id, cursor);
ALTER TABLE faucet_subscriptions
  ALTER COLUMN position SET NOT NULL,
  ADD CONSTRAINT faucet_subscriptions_position_check CHECK (position >= 0);

CREATE FUNCTION faucet_subscriptions_default_position() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.position IS NULL THEN
    NEW.position := faucet_line_position(NEW.target_kind, NEW.repo_id, NEW.owner_id, NEW.cursor);
  END IF;
  RETURN NEW;
END
$$;

CREATE TRIGGER faucet_subscriptions_default_position
  BEFORE INSERT ON faucet_subscriptions
  FOR EACH ROW EXECUTE FUNCTION faucet_subscriptions_default_position();
