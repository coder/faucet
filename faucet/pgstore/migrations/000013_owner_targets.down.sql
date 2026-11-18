DROP INDEX faucet_repos_owner_login_idx;

DELETE FROM faucet_raw_leases WHERE target_kind <> 'repo';
ALTER TABLE faucet_raw_leases
  DROP CONSTRAINT faucet_raw_leases_target_check,
  DROP COLUMN repos,
  DROP COLUMN owner_id,
  DROP COLUMN target_kind,
  ADD CONSTRAINT faucet_raw_leases_repo_id_check CHECK (repo_id > 0);

DELETE FROM faucet_raw_events WHERE repo_id = 0;
ALTER TABLE faucet_raw_events
  DROP CONSTRAINT faucet_raw_events_named_check,
  DROP CONSTRAINT faucet_raw_events_repo_id_check,
  DROP COLUMN owner_id,
  ADD CONSTRAINT faucet_raw_events_repo_id_check CHECK (repo_id > 0);

DELETE FROM faucet_subscriptions WHERE target_kind <> 'repo';
ALTER TABLE faucet_subscriptions
  DROP CONSTRAINT faucet_subscriptions_target_check,
  DROP COLUMN owner_id,
  DROP COLUMN target_kind,
  ADD CONSTRAINT faucet_subscriptions_repo_id_check CHECK (repo_id > 0);

DROP INDEX faucet_events_owner;
ALTER TABLE faucet_events
  DROP CONSTRAINT faucet_events_owner_id_check,
  DROP COLUMN repo_name,
  DROP COLUMN owner_id;
