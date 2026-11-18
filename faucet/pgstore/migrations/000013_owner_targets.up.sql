-- An event records the owner and the full name of its repository, so an owner
-- target can read it and a list of repository names can filter it. A row
-- stored before this migration keeps 0 and '': no owner target reads it. Each
-- column has a constant default, so adding it rewrites no row, and the check
-- is NOT VALID, so adding it reads none.
ALTER TABLE faucet_events
  ADD COLUMN owner_id bigint NOT NULL DEFAULT 0,
  ADD COLUMN repo_name text NOT NULL DEFAULT '';
ALTER TABLE faucet_events ADD CONSTRAINT faucet_events_owner_id_check CHECK (owner_id >= 0) NOT VALID;
CREATE INDEX faucet_events_owner ON faucet_events (owner_id, type, id) WHERE owner_id > 0;

-- A subscription reads one repository (repo_id), every repository of one
-- owner (owner_id), or every repository. The default keeps the rows that an
-- instance of the build before this migration inserts during a rolling deploy
-- valid repo rows.
ALTER TABLE faucet_subscriptions
  ADD COLUMN target_kind text NOT NULL DEFAULT 'repo',
  ADD COLUMN owner_id bigint NOT NULL DEFAULT 0,
  DROP CONSTRAINT faucet_subscriptions_repo_id_check,
  ADD CONSTRAINT faucet_subscriptions_target_check CHECK (
    (target_kind = 'repo' AND repo_id > 0 AND owner_id = 0)
    OR (target_kind = 'owner' AND repo_id = 0 AND owner_id > 0)
    OR (target_kind = 'all' AND repo_id = 0 AND owner_id = 0)
  );

-- A raw event of an organization's delivery names no repository: its repo_id
-- is 0 and its owner_id is the organization's.
ALTER TABLE faucet_raw_events
  ADD COLUMN owner_id bigint NOT NULL DEFAULT 0 CHECK (owner_id >= 0),
  DROP CONSTRAINT faucet_raw_events_repo_id_check,
  ADD CONSTRAINT faucet_raw_events_repo_id_check CHECK (repo_id >= 0),
  ADD CONSTRAINT faucet_raw_events_named_check CHECK (repo_id > 0 OR owner_id > 0);

-- A lease has the target of a subscription, and repos limits it to the
-- repositories of these lower-case full names when it is not empty.
ALTER TABLE faucet_raw_leases
  ADD COLUMN target_kind text NOT NULL DEFAULT 'repo',
  ADD COLUMN owner_id bigint NOT NULL DEFAULT 0,
  ADD COLUMN repos text[] NOT NULL DEFAULT '{}',
  DROP CONSTRAINT faucet_raw_leases_repo_id_check,
  ADD CONSTRAINT faucet_raw_leases_target_check CHECK (
    (target_kind = 'repo' AND repo_id > 0 AND owner_id = 0)
    OR (target_kind = 'owner' AND repo_id = 0 AND owner_id > 0)
    OR (target_kind = 'all' AND repo_id = 0 AND owner_id = 0)
  );

-- An owner target names its owner by login, and the lookup takes the row
-- that was seen latest.
CREATE INDEX faucet_repos_owner_login_idx ON faucet_repos (lower(owner_login), seen_at DESC);
