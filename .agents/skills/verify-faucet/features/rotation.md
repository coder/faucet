# Secret rotation

An agent that renews its subscription with a new `delivery.secret` rotates the secret that signs its webhooks. For a grace window faucet signs every request to the callback URL with both secrets, so a receiver that still holds the old one and one that already holds the new one both verify (SEP-3415, "Secret rotation"). Needs webhook mode.

## Sub-features

- `events/subscribe` for an existing subscription with a different `delivery.secret` replaces the current secret and keeps the replaced one as the previous secret, until `now + min(10 minutes, TTL that the call was granted)`. Since a grant is 10 minutes at most, the window is the grant. A renewal with the same secret changes neither the secret nor the window, so it does not extend it. A second rotation inside the window replaces the previous secret with the one it replaced: only one previous secret is kept.
- While `now` is before the end, `webhook-signature` of an event, a `gap` envelope and a `terminated` envelope holds one `v1,` entry per secret, space-delimited, as Standard Webhooks defines. A receiver accepts the request when any entry verifies. After the end it holds the entry of the current secret only, whether or not a run has cleared the previous secret yet.
- The handshake of a first subscribe is signed with the secret of that request only.
- The previous secret is sealed as the current one is (AES-256-GCM, the row's `key_hash` as additional data). No row holds either secret in the clear. `previous_secret_until` is set by the server, never by the request.
- `GET /cron/deliver` sets `previous_secret_enc` and `previous_secret_until` to NULL for every row whose window has ended, and reports how many in `previous_secrets_cleared`. The subscription and its current secret stay.
- A `terminated` envelope is signed with the stored secrets (current and previous), never with the secret of the renewal that failed the access check.
- No log line, reply or row holds either secret.

## How to get to it (user POV)

An agent rotates its secret by giving its receiver the new secret and then sending `events/subscribe` again with the same `name`, `arguments` and `delivery.url`, and the new `delivery.secret`. Deliveries that are in flight verify under either secret. When the agent's receiver holds only the new one, the old one stops signing within 10 minutes with no further call. The agent cannot see the previous secret, the window or the clear-out.

## Driving it with e2e

`tests/rotation.e2e.ts`, on a faucet that the test starts with `withDeliveryFaucet`. `endpoint(receiver, 'ok')` registers a receiver with the old secret. A refresh with `{ ...params, delivery: { url, secret: next } }` rotates. `receiver.register(at.id, next)` plays a receiver that holds only the new secret. `started.subscriptions(at.id)` returns `previous_secret_enc` (hex) and `previous_secret_until` (epoch milliseconds), `started.openSecret(row)` and `started.openPreviousSecret(row)` open them, and `started.endRotationWindows(at.id)` moves the end of the window into the past, since a window lasts up to 10 minutes. `started.secretCopies(secret)` also looks in `previous_secret_enc`.

## Proof

Receiver: the request has `signature` `valid` for the secret that the receiver holds, and the test computes both entries in Node (HMAC-SHA256 over `<webhook-id>.<webhook-timestamp>.<body>`) and compares the header with them: two entries during the window, only the new one after it. Response: `/cron/deliver` reports `previous_secrets_cleared: 1` once, then 0. Store: after the rotation `previous_secret_enc` opens to the old secret and `secret_enc` to the new, and `secretCopies` is 0 for both; after the run both previous columns are NULL. Log: no secret.

## Charters

No charter. Every check needs the receiver's records, the cron secret and the subscription table of a faucet that the test started.

## Gotchas

- A run clears an ended window before it sends, so e2e cannot show an ended but unswept window that still does not sign. A Go test pins that with a store that clears nothing.
- `gap` is not driven here. Its signing is pinned by Go tests, and the scan target's columns by the store contract (`ScanTargetsAndClaimsCarryThePreviousSecret`).
- The handshake result of the first subscribe is cached for the login and URL, so the rotating renewal sends no verification request. A rotation that the cache missed would sign the verification with the new secret only.
