# Redelivery sweep

`GET /cron/redeliver` asks GitHub to deliver again the webhook deliveries that faucet missed. A delivery qualifies when its first attempt and its newest failed attempt are inside the last two hours, no successful attempt came after the failure, faucet has no event with that delivery GUID, and GitHub has recorded fewer than 3 redeliveries of it. A failure is any answer outside 200 to 299, including no answer.

## Sub-features

- The reply is 200 JSON `{"examined", "pending", "gave_up", "requested", "incomplete"}`: attempts inside the window, deliveries that qualify, deliveries that failed and are not stored but that the sweep will not ask for again, requests GitHub accepted, and whether a bound stopped the run.
- It asks for the newest failed attempt of each delivery, the deliveries with the fewest redeliveries first and then the oldest. A delivery that failed and later succeeded, a failed delivery faucet has stored, and a delivery whose first attempt is older than two hours are not asked for.
- A delivery that faucet keeps rejecting, such as an unreadable payload, is asked for 3 times and then not again. Each redelivery is a new attempt with its own time and the flag `redelivery`, so the window alone would never end it. A delivery whose first attempt (the one without the flag) is older than the window is not asked for either. Every delivery that the sweep gives up on is counted in `gave_up` and logged as `redelivery gave up` with `reason=limit` or `reason=window`, on every call that still sees an attempt of it. It is never dropped silently.
- The window is two hours because the sweep runs every 30 minutes: the limit is 3 redeliveries, the third request comes up to 3 schedules after the first attempt, and the sweep that finds the limit reached comes one schedule later. A delivery that failed in an outage that ends within an hour is recovered. The unit tests pin this with the outage of the verifier's second round.
- One run sends at most 50 requests. A run that hits this bound says `incomplete: true`, and the next run goes on with the rest, because a delivery that faucet stored is skipped.
- One run reads at most 10 pages of 100 attempts, the newest first, and says `incomplete: true` when the window holds more. The next run does not go on where this one stopped: faucet keeps no state, so it reads the newest 1000 again. A request for a delivery needs its first attempt among the newest 1000, and the n-th request comes less than n schedules after it. At the 30 minute schedule the third request is safe while the hook records at most 1000 / (3 × 0.5 h) = 666 attempts an hour, and the first while it records at most 2000 an hour. A delivery that falls out of view is given up and logged.
- The first redelivery request that GitHub refuses stops the run. The reply is 502 with the body `redelivery failed`, and no response or log holds the hook token or GitHub's body.
- Without the hook settings the route answers 503 with `redelivery is not configured`, after the secret check.
- Only the hook token goes to GitHub, and only to two endpoints: `GET .../hooks/{id}/deliveries` and `POST .../deliveries/{attempt}/attempts`.

## How to get to it (user POV)

An operator sets `FAUCET_HOOK_TOKEN`, `FAUCET_HOOK_ID` and one of `FAUCET_HOOK_ORG` or `FAUCET_HOOK_REPO` (production only) and schedules the route every 30 minutes or more often. The two hour lookback is four schedules at that interval. A webhook that arrived while faucet was down or the database refused it is then delivered again without anyone clicking.

## Driving it with e2e

`tests/redeliver.e2e.ts`, on the cron faucet. The stub in `app.mjs` plays the org webhook `verify/7`: `hooks.set(deliveries, { pageSize, redeliveryStatus })` sets what it lists (`stubDelivery(repoId, { id, guid, ageMs, status })` builds an entry with a payload), and `hooks.requests()` returns each request it got with the kind of credential it carried. On a redelivery request the stub posts the stored payload, signed, to the cron faucet and records a new attempt, flagged `redelivery` and dated now, with the status that the faucet answered, as GitHub does. Not reachable from a browser: use `run_tests {files: ["tests/redeliver.e2e.ts"]}`.

## Proof

Response: the counts. Stub: the attempt IDs of the requests, in order, and that every request carried the hook token and went to one of the two endpoints. Store: a delivery that was asked for is now a row of the repository, and a second sweep right after asks for nothing. The 503 case starts a faucet with `faucet.withFaucet({})`, which has a cron secret and no hook settings.

## Charters

No charter. The stub's control calls are the only way to put deliveries in place, and a browser session cannot make them.

## Gotchas

- The give-up rule is pinned by a test with the stub: a delivery without a payload fails every redelivery, and the fourth sweep asks for nothing and counts one `gave_up`. The stub dates the attempt it adds when the request arrives and flags it as a redelivery, as GitHub does. A stub that dated a redelivery like its original would hide the problem.
- A delivery that faucet gave up on stays lost until someone redelivers it by hand. The unit tests pin the rule at 3, the two hour anchor, the counting of every give-up and the order within a run (the fewest redeliveries first) so that a delivery that keeps failing cannot use up the 50 requests.
- Two sweeps that overlap can ask for the same delivery twice. Faucet stores one event per GUID, so the second delivery is a duplicate.
- GitHub keeps deliveries for three days, so a sweep cannot see older ones. The lookback is two hours and is not a setting. A schedule slower than 30 minutes makes the sweep give up on deliveries that it could still recover.
- The stub's `status_code` 0 stands for a delivery that got no HTTP response.
