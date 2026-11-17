-- faucet_repos maps the GitHub repositories that signed deliveries named to
-- their IDs, so a service granted a repository by name needs no GitHub call.
-- A row keeps the names it was first seen under until a repository event
-- renames or transfers it, and seen_at is when it took them. A name can move
-- to another repository, so it is not unique: a lookup takes the row that
-- took it latest.
CREATE TABLE faucet_repos (
  repo_id bigint PRIMARY KEY CHECK (repo_id > 0),
  owner_id bigint NOT NULL CHECK (owner_id > 0),
  owner_login text NOT NULL CHECK (owner_login <> ''),
  full_name text NOT NULL CHECK (full_name LIKE '_%/_%'),
  seen_at timestamptz NOT NULL
);
CREATE INDEX faucet_repos_full_name_idx ON faucet_repos (lower(full_name), seen_at DESC);

-- faucet_service_clients holds the clients that an operator made for
-- services. secret_hash is the SHA-256 of the client secret, never the
-- secret. resource is the one endpoint that the client's tokens are for, and
-- grants name what it may read. A disabled client stays, so its ID is never
-- given to another client.
CREATE TABLE faucet_service_clients (
  client_id text PRIMARY KEY CHECK (client_id <> ''),
  secret_hash bytea NOT NULL CHECK (octet_length(secret_hash) = 32),
  resource text NOT NULL CHECK (resource <> ''),
  grants text[] NOT NULL,
  scopes text[] NOT NULL CHECK (cardinality(scopes) > 0),
  created_at timestamptz NOT NULL,
  disabled_at timestamptz
);
