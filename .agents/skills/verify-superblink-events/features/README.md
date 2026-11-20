# Feature map

One file per user-facing behavior of the Superblink Events server at PR 8b, from the point of view of its four users: GitHub, which POSTs signed webhooks, an operator who watches `/healthz`, schedules the cron routes and makes clients for services, a scheduler such as Vercel Cron, and an agent that waits for, lists, polls or subscribes to events through `/github/mcp` and receives the signed webhooks at its callback URL. Each file names the test that pins it, the proof, and its bug-bash charters. Paths are relative to `verify/`.

| Feature | Route | Test | Status |
| --- | --- | --- | --- |
| [Health](health.md) | `GET /healthz` | `tests/health.e2e.ts`, `tests/smoke.e2e.ts` | tested |
| [Stored events](ingest-stored.md) | `POST /webhook/github` | `tests/ingest-stored.e2e.ts` | tested |
| [Duplicate deliveries](ingest-duplicate.md) | `POST /webhook/github` | `tests/ingest-duplicate.e2e.ts` | tested |
| [Bad signature](ingest-bad-signature.md) | `POST /webhook/github` | `tests/ingest-bad-signature.e2e.ts` | tested |
| [Missing headers](ingest-missing-headers.md) | `POST /webhook/github` | `tests/ingest-missing-headers.e2e.ts` | tested |
| [Ignored events](ingest-ignored.md) | `POST /webhook/github` | `tests/ingest-ignored.e2e.ts` | tested |
| [Malformed payloads](ingest-malformed.md) | `POST /webhook/github` | `tests/ingest-malformed.e2e.ts` | tested |
| [Ping](ingest-ping.md) | `POST /webhook/github` | `tests/ingest-ping.e2e.ts` | tested |
| [Oversize body](ingest-oversize.md) | `POST /webhook/github` | `tests/ingest-oversize.e2e.ts` | tested |
| [Wrong method](ingest-wrong-method.md) | `/webhook/github` | `tests/ingest-wrong-method.e2e.ts` | tested |
| [Startup configuration](startup.md) | the `faucet` process, before it listens | `tests/startup.e2e.ts` | tested |
| [MCP paths](mcp-paths.md) | `POST /github/mcp` and its alias `POST /mcp` | `tests/mcp-paths.e2e.ts` | tested |
| [Wait tool](wait-tool.md) | `POST /github/mcp` | `tests/wait-tool.e2e.ts` | tested |
| [Events list](events-list.md) | `POST /github/mcp`, `server/discover` and `events/list` | `tests/events-list.e2e.ts` | tested |
| [Events poll](events-poll.md) | `POST /github/mcp`, `events/poll` | `tests/events-poll.e2e.ts` | tested |
| [Events subscribe](events-subscribe.md) | `POST /github/mcp`, `events/subscribe` | `tests/events-subscribe.e2e.ts`, `tests/events-subscribe-types.e2e.ts` | tested |
| [Field filters](field-filters.md) | `POST /github/mcp`, `events/poll`, `events/subscribe` and the wait tool with `base_branch`, `head_branch` or `conclusion`, then `GET /cron/deliver` | `tests/field-filters.e2e.ts` | tested |
| [Raw events](raw-events.md) | `POST /webhook/github` for an event with no typed rule, `events/poll` and `events/subscribe` with a `github.<event>` name, `GET /cron/deliver`, `GET /cron/purge` | `tests/raw-events.e2e.ts`, `tests/events-list.e2e.ts` | tested |
| [Events unsubscribe](events-unsubscribe.md) | `POST /github/mcp`, `events/unsubscribe` | `tests/events-unsubscribe.e2e.ts` | tested |
| [Webhook handshake](webhook-handshake.md) | `POST /github/mcp`, `events/subscribe` in `handshake` mode, and the request that Superblink Events sends to the callback URL | `tests/handshake.e2e.ts` | tested |
| [Cron routes](cron-auth.md) | `GET /cron/purge`, `GET /cron/deliver`, `GET /cron/redeliver`, the secret check | `tests/cron-auth.e2e.ts` | tested |
| [Purge](purge.md) | `GET /cron/purge`, then `events/poll`, the wait tool and the subscriptions table | `tests/purge.e2e.ts` | tested |
| [Control envelopes](envelopes.md) | `GET /cron/deliver` after a purge, and `events/subscribe` after a revoked renewal, then the signed `POST` of a `gap` or `terminated` body to the callback URL | `tests/envelopes.e2e.ts` | tested |
| [Secret rotation](rotation.md) | `POST /github/mcp`, `events/subscribe` with a new secret, then `GET /cron/deliver` and the signed `POST` to the callback URL | `tests/rotation.e2e.ts` | tested |
| [Webhook delivery](delivery.md) | `GET /cron/deliver`, then the signed `POST` to the callback URL | `tests/delivery.e2e.ts`, `tests/delivery-retry.e2e.ts`, `tests/cron-auth.e2e.ts` | tested |
| [Service accounts](service-accounts.md) | `faucet clients`, `POST /oauth/token`, `/.well-known/oauth-*`, then `POST /github/mcp` and `POST /mcp` with a Superblink Events token | `tests/service-accounts.e2e.ts` | tested |
| [Owner targets](owner-targets.md) | `faucet clients create -grant org:<login>`, then `events/subscribe`, `events/poll` and the wait tool on `POST /github/mcp` with `owner` or `all`, then `GET /cron/deliver` | `tests/owner-targets.e2e.ts` | tested |
| [Browser login](browser-login.md) | `GET` and `POST /oauth/authorize`, the stub's GitHub, `GET /oauth/github/callback`, `POST /oauth/token` with a code or a refresh token, then `POST /github/mcp` with a person's Superblink Events token | `tests/browser-login.e2e.ts` | tested |
| [Redelivery](redeliver.md) | `GET /cron/redeliver`, the deliveries stub, the redelivery tables and the app log | `tests/redeliver.e2e.ts`, `tests/cron-auth.e2e.ts`, `tests/startup.e2e.ts` | tested |

The webhook handler checks in this order: method, signature header shape, body size, the HMAC, the event and delivery headers, `ping`, then the payload. A rejection earlier in that order wins. A test pins only the first ordering, the signature before the headers. Two more were checked once by hand at PR 2 and are not pinned: an oversize body is 413 before any HMAC, and `ping` needs both headers.
