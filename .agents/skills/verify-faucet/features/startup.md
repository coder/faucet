# Startup configuration

The `faucet` process reads `DATABASE_URL`, `GITHUB_WEBHOOK_SECRET` and `PORT` from its environment and refuses to start, with exit status 1 and one line on stderr, when they are wrong. It never prints a secret or a database password.

## Sub-features

- A missing `DATABASE_URL` or `GITHUB_WEBHOOK_SECRET` names the missing variables (both when both are missing).
- A `GITHUB_WEBHOOK_SECRET` shorter than 16 bytes is refused.
- A `PORT` that is not a whole number from 1 to 65535 is refused. An empty `PORT` means 8080.
- A `DATABASE_URL` that does not parse is refused with `DATABASE_URL: not a valid Postgres connection string`. The text never quotes the URL, because the driver's own parse error would.
- `GET /healthz`, an unknown path and `GET /webhook/github` answer as the running server should. Their tests are `tests/health.e2e.ts` and `tests/ingest-wrong-method.e2e.ts`.

## How to get to it (user POV)

An operator starts `faucet` with a bad environment, for example a mistyped secret or a connection string pasted with a stray character.

## Driving it with e2e

`tests/startup.e2e.ts` runs `faucet.binary` with an explicit environment (no inherited variables) for each case, and reads its exit status, stdout and stderr. `PORT` is the running server's own port, so a process that wrongly started could not bind. Not reachable from a browser, so there is no live lane: use `run_tests {files: ["tests/startup.e2e.ts"]}`.

## Proof

Exit status 1, stderr holding the message above, and zero occurrences of the fake password, the fake secret and the short secret in stdout and stderr. A well-formed `DATABASE_URL` is used where the case does not test it, so the case fails for the reason it names. A well-formed URL to an unreachable database is not tested, because the connect attempt can take up to 30 seconds.

## Charters

No charter. The checks run before the server listens, so no browser session can reach them, and the test above is the whole proof.

## Gotchas

- The checks run in this order: environment variables present, `PORT`, `DATABASE_URL` parses, secret length, then the database connection. The first failure wins.
- Exit status 1 is an error faucet chose. A panic would exit with 2, so the tests pin 1.
