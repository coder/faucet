# Startup configuration

The `faucet` process reads `DATABASE_URL`, `GITHUB_WEBHOOK_SECRET`, `PORT`, `FAUCET_GITHUB_API_URL`, `CRON_SECRET`, `FAUCET_RETENTION_DAYS` and `FAUCET_WEBHOOK_KEY` and `FAUCET_WEBHOOK_ALLOWED_ORIGINS` from its environment and refuses to start, with exit status 1 and one line on stderr, when they are wrong. It never prints a secret or a database password.

## Sub-features

- A missing `DATABASE_URL` or `GITHUB_WEBHOOK_SECRET` names the missing variables (both when both are missing).
- A `GITHUB_WEBHOOK_SECRET` shorter than 16 bytes is refused.
- A `PORT` that is not a whole number from 1 to 65535 is refused. An empty `PORT` means 8080.
- A `FAUCET_GITHUB_API_URL` that is not an absolute `https` URL (or `http` on a loopback host), or that carries user info, a query or a fragment, is refused with `FAUCET_GITHUB_API_URL: GitHub API URL must ...`. The text never quotes the URL. An empty value means `https://api.github.com`. The stub in `app.mjs` is the loopback `http` case.
- A `CRON_SECRET` shorter than 16 bytes is refused. Unset or empty is fine: the [cron routes](cron-auth.md) then answer 503.
- A `FAUCET_RETENTION_DAYS` that is not a whole number from 1 to 3650 is refused. Empty means 30.
- The webhook variables go together. With neither set, webhook delivery is off and the server starts (see [events/list](events-list.md)). With only one set, the server refuses to start and names the one that is missing (`webhook delivery needs ...`), so a typo does not turn the mode off. `FAUCET_WEBHOOK_KEY` must be the standard base64 of exactly 32 bytes that are not all zero: text that is not standard base64, or in the URL-safe alphabet, is refused with `FAUCET_WEBHOOK_KEY must be the standard base64 of exactly 32 bytes`, and 31 bytes, 33 bytes and 32 zero bytes with the same text plus `that are not all zero`. `FAUCET_WEBHOOK_ALLOWED_ORIGINS` is a comma-separated list of `https://host[:port]` origins, each trimmed of spaces. An `http` origin, an origin with a path, user info or a loopback address, and an empty entry (`a,`) are refused with `FAUCET_WEBHOOK_ALLOWED_ORIGINS must be comma-separated https origins written scheme://host[:port]:` and the number of the entry. No message quotes a value, because one is a key. With both set and valid the server starts, and [events/subscribe](events-subscribe.md) accepts the listed origins only.
- A `DATABASE_URL` that does not parse is refused with `DATABASE_URL: not a valid Postgres connection string`. The text never quotes the URL, because the driver's own parse error would.
- `GET /healthz`, an unknown path and `GET /webhook/github` answer as the running server should. Their tests are `tests/health.e2e.ts` and `tests/ingest-wrong-method.e2e.ts`.

## How to get to it (user POV)

An operator starts `faucet` with a bad environment, for example a mistyped secret or a connection string pasted with a stray character.

## Driving it with e2e

`tests/startup.e2e.ts` runs `faucet.binary` with an explicit environment (no inherited variables) for each case, and reads its exit status, stdout and stderr. `PORT` is the running server's own port, so a process that wrongly started could not bind. The test of a webhook configuration that starts uses `faucet.withFaucet(env, ...)` instead, because it needs a running server. Not reachable from a browser, so there is no live lane: use `run_tests {files: ["tests/startup.e2e.ts"]}`.

## Proof

Exit status 1, stderr holding the message above, and zero occurrences of the fake password, the fake webhook secret, the short secret, the fake cron secret, every fake webhook key (a valid one, one that is not base64, the URL-safe one, 31 and 33 bytes, all zero) and the host, path and user info of the bad origins in stdout and stderr. A well-formed `DATABASE_URL` is used where the case does not test it, so the case fails for the reason it names. A well-formed URL to an unreachable database is not tested, because the connect attempt can take up to 30 seconds.

## Charters

No charter. The checks run before the server listens, so no browser session can reach them, and the test above is the whole proof.

## Gotchas

- The checks run in this order: environment variables present, `PORT`, `FAUCET_GITHUB_API_URL`, `CRON_SECRET`, `FAUCET_RETENTION_DAYS`, the webhook variables, `DATABASE_URL` parses, secret length, then the database connection. The first failure wins.
- Exit status 1 is an error faucet chose. A panic would exit with 2, so the tests pin 1.
