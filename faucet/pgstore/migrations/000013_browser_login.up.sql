-- faucet_connections holds, for each GitHub user who logged in through a
-- browser, the GitHub App user tokens that faucet got for them. The tokens
-- are sealed with AES-256-GCM under the key of FAUCET_TOKEN_KEY that key_id
-- names. id is the provider and the user's ID there, such as github:583231,
-- so a user has one row however often they log in.
CREATE TABLE faucet_connections (
  id text PRIMARY KEY CHECK (id <> ''),
  provider text NOT NULL CHECK (provider IN ('github')),
  upstream_user_id bigint NOT NULL CHECK (upstream_user_id > 0),
  login text NOT NULL CHECK (login <> ''),
  access_token_enc bytea NOT NULL CHECK (octet_length(access_token_enc) > 0),
  refresh_token_enc bytea NOT NULL CHECK (octet_length(refresh_token_enc) > 0),
  key_id text NOT NULL CHECK (key_id <> ''),
  access_expires_at timestamptz NOT NULL,
  refresh_expires_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  UNIQUE (provider, upstream_user_id)
);

-- faucet_auth_requests holds one row per browser login of a client, from the
-- authorization request to the code that the client redeems. It keeps the
-- SHA-256 of the browser's cookie, of the state sent to GitHub and of the
-- code, never the values. A pending row expires 10 minutes after it was
-- made, and an approved one 60 seconds after its approval. A row is made
-- only when the user presses Continue: the consent page carries the request
-- sealed until then.
CREATE TABLE faucet_auth_requests (
  id text PRIMARY KEY CHECK (id <> ''),
  state text NOT NULL CHECK (state IN ('pending', 'approved', 'redeemed', 'expired')),
  client_id text NOT NULL CHECK (client_id <> ''),
  client_name text NOT NULL,
  redirect_uri text NOT NULL CHECK (redirect_uri <> ''),
  client_state text NOT NULL,
  resource text NOT NULL CHECK (resource <> ''),
  scopes text[] NOT NULL CHECK (cardinality(scopes) > 0),
  code_challenge text NOT NULL CHECK (code_challenge <> ''),
  browser_hash bytea NOT NULL CHECK (octet_length(browser_hash) = 32),
  github_state_hash bytea CHECK (octet_length(github_state_hash) = 32),
  code_hash bytea CHECK (octet_length(code_hash) = 32),
  connection_id text REFERENCES faucet_connections (id),
  created_at timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  CHECK ((state IN ('approved', 'redeemed')) = (code_hash IS NOT NULL AND connection_id IS NOT NULL))
);
CREATE UNIQUE INDEX faucet_auth_requests_github_state_idx ON faucet_auth_requests (github_state_hash) WHERE github_state_hash IS NOT NULL;
CREATE UNIQUE INDEX faucet_auth_requests_code_idx ON faucet_auth_requests (code_hash) WHERE code_hash IS NOT NULL;
CREATE INDEX faucet_auth_requests_expires_idx ON faucet_auth_requests (expires_at);

-- faucet_user_sessions holds the logins that refresh tokens keep alive. id
-- is the ID of the authorization request that made the session, so a
-- request makes one session at most.
CREATE TABLE faucet_user_sessions (
  id text PRIMARY KEY CHECK (id <> ''),
  client_id text NOT NULL CHECK (client_id <> ''),
  client_name text NOT NULL,
  connection_id text NOT NULL REFERENCES faucet_connections (id),
  resource text NOT NULL CHECK (resource <> ''),
  scopes text[] NOT NULL CHECK (cardinality(scopes) > 0),
  refreshed_at timestamptz NOT NULL,
  ended_at timestamptz
);
CREATE INDEX faucet_user_sessions_connection_idx ON faucet_user_sessions (connection_id) WHERE ended_at IS NULL;

-- faucet_refresh_tokens holds the SHA-256 of each refresh token that a
-- session gave out, never the token, until the session goes. used_at is when
-- a client first traded it for the next one, and a use 30 seconds or more
-- after that ends the session. retired_at is when a refresh of another token
-- retired it unused, and any use of it then ends the session.
CREATE TABLE faucet_refresh_tokens (
  token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
  session_id text NOT NULL REFERENCES faucet_user_sessions (id) ON DELETE CASCADE,
  issued_at timestamptz NOT NULL,
  used_at timestamptz,
  retired_at timestamptz,
  CONSTRAINT faucet_refresh_tokens_used_or_retired CHECK (used_at IS NULL OR retired_at IS NULL)
);
CREATE INDEX faucet_refresh_tokens_session_idx ON faucet_refresh_tokens (session_id);
-- A session keeps every token it gave out, so the step of a rotation that
-- retires the unused ones reads only those.
CREATE INDEX faucet_refresh_tokens_unused_idx ON faucet_refresh_tokens (session_id) WHERE used_at IS NULL AND retired_at IS NULL;
