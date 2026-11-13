# Feature map

One file per user-facing behavior of the faucet server at PR 5, from the point of view of its four users: GitHub, which POSTs signed webhooks, an operator who watches `/healthz` and schedules the cron routes, a scheduler such as Vercel Cron, and an agent that waits for, lists or polls events through `/mcp`. Each file names the test that pins it, the proof, and its bug-bash charters. Paths are relative to `verify/`.

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
| [Wait tool](wait-tool.md) | `POST /mcp` | `tests/wait-tool.e2e.ts` | tested |
| [Events list](events-list.md) | `POST /mcp`, `server/discover` and `events/list` | `tests/events-list.e2e.ts` | tested |
| [Events poll](events-poll.md) | `POST /mcp`, `events/poll` | `tests/events-poll.e2e.ts` | tested |
| [Cron routes](cron-auth.md) | `GET /cron/purge`, `GET /cron/redeliver`, the secret check | `tests/cron-auth.e2e.ts` | tested |
| [Purge](purge.md) | `GET /cron/purge`, then `events/poll` and the wait tool | `tests/purge.e2e.ts` | tested |
| [Redelivery sweep](redeliver.md) | `GET /cron/redeliver` | `tests/redeliver.e2e.ts` | tested |

The webhook handler checks in this order: method, signature header shape, body size, the HMAC, the event and delivery headers, `ping`, then the payload. A rejection earlier in that order wins. A test pins only the first ordering, the signature before the headers. Two more were checked once by hand at PR 2 and are not pinned: an oversize body is 413 before any HMAC, and `ping` needs both headers.

## Later

Planned for PR 6 onward and not in the server yet, so it has no test or feature file. Mark it `tested` here when its file and test land.

| Feature | Planned surface | Status |
| --- | --- | --- |
| Subscribe | `events/subscribe` and `events/unsubscribe`, signed webhook delivery to subscribers (PR 6 onward) | untested |

Subscribe needs more than this harness has: a callback receiver that the test owns, to take the signed deliveries. `faucet.mcp` calls any MCP method, so the subscribe calls need no new client. `faucet.sql` reads any table the store gains.
