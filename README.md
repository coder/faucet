# faucet

Faucet lets agents wait for or subscribe to GitHub events, such as a CI run finishing or a PR approval, through MCP.

It is a Go library first, with a thin HTTP server on top, so coder/coder can embed it in coderd.

## Status

PR 6 of 8. Faucet receives GitHub webhooks, stores them as typed events, and lets an agent wait for them through the `wait_for_github_events` MCP tool, or poll them with the MCP Events extension (SEP-3415). It deletes old events and asks GitHub again for the deliveries it missed. An agent can also subscribe a callback URL with `events/subscribe`. Faucet stores the subscription. It does not send any event yet, which PR 7 adds. The first half of PR 7 adds what delivery needs: a guarded HTTP client, a webhook signer and the handshake that checks a callback URL.

- `faucet`: `Event`, `Store`, `Normalize`, `Service.WebhookHandler`, `Service.Register` (the MCP tool and the `events/list`, `events/poll`, `events/subscribe` and `events/unsubscribe` methods), `ServerCapabilities` and `RequireEventName` (the Events extension), and `Principal`, `Authorizer` and `TokenAuthorizer` (the per-repo access check), `Service.Purge`, `Service.Redeliver`, `HookAdmin` and `GitHubHookAdmin` (retention and redelivery), and `WebhookDeps`, `WebhookKey`, `CallbackVerifier`, `NewCallbackAllowlist` and `NewCallbackHandshake` (webhook subscriptions), and `NewHTTPClient` and `SignWebhook` (the guarded client and the signer that deliveries use).
- `faucet/pgstore`: the Postgres store, with embedded migrations and sqlc queries.
- `faucet/storetest`: the contract tests every `Store` must pass.
- `cmd/faucet`: the server. It serves `POST /webhook/github`, `/mcp`, `GET /healthz`, `GET /cron/purge` and `GET /cron/redeliver`.

## Run the server

```sh
DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' \
GITHUB_WEBHOOK_SECRET='<a random string of at least 16 bytes>' \
go run ./cmd/faucet
```

`PORT` defaults to 8080. The server applies pending migrations at startup. It exits at once when `DATABASE_URL` or `GITHUB_WEBHOOK_SECRET` is missing.

In the GitHub webhook settings, set the payload URL to `/webhook/github`, the content type to `application/json`, and the secret to the same value. Faucet stores finished workflow runs and check runs, submitted reviews, closed pull requests, and new pull request comments. It acknowledges every other delivery with 202 and stores nothing.

## Wait for events

`/mcp` is a stateless MCP endpoint. Every request must carry `Authorization: Bearer <GitHub token>`. Faucet checks the token with `GET /user`, then checks that the same token can read the repository with `GET /repos/{owner}/{name}`. The token is held for that one request and sent only to the GitHub API URL. `FAUCET_GITHUB_API_URL` sets that URL. It defaults to `https://api.github.com`, and it must be `https`, except `http` is accepted for a loopback host.

`wait_for_github_events` takes:

- `repo`: `owner/name`. Required.
- `types`: one or more of `ci.run_completed`, `ci.check_completed`, `pr.review_submitted`, `pr.closed`, `pr.comment_created`. Required.
- `sha`: a 40 character head SHA. Optional.
- `pr`: a pull request number. Optional.
- `cursor`: the `cursor` of an earlier result. Optional.
- `timeout_s`: 1 to 50, default 45.

It returns `events`, a `cursor` and `truncated` as soon as a matching event exists, or no events and a cursor when `timeout_s` runs out. Pass that cursor to the next call to continue without a gap or a repeat. `truncated` is true when a retention purge deleted events after the cursor you passed, so some events may be missing. Without a cursor the wait starts at the present, or ten minutes back when `sha` is set.

## Poll events with MCP Events

`/mcp` also speaks the draft Events extension, SEP-3415, in poll mode. A client finds it in the `capabilities.extensions` of `server/discover`, under `io.modelcontextprotocol/events`. Push delivery is not offered. Every event type lists `delivery: ["poll"]`, or `["poll", "webhook"]` when webhook mode is on (see Subscribe with webhooks).

