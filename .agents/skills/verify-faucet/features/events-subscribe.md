# Events subscribe

An agent asks faucet to send the events of one repository to a webhook it runs, through `events/subscribe` on `POST /mcp`. This is the webhook delivery mode of the MCP Events extension (SEP-3415). At PR 6 faucet only stores the subscription. It sends nothing, and it makes no request to the callback URL. The agent ends a subscription with [events/unsubscribe](events-unsubscribe.md). [events/list](events-list.md) says whether the mode is offered, and [startup](startup.md) says what turns it on.

## Sub-features

- Webhook mode is on when both `FAUCET_WEBHOOK_KEY` and `FAUCET_WEBHOOK_ALLOWED_ORIGINS` are set. The main faucet has both. With neither set, the call is the error -32026 with `data` `{"feature": "deliveryMode", "value": "webhook"}`, before any other check.
- A bearer token that GitHub accepts is required (401 `unauthorized` otherwise), and `Mcp-Name` must equal `params.name` (HTTP 400, error -32020, when it is missing or different). The principal is the login of the token: `verify-user` for the run's token, `verify-other` for the second one.
- `params` are `name` (one of the five event types), `arguments` (`repo`, and optionally `sha` and `pr`, as for [events/poll](events-poll.md)), `delivery` (`url`, `secret` and an optional `mode` that is `webhook`, absent or null), and optionally `cursor`, `maxAgeMs` and `ttlMs`.
- The checks run in this order, and the first failure wins: the parameters (error -32602 with HTTP 400, an unknown event -32023, a `delivery.mode` other than `webhook` -32026 with `data` `{"feature": "deliveryMode", "value": "<the mode>"}`), then the principal, then read access to the repository (error -32024, the same reply for a repository the token cannot read and one that does not exist), then the callback origin (error -32027 with `data` `{"reason": "challenge_failed"}`), then the write. A refused call writes no row.
- The callback URL is https, 1 to 2048 bytes, with no user info, fragment, whitespace or backslash, and its host is a public name (two or more labels, not a number in decimal or hex) or a public IP address. http, a private, link-local, unique-local or loopback address, `localhost`, and a single-label host are -32602 with a message that starts `delivery.url`. The reply never quotes the URL.
- The origin of the URL (scheme, host and port) must be on `FAUCET_WEBHOOK_ALLOWED_ORIGINS`. The main faucet allows `https://hooks.verify.test` only, so another host, another port and a public IP address are -32027. The host is compared in lower case and the default port 443 is dropped, so `https://HOOKS.Verify.Test:443/x` is stored as `https://hooks.verify.test/x`.
- The secret is `whsec_` and the standard, padded base64 of 24 to 64 bytes. Anything else is -32602 with a message that starts `delivery.secret`, and the reply never quotes it. A secret that is missing, or a `url` or `secret` that is not a string, is -32602 too.
- A parameter of the wrong type (`cursor` as a number, `maxAgeMs` as text or a fraction, a `name` that is a number) is -32602 with HTTP 400, and the reply never quotes the request, so the secret in `delivery` stays out of it.
- The secret is never echoed, logged or stored in the clear. `faucet_subscriptions.secret_enc` holds `0x01`, a 12-byte random nonce, then the AES-256-GCM ciphertext and tag of the secret, sealed with `FAUCET_WEBHOOK_KEY` and with the row's `key_hash` as the additional data. Sealing the same secret twice gives different bytes.
- The result is `resultType` `complete`, `id` (`sub_` and the first 16 hex characters of `key_hash`), `refreshBefore` (the expiry, RFC 3339 in UTC, to the second), `cursor` (`v1.<n>`) and `truncated`. The row holds `key_hash`, `principal`, `callback_url`, `event_type`, `arguments`, `repo_id`, `secret_enc`, `cursor`, `created_at`, `updated_at` and `expires_at`, and no GitHub token.
- The grant is `ttlMs` capped at 10 minutes and raised to 30 seconds, however large or negative `ttlMs` is: a value so negative that its nanoseconds overflow an int64 still gets 30 seconds. An absent or null `ttlMs` is 10 minutes. A `ttlMs` that is not an integer is -32602.
- A subscription starts at the newest event of any repository. A `cursor` that an earlier call returned starts there instead, and `truncated` says that a purge or `maxAgeMs` moved it up.
- The key is (principal, callback URL, event name, arguments). Calling again with the same key refreshes the row in place: the same `id` and `created_at`, a new secret, `updated_at` and expiry, and the cursor stays. The key order of `arguments`, the case of the host and the default port do not matter. Another path, query, event, `pr`, `sha`, repository or principal is another subscription with its own `id` and row.
- A principal holds at most 100 live subscriptions. The 101st is -32025 with `data` `{"limit": "subscriptions", "max": 100}`. A refresh at the limit, another principal and a slot freed by an unsubscribe all work.
- The log holds `subscribed` (`repo`, `event`, `login`, `subscription`, `expires_at`), `subscribe denied` (`repo`, `event`, `login`) and `callback not verified` (`repo`, `event`, `login`, `reason`). None holds a secret, the key, or any part of the callback URL.

