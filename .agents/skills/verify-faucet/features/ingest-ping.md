# Ping

GitHub sends a `ping` event when a webhook is created. faucet answers it so the webhook shows as healthy.

## Sub-features

- A signed ping is 200 with the body `pong` and stores nothing.
- A ping with a wrong signature is 401 like any other delivery.

## How to get to it (user POV)

Create the webhook in the GitHub settings. GitHub POSTs one `ping` at once.

## Driving it with e2e

`tests/ingest-ping.e2e.ts` sends a signed ping that carries a `repository.id`, then one with a wrong signature.

## Proof

Response: 200 `pong\n`. Store: no row for the repository the ping names.

## Charters

```
ping-basics|http|skeptic|Send pings signed correctly, signed wrongly and unsigned, with and without a delivery header; check the answer and that no row appears
```

## Gotchas

- `ping` needs both headers like every event: the header check runs first, so a ping without a delivery header is 400 (checked at PR 2, not pinned).