- `events/list` returns the five event types with their `inputSchema` and `payloadSchema`.
- `events/poll` takes `name` (an event type), `arguments` (`repo`, and optionally `sha` and `pr`) and `cursor`. It returns `events`, a `cursor` in every result, `hasMore`, `truncated` and `nextPollMs`. `truncated` is true when `maxAgeMs` skipped events or when a retention purge deleted events after the cursor. Each event has `eventId` (the GitHub delivery GUID), `name`, `timestamp` and `data`. Without a cursor the poll returns no events and the cursor of the present. `maxEvents` and `maxAgeMs` bound a replay.
- Every `events/poll` request over HTTP must carry the header `Mcp-Name` with the event name, or faucet answers 400. The SDK client does not set it for `events/*` yet.
- The access check is the one `wait_for_github_events` uses. A repository the token cannot read gives the same `Forbidden` error as one that does not exist.

To embed faucet in another server, build a `faucet.Service` with `faucet.New`, mount `Service.WebhookHandler()` on your own router, and call `Service.Register` on your `*mcp.Server`. Create that server with `mcp.ServerOptions{Capabilities: faucet.ServerCapabilities()}` so clients see the Events extension, and put `faucet.RequireEventName` in front of its HTTP handler. Provide your own `Authorizer` and `PrincipalFrom`, or use `TokenAuthorizer`.

## Subscribe with webhooks

Webhook mode is off until the operator sets both variables below. While it is off, `events/list` offers `poll` only and `events/subscribe` and `events/unsubscribe` answer `Unsupported`.

| Variable | Meaning |
| --- | --- |
| `FAUCET_WEBHOOK_KEY` | The standard base64 of exactly 32 random bytes that are not all zero, for example `openssl rand -base64 32`. It seals subscriber secrets in the database. |
| `FAUCET_WEBHOOK_ALLOWED_ORIGINS` | Comma-separated `https://host[:port]` origins that may receive events. Nothing else is accepted as a callback. |
| `FAUCET_WEBHOOK_CALLBACK_VERIFICATION` | How faucet decides that a callback URL may receive events: `allowlist` (the default) or `handshake`. |

The key and a way to verify callback URLs go together. In `allowlist` mode, which is the default, faucet needs `FAUCET_WEBHOOK_KEY` and `FAUCET_WEBHOOK_ALLOWED_ORIGINS`, and it makes no request to a callback URL. In `handshake` mode faucet needs `FAUCET_WEBHOOK_KEY`, and `FAUCET_WEBHOOK_ALLOWED_ORIGINS` must be unset, because an operator who set both could think that the allowlist still limits the destinations. At start the server refuses to run, and names the variable but never the value, when only one of the pair is set in `allowlist` mode, when the key is missing in either mode, when the origins are set in `handshake` mode, when the mode is neither `allowlist` nor `handshake`, or when a value is invalid. Set the key in production only, and keep it when you restart: rows sealed under another key cannot be read, and subscribers seal them again with their next refresh (within 10 minutes).

In `handshake` mode, a first `events/subscribe` for a login and a callback URL makes faucet send one signed `POST` to that URL (SEP-3415 verification option 1), and the subscribe call waits for the answer, for 5 seconds at most. The body is `{"type":"verification","challenge":"<nonce>"}` with the Standard Webhooks headers `webhook-id` (`msg_verification_...`), `webhook-timestamp` and `webhook-signature`, signed with the subscriber's `whsec_` secret, and with `X-MCP-Subscription-Id`. The endpoint must verify the signature and answer `2xx` with a JSON object whose only member is `challenge`, set to the nonce. Any other answer fails the call with `CallbackEndpointError` (`connection_refused`, `timeout`, `tls_error`, `http_4xx`, `http_5xx` or `challenge_failed`) and stores nothing. Success is remembered for the login and URL for 10 minutes. Each destination host gets 5 handshakes at once and then one every 12 seconds. A handshake that the endpoint answers correctly gives its token back, so only endpoints that do not answer use up the limit. When the limit is reached, the call answers `ResourceExhausted` with `limit` `callbackVerifications` and sends nothing.

