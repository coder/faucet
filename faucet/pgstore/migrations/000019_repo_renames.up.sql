-- faucet_repo_renames holds the SHA-256 of the signed body of each repository
-- event that renamed or transferred a repository and that the store judged,
-- so that it judges each body once. GitHub signs the body alone, so whoever
-- kept the body of a transfer can send it again under another delivery GUID.
-- A row is kept for good: an old body stays signed.
CREATE TABLE faucet_repo_renames (
  body_sha256 bytea PRIMARY KEY CHECK (octet_length(body_sha256) = 32),
  repo_id bigint NOT NULL CHECK (repo_id > 0),
  seen_at timestamptz NOT NULL
);

-- A fresh rename or transfer from a name that a row does not hold may be a
-- missed rename or an old body sent again, and the store cannot tell which.
-- It keeps the rename's names as the candidate, and a row with a candidate
-- resolves no name until a fresh rename from its name or its candidate.
ALTER TABLE faucet_repos
  ADD COLUMN candidate_owner_id bigint CHECK (candidate_owner_id > 0),
  ADD COLUMN candidate_full_name text,
  ADD CONSTRAINT faucet_repos_candidate_whole CHECK ((candidate_owner_id IS NULL) = (candidate_full_name IS NULL));
