# Deploying and operating Superblink Events

Superblink Events is one Go binary plus Postgres 13 or newer. This repository deploys to Vercel with Neon Postgres, and most of this page is about that setup. The [database](#database) and [backups](#restoring-from-a-backup) notes apply anywhere.

## Database

- The server applies pending migrations at startup, before it serves a request.
- `DATABASE_URL` serves requests and can be a transaction-mode pooler, such as Neon's pooled URL.
- When `DATABASE_URL_UNPOOLED` is set, migrations run over it instead. They hold a session-level advisory lock that a pooler doesn't keep.

## Vercel

[`vercel.json`](../vercel.json) deploys `cmd/faucet` as a Vercel Go service with a 90 second request limit (a wait lasts up to 50 seconds), and schedules the three cron routes. The every-minute `/cron/deliver` needs the Pro plan. Vercel runs crons on production deployments only, and sends `Authorization: Bearer $CRON_SECRET`.

### Previews are off, on purpose

A preview runs any branch's code, and the Neon integration gives previews the production database. Since the server migrates at start, a preview of an unmerged migration would change the production schema before review, and every production instance that started afterwards would exit. Three guards prevent that:

- `vercel.json` sets `git.deploymentEnabled` to `false` for every branch except `main`. This only covers Git deployments: `vercel deploy` from a terminal skips it, and Vercel reads it from the commit it builds, so an old branch can still build a preview.
- On Vercel, the server migrates only when `VERCEL_ENV` is `production`. Anywhere else on Vercel it only checks the schema, and exits unless the database is already at the newest migration. Vercel sets `VERCEL_ENV` only when **Automatically expose System Environment Variables** is on, so turn it on, or the server never migrates.
- Database variables and secrets are set for Production only, as sensitive (below).

A preview that starts anyway also needs a public URL: it never derives one (see [the public URL](#the-public-url)), so without `FAUCET_PUBLIC_URL` set for Preview it exits at start.

### Setup

1. **Create the project and database.** Create a Vercel project from this repository and attach a Neon Postgres database with the Vercel integration. It sets `DATABASE_URL` (pooled), `DATABASE_URL_UNPOOLED` (direct) and the `POSTGRES_*` and `PG*` variables, by default for Preview and Development too. Open the database (**Integrations**, **Manage**, the resource, **Settings**), set **Allowed Environments** to **Production only**, then [rotate the credentials](#rotate-the-database-credentials), because the old values were readable.

   Without the dashboard: `vercel integration resource disconnect <resource> <project> --yes`, then reconnect with `vercel api "/v1/integrations/installations/<configuration ID>/resources/<resource ID>/connections?teamId=<team ID>" -X POST --input <file>`, where the file holds `{"projectId": "<project ID>", "envVarEnvironments": ["production"], "makeEnvVarsSensitive": true}`. A build that starts between the two commands has no `DATABASE_URL` and exits.

2. **Set the secrets** for Production only, as sensitive:

   | Variable | Value |
   | --- | --- |
   | `GITHUB_WEBHOOK_SECRET` | The GitHub webhook's secret. |
   | `CRON_SECRET` | `openssl rand -hex 32` |
   | `FAUCET_WEBHOOK_KEY` | `openssl rand -base64 32` |
   | `FAUCET_WEBHOOK_CALLBACK_VERIFICATION` | `handshake`, or unset it and set `FAUCET_WEBHOOK_ALLOWED_ORIGINS` instead. |

   To turn on [redelivery](redelivery.md), also set `FAUCET_HOOK_TOKEN`, `FAUCET_HOOK_ID` and `FAUCET_HOOK_ORG` or `FAUCET_HOOK_REPO` once the GitHub webhook exists. To turn on the [browser login](browser-login.md), also set `FAUCET_GITHUB_APP_CLIENT_ID`, `FAUCET_GITHUB_APP_CLIENT_SECRET` and `FAUCET_TOKEN_KEY`, and keep a copy of the key outside Vercel, because a rotation needs it. Set `FAUCET_PUBLIC_URL` to the production URL as well: every caller logs in there (see [the public URL](#the-public-url)).

3. **Deploy by pushing to `main`.** A variable change only applies to deployments built after it. Don't run `vercel deploy` by hand: on a project with no production deployment, it created one.

4. **Point the GitHub webhook** at `https://<production domain>/webhook/github`. Don't put Vercel Deployment Protection on that domain, because GitHub and MCP clients can't pass it.
5. **Apply the [firewall rules](#firewall).**

### Firewall

[`vercel.firewall.json`](../vercel.firewall.json) holds the project's Vercel Firewall rules. Vercel does not read the file at deploy: `vercel.json` can deny or challenge a route but not rate limit it. Apply the rules with the CLI, from a directory linked to the project (`vercel link`), and publish them:

```sh
jq -c '.[]' vercel.firewall.json | while read -r rule; do
  name=$(jq -r .name <<<"$rule")
  if vercel firewall rules inspect "$name" >/dev/null 2>&1; then
    vercel firewall rules edit "$name" --json "$rule" --yes
  else
    vercel firewall rules add --json "$rule" --yes
  fi
done
vercel firewall diff
vercel firewall publish
```

A change to a rule changes the file and the project together: merge the file, then run the commands. `vercel firewall rules list` shows what is live.

| Rule | Matches | Per IP | Why |
| --- | --- | --- | --- |
| Rate limit MCP | `/github/mcp` and `/mcp` | 120 requests a minute, then 429 | Every caller reaches these before Superblink Events checks its token, and each well-formed Superblink Events access token (`fct_`) costs a database lookup, which anyone can make. Superblink Events' own limits per login hold per process, and Vercel runs many processes. An agent sends a few requests a minute, and a wait holds one for up to 50 seconds. |
| Rate limit consent form | `POST /oauth/authorize` | 10 requests a minute, then 429 | Each post that carries a valid form stores a login request for 10 minutes, and a script can send Superblink Events' `Origin` and keep the cookie, so Superblink Events bounds the rows only by time (see [security.md](security.md#the-browser-login)). A person presses Continue once per login. `GET /oauth/authorize` stores nothing and is left out. |

Vercel counts each region apart, so traffic spread over regions can pass a limit, and callers behind one address share it. Leave `/webhook/github` without a rule. GitHub sends bursts from a few addresses and does not resend a failed delivery, so one that got 429 is lost unless [redelivery](redelivery.md) asks for it again.

### The public URL

Every caller logs in at Superblink Events' public URL, so the server refuses to start without one. With `FAUCET_PUBLIC_URL` unset, a production deployment takes `https://` and `VERCEL_PROJECT_PRODUCTION_URL`, which Vercel sets to the shortest custom domain of the project's production deployment, or its `vercel.app` domain, and logs the URL once at start. A preview or a development deployment never derives the URL, because a preview must not take the issuer of production, so it exits at start without `FAUCET_PUBLIC_URL`, and so does a process without `VERCEL`. **Set `FAUCET_PUBLIC_URL` to pin the URL.** Without it, adding a custom domain shorter than the current one changes the URL at the next deploy. Superblink Events' issuer and its endpoint's URL change with it, so every browser session and access token ends, and every service client made for the old URL gets `invalid_client` until an operator makes a new one.

## Scheduled routes

Each `/cron/*` route answers 200 with a JSON object, and the result type in the table documents each field. A failed `/cron/redeliver` run answers 500 with the same object, holding what the run did, and an `error` that names nothing; the log has the cause. A failed `/cron/deliver` or `/cron/purge` answers 500 with a line of text and no object. When `/cron/deliver` can't read one subscription's events, it skips that subscription and still sends the others' webhooks before it answers 500. A run reads each subscription's events in ranges of 50000 event IDs and keeps its place after each range, and the reads of one principal's subscriptions get 1 second of the run. A run whose reads ran out of their second answers 200 with `incomplete` set, and the next run goes on from where each read stopped. A failure of the raw event store fails neither: the route does its typed work, answers 200 and counts the failure in `raw_failures`, and the log has the cause. A route whose feature is off answers 503.

<!-- BEGIN generated:cron-replies -->
| Route | Reply fields | Documented on |
| --- | --- | --- |
| `GET /cron/purge` | `deleted`, `purged_through`, `expired_subscriptions`, `ended_deliveries`, `raw_events_cleared`, `raw_events_deleted`, `expired_leases`, `raw_failures`, `expired_access_tokens`, `expired_auth_requests`, `ended_sessions`, `unused_connections`, `unused_access_checks` | `faucet.PurgeResult`, `oauth.PurgeResult` |
| `GET /cron/deliver` | `enqueued`, `gaps`, `gaps_sent`, `gaps_failed`, `claimed`, `sent`, `delivered`, `retried`, `gave_up`, `superseded`, `previous_secrets_cleared`, `incomplete`, `raw_failures` | `faucet.DeliverResult` |
| `GET /cron/redeliver` | `busy`, `pages`, `attempts`, `tracked`, `asked`, `recovered`, `gave_up.limit`, `gave_up.expired`, `gaps`, `pending`, `ranges`, `error` | `redelivery.Result` |
<!-- END generated:cron-replies -->

## Runbooks

### Rolling back

**Don't roll back past the newest migration.** A build that doesn't embed the migration that production is at can't start (`no migration found for version N`), so Vercel Instant Rollback to it serves nothing. Recover in one of two ways:

- Redeploy a build that embeds the migration.
- Roll back, then at once apply the migration's `.down.sql` from `faucet/pgstore/migrations/` with golang-migrate against the `faucet_schema_migrations` table. In that order, because production migrates at start: an instance of the newer build that starts after the down migration applies it again. Between the two steps the older build cannot start, so Superblink Events serves nothing. Read the down file first: some drop state. For example, `000007_gap_pending` drops gaps that are owed but not yet sent.

### Rotate the database credentials

Neon rejects the old password as soon as a rotation ends, and a running deployment keeps the values it was built with, so requests fail with 500 until a new production deployment is ready. One rotation took about 40 seconds and failed 20 GitHub webhooks. Turn on [redelivery](redelivery.md) first, so that the next `/cron/redeliver` asks GitHub to resend them.

1. Find the configuration ID (`icfg_...`) with `vercel api "/v1/integrations/configurations?view=account&teamId=<team ID>"`, and the resource ID (`store_...`) in the `contentHint.storeId` of a database variable in `vercel api "/v10/projects/<project>/env?teamId=<team ID>"`.
2. Rotate: write `{"reason": "<why>"}` to a file and run `vercel api "/v1/integrations/installations/<configuration ID>/resources/<resource ID>/secrets/rotate?teamId=<team ID>" -X POST --input <file>`. Or follow [Vercel's guide](https://vercel.com/kb/guide/how-to-reset-a-secret-for-a-neon-integration) in the Neon console.
3. Immediately run `vercel redeploy <production deployment ID> --target production`. It rebuilds the current production commit with the new values.

After a rotation, no deployment built before it can reach the database, so don't Instant Rollback to one. Redeploy its commit instead, and only if it embeds the newest migration.

### Rotating `FAUCET_WEBHOOK_KEY`

Rows sealed under the old key can't be opened. Each subscription's deliveries end with `secret_unreadable` and the subscription is deleted until its subscriber renews, which a live subscriber does within 10 minutes. A renewal that also rotates the secret signs with the new secret only, because the old one can't be opened, so give receivers the new secret first.

`webhook-id` is an HMAC under the key, so after the change a replay of an event that a receiver already has, such as a subscribe after a lapse that sends an old cursor, arrives with a new ID, and a receiver that deduplicates on the ID gets it twice. Warn receivers to expect those duplicates once.

### Rotating `FAUCET_TOKEN_KEY`

The variable holds several keys, so a rotation loses no GitHub token of a user as long as you keep the old key until no row uses it. [browser-login.md](browser-login.md#rotate-the-token-key) has the steps.

### Restoring from a backup

A restore puts event IDs back to the backup's, and Superblink Events can't detect it.

- Each target's positions go back too, so a cursor that a client holds can be above the newest event of its target. Superblink Events reads such a cursor as that newest event at the time of the read, and answers with it. That read skips the events that the target received after the restore, before the read, at or below the cursor, and says `truncated: false`. Clients must drop their cursors, and subscribers must subscribe again without one.
- A webhook's `webhook-id` is derived from the subscription and the event ID, so a new event can reuse the ID of one delivered before it, and a receiver that deduplicates drops it. Clear the receivers' ID caches, or change `FAUCET_WEBHOOK_KEY`.

### Retention at scale

`/cron/purge` scans the event table without an index on `received_at`, about 0.3 seconds per million rows per scan, and stops after 25 seconds. Around 40 million rows, every purge would roll back. No deployment is near that, so Superblink Events doesn't add the index yet.