The handshake and every later request to a callback URL go through the guarded client (`faucet.NewHTTPClient`). It speaks `https` only. It looks the host name up, refuses the request when any address in the answer is loopback, private, link-local, CGNAT, multicast, unspecified, IPv4-mapped, NAT64, 6to4, Teredo or otherwise not public, and then dials the address it checked, so the DNS answer cannot change between the check and the connection. It still verifies the certificate against the host name. It follows no redirect, ignores `HTTPS_PROXY` and the other proxy variables, stops after 5 seconds, and refuses a response body past 64 KiB. Hosts written as a number (`2130706433`, `0x7f.1`) or as one label are refused before any lookup. The error for a host that points inside your network is `connection_refused`, the same as for a closed port, so a caller cannot map your network. Use `allowlist` when a public endpoint can front an internal service.

A binary built with `-tags faucet_e2e` lets the guarded client reach loopback addresses over `http`, so the end-to-end tests can run a receiver on `127.0.0.1`. No variable does this, so a production binary has no switch that turns the guard off. The tagged binary logs a warning at start. Never ship it.

`events/subscribe` follows the draft SEP-3415 text. It takes `name`, `arguments` (`repo`, and optionally `sha` and `pr`), `delivery` (`url` and `secret`, and `mode` which may be `webhook` or absent), and optionally `cursor`, `maxAgeMs` and `ttlMs`. The `Mcp-Name` header rule of `events/poll` applies. It returns `id`, `refreshBefore`, `cursor` and `truncated`.

- The caller must be authenticated. Faucet checks that the same token can read the repository, as `events/poll` does. A repository the token cannot read gives the same `Forbidden` error as one that does not exist. Faucet stores no token, so it checks again at every renewal.
- `delivery.secret` is `whsec_` followed by the standard base64 of 24 to 64 random bytes, as Standard Webhooks defines. The client makes it. Faucet seals it with AES-256-GCM and never returns or logs it.
- `delivery.url` must be `https`, with a public host name or address, no user info and no fragment, and it must pass the callback verification of the server. With the allowlist, its origin must be on the list, faucet makes no request to it, and an origin that is not listed gives `CallbackEndpointError` with reason `challenge_failed`. With the handshake, faucet sends the signed challenge that the section below describes.
- A subscription is the tuple (login, callback URL, event name, arguments). Subscribing again with the same tuple renews it: same `id`, the secret is replaced, and the expiry is granted again. Another tuple, or another login, is another subscription.
- The grant is `ttlMs`, never more than 10 minutes and never less than 30 seconds. A missing or `null` `ttlMs`, or any value above 10 minutes, gives 10 minutes. Zero, a negative value of any size, or any value below 30 seconds gives 30 seconds. A `ttlMs` that is not an integer is `InvalidParams`. Renew before `refreshBefore`. A login can hold 100 live subscriptions, then `ResourceExhausted`.
- A call that is refused stores nothing.

`events/unsubscribe` takes `name`, `arguments` and `delivery.url`. It deletes the caller's own subscription and answers `NotFound` for any other, whoever owns it. It needs no repository access.

To embed faucet, set `Deps.Webhook` to a `WebhookDeps`: a `WebhookKey` from `NewWebhookKey` (for example one your own key store hands out), and a `CallbackVerifier`. `NewCallbackAllowlist` is one that makes no request. `NewCallbackHandshake` sends the signed challenge through a client that you build with `NewHTTPClient`. Your own verifier gets a `CallbackRequest` with the login, the canonical URL and the subscription ID. Its `Sign` method signs a message with the subscriber's secret, which the verifier never holds. It returns `ErrCallbackRateLimited` when it chose not to send, or a `*CallbackError` with one of the SEP's categories. Your own `Store` must implement `UpsertSubscription`, `DeleteSubscription` and `DeleteExpiredSubscriptions`, and `storetest` checks them. `Service.Purge` deletes expired subscriptions as well as old events.

## Retention and redelivery

Two routes keep the event table small and fill the gaps that a missed webhook leaves. A scheduler calls each of them with `GET` and `Authorization: Bearer <CRON_SECRET>`. Vercel Cron does this for a project that has `CRON_SECRET`, from the `crons` entries of its `vercel.json`.

| Variable | Meaning |
| --- | --- |
| `CRON_SECRET` | At least 16 bytes. Unset means both routes answer 503 and run nothing. A wrong or missing bearer token is 401. |
| `FAUCET_RETENTION_DAYS` | Whole days, 1 to 3650. Default 30. |
| `FAUCET_HOOK_TOKEN` | A GitHub token that can list the webhook's deliveries and redeliver them. |
| `FAUCET_HOOK_ORG` or `FAUCET_HOOK_REPO` | The owner of the webhook: an organization, or `owner/name`. Set exactly one. |
| `FAUCET_HOOK_ID` | The webhook's numeric ID. |

