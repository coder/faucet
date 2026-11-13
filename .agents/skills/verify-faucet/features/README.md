# Feature map

One file per user-facing behavior of the faucet server at PR 3, from the point of view of its three users: GitHub, which POSTs signed webhooks, an operator who watches `/healthz`, and an agent that waits for events through `/mcp`. Each file names the test that pins it, the proof, and its bug-bash charters. Paths are relative to `verify/`.

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
| [Wait tool](wait-tool.md) | `POST /mcp` | `tests/wait-tool.e2e.ts` | tested |

The webhook handler checks in this order: method, signature header shape, body size, the HMAC, the event and delivery headers, `ping`, then the payload. A rejection earlier in that order wins. A test pins only the first ordering, the signature before the headers. Two more were checked once by hand at PR 2 and are not pinned: an oversize body is 413 before any HMAC, and `ping` needs both headers.

## Later

Planned for PR 4 onward and not in the server yet, so none of it has a test or a feature file. Mark each `tested` here when its file and test land.

| Feature | Planned surface | Status |
| --- | --- | --- |
| Poll | the `events/poll` MCP method with a cursor (PR 4) | untested |
| Subscribe | `events/subscribe` and `events/unsubscribe`, signed webhook delivery to subscribers (PR 6 onward) | untested |

Poll and subscribe need more than this harness has: a longer MCP conversation, and for subscribe a callback receiver the test owns. `faucet.waitForEvents` is the one-call client the wait tool needed. The Postgres store the ingest tests read is the same one they will write to, so `faucet.rows` stays useful.
