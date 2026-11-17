# faucet

GitHub events for AI agents, over MCP.

An agent that opened a PR usually has to keep polling GitHub to find out whether CI passed or a reviewer answered. With faucet, it asks once and waits. Faucet receives GitHub webhooks, stores them as typed events in Postgres, and hands them to agents over [MCP](https://modelcontextprotocol.io).

Faucet is a Go library first, with a thin HTTP server on top, so [coder/coder](https://github.com/coder/coder) can embed it in `coderd`.

```text
GitHub ── webhook ──▶ POST /webhook/github ──▶ Postgres ──▶ POST /mcp ◀── agent (wait / poll / subscribe)
                                                  │
                                                  └──▶ GET /cron/deliver ── signed POST ──▶ your callback URL
```

## What an agent gets

Three ways to consume events, all on `/mcp`:

- **Wait.** The `wait_for_github_events` tool long-polls (up to 50 seconds) and returns as soon as a matching event exists. Pass back the returned `cursor` to continue with no gaps or repeats.
- **Poll.** `events/list` and `events/poll` from the draft MCP Events extension (SEP-3415).
- **Subscribe.** `events/subscribe` registers a callback URL. Faucet sends each matching event to it as a signed [Standard Webhooks](https://www.standardwebhooks.com) request. See [docs/webhooks.md](docs/webhooks.md).

Event types, filterable by `repo` and optionally `sha` and `pr`:

| Type | GitHub webhook |
| --- | --- |
| `ci.run_completed` | `workflow_run.completed` |
| `ci.check_completed` | `check_run.completed` |
| `pr.review_submitted` | `pull_request_review.submitted` |
| `pr.closed` | `pull_request.closed` (merged or not) |
| `pr.comment_created` | `issue_comment.created` on a pull request |

Every `/mcp` request carries `Authorization: Bearer <GitHub token>`. Faucet holds no GitHub token of its own: it only returns events for repositories that the caller's token can read. See [docs/security.md](docs/security.md).

Faucet also deletes events after a retention period, and it can ask GitHub to [redeliver webhooks that failed](docs/redelivery.md) while it was down.

## Quick start

You need Go and Postgres 13 or newer.

```sh
DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' \
GITHUB_WEBHOOK_SECRET="$(openssl rand -hex 32)" \
go run ./cmd/faucet
```

The server listens on `PORT` (default 8080) and applies migrations at startup. Then add a webhook to your GitHub org or repository:

- **Payload URL:** `https://<your host>/webhook/github`
- **Content type:** `application/json`
- **Secret:** the value of `GITHUB_WEBHOOK_SECRET`
- **Events:** workflow runs, check runs, pull requests, pull request reviews and issue comments

Point any MCP client at `https://<your host>/mcp` with a GitHub token as the bearer token.

## Configuration

| Variable | Required | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | yes | Postgres connection for requests. A transaction-mode pooler works. |
| `DATABASE_URL_UNPOOLED` | no | Direct connection for migrations, when `DATABASE_URL` is a pooler. |
| `GITHUB_WEBHOOK_SECRET` | yes | The GitHub webhook's secret, at least 16 bytes. |
| `PORT` | no | Listen port. Default 8080. |
| `FAUCET_GITHUB_API_URL` | no | GitHub API base URL. Default `https://api.github.com`. |
| `CRON_SECRET` | for cron routes | Bearer token for `/cron/*`. Unset turns the cron routes off. |
| `FAUCET_RETENTION_DAYS` | no | How long events are kept. Default 30. |
| `FAUCET_WEBHOOK_KEY` | for webhooks | 32 random bytes, base64. Seals subscriber secrets at rest. |
| `FAUCET_WEBHOOK_CALLBACK_VERIFICATION` | for webhooks | `allowlist` (default) or `handshake`. |
| `FAUCET_WEBHOOK_ALLOWED_ORIGINS` | in `allowlist` mode | Comma-separated `https://host[:port]` origins that may receive webhooks. |
| `FAUCET_HOOK_TOKEN`, `FAUCET_HOOK_ID`, `FAUCET_HOOK_ORG` or `FAUCET_HOOK_REPO` | for redelivery | The GitHub webhook to watch for failed deliveries. |

The server refuses to start on an invalid or incomplete feature configuration, and it names the variable but never the value. A feature whose variables are unset stays off, and its routes answer 503.

## Scheduled routes

Faucet sends nothing on its own between requests. A scheduler calls these with `Authorization: Bearer <CRON_SECRET>`:

| Route | Suggested schedule | What it does |
| --- | --- | --- |
| `GET /cron/deliver` | every minute | Sends due webhooks to subscribers and retries failed ones. |
| `GET /cron/redeliver` | every 10 minutes | Asks GitHub to resend webhooks that faucet failed to store. |
| `GET /cron/purge` | hourly | Deletes events past retention, and expired subscriptions. |

[`vercel.json`](vercel.json) sets these schedules for Vercel Cron.

## Deploy

Faucet runs anywhere that runs a Go binary next to Postgres. This repository deploys to Vercel with Neon Postgres. [docs/deploy.md](docs/deploy.md) has the setup, plus the runbooks for credential rotation, rollbacks and database restores.

## Embed it

The packages:

- [`faucet`](faucet): the core. `faucet.New` builds a `Service`. Mount `Service.WebhookHandler()` on your router and call `Service.Register` on your `*mcp.Server`. Bring your own `Authorizer` and principal lookup, or use `TokenAuthorizer`.
- [`faucet/redelivery`](faucet/redelivery): the failed-webhook sweep, built with `redelivery.New`.
- [`faucet/pgstore`](faucet/pgstore): the Postgres store, with embedded migrations.
- [`faucet/storetest`](faucet/storetest): the contract tests that every `Store` must pass.

[`cmd/faucet`](cmd/faucet) is the reference host and wires all of it together. The host decides who may call `Service.Purge`, `Service.DeliverDue` and `Runner.Run`, and when.

## Docs

- [docs/webhooks.md](docs/webhooks.md): the contract for a callback receiver (signatures, retries, cursors, control messages, secret rotation).
- [docs/redelivery.md](docs/redelivery.md): how failed GitHub deliveries are recovered, with the sweep's guarantees, capacity and known limits.
- [docs/security.md](docs/security.md): the access checks, the callback URL guard and secret handling.
- [docs/deploy.md](docs/deploy.md): deploying and operating faucet.

## Development

```sh
FAUCET_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' go test -race ./...
```

Each test creates and drops its own database. After you change a migration or a query, run `go -C tools tool sqlc generate -f ../sqlc.yaml`. The end-to-end checks live in [`verify/`](verify). [AGENTS.md](AGENTS.md) has the rules for working on the code.

## License

[MIT](LICENSE)
