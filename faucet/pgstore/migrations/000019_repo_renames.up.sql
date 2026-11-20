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
