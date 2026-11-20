ALTER TABLE faucet_repos
  DROP CONSTRAINT faucet_repos_candidate_whole,
  DROP COLUMN candidate_full_name,
  DROP COLUMN candidate_owner_id;
DROP TABLE faucet_repo_renames;
