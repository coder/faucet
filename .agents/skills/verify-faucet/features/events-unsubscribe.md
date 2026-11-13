# Events unsubscribe

An agent ends a webhook subscription through `events/unsubscribe` on `POST /mcp`, before it would lapse. The subscription is the one that [events/subscribe](events-subscribe.md) made for the same principal, callback URL, event and arguments.

## Sub-features

- With webhook mode off the call is the error -32026 (see [events/list](events-list.md)). With it on, a bearer token that GitHub accepts and a matching `Mcp-Name` header are required, as for subscribe.
- `params` are `name`, `arguments` (as for subscribe) and `delivery` with a `url`. The `secret` is not needed. The parameters are checked as for subscribe: an unknown event is -32023, a `delivery.mode` other than `webhook` is -32026, and a bad URL or arguments is -32602.
- A match is deleted and the result is `resultType` `complete`.
- A call that matches nothing is the error -32023 with `data` `{"kind": "subscription"}` and the message `no such subscription`. That covers a second call, a key nobody subscribed, and a key that only another principal subscribed. The three replies are the same, so one principal cannot tell that another holds a subscription. The other principal's row is not touched.
- The key is (principal, callback URL, event, arguments). Another `pr`, `sha`, repository, event, path or query is a different key and removes nothing. The key order of `arguments`, the case of the host and the default port do not matter.
- The call needs no read access to the repository and no allowlist check on the URL. A repository the token cannot read, and a URL that is not on the allowlist, are -32023, never -32024 or -32027.
- The log holds `unsubscribed` (`repo`, `event`, `login`, `subscription`) and no part of the callback URL.

## How to get to it (user POV)

An agent that stops caring about a pull request sends `events/unsubscribe` with the `name`, `arguments` and `delivery.url` it subscribed with. The test plays the agent through `faucet.mcp` and `unsubscribe` in `tests/subscriptions.ts`, and the second principal through `{ as: 'other' }`.

## Driving it with e2e

`tests/events-unsubscribe.e2e.ts`. Not reachable from a browser: use `run_tests {files: ["tests/events-unsubscribe.e2e.ts"]}`.

## Proof

Response: 200 with the result or with the error -32023, -32026, 400 with -32020 or -32602, or 401 with `unauthorized\n`. Store: `faucet.subscriptions(marker)` is empty after a removal. After a call that must remove nothing it returns the same rows as before, to the last field (`updated_at`, `secret_enc`), and the secret still opens with the run's key. The reply for the other principal's key must equal, byte for byte, the reply for a key nobody subscribed.

## Charters

```
unsubscribe-identity|http|skeptic|As two principals, subscribe and unsubscribe keys that differ in one part (path, query, event, pr, sha, repository, host case, default port, key order) and report any call that removes a row it should not, leaves one it should remove, or answers differently for another principal's key than for a key nobody made
```

## Gotchas

- The second principal is `verify-other`, so `{ as: 'other' }` is the other principal. Its own subscribe call must come first when a test wants it to own a row.
- Because the call needs no repository access, it works for a reader who lost access. The stub cannot revoke a reader, so the test pins this with a repository the token cannot read, which is -32023 and not -32024.
