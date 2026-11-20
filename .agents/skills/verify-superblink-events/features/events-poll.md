# Events poll

An agent reads the events of one repository through `events/poll` on `POST /github/mcp`. Each call returns at once with the events after a cursor and a new cursor. The agent waits `nextPollMs` and calls again. This is the poll delivery of the MCP Events extension (SEP-3415). The [wait tool](wait-tool.md) reads the same store, and [events/list](events-list.md) names the five families and the five exact types.

## Sub-features

- A token that Superblink Events minted for `/github/mcp` is required. No header, a malformed header, a token that Superblink Events did not mint, or a GitHub token is 401 with the body `unauthorized` and the challenge `Bearer resource_metadata="<origin>/.well-known/oauth-protected-resource/github/mcp", scope="events:read events:subscribe"`. This comes before every other check, the `Mcp-Name` check included.
- The request must carry `Mcp-Name: <params.name>`, plain or in the `=?base64?<base64>?=` form. A missing or different value is HTTP 400 with the JSON-RPC error -32020 (HeaderMismatch) and the request id echoed. Superblink Events checks the header itself, because the SDK does not check it for `events/*`.
- `name` is one of the fifteen typed event names. A raw name such as `github.push` is served from a poll lease, see [Raw events](raw-events.md). An unknown name is the error -32023 with `data` `{"kind": "event"}` and a message that lists the types. The HTTP status is 200.
- `arguments` take one target, `repo` (`owner/name`) or, for a service, `owner` or `all` (see [Owner targets](owner-targets.md)), and `sha` (40 hex characters, either case) and `pr` (1 to 2147483647). Anything else is the error -32602 with HTTP 400, and the message names the problem. That covers an unknown argument, a bad repo, a short sha, a pr below 1, a cursor that is not `v2.<n>` or `v1.<n>`, and a `maxEvents` or `maxAgeMs` below 1.
- The principal must be able to read the repository. For the person of the browser login, GitHub decides: a repository the person cannot see and one that does not exist give the same reply: HTTP 200, the error -32024, the message `repository not found or not accessible`, no `data` and no events. A service reads a repository that a grant covers and a delivery named, and any other is the error -32024 with a message that says why.
- A poll without `cursor`, or with `cursor` null, starts from now. It returns no events and a cursor at the present, so an event delivered earlier is not replayed. A given `cursor` returns the events after it, oldest first.
- `cursor` is `v2.<n>` and is always in the result, also when `events` is empty. `n` is a place on the line of the target that the call names: a repository numbers its events 1, 2, 3 in the order Superblink Events stored them (`faucet_events.repo_seq`), an owner the events of its repositories (`owner_seq`), and `all` counts every event (the event ID). So the cursor, `hasMore` and `truncated` count the target's events and never move for another repository's. A repository with no event is at `v2.0`, and `v2.0` reads it from its start. A cursor of another target is read as a place on this target's line.
- A `v2` cursor above the head of the line reads as the head: the result names the head, not the given value, so the next event is not skipped. A `v1.<n>` cursor of the build before counts the events of every repository, so it is never read as a place: the result has no events, `truncated: true` and the cursor of the head.
- The event name picks the types: an exact type itself, a family every type of its GitHub event, and a family with `actions` the types of those actions. An action outside the family's enum, an empty `actions` and `actions` for an exact type are `InvalidParams`. A family also takes the [field filters](field-filters.md) that it offers. `sha` keeps the events with that head SHA. `pr.comments` and `pr.comment_created` take no `sha`, because GitHub names no head commit in a comment, and a `sha` for it is `InvalidParams`. `pr` keeps the events whose `prs` hold that number. `ci.jobs` and `ci.statuses` take no `pr`, because GitHub names no pull request in a workflow job or a status, and a `pr` for them is `InvalidParams`.
- Each CI family returns the recorded webhooks of its actions (`tests/payloads.ts` `recordedSamples`, from `faucet/testdata`) with their exact type in `data.event`. A status has no action, so its `data.action` is its state. `ci.runs` with `actions: ["completed"]` returns a completed run and not a requested one.
- The result is `resultType` `complete`, `events`, `cursor`, `truncated`, `hasMore` and `nextPollMs` (always 5000). An event is `eventId`, `name` (the polled name, also for a family), `timestamp` and `data`. `eventId` is the GitHub delivery GUID, the `delivery_id` of the Postgres row. `timestamp` is when Superblink Events received the delivery, ISO 8601 in UTC. `data` is the stored data plus `prs` (maybe empty), `head_sha` (only when the event has one), `event` (the exact type) and `action` (the GitHub action).
- A page holds at most 100 events. `maxEvents` lowers that and a larger value is cut to 100. `hasMore` says that more may wait: either the page is full, and the cursor stands at its last event, or the read stopped at the end of its window of 50000 places on the target's line before the newest event, and the cursor stands at the window's end. A page that stopped at a window can be short or empty. Poll again with the cursor until `hasMore` is false.
- `maxAgeMs`, with a cursor, skips the events of the target older than that many milliseconds. `truncated` is true when it skipped events. It is also true when a [purge](purge.md) deleted events of the target after the cursor. An old event of another repository sets neither.

