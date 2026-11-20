# Purge

`GET /cron/purge` deletes the events that Superblink Events received before `now - retention`, in one transaction, and records the highest ID it deleted as the global purge mark (`faucet_purge_mark`) and, for each repository and owner it deleted events of, the highest place on that line (`faucet_event_lines`). It also deletes the webhook subscriptions that have expired, whether or not webhook mode is on. A poller or waiter whose cursor is below the mark of its target's line learns that events were lost, and one of a repository that the purge did not touch learns nothing.

## Sub-features

- The retention is `FAUCET_RETENTION_DAYS`, a whole number of days from 1 to 3650, default 30. An event received exactly that many days ago or later stays.
- The reply is 200 JSON `{"deleted": <rows deleted>, "purged_through": <the mark after the purge>, "expired_subscriptions": <rows deleted>, "ended_deliveries": <deliveries that ended with them>, "raw_events_cleared": <raw payloads dropped>, "raw_events_deleted": <raw events deleted>, "expired_leases": <poll leases deleted>, "raw_failures": <calls of the raw store that failed>, "expired_access_tokens": <Superblink Events access tokens past their expiry>, "expired_auth_requests", "ended_sessions", "unused_connections", "unused_access_checks"}`. The last four are the browser login's (see [Browser login](browser-login.md)); the cron Superblink Events server has no browser login, so they are 0 there. The three raw counts are covered by [Raw events](raw-events.md). `purged_through` is the global mark, an event ID, and never a cursor that a reader gets. The mark never moves down: a second purge with nothing to delete returns `deleted` 0 and the same mark.
- `events/poll` and `wait_for_github_events` with a cursor below the mark of the target's line return `truncated: true`, and the events that remain above the cursor. A cursor at or above that mark is not flagged, and a purge that deleted only other repositories' events flags no cursor of this one.
- A cursor that a call returns is never below the mark of its line, also when the purge deleted every event of the repository. A waiter that passes it back is not flagged again.
- A subscription whose `expires_at` has passed is deleted by the purge, and one that has not stays. The deliveries that such a subscription still held end with it: the reply counts them in `ended_deliveries`, and the log line `events purged` adds `expired_subscriptions_deleted` and `deliveries_ended`.
- `PositionAt`, which the poll's `maxAgeMs` and the wait's default start use, is never below the mark of the line either.

## How to get to it (user POV)

An operator schedules the route daily. An agent that waits on a repository through a long break resumes with an old cursor and sees `truncated: true`, so it knows to look at the pull request again instead of trusting an empty result.

## Driving it with e2e

`tests/purge.e2e.ts`, on the cron Superblink Events server. A test stores events through `faucet.deliver` and ages them with `faucet.backdate(deliveryId, days)`, which sets `received_at`. `faucet.purgeMark()` reads `faucet_purge_mark`, and `faucet.repoMark(repoId)` the mark of a repository's line. One test starts its own Superblink Events server with `faucet.withFaucet({ FAUCET_RETENTION_DAYS: '2' })`, and another uses one so that the table is empty after the purge. The subscription test starts a Superblink Events server with webhook mode on, subscribes twice, moves the expiry of one row into the past with `faucet.expireSubscriptions(marker)` (the stub cannot move the server's clock), and purges. Not reachable from a browser: use `run_tests {files: ["tests/purge.e2e.ts"]}`.

## Proof

Response: 200 with the counts. The expected `deleted` and `purged_through` come from Postgres itself (a count and a max over `received_at` older than 30 days, and the mark before the call), never from the reply. Store: the old rows are gone, the newer ones stay, and `faucet_purge_mark.purged_through` equals the reply. Poll and wait: `truncated` for a cursor below `faucet.repoMark(repoId)`, `false` at it and above, and `false` for a repository whose events the purge left alone. The app log holds `events purged` with `deleted`, `purged_through`, `before` and `expired_subscriptions_deleted`. For subscriptions the proof is the rows: `faucet.subscriptions(marker)` is empty for the expired one and holds the live one.

## Charters

No charter. Aging rows needs `psql`, which a browser session has no tool for.

## Gotchas

- All tests of the cron Superblink Events server share one global purge mark, and each test's repository has its own line mark. A test that needs a cursor below the mark takes the cursor before it purges, and a test that needs a clean table starts its own Superblink Events server.
- A purge also deletes the events that earlier tests left old, such as the ones `cron-auth.e2e.ts` backdates. The tests compute the expected counts from Postgres for that reason.
- `maxAgeMs` on `events/poll` also sets `truncated`, and so does a `v1` cursor. A true flag means that events may be missing, for any of these reasons.
- The purge does not take the insert lock, so a webhook for a new delivery does not wait for a large delete. An event that arrives during a purge gets an ID above everything the purge can delete. One case waits: an insert for a delivery ID that the purge is deleting right then (a manual redelivery of an event older than the retention) waits until the purge commits, and it holds the insert lock while it waits, so the webhooks that arrive behind it wait too. The wait lasts as long as the delete, and no test pins it.
