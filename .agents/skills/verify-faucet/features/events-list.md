# Events list

An agent finds out which events faucet offers through `server/discover` and `events/list` on `POST /mcp`. This is the discovery half of the MCP Events extension (SEP-3415). The agent reads it before it polls with [events/poll](events-poll.md).

## Sub-features

- A bearer token that GitHub accepts is required. No header, a malformed header, or a token GitHub refuses is 401 with the body `unauthorized` and `WWW-Authenticate: Bearer`. The reply does not depend on which repositories the token can read.
- `server/discover` advertises the extension: `capabilities.extensions["io.modelcontextprotocol/events"]` is `{"listChanged": false}`.
- `events/list` returns `resultType` `complete` and `events`, five descriptors sorted by name: `ci.check_completed`, `ci.run_completed`, `pr.closed`, `pr.comment_created`, `pr.review_submitted`.
- A descriptor has `name`, `description`, `delivery` (`["poll"]` for all five), `inputSchema` and `payloadSchema`. Both schemas are JSON Schema objects. `inputSchema` says what the `arguments` of `events/poll` take: `repo` (required), `sha` and `pr`. `payloadSchema` says what the `data` of an event holds: `prs`, `head_sha` and the fields of that type.
- The method needs no `Mcp-Name` header. One that is sent is ignored.
- A non-empty `cursor` is the JSON-RPC error -32602, because the five types fit one page. An empty `cursor` counts as none. Other unknown `params` fields are ignored.

## How to get to it (user POV)

An agent's MCP client sends `server/discover`, then `events/list`, to `/mcp` with `Authorization: Bearer <GitHub token>`. Both are stateless 2026-07-28 requests: the header `MCP-Protocol-Version: 2026-07-28`, an `Mcp-Method` header and the `_meta` triple in `params`. The stub plays GitHub: it knows one token. The test plays the agent through `faucet.mcp`.

## Driving it with e2e

`tests/events-list.e2e.ts`. Live: not reachable from a browser, use `run_tests {files: ["tests/events-list.e2e.ts"]}`.

## Proof

Response: 200 with a JSON-RPC `result`, 400 with the error -32602, or 401 with `unauthorized\n`. The list reads no store, so there are no rows to check. The app log never holds the token or the secret. The last test of `tests/events-poll.e2e.ts` sends `server/discover` and `events/list` with its polls and checks their replies and the log.

## Charters

```
events-list-schemas|http|skeptic|Read events/list, then check each inputSchema against what events/poll accepts and rejects, and each payloadSchema against the data of a delivered event of that type, and report any field, type or required name that differs
events-list-input|http|fuzzer|Send server/discover and events/list with odd params and headers: a cursor of every shape, extra fields, wrong types, a missing params object, odd Mcp-Name values, and report anything that is not a normal result, a -32602 error or a 401
```

## Gotchas

- A JSON-RPC error has an HTTP status of its own. The error -32602 is HTTP 400, not 200, so read the `error` object and not only the status.
- A field of the wrong type (`cursor` as a number) gives a -32602 message from the SDK. It repeats the whole `params` JSON, `_meta` included. The bearer token is in a header, never in `params`, so this is not a leak.
- `delivery` is `["poll"]` only. `events/subscribe` is not in the server yet.
