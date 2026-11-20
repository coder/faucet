# Redelivery

`GET /cron/redeliver` asks GitHub to deliver again each webhook delivery that failed, that Superblink Events would store, and that Superblink Events has not stored. It keeps its place in GitHub's delivery list in Postgres, so a run that cannot read all of it goes on where it stopped. Every failed delivery ends in one counted, logged final state: recovered, or given up with a reason (`limit`, `expired`), and a range of attempts that expired unread is counted and logged as a gap.

## Sub-features

- The reply is 200 JSON `{"busy", "pages", "attempts", "tracked", "asked", "recovered", "gave_up": {"limit", "expired"}, "gaps", "pending", "ranges"}`: pages and attempts read, deliveries that became pending, requests GitHub accepted, deliveries recovered, deliveries given up by reason, gaps recorded, deliveries still pending and ranges of the list still unread.
- A delivery qualifies when an attempt failed (any answer outside 200 to 299, or none), its event and action have a rule in `Normalize`, and Superblink Events has no event with its GUID. A later attempt that succeeded, or a stored event, recovers it with no further request.
- A delivery is asked for at most 7 times, at 0, 10, 30, 70, 150, 310 and 630 minutes after the run that first read it, and at most 50 asks go out per run, the delivery that has waited longest first. The ask count is raised in the statement that claims the delivery, before the request. After the last ask and 5 minutes the delivery is given up with `reason=limit`. GitHub keeps a delivery for 72 hours, counted from its first attempt, and a delivery still pending then is given up with `reason=expired`.
- A run reads the newest attempts first, down to the newest attempt of the run before it less 5 minutes (the overlap that catches an attempt GitHub wrote late), and at most 20 pages. A stretch it cannot read is a range: its cursor and its two times are rows of `faucet_redelivery_ranges`, and later runs read the ranges, newest first, with the pages that the head read leaves. A range whose old end leaves GitHub's retention unread becomes a gap, and so does the time before the first run's lookback of 24 hours below the newest attempt (`cause` `expired unread` and `before tracking began`).
- One run holds a lease (`faucet_redelivery_scan`) at a time. A second run that starts meanwhile answers `busy: true` and does nothing.
- A page that fails to list stops the asks of the run, and so does a request that GitHub refuses (500 with the counts of the run and `"error": "redelivery failed"`, so the outcomes recorded before the failure still reach a reply. No response or log holds the hook token or GitHub's body). A refused ask still counts, and a rate limit is told from a denial in the log and handled the same way. The settle step needs no GitHub, so an expiry is still given up.
- A run keeps to the 25 second deadline of the route: half for reading the list (a page not back by then is dropped, and the rest is an open range), then the settle, asks until 80 percent, and the purge. A slow list costs pages, never the asks.
- A pending delivery belongs to the webhook that held the lease when it was tracked. After the webhook is replaced, a run does not ask the new one for the old one's deliveries: they end `recovered` or `expired`.
- The position of a run is the time of the newest attempt that it read. An empty list leaves it. The position never comes from Superblink Events' clock or from GitHub's `Date` header, and a `Date` can only lower where a read stops. The `Date` says where a gap ends (72 hours before it, plus 1 hour of slack), and the `Date` of the last run says where a gap after a silence starts (6 hours and 5 minutes before it). A first page whose `Date` is missing, or more than 1 hour behind the newest attempt, fails the run: 500, and no asks.
- A final delivery and a recorded gap are deleted 72 hours after their last attempt, so each give-up is logged once. Pending deliveries, open ranges and the scan row stay.
- Without the hook settings the route answers 503 with `redelivery is not configured`, after the secret check.
- Only the hook token goes to GitHub, and only to two endpoints: `GET .../hooks/{id}/deliveries` and `POST .../deliveries/{attempt}/attempts`.

## How to get to it (user POV)

An operator sets `FAUCET_HOOK_TOKEN`, `FAUCET_HOOK_ID` and one of `FAUCET_HOOK_ORG` or `FAUCET_HOOK_REPO` (production only) and schedules the route every 10 minutes. A webhook that arrived while Superblink Events was down or the database refused it is then delivered again without anyone clicking, and the operator reads the reply and the log to see what ended how.

## Driving it with e2e

`tests/redeliver.e2e.ts`, each test on its own Superblink Events server from `cron.withFaucet(cron.hookEnv(), ...)`, because a run's position is state that the next test would inherit. The stub in `app.mjs` plays the org webhook `verify/7`: `hooks.arm(faucet, deliveries, { pageSize, redeliveryStatus, listDelayMs })` sets what it lists, newest first with a keyset cursor, `hooks.add` lists more, and `hooks.requests()` returns each request it got with the kind of credential it carried. On a redelivery request the stub posts the stored payload, signed with the secret of the Superblink Events server that the test armed it with, and records a new attempt with the status that Superblink Events answered. The time that a doubling wait needs is moved with `update faucet_redeliveries set next_ask_at = now()`. Not reachable from a browser: use `run_tests {files: ["tests/redeliver.e2e.ts"]}`.

## Proof

Response: the counts. Stub: the requests, each with the hook token and to one of the two endpoints, and the number of redelivery requests. Postgres: `faucet_redeliveries` (`state`, `reason`, `asks`), the open and gap rows of `faucet_redelivery_ranges`, and `faucet_events` for the stored event. App log: one `redelivery recovered`, `redelivery gave up` or `redelivery gap` line per outcome. The 503 case runs on the cron Superblink Events server, which has a cron secret and no hook settings.

## Charters

No charter. The stub's control calls are the only way to put deliveries in place, and a browser session cannot make them.

## Gotchas

- The first run that reads an attempt records the part of GitHub's 72 hours before its 24 hour lookback as one gap, so its reply says `gaps: 1`. Both are counted from the newest attempt, not from the time of the run. A run on an empty list says `gaps: 0`. That is the design, not a fault.
- The stub sends Node's `Date` header, which is the stub's clock. A test of a `Date` that leads, lags or is missing belongs in `faucet/redelivery/redeliver_gaps_test.go`, where the fake GitHub sets it.
- A delivery that is given up stays lost until someone redelivers it by hand.
- A stored cursor is GitHub's opaque value. If GitHub keeps refusing it, the range is retried by every run until it expires, and then it is a gap.
- GitHub lists an attempt when it ends, so an attempt that ended more than 5 minutes late and sits below the position of the last run is not read.
- The stub's `status_code` 0 stands for a delivery that got no HTTP response.
