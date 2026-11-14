# Cron route

Vercel Cron calls `GET /cron/purge`, `GET /cron/deliver` and `GET /cron/redeliver` with `Authorization: Bearer $CRON_SECRET`. The [purge](purge.md), the [webhook delivery](delivery.md) and the [redelivery](redeliver.md) are the three jobs. None takes an input.

## Sub-features

- Without `CRON_SECRET` in the faucet's environment every route answers 503 with the body `cron is not configured` and runs nothing, whatever the request holds.
- With it, a request without `Authorization: Bearer <secret>` is 401 with `WWW-Authenticate: Bearer` and the body `unauthorized`. One body covers every refusal: no header, a wrong secret (also one of the right length), the right secret in another scheme or with no scheme, one character short, or only in the query string. A refused request runs nothing and asks GitHub for nothing.
- With the right secret, `/cron/deliver` answers 503 with the body `delivery is not configured` when webhook mode is off, and runs nothing. With webhook mode on, a refused request sends no webhook and queues none.
- With the right secret but without the hook settings, `/cron/redeliver` answers 503 with `redelivery is not configured`.
- Only `GET` is routed. `POST`, `PUT` and `DELETE` with the right secret are 405 and run nothing.
- The right secret runs the job and answers 200 with a JSON body. The secret is in no response and no log line.
- The comparison hashes both sides with SHA-256 and compares the digests in constant time, so it does not leak the length or a prefix of the secret. A test cannot see timing, so this is pinned by the Go unit tests, not here.
- A `CRON_SECRET` shorter than 16 bytes stops the start. See [startup](startup.md).

## How to get to it (user POV)

An operator sets `CRON_SECRET` in Vercel (production only) and schedules the two routes (on Vercel, `crons` entries in `vercel.json`). Vercel Cron sends the secret on each call. Nobody else should be able to start a job.

## Driving it with e2e

`tests/cron-auth.e2e.ts`. `app.mjs` starts a second faucet, the cron faucet, with `CRON_SECRET` and no hook settings, on its own database, because a purge moves the purge mark of the whole table. The tests reach it through the `cron` fixture, and `faucet.cronJob(job, opts)` sends the request: `opts.send` says what it carries as a function of the secret, so no test names the secret. The main faucet has no `CRON_SECRET`, which is the 503 case, and it has webhook mode on. The cron faucet has `CRON_SECRET` and no webhook mode, so it answers `/cron/deliver` 503 `delivery is not configured`. A test that needs `/cron/deliver` to run starts a faucet with `withDeliveryFaucet` (see [webhook delivery](delivery.md)). Not reachable from a browser with a secret, so there is no live lane: use `run_tests {files: ["tests/cron-auth.e2e.ts"]}`.

## Proof

Response status and body as above. A refused request leaves an event that is 40 days old in Postgres (`faucet.rows`). `faucet.leaksSecret` is false for every refused and accepted response and for the whole app log.

## Charters

No charter. Every check needs the cron secret or a backdated row, and a browser session reaches neither.

## Gotchas

- The 503 for missing webhook mode comes after the secret check, so a caller without the secret gets 401 and learns nothing about the configuration.
- The routes are mounted outside the bearer middleware of `/mcp`. A GitHub token is not a cron secret, and the cron secret is not a GitHub token.
