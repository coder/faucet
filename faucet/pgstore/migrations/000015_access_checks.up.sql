-- faucet_access_checks holds, for each connection, the repositories that its
-- GitHub token could read when faucet asked GitHub. repo is owner/name in
-- lower case, as GitHub ignores the case of a name. checked_at is when the
-- lookup that GitHub answered started. A session takes a row as its answer
-- only when checked_at is at or after its own refreshed_at, so each refresh
-- of a session makes the older rows count for nothing until a lookup checks
-- them again. A denial deletes the row. The rows of a connection go with it.
CREATE TABLE faucet_access_checks (
  connection_id text NOT NULL REFERENCES faucet_connections (id) ON DELETE CASCADE,
  repo text NOT NULL CHECK (repo <> '' AND repo = lower(repo)),
  repo_id bigint NOT NULL CHECK (repo_id > 0),
  checked_at timestamptz NOT NULL,
  last_used_at timestamptz NOT NULL,
  PRIMARY KEY (connection_id, repo)
);
