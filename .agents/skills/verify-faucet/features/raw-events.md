# Raw events

faucet stores a GitHub event that has no typed family only while a live interest wants it: a webhook subscription to its raw name, or a poll lease. The raw name is `github.<event>`, for example `github.push`. With no interest the delivery is acknowledged with 202 and nothing is stored. faucet offers only the events that `FAUCET_RAW_EVENTS` lists, and none while it is unset.

## Sub-features

- Every faucet that the suite starts has `FAUCET_RAW_EVENTS` set to `RAW_EVENTS` of `verify/env.mjs` (`issue_comment,issues,push,release`). A test sets `FAUCET_RAW_EVENTS: ''` to start one without raw events: its `events/list` offers the typed names only, and a raw name is the error -32023 `unknown event type`.
- A refused event (`repository_advisory`, the secret scanning and Dependabot alerts, `member`, `deploy_key` and the others of the `raw-events` table in `docs/events.md`) in `FAUCET_RAW_EVENTS` makes the server exit 1 with `FAUCET_RAW_EVENTS: <event> is refused: ...`. A name that faucet cannot offer raw, such as `pull_request`, exits 1 too. `tests/startup.e2e.ts` covers both.
- `events/list` offers the raw names of `FAUCET_RAW_EVENTS` after the fifteen typed ones, in rising order. Each takes `repo` (a service also `owner` or `all`, see [Owner targets](owner-targets.md)), and optionally `actions` (the payload's top-level `action`) and `fields` (paths of payload members, for example `repository.id`).
- A signed delivery of such an event is 202 `ignored` with no row while no interest of its repository wants it. With one it is 200 `stored`, and the same delivery ID again is 200 `duplicate`.
- A `release` delivery whose `release.draft` is not `false` is 202 `ignored` with no row, whatever its action and whoever wants it, because GitHub shows a draft only to people with push access. A published release is stored as usual.
- `events/poll` with a raw name and no cursor opens a poll lease and returns no events and a cursor `r1.<position>.<lease>`. A later poll with that cursor returns the events that arrived since, with GitHub's delivery ID as `eventId` and the payload as `data`. The next cursor acknowledges them, and the same cursor returns them again.
- A lease belongs to one poller: another login, other arguments or another target with the same cursor open a new lease and say `truncated`. A lease lives one hour after each poll.
- With `fields`, a poller gets only those members, and faucet stores only the members that some interest asked for.
- `events/subscribe` with a raw name answers a cursor `r1.<position>`. A stored event is sent by `/cron/deliver` as a signed webhook whose body has the raw name as `name`, the payload as `data` and an `r1.` cursor.
- `/cron/purge` drops the payload of a raw event once no delivery and no lease waits for it, and keeps its delivery ID until the retention, so a redelivery stays a duplicate. The reply counts it in `raw_events_cleared`.
- A typed cursor (`v2.` or `v1.`) with a raw name is the JSON-RPC error -32602.
- One event of one target holds at most 100 live subscriptions and leases, and one login at most 10 leases and 10 subscriptions of it. The Go tests pin these limits (`faucet/rawlimits_test.go`, `storetest.RunRaw`); the e2e suite does not fill them.

## How to get to it (user POV)

An agent that wants pushes, releases or issues of a repository polls or subscribes with the raw name. GitHub's webhook must send that event to faucet. Nothing is kept for a repository that nobody asked about.

## Driving it with e2e

`tests/raw-events.e2e.ts` sends signed `push` deliveries, and draft and published `release` deliveries to a lease of `github.release`. The subscribe case runs on a delivery faucet (`withDeliveryFaucet`) with the receiver. The poll, fields and purge cases each run on a faucet of their own (`withFaucet`), so that the purge counts only their rows. `tests/events-list.e2e.ts` checks the raw descriptors.

## Proof

Response: 202 `ignored`, 200 `stored` or 200 `duplicate` from the webhook, and the poll result with `eventId` equal to the delivery ID. Store: `faucet_raw_events` queried by the delivery ID has no row for an unwanted push, one row with a body for a wanted one, and one row without a body after the purge. Receiver: one request with a `valid` signature, `name` `github.push` and the payload as `data`.

## Charters

```
raw-events-interest|http|skeptic|Send signed pushes, releases and issues for repositories with and without a poll lease or a subscription; check that only wanted ones are stored, that a lease gets each once, and that another login's cursor never reads them
raw-events-fields|http|skeptic|Poll and subscribe with fields and actions, with overlapping paths and paths through arrays; check that each interest gets exactly its members and that the stored body holds no other member
```

## Gotchas

- A push is stored only if the interest exists before the delivery arrives. Open the lease or subscribe first.
- The first poll never returns events. It only opens the lease.
- `raw_events_cleared` counts payloads, not rows: the row stays until the retention.
- The shared faucets must hold no raw event and no lease, because `tests/purge.e2e.ts` expects the raw counts of a purge to be zero there.
