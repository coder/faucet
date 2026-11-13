# Redelivery sweep

`GET /cron/redeliver` asks GitHub to deliver again the webhook deliveries that faucet missed. A delivery qualifies when its newest failed attempt is inside the last hour, no successful attempt came after it, and faucet has no event with that delivery GUID. A failure is any answer outside 200 to 299, including no answer.

## Sub-features

- The reply is 200 JSON `{"examined", "pending", "requested", "incomplete"}`: attempts inside the window, deliveries that qualify, requests GitHub accepted, and whether a bound stopped the run.
- It asks for the newest failed attempt of each delivery, oldest delivery first. A delivery that failed and later succeeded, a failed delivery faucet has stored, and a delivery older than one hour are not asked for.
- One run reads at most 10 pages of 100 deliveries and sends at most 50 requests. A run that hits a bound says `incomplete: true`, and the next run goes on, because a delivery that faucet stored is skipped.
- The first redelivery request that GitHub refuses stops the run. The reply is 502 with the body `redelivery failed`, and no response or log holds the hook token or GitHub's body.
- Without the hook settings the route answers 503 with `redelivery is not configured`, after the secret check.
- Only the hook token goes to GitHub, and only to two endpoints: `GET .../hooks/{id}/deliveries` and `POST .../deliveries/{attempt}/attempts`.

## How to get to it (user POV)

An operator sets `FAUCET_HOOK_TOKEN`, `FAUCET_HOOK_ID` and one of `FAUCET_HOOK_ORG` or `FAUCET_HOOK_REPO` (production only) and schedules the route more often than the one hour lookback, for example every 30 minutes. A webhook that arrived while faucet was down or the database refused it is then delivered again without anyone clicking.

## Driving it with e2e

`tests/redeliver.e2e.ts`, on the cron faucet. The stub in `app.mjs` plays the org webhook `verify/7`: `hooks.set(deliveries, { pageSize, redeliveryStatus })` sets what it lists (`stubDelivery(repoId, { id, guid, ageMs, status })` builds an entry with a payload), and `hooks.requests()` returns each request it got with the kind of credential it carried. On a redelivery request the stub posts the stored payload, signed, to the cron faucet and records a new attempt with the status that the faucet answered, as GitHub does. Not reachable from a browser: use `run_tests {files: ["tests/redeliver.e2e.ts"]}`.

## Proof

Response: the counts. Stub: the attempt IDs of the requests, in order, and that every request carried the hook token and went to one of the two endpoints. Store: a delivery that was asked for is now a row of the repository, and a second sweep right after asks for nothing. The 503 case starts a faucet with `faucet.withFaucet({})`, which has a cron secret and no hook settings.

## Charters

No charter. The stub's control calls are the only way to put deliveries in place, and a browser session cannot make them.

## Gotchas

- Known limit, pinned by a test: a delivery that always fails, for example a payload faucet cannot read, is asked for again on every sweep until it leaves the window. Each run asks at most 50 times.
- Two sweeps that overlap can ask for the same delivery twice. Faucet stores one event per GUID, so the second delivery is a duplicate.
- GitHub keeps deliveries for three days, so a sweep cannot see older ones. The lookback is one hour and is not a setting.
- The stub's `status_code` 0 stands for a delivery that got no HTTP response.