## How to get to it (user POV)

An agent that runs a webhook receiver sends `events/subscribe` to `/mcp` with `Authorization: Bearer <GitHub token>` and `Mcp-Name: <event name>`, as a stateless 2026-07-28 request. It sends again before `refreshBefore` to keep the subscription, and `events/unsubscribe` to end it. The stub plays GitHub: it knows two tokens, for `verify-user` and `verify-other`, and the repositories `verify/r<id>`. The test plays the agent through `faucet.mcp`, `subscribe` in `tests/subscriptions.ts`, and the second principal through `{ as: 'other' }`.

## Driving it with e2e

`tests/events-subscribe.e2e.ts`, with its helpers in `tests/subscriptions.ts`, and `tests/events-subscribe-types.e2e.ts` for the wrongly typed parameters. The webhook-off case is in `tests/events-list.e2e.ts`, the startup refusals are in `tests/startup.e2e.ts`, and the purge of expired rows is in `tests/purge.e2e.ts`. Not reachable from a browser: use `run_tests {files: ["tests/events-subscribe.e2e.ts"]}`.

## Proof

Response: 200 with the result, 200 with the error -32023 to -32027, 400 with -32020 or -32602, or 401 with `unauthorized\n`. Store: `faucet.subscriptions(marker)` returns the rows whose callback URL holds the test's marker (the test's own path), so a refused call is proved by an empty list. The row is checked field by field, and the sealed bytes are opened in Node with `aes-256-gcm`, the run's key and the row's `key_hash`, which must give exactly the secret that was sent. A wrong key, and the `key_hash` of another row, must fail to open it. `faucet.secretCopies(secret)` must count 0 rows that hold the secret in any column, as text, without its prefix or as decoded bytes. The app log must hold 0 copies of the secret, of the key and of the marker, after every kind of refusal, and no -32602 reply holds the secret, also when the SDK would have decoded `cursor`, `maxAgeMs` or `name`.

## Charters

```
subscribe-callback|http|fuzzer|Send events/subscribe with callback URLs that are odd: other schemes, IPv4 and IPv6 literals in every notation, userinfo, ports, dots, case, percent escapes, very long paths, and report any URL that is stored but is not https on the allowlist, and any reply that quotes the URL
subscribe-secret|http|skeptic|Subscribe with secrets of every shape and with every other field wrongly typed or missing, then look for the secret in any reply, header, the app log and every column of faucet_subscriptions, and report any copy
subscribe-identity|http|skeptic|Subscribe, refresh and unsubscribe the same key and nearly the same keys as the two principals, with spelled-differently arguments and URLs, and report any case where one principal's call changes, reveals or removes the other's row
```

## Gotchas

- A test finds its rows by a marker in its own callback URLs, never by a table-wide count. The tests of one faucet share its table.
- A row lives 10 minutes and counts toward the 100 of `verify-user` while it lives. The limit test therefore runs on its own faucet (`faucet.withFaucet` with a key and an allowlist), and a new test on the main faucet must not subscribe dozens of keys.
- faucet makes no request to a callback URL at PR 6, so `hooks.verify.test` needs no server. Signed delivery to it is PR 7.
- `refreshBefore` and `expires_at` are cut to the second, so a test checks them against the time it sent the call and the time it got the reply, each cut to the second.
- The error -32602 and -32020 are HTTP 400, but -32023 to -32027 are HTTP 200. Read the `error` object.
- The principal check and the repository check use the stub, so a revoked reader cannot be played. The stub answers 404 for every repository that is not `verify/r<id>`, which is the denied and the nonexistent case alike.
- `faucet.subscriptions` returns times as epoch milliseconds and `key_hash` and `secret_enc` as hex.
- A row opens only with the key and the `key_hash` that sealed it. Another key, or the `key_hash` of another row as the additional data, fails the GCM tag check.