The three `FAUCET_HOOK_*` variables go together. When none is set, `/cron/redeliver` answers 503. When only some are set, the server refuses to start.

**`GET /cron/purge`** deletes the events received more than `FAUCET_RETENTION_DAYS` ago and answers `{"deleted": n, "purged_through": id}`. `purged_through` is the purge mark, the highest event ID any purge has deleted. A wait or poll whose cursor is below the mark reports `truncated: true`. It also deletes the webhook subscriptions that have expired, which the response does not count.

**`GET /cron/redeliver`** lists the webhook's deliveries from the last two hours and asks GitHub to deliver again each one that failed (any answer outside 200 to 299, or none), that faucet has not stored, and that has no later successful attempt. It answers `{"examined", "pending", "gave_up", "requested", "incomplete"}`. The first request that GitHub refuses stops the run with a 502. A run has two bounds, and it says `incomplete: true` when one stopped it:

- It sends at most 50 requests, the deliveries with the fewest redeliveries first and then the oldest. The next run goes on with the rest, because a delivery that faucet stored is skipped.
- It reads at most 10 pages of 100 attempts, the newest first. Faucet keeps no state, so the next run reads the newest 1000 attempts again and does not go on where this one stopped, and `incomplete: true` says that the window held more. A request for a delivery needs its first attempt among the newest 1000, and the n-th request comes less than n schedules after that attempt. At a 30 minute schedule the third request is therefore safe while the hook records at most 1000 / (3 × 0.5 h) = 666 attempts an hour, redeliveries included, and the first request while it records at most 1000 / 0.5 h = 2000 an hour. A delivery that falls out of view is given up and logged. A shorter schedule raises both limits: at 10 minutes they are 2000 and 6000 an hour.

- Schedule the sweep every 30 minutes or more often. The window is two hours because faucet asks at most 3 times for a delivery, the third request comes up to 3 schedules after the first attempt, and a fourth schedule covers a sweep that runs late or not at all. A slower schedule lets the first attempt leave the window before faucet has asked 3 times, so a delivery that a later request would have recovered is given up. GitHub keeps deliveries for three days, so a sweep cannot see older ones.
- Faucet gives up on a delivery after 3 redeliveries of it, or once its first attempt is more than two hours old. Each call counts the deliveries it gave up on in `gave_up` and logs `redelivery gave up` for each, with `reason=limit` or `reason=window`, on every call that still sees an attempt of the delivery. The delivery stays lost until someone redelivers it by hand. GitHub records each redelivery as a new attempt with a new time, so the window alone cannot end it: a payload that faucet always answers with 400 would use up the 50 requests of every sweep. The third request comes 60 to 90 minutes after the failure, so a delivery that failed in an outage is recovered when the outage ends within an hour of the failure.
- Two sweeps that overlap can ask for the same delivery twice. Faucet stores one event per delivery GUID, so the second delivery is a duplicate.
- `FAUCET_HOOK_TOKEN` is the most powerful credential here. GitHub offers no narrower grant than the webhook permission, so the token can also edit or delete the webhook. Use a dedicated machine user and set the variable in production only. Faucet sends it to the GitHub API URL and nowhere else, and it calls only the delivery list and the redelivery endpoint.

To embed faucet, call `Service.Purge(ctx, before)` and `Service.Redeliver(ctx, since)` on your own schedule. Neither method has auth. The host decides who may call them. `Redeliver` needs `Deps.HookAdmin`, for example a `GitHubHookAdmin` from `faucet.NewGitHubHookAdmin`, and returns `ErrNoHookAdmin` without one. A `Store` of your own must implement `Purge` and `StoredDeliveries`, and `storetest` checks them.

## Tests

Start a local Postgres 13 or newer, then run:

```sh
FAUCET_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' go test -race ./...
```

Each test creates and drops its own database. The Postgres tests skip when the variable is unset and fail when `CI` is set. After you change a migration or query, run `go -C tools tool sqlc generate -f ../sqlc.yaml`. sqlc lives in its own module under `tools/`, so it never reaches the `go.mod` of a project that imports faucet.
