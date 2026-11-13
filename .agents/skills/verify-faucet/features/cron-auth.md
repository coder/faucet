# Cron routes

Vercel Cron calls `GET /cron/purge` and `GET /cron/redeliver` with `Authorization: Bearer $CRON_SECRET`. The [purge](purge.md) and the [redelivery sweep](redeliver.md) are the two jobs. Neither takes an input.

## Sub-features

- Without `CRON_SECRET` in the faucet's environment both routes answer 503 with the body `cron is not configured` and run nothing, whatever the request holds.
- With it, a request without `Authorization: Bearer <secret>` is 401 with `WWW-Authenticate: Bearer` and the body `unauthorized`. One body covers every refusal: no header, a wrong secret (also one of the right length), the right secret in another scheme or with no scheme, one character short, or only in the query string. A refused request runs nothing.
- Only `GET` is routed. `POST`, `PUT` and `DELETE` with the right secret are 405 and run nothing.
- The right secret runs the job and answers 200 with a JSON body. The secret is in no response and no log line.
- The comparison hashes both sides with SHA-256 and compares the digests in constant time, so it does not leak the length or a prefix of the secret. A test cannot see timing, so this is pinned by the Go unit tests, not here.
- A `CRON_SECRET` shorter than 16 bytes stops the start. See [startup](startup.md).

## How to get to it (user POV)

An operator sets `CRON_SECRET` in Vercel (production only) and schedules the two routes (on Vercel, `crons` entries in `vercel.json`). Vercel Cron sends the secret on each call. Nobody else should be able to start a job.

## Driving it with e2e

`tests/cron-auth.e2e.ts`. `app.mjs` starts a second faucet, the cron faucet, with `CRON_SECRET` and the hook settings of the stub's org webhook, on its own database, because a purge moves the purge mark of the whole table. The tests reach it through the `cron` fixture, and `faucet.cronJob(job, opts)` sends the request: `opts.send` says what it carries as a function of the secret, so no test names the secret. The main faucet has no `CRON_SECRET`, which is the 503 case. Not reachable from a browser with a secret, so there is no live lane: use `run_tests {files: ["tests/cron-auth.e2e.ts"]}`.

## Proof

Response status and body as above. A refused request leaves an event that is 40 days old in Postgres (`faucet.rows`) and sends no request to the stub's hook API (`hooks.requests()`). `faucet.leaksSecret` is false for every refused and accepted response and for the whole app log.

## Charters

No charter. Every check needs the cron secret, a backdated row or the stub's control calls, and a browser session reaches none of them.

## Gotchas

- The 503 for a missing hook configuration comes after the secret check, so a caller without the secret gets 401 and learns nothing about the configuration.
- The routes are mounted outside the bearer middleware of `/mcp`. A GitHub token is not a cron secret, and the cron secret is not a GitHub token.
