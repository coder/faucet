# Control envelopes

Faucet POSTs two signed bodies with a top-level `type` to a subscription's callback URL, besides the event deliveries (SEP-3415, "Non-event webhook bodies"). Each goes once, with one attempt and no retry.

## Sub-features

- `gap`: `{"type":"gap","cursor":"v1.<n>"}`, sent by `GET /cron/deliver` to a subscription that a purge overtook: the purge deleted an event of its repository and event name that it had not read, so its read position stayed below the purge mark. A purge that deleted only other repositories' events sends it nothing. The request goes out in the send phase, after every subscription's events are queued, and a gap that the run ended before sending stays recorded, so the next run sends it. `n` is the purge mark. Resuming from that cursor replays every event that still exists. The run reports the subscription in `gaps` and in `gaps_sent` or `gaps_failed`. Events keep flowing. A second run sends no second gap.
- `terminated`: `{"type":"terminated","error":{"code":-32024,"message":"Forbidden","data":{"reason":"Access revoked"}}}`, sent by `events/subscribe` when a renewal of an existing subscription fails the repository access check with no access. The subscription is deleted and the renewal still answers `-32024`. A second renewal finds nothing to end and sends nothing. A renewal that keeps its access, a first subscribe that is denied, a rejected token and a failed access check send nothing.
- Both: `webhook-id` is `msg_<type>_` and 22 URL-safe characters, new for each envelope. The signature is Standard Webhooks, keyed with the secret that the subscription stored (for `terminated`, never the secret in the renewal). The headers are those of an event, with `X-MCP-Subscription-Id`. An endpoint that answers anything but 2xx makes a failed send: it is counted (`gaps_failed`, or `Service.Terminations()`), logged once with its category and never retried. No log line holds the secret or the callback URL.

## How to get to it (user POV)

An agent that subscribes and then goes quiet through a purge learns that it missed events when the `gap` envelope reaches its endpoint, and resumes from the cursor. An agent whose access to the repository was removed learns it when its next renewal fails, and its endpoint gets `terminated` once.

## Driving it with e2e

`tests/envelopes.e2e.ts`, on a faucet that the test starts with `withDeliveryFaucet`, so that the purge mark is its own. A gap test subscribes, stores two events and ages them with `faucet.backdate(deliveryId, 40)`, runs `cronJob('purge')`, stores a fresh event, then runs `cronJob('deliver')`. A terminated test calls `receiver.revoke(repoId)` and renews with the same params. `envelopeRequests(receiver, at, type)` returns the envelopes of one type, and `expectSignedEnvelope` checks what each owes the receiver.

## Proof

Response: the reply of `/cron/deliver` has every count, with `gaps_sent` or `gaps_failed` set. Receiver: the request has a `valid` signature, which the receiver's own Node HMAC computes, a `msg_<type>_` webhook-id, the subscription's ID and no credential, and the body is exactly `{type, cursor}` or the SEP's `terminated` object. The cursor equals `v1.` and the `purged_through` that the purge reported, and `events/poll` from it returns the surviving event. Store: after `terminated` the subscription row is gone. Log: one line per envelope, and no secret and no token of the callback URL.

## Charters

No charter. Every check needs the receiver's records, the cron secret and a purge of a database that the test owns.

## Gotchas

- The purge mark is table-wide, so a test that purges runs on its own faucet (`withDeliveryFaucet`). Events must be stored after the subscription, or the subscription starts above them.
- The receiver answers an envelope like an event: the behavior `ok` needs a valid signature and answers 200, `fail500` answers 500.
- `Service.Terminations()` has no route. The e2e test checks the log line for a failed `terminated`, and the Go test checks the counter.
- A GitHub rate limit (429, or 403 with no quota left, `Retry-After` or a message that names the limit) is a failed check and ends nothing, while any other 403 is no access. The stub cannot make GitHub's limiter answer, so only the Go tests (`TestTokenAuthorizerRepoID`, `TestRenewalDuringAGitHubRateLimit...`) pin it.
