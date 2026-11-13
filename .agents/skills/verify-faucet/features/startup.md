# Startup configuration

The `faucet` process reads `DATABASE_URL`, `GITHUB_WEBHOOK_SECRET`, `PORT`, `FAUCET_GITHUB_API_URL`, `CRON_SECRET`, `FAUCET_RETENTION_DAYS` and the hook variables `FAUCET_HOOK_TOKEN`, `FAUCET_HOOK_ORG`, `FAUCET_HOOK_REPO` and `FAUCET_HOOK_ID` from its environment and refuses to start, with exit status 1 and one line on stderr, when they are wrong. It never prints a secret or a database password.

## Sub-features

- A missing `DATABASE_URL` or `GITHUB_WEBHOOK_SECRET` names the missing variables (both when both are missing).
- A `GITHUB_WEBHOOK_SECRET` shorter than 16 bytes is refused.
- A `PORT` that is not a whole number from 1 to 65535 is refused. An empty `PORT` means 8080.
- A `FAUCET_GITHUB_API_URL` that is not an absolute `https` URL (or `http` on a loopback host), or that carries user info, a query or a fragment, is refused with `FAUCET_GITHUB_API_URL: GitHub API URL must ...`. The text never quotes the URL. An empty value means `https://api.github.com`. The stub in `app.mjs` is the loopback `http` case.
- A `CRON_SECRET` shorter than 16 bytes is refused. Unset or empty is fine: the [cron routes](cron-auth.md) then answer 503.
- A `FAUCET_RETENTION_DAYS` that is not a whole number from 1 to 3650 is refused. Empty means 30.
- The hook variables go together. `FAUCET_HOOK_TOKEN`, `FAUCET_HOOK_ID` and one of `FAUCET_HOOK_ORG` or `FAUCET_HOOK_REPO` are needed once any of the four is set, and the message names what is missing (`redelivery needs ...`). Both org and repo is refused (`set only one of ...`). An id that is not a positive integer, a token with a space or a control character, and an org or repo that is not a valid name are refused. With none set, redelivery is off and the server starts. No message quotes a value, because the token is a credential.
- A `DATABASE_URL` that does not parse is refused with `DATABASE_URL: not a valid Postgres connection string`. The text never quotes the URL, because the driver's own parse error would.
- `GET /healthz`, an unknown path and `GET /webhook/github` answer as the running server should. Their tests are `tests/health.e2e.ts` and `tests/ingest-wrong-method.e2e.ts`.

## How to get to it (user POV)

An operator starts `faucet` with a bad environment, for example a mistyped secret or a connection string pasted with a stray character.

## Driving it with e2e

`tests/startup.e2e.ts` runs `faucet.binary` with an explicit environment (no inherited variables) for each case, and reads its exit status, stdout and stderr. `PORT` is the running server's own port, so a process that wrongly started could not bind. Not reachable from a browser, so there is no live lane: use `run_tests {files: ["tests/startup.e2e.ts"]}`.

## Proof

Exit status 1, stderr holding the message above, and zero occurrences of the fake password, the fake webhook secret, the short secret, the fake cron secret and the fake hook token in stdout and stderr. A well-formed `DATABASE_URL` is used where the case does not test it, so the case fails for the reason it names. A well-formed URL to an unreachable database is not tested, because the connect attempt can take up to 30 seconds.

## Charters

No charter. The checks run before the server listens, so no browser session can reach them, and the test above is the whole proof.

## Gotchas

- The checks run in this order: environment variables present, `PORT`, `FAUCET_GITHUB_API_URL`, `CRON_SECRET`, `FAUCET_RETENTION_DAYS`, the hook variables, `DATABASE_URL` parses, secret length, then the database connection. The first failure wins.
- Exit status 1 is an error faucet chose. A panic would exit with 2, so the tests pin 1.
