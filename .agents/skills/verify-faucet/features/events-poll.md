# Events poll

An agent reads the events of one repository through `events/poll` on `POST /mcp`. Each call returns at once with the events after a cursor and a new cursor. The agent waits `nextPollMs` and calls again. This is the poll delivery of the MCP Events extension (SEP-3415). The [wait tool](wait-tool.md) reads the same store, and [events/list](events-list.md) names the five event types.

## Sub-features

- A bearer token that GitHub accepts is required. No header, a malformed header, or a token GitHub refuses is 401 with the body `unauthorized` and `WWW-Authenticate: Bearer`. This comes before every other check, the `Mcp-Name` check included.
- The request must carry `Mcp-Name: <params.name>`, plain or in the `=?base64?<base64>?=` form. A missing or different value is HTTP 400 with the JSON-RPC error -32020 (HeaderMismatch) and the request id echoed. faucet checks the header itself, because the SDK does not check it for `events/*`.
- `name` is one of the five event types. An unknown name is the error -32023 with `data` `{"kind": "event"}` and a message that lists the types. The HTTP status is 200.
- `arguments` take `repo` (`owner/name`, required), `sha` (40 hex characters, either case) and `pr` (1 to 2147483647). Anything else is the error -32602 with HTTP 400, and the message names the problem. That covers an unknown argument, a bad repo, a short sha, a pr below 1, a cursor the server did not return, and a `maxEvents` or `maxAgeMs` below 1.
- The token must be able to read the repository. A repository it cannot see and one that does not exist give the same reply: HTTP 200, the error -32024, the message `repository not found or not accessible`, no `data` and no events.
- A poll without `cursor`, or with `cursor` null, starts from now. It returns no events and a cursor at the present, so an event delivered earlier is not replayed. A given `cursor` returns the events after it, oldest first.
- `cursor` is an opaque string and is always in the result, also when `events` is empty. It is a position among the events of all repositories, so it is not tied to a repository or an event name. A cursor taken for one repository works for another.
- The event name picks the type. `sha` keeps the events with that head SHA, and an event that has none (`pr.comment_created`) never matches. `pr` keeps the events whose `prs` hold that number.
- The result is `resultType` `complete`, `events`, `cursor`, `truncated`, `hasMore` and `nextPollMs` (always 5000). An event is `eventId`, `name`, `timestamp` and `data`. `eventId` is the GitHub delivery GUID, the `delivery_id` of the Postgres row. `timestamp` is when faucet received the delivery, ISO 8601 in UTC. `data` is the stored data plus `prs` (maybe empty) and `head_sha` (only when the event has one).
- A page holds at most 100 events. `maxEvents` lowers that and a larger value is cut to 100. `hasMore` says that more events wait, and the cursor then stands at the last event of the page.
- `maxAgeMs`, with a cursor, skips the events older than that many milliseconds. `truncated` is true when it skipped events. It is also true when a [purge](purge.md) deleted events after the cursor.

## How to get to it (user POV)

An agent's MCP client sends `events/poll` to `/mcp` with `Authorization: Bearer <GitHub token>` and `Mcp-Name: <event name>`. The request is a stateless 2026-07-28 request: `MCP-Protocol-Version: 2026-07-28`, `Mcp-Method: events/poll` and the `_meta` triple in `params`. The stub plays GitHub: it knows one token and the repositories named `verify/r<id>`. The test plays GitHub's webhooks through `faucet.deliver` and the agent through `faucet.mcp`.

## Driving it with e2e

`tests/events-poll.e2e.ts`. Live: not reachable from a browser, use `run_tests {files: ["tests/events-poll.e2e.ts"]}`.

## Proof

Response: 200 with the result or with the error -32023 or -32024, 400 with the error -32020 or -32602, or 401 with `unauthorized\n`. Store, through `faucet.rows(repoId)`: the `eventId` of each returned event equals the `delivery_id` of a row of that repository, in order, and a denied or invalid call returns none. The app log holds `poll denied` (fields `repo`, `event`, `login`) and never the token or the secret.

## Charters

```
poll-filters|http|skeptic|Deliver events of every type with different head SHAs and pull requests, then poll each event name with sha and pr alone and combined, and check every returned eventId and data against the rows in Postgres
poll-cursor|http|fuzzer|Chain polls with the cursor of the one before while webhooks arrive for two repositories, with and without maxEvents, and report any event that is returned twice or never
poll-input|http|fuzzer|Send name, arguments, cursor, maxEvents and maxAgeMs values that are odd, huge, empty, null, wrongly typed or oddly encoded, with and without the Mcp-Name header, and report anything that is not a normal result or a clear JSON-RPC error
poll-access|http|skeptic|Poll repositories the token cannot read, that do not exist, or that are spelled with odd case, slashes or encoding, and report any reply that tells the cases apart, returns an event, or leaks the token or the secret
```

## Gotchas

- A poll never waits. A test delivers first and polls second. The start cursor is the one an earlier poll returned, taken before the deliveries.
- An empty poll still returns a cursor, at the newest event of any repository. Do not compare cursors between polls.
- The error -32602 is HTTP 400, but -32023 and -32024 are HTTP 200. Read the `error` object.
- A field of the wrong type (`maxEvents` as 1.5, `cursor` as a number) gives a -32602 message from the SDK. It repeats the whole `params` JSON, `_meta` included. The bearer token is in a header, never in `params`, so this is not a leak.
- The server checks the shape of a cursor, not that it issued it. A well formed cursor ahead of every event, such as `v1.99999999999`, is accepted and returned as it is. Unknown fields next to `name` and `arguments` are ignored.
- `truncated` can be true when no event of the polled repository was skipped. The age floor is the newest event of any repository that is older than `maxAgeMs`, so another repository's old event can move it past the cursor. A true flag means that events may be missing, for this reason or because a purge deleted events after the cursor.
- The `maxAgeMs` test lets 1.5 seconds pass between two deliveries, because the server compares event ages with its own clock.
