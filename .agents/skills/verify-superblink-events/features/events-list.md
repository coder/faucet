# Events list

An agent finds out which events Superblink Events offers through `server/discover` and `events/list` on `POST /github/mcp`. This is the discovery half of the MCP Events extension (SEP-3415). The agent reads it before it polls with [events/poll](events-poll.md).

## Sub-features

- A token that Superblink Events minted for `/github/mcp` is required. No header, a malformed header, a token that Superblink Events did not mint, or a GitHub token is 401 with the body `unauthorized` and the challenge `Bearer resource_metadata="<origin>/.well-known/oauth-protected-resource/github/mcp", scope="events:read events:subscribe"`. The reply does not depend on which repositories the principal can read.
- `server/discover` advertises the extension: `capabilities.extensions["io.modelcontextprotocol/events"]` is `{"listChanged": false}`.
- `events/list` returns `resultType` `complete` and `events`: fifteen typed descriptors, then the raw names that `FAUCET_RAW_EVENTS` lists (see [Raw events](raw-events.md)) in rising order, and none while it is unset. The typed ones are the families `ci.runs`, `ci.jobs`, `ci.checks`, `ci.suites`, `ci.statuses`, `pr.activity`, `pr.reviews`, `pr.comments`, `pr.review_comments` and `pr.review_threads`, then the exact types `ci.check_completed`, `ci.run_completed`, `pr.closed`, `pr.comment_created` and `pr.review_submitted`. See **Event names** in the README.
- A descriptor has `name`, `description`, `delivery` (the same list for every descriptor: `["poll", "webhook"]` when webhook mode is on, which the main Superblink Events server has, and `["poll"]` when it is off), `inputSchema` and `payloadSchema`. Both schemas are JSON Schema objects. `inputSchema` says what the `arguments` of `events/poll` take: a target (`repo`, `owner`, `all` and `repos`, with no required field, since a caller names exactly one of the first three), `sha` and `pr`, and for a family `actions` (a non-empty list from the enum of its GitHub actions) and the [field filters](field-filters.md) whose field its data has, except that `pr.comments` and `pr.comment_created` have no `sha`, and `ci.jobs` and `ci.statuses` have no `pr`. `payloadSchema` says what the `data` of an event holds: `prs`, `head_sha` (not for `pr.comments` and `pr.comment_created`, which have none), `event` (the exact type), `action` and the fields of that type. A family requires `prs`, `event` and `action` only, and an exact type requires the fields it required before families, with `event` and `action` optional.
- With webhook mode off (neither `FAUCET_WEBHOOK_KEY` nor `FAUCET_WEBHOOK_ALLOWED_ORIGINS` set) the list is `["poll"]`, and `events/subscribe` and `events/unsubscribe` are the error -32026 with `data` `{"feature": "deliveryMode", "value": "webhook"}`, whatever their parameters, and write no row. See [events/subscribe](events-subscribe.md).
- The method needs no `Mcp-Name` header. One that is sent is ignored.
- A non-empty `cursor` is the JSON-RPC error -32602, because the names fit one page. An empty `cursor` counts as none. Other unknown `params` fields are ignored.

## How to get to it (user POV)

An agent's MCP client sends `server/discover`, then `events/list`, to `/github/mcp` with `Authorization: Bearer <Superblink Events token>`. Both are stateless 2026-07-28 requests: the header `MCP-Protocol-Version: 2026-07-28`, an `Mcp-Method` header and the `_meta` triple in `params`. The test plays the agent through `faucet.mcp`, as the Superblink Events server's service.

## Driving it with e2e

`tests/events-list.e2e.ts`. The webhook-off case starts its own Superblink Events server with `faucet.withFaucet({}, ...)`, which has no webhook variables, and reads `faucet_subscriptions` on its new database. Live: not reachable from a browser, use `run_tests {files: ["tests/events-list.e2e.ts"]}`.

## Proof

Response: 200 with a JSON-RPC `result`, 400 with the error -32602, or 401 with `unauthorized\n`. The list reads no store. On the Superblink Events server with webhook mode off, `select count(*) from faucet_subscriptions` is 0 after a subscribe and an unsubscribe call. The app log never holds the token or the secret. The last test of `tests/events-poll.e2e.ts` sends `server/discover` and `events/list` with its polls and checks their replies and the log.

## Charters

```
events-list-schemas|http|skeptic|Read events/list, then check each inputSchema against what events/poll accepts and rejects, and each payloadSchema against the data of a delivered event of that type, and report any field, type or required name that differs
events-list-input|http|fuzzer|Send server/discover and events/list with odd params and headers: a cursor of every shape, extra fields, wrong types, a missing params object, odd Mcp-Name values, and report anything that is not a normal result, a -32602 error or a 401
```

## Gotchas

- A JSON-RPC error has an HTTP status of its own. The error -32602 is HTTP 400, not 200, so read the `error` object and not only the status.
- A field of the wrong type (`cursor` as a number) gives a -32602 message from the SDK. It repeats the whole `params` JSON, `_meta` included. The bearer token is in a header, never in `params`, so this is not a leak.
- `delivery` depends on the Superblink Events server, not on the caller. The main Superblink Events server lists `["poll", "webhook"]`, and a Superblink Events server that `withFaucet` starts without the webhook variables lists `["poll"]`.
