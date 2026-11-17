# Webhook subscriptions

An MCP client can call `events/subscribe` to have faucet push matching events to a callback URL, as an alternative to waiting or polling. This page is the contract for whoever writes the receiver. It follows the draft MCP Events extension (SEP-3415) and [Standard Webhooks](https://www.standardwebhooks.com).

## Turn it on

Webhook mode is off until the operator sets `FAUCET_WEBHOOK_KEY` (`openssl rand -base64 32`; it seals subscriber secrets at rest) and chooses how faucet checks callback URLs:

- **`allowlist`** (default): set `FAUCET_WEBHOOK_ALLOWED_ORIGINS` to the `https://host[:port]` origins that may receive events. Faucet makes no request to a callback URL until it has an event to send.
- **`handshake`**: set `FAUCET_WEBHOOK_CALLBACK_VERIFICATION=handshake` and leave the allowlist unset. A first subscribe for a login and URL makes faucet send a signed challenge, which the endpoint must echo back (see [Handshake](#handshake)).

While webhook mode is off, `events/list` offers `poll` only, `events/subscribe` and `events/unsubscribe` answer `Unsupported`, and `/cron/deliver` answers 503.

Keep `FAUCET_WEBHOOK_KEY` stable. Rows sealed under another key can't be read. Their deliveries end with `secret_unreadable` and the subscription is deleted until its subscriber renews.

## Subscribe

`events/subscribe` takes `name` (an event type), `arguments` (`repo`, optionally `sha` and `pr`), `delivery` (`url` and `secret`) and optionally `cursor`, `maxAgeMs` and `ttlMs`. Like `events/poll`, the HTTP request must carry an `Mcp-Name` header with the event name.

- The token must be able to read the repository. Faucet checks again on every renewal, because it stores no token.
- `delivery.secret` is `whsec_` followed by base64 of 24 to 64 random bytes. The client makes it. Faucet never returns or logs it.
- `delivery.url` must be `https` with a public host, and it must pass the server's callback check.
- A subscription is the tuple (login, URL, event name, arguments). Subscribing again with the same tuple renews it: same `id`, new expiry, and the secret is replaced (see [Secret rotation](#secret-rotation)).
- A subscription lives for `ttlMs`, clamped to between 30 seconds and 10 minutes (default 10 minutes). Renew before `refreshBefore`.
- A login can hold 100 live subscriptions.

A renewal keeps the subscription's read position and ignores any `cursor` it carries. A subscribe after the subscription lapsed (it expired or was unsubscribed) starts after the `cursor` you send. A renewal also reports `deliveryStatus`: `lastDeliveryAt`, `lastError` and `failedSince`.

`events/unsubscribe` takes `name`, `arguments` and `delivery.url`, and deletes the caller's own live subscription.

## Latency

Faucet sends webhooks only when `GET /cron/deliver` runs. With Vercel Cron that is once a minute, so **an event can wait up to a minute** before it is sent. A host that needs seconds can call `Service.DeliverDue` in its own loop.

## The request

```http
POST <delivery.url>
Content-Type: application/json
X-MCP-Subscription-Id: <subscription id>
webhook-id: msg_...
webhook-timestamp: <unix seconds>
webhook-signature: v1,<base64 HMAC-SHA256>

{"eventId": "msg_...", "name": "ci.run_completed", "timestamp": "...", "data": {...}, "cursor": "v1.123"}
```

- The signature is the Standard Webhooks HMAC over `<webhook-id>.<webhook-timestamp>.<body>`, keyed with the subscriber's secret. Verify it with any Standard Webhooks library.
- `name` and `data` match what `events/poll` returns. `timestamp` is when faucet received the event.
- `webhook-id` (and `eventId`, which equals it) is stable for one event to one subscription, across every retry and replay. **Deduplicate on it.** It differs from the GitHub delivery ID that `events/poll` reports.
- `cursor` is a safe watermark: every event of the subscription at or below it has been delivered or given up. Persist it, and pass it to `events/subscribe` if the subscription lapses.
- The request carries no `Authorization` header and no cookies. Nothing the endpoint sends back reaches faucet's replies, logs or tables.

## Retries and outcomes

Any `2xx` answer ends the delivery. `410 Gone` and `413` end that delivery but not the subscription. Anything else, including a redirect (never followed), a refused connection, a TLS error or a 5 second timeout, is retried after 15 seconds, 1, 3 and 6 minutes: at most 5 attempts within 15 minutes.

Delivery is **at least once**. An attempt whose result faucet could not record is sent again with the same `webhook-id`.

A delivery that doesn't succeed ends with a reason, which `/cron/deliver` counts in its reply and logs. The reasons include `attempts_exhausted`, `window_expired`, `event_too_large` (bodies over 256 KiB aren't sent), `event_purged` and `subscription_expired`. A subscription that holds 100 unsent deliveries reads no new events until some of them end.

Faucet holds no GitHub token, so it only sees revoked access **when the subscriber renews**. A subscription that is never renewed simply expires.

## Control messages

Faucet also sends two bodies with a top-level `type` and no `eventId` or `data`. They carry the same headers and signature. Each send gets a fresh `webhook-id` and is attempted once.

- `{"type":"gap","cursor":"v1.<n>"}`: a retention purge deleted events that the subscription hadn't read yet. Treat it like `truncated: true` and persist the `cursor`. A gap can arrive more than once, so deduplicate gaps by `cursor` and keep the highest.
- `{"type":"terminated","error":{"code":-32024,"message":"Forbidden","data":{"reason":"Access revoked"}}}`: a renewal found that the token can no longer read the repository, so the subscription was deleted. Delivery is at most once.

## Secret rotation

To rotate, call `events/subscribe` again with a new `delivery.secret`. For a grace window of up to 10 minutes (never longer than the subscription's TTL), faucet signs with both secrets: `webhook-signature` holds `v1,<new> v1,<old>`, so a receiver that knows either one still verifies. Faucet keeps one previous secret, so a second rotation inside the window drops the first one.

Give your receiver the new secret before you rotate: a handshake that the rotating call triggers is signed with the new secret only.

## Handshake

In `handshake` mode, the first `events/subscribe` for a login and URL makes faucet send one signed `POST` before the call returns:

```json
{"type": "verification", "challenge": "<nonce>"}
```

The endpoint must verify the signature and answer `2xx` within 5 seconds, with `{"challenge": "<nonce>"}`. Any other answer fails the subscribe with `CallbackEndpointError` and stores nothing. A success is remembered for that login and URL for 10 minutes.

Handshakes are rate limited per login and per destination, and a call over the limit gets `ResourceExhausted`. If a public endpoint can front an internal service, prefer `allowlist` mode. See [security.md](security.md#callback-urls) for how faucet guards callback URLs.
