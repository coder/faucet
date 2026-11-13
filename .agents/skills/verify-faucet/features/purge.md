# Purge

`GET /cron/purge` deletes the events that faucet received before `now - retention`, in one transaction, and records the highest ID it deleted as the purge mark. A poller or waiter whose cursor is below the mark learns that events were lost.

## Sub-features

- The retention is `FAUCET_RETENTION_DAYS`, a whole number of days from 1 to 3650, default 30. An event received exactly that many days ago or later stays.
- The reply is 200 JSON `{"deleted": <rows deleted>, "purged_through": <the mark after the purge>}`. The mark never moves down: a second purge with nothing to delete returns `deleted` 0 and the same mark.
- `events/poll` and `wait_for_github_events` with a cursor below the mark return `truncated: true`, and the events that remain above the cursor. A cursor at or above the mark is not flagged. The wait tool's `truncated` is new in the structured output.
- A cursor that a call returns is never below the mark, also when the purge emptied the table. A waiter that passes it back is not flagged again.
- `CursorAt`, which the poll's `maxAgeMs` and the wait's default start use, is never below the mark either.

## How to get to it (user POV)

An operator schedules the route daily. An agent that waits on a repository through a long break resumes with an old cursor and sees `truncated: true`, so it knows to look at the pull request again instead of trusting an empty result.

## Driving it with e2e

`tests/purge.e2e.ts`, on the cron faucet. A test stores events through `faucet.deliver` and ages them with `faucet.backdate(deliveryId, days)`, which sets `received_at`. `faucet.purgeMark()` reads `faucet_purge_mark`. One test starts its own faucet with `faucet.withFaucet({ FAUCET_RETENTION_DAYS: '2' })`, and another uses one so that the table is empty after the purge. Not reachable from a browser: use `run_tests {files: ["tests/purge.e2e.ts"]}`.

## Proof

Response: 200 with the counts. The expected `deleted` and `purged_through` come from Postgres itself (a count and a max over `received_at` older than 30 days, and the mark before the call), never from the reply. Store: the old rows are gone, the newer ones stay, and `faucet_purge_mark.purged_through` equals the reply. Poll and wait: `truncated` for a cursor below the mark, and `false` at the mark and above. The app log holds `events purged` with `deleted`, `purged_through` and `before`.

## Charters

No charter. Aging rows needs `psql`, which a browser session has no tool for.

## Gotchas

- All tests of the cron faucet share one purge mark. A test that needs a cursor below it takes the cursor before it purges, and a test that needs a clean table starts its own faucet.
- A purge also deletes the events that earlier tests left old, such as the ones `cron-auth.e2e.ts` backdates. The tests compute the expected counts from Postgres for that reason.
- `maxAgeMs` on `events/poll` also sets `truncated`. A true flag means that events may be missing, for either reason.
- The purge does not take the insert lock, so a webhook never waits for a large delete. An event that arrives during a purge gets an ID above everything the purge can delete.
