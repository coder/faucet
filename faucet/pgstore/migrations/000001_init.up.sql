CREATE TABLE faucet_events (
  id           bigserial PRIMARY KEY,
  source       text NOT NULL DEFAULT 'github',
  delivery_id  text NOT NULL UNIQUE,
  type         text NOT NULL,
  repo_id      bigint NOT NULL,
  prs          int[] NOT NULL DEFAULT '{}',
  head_sha     text,
  data         jsonb NOT NULL,
  received_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX faucet_events_poll ON faucet_events (repo_id, type, id);