## How to get to it (user POV)

An agent's MCP client sends `events/poll` to `/github/mcp` with `Authorization: Bearer <Superblink Events token>` and `Mcp-Name: <event name>`. The request is a stateless 2026-07-28 request: `MCP-Protocol-Version: 2026-07-28`, `Mcp-Method: events/poll` and the `_meta` triple in `params`. The test plays GitHub's webhooks through `faucet.deliver` and the agent through `faucet.mcp`, as the Superblink Events server's service, or as the person on a Superblink Events server with the browser login, where the stub plays GitHub and knows the repositories named `verify/r<id>`.

## Driving it with e2e

`tests/events-poll.e2e.ts`. It sends each of the `recordedSamples` in `tests/payloads.ts`, a recorded GitHub payload of one new action per pull request family, and polls its family narrowed to that action and to another one. Live: not reachable from a browser, use `run_tests {files: ["tests/events-poll.e2e.ts"]}`.

## Proof

Response: 200 with the result or with the error -32023 or -32024, 400 with the error -32020 or -32602, or 401 with `unauthorized\n`. Store, through `faucet.rows(repoId)`: the `eventId` of each returned event equals the `delivery_id` of a row of that repository, in order, and a denied or invalid call returns none. The app log holds `poll denied` (fields `repo`, `event`, `login`) and never the token or the secret.

## Charters

```
poll-filters|http|skeptic|Deliver events of every type with different head SHAs and pull requests, then poll each event name with sha and pr alone and combined, and check every returned eventId and data against the rows in Postgres
poll-cursor|http|fuzzer|Chain polls with the cursor of the one before while webhooks arrive for two repositories, with and without maxEvents, and report any event that is returned twice or never
poll-input|http|fuzzer|Send name, arguments, cursor, maxEvents and maxAgeMs values that are odd, huge, empty, null, wrongly typed or oddly encoded, with and without the Mcp-Name header, and report anything that is not a normal result or a clear JSON-RPC error
poll-access|http|skeptic|Poll repositories the principal cannot read, that do not exist, or that are spelled with odd case, slashes or encoding, and report any reply that tells the cases apart, returns an event, or leaks the token or the secret
```

## Gotchas

- A poll never waits. A test delivers first and polls second. The start cursor is the one an earlier poll returned, taken before the deliveries.
- An empty poll still returns a cursor, at the newest event of the polled repository. Each test polls a repository of its own (`newRepoId`), so it can name the cursor exactly (`v2.0` before its first delivery).
- The error -32602 is HTTP 400, but -32023 and -32024 are HTTP 200. Read the `error` object.
- A field of the wrong type (`maxEvents` as 1.5, `cursor` as a number) gives a -32602 message from the SDK. It repeats the whole `params` JSON, `_meta` included. The bearer token is in a header, never in `params`, so this is not a leak.
- The server checks the shape of a cursor, not that it issued it. A well formed cursor ahead of the head, such as `v2.99999999999`, reads as the head. Unknown fields next to `name` and `arguments` are ignored.
- A true `truncated` means that events of the target may be missing: `maxAgeMs` skipped some, a purge deleted some after the cursor, or the cursor was a `v1` one.
- The `maxAgeMs` test lets 1.5 seconds pass between two deliveries, because the server compares event ages with its own clock.
