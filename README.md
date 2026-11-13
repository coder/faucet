# faucet

Faucet lets agents wait for or subscribe to GitHub events, such as a CI run finishing or a PR approval, through MCP.

It is a Go library first, with a thin HTTP server on top, so coder/coder can embed it in coderd.

## Status

PR 7 of 8. Faucet receives GitHub webhooks, stores them as typed events, and lets an agent wait for them through the `wait_for_github_events` MCP tool, or poll them with the MCP Events extension (SEP-3415). It deletes old events and asks GitHub again for the deliveries it missed. An agent can also subscribe a callback URL with `events/subscribe`, and faucet sends each matching event to it as a signed webhook when a scheduler calls `GET /cron/deliver`. The next PR adds the `terminated` notice, renewal edge cases and secret rotation.

- `faucet`: `Event`, `Store`, `Normalize`, `Service.WebhookHandler`, `Service.Register` (the MCP tool and the `events/list`, `events/poll`, `events/subscribe` and `events/unsubscribe` methods), `ServerCapabilities` and `RequireEventName` (the Events extension), and `Principal`, `Authorizer` and `TokenAuthorizer` (the per-repo access check), `Service.Purge`, `Service.Redeliver`, `HookAdmin` and `GitHubHookAdmin` (retention and redelivery), and `WebhookDeps`, `WebhookKey`, `CallbackVerifier`, `NewCallbackAllowlist` and `NewCallbackHandshake` (webhook subscriptions), `NewHTTPClient` and `SignWebhook` (the guarded client and the signer that deliveries use), and `Service.DeliverDue` (webhook delivery).
- `faucet/pgstore`: the Postgres store, with embedded migrations and sqlc queries.
- `faucet/storetest`: the contract tests every `Store` must pass.
- `cmd/faucet`: the server. It serves `POST /webhook/github`, `/mcp`, `GET /healthz`, `GET /cron/purge`, `GET /cron/redeliver` and `GET /cron/deliver`.

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

`events/subscribe` follows the draft SEP-3415 text. It takes `name`, `arguments` (`repo`, and optionally `sha` and `pr`), `delivery` (`url` and `secret`, and `mode` which may be `webhook` or absent), and optionally `cursor`, `maxAgeMs` and `ttlMs`. The `Mcp-Name` header rule of `events/poll` applies. It returns `id`, `refreshBefore`, `cursor` and `truncated`, and when the call renews a subscription that existed, `deliveryStatus` (below).

- The caller must be authenticated. Faucet checks that the same token can read the repository, as `events/poll` does. A repository the token cannot read gives the same `Forbidden` error as one that does not exist. Faucet stores no token, so it checks again at every renewal.
- `delivery.secret` is `whsec_` followed by the standard base64 of 24 to 64 random bytes, as Standard Webhooks defines. The client makes it. Faucet seals it with AES-256-GCM and never returns or logs it.
- `delivery.url` must be `https`, with a public host name or address, no user info and no fragment, and it must pass the callback verification of the server. With the allowlist, its origin must be on the list, faucet makes no request to it, and an origin that is not listed gives `CallbackEndpointError` with reason `challenge_failed`. With the handshake, faucet sends the signed challenge that the section below describes.
- A subscription is the tuple (login, callback URL, event name, arguments). Subscribing again with the same tuple renews it: same `id`, the secret is replaced, and the expiry is granted again. Another tuple, or another login, is another subscription.
- The grant is `ttlMs`, never more than 10 minutes and never less than 30 seconds. A missing or `null` `ttlMs`, or any value above 10 minutes, gives 10 minutes. Zero, a negative value of any size, or any value below 30 seconds gives 30 seconds. A `ttlMs` that is not an integer is `InvalidParams`. Renew before `refreshBefore`. A login can hold 100 live subscriptions, then `ResourceExhausted`.
- A call that is refused stores nothing.

`events/unsubscribe` takes `name`, `arguments` and `delivery.url`. It deletes the caller's own subscription and answers `NotFound` for any other, whoever owns it. It needs no repository access.

To embed faucet, set `Deps.Webhook` to a `WebhookDeps`: a `WebhookKey` from `NewWebhookKey` (for example one your own key store hands out), and a `CallbackVerifier`. `NewCallbackAllowlist` is one that makes no request. `NewCallbackHandshake` sends the signed challenge through a client that you build with `NewHTTPClient`. Your own verifier gets a `CallbackRequest` with the login, the canonical URL and the subscription ID. Its `Sign` method signs a message with the subscriber's secret, which the verifier never holds. It returns `ErrCallbackRateLimited` when it chose not to send, or a `*CallbackError` with one of the SEP's categories. Your own `Store` must implement `UpsertSubscription`, `DeleteSubscription`, `DeleteExpiredSubscriptions` and the delivery queue (`ScanTargets`, `EnqueueDeliveries`, `ClaimDelivery`, `FinishDelivery` and `ExpireDeliveries`), and `storetest` checks them. `Service.Purge` deletes expired subscriptions as well as old events.

## Deliver webhooks

`GET /cron/deliver` (or `Service.DeliverDue` in a host that embeds faucet) sends the webhooks that are due. A run does four things in this order: it queues the events that live subscriptions have not read yet, ends the unsent deliveries of subscriptions that expired, claims due deliveries, and sends each through the guarded client. A run starts no attempt with less than 10 seconds left on its deadline, claims at most 200 attempts and sends 8 at a time.

**When a webhook arrives.** Faucet sends nothing by itself between runs. On Vercel the cron schedule of the project sets the pace, and a Pro cron runs at most once a minute, so **a webhook can wait up to one minute** (and a retry a minute past its backoff). This repository adds no `vercel.json`. A host that needs seconds calls `DeliverDue` in its own loop, or calls the route more often: extra calls run the same due work earlier and send nothing twice.

**What a subscription gets.** Only events of the repository that the subscriber could read at its last renewal, of the subscribed event name, that match its `sha` (in any letter case) and `pr` arguments, from the cursor on, and only while it is live. A subscription that holds 100 unsent deliveries reads no more events until some end, and the events wait in the log.

**The request.** `POST <delivery.url>` with `Content-Type: application/json`, `X-MCP-Subscription-Id`, and the Standard Webhooks headers: `webhook-id` (`msg_` and 22 random characters, one per subscription and event, the same on every attempt), `webhook-timestamp` (new on every attempt) and `webhook-signature` (`v1,` and the base64 HMAC-SHA256 over `<webhook-id>.<webhook-timestamp>.<body>`, keyed with the subscriber's secret). The request has no `Authorization` header and no cookie. The body is `{"eventId", "name", "timestamp", "data", "cursor"}`: `name` and `data` are what `events/poll` returns, `timestamp` is when faucet received the event, and `eventId` is the `webhook-id`. (SEP-3415 describes `eventId` as the upstream event. Faucet uses the message ID so that a receiver that deduplicates on `eventId` without scoping it to the subscription cannot drop the same event on two subscriptions. `events/poll` keeps the GitHub delivery ID, so the two differ.) `cursor` is the safe-to-persist watermark: every event of the subscription at or below it is delivered or given up. Persist it and send it as `cursor` on a renewal.

**Retries.** An answer in 200 to 299 ends the delivery. `410 Gone` and `413` end that delivery and not the subscription. Every other answer, a redirect (never followed), a refused connection, a TLS error and a timeout (5 seconds) fail the attempt, and the next attempt comes after 15 seconds, 1 minute, 3 minutes and 6 minutes. A delivery gets at most 5 attempts in 15 minutes from the queue, and a crash that leaves a claim open costs the attempt after a 30 second lease.

**What ends a delivery.** Every delivery ends as delivered or with one reason, counted in the reply of the route and named in one log line: `endpoint_gone`, `endpoint_payload_too_large`, `event_too_large` (a body past 256 KiB is not sent), `event_malformed`, `attempts_exhausted`, `window_expired`, `event_purged`, `repo_mismatch` (the name now points to another repository), `secret_unreadable` (the sealed secret does not open, so the subscription is deleted until the next renewal seals it again) and `subscription_expired`. A delivery also ends with its subscription when `events/unsubscribe` deletes it, and the run does not count that. The cursor of the last delivery that the subscriber received is behind those events, so a renewal with it replays them. A purge that deletes events before a subscription read them is a gap: the run counts it in `gaps` and logs it. `gap` notices are a later PR.

**Delivery status.** A renewal that finds the subscription reports `deliveryStatus`: `active` (always true, because faucet never suspends), `lastDeliveryAt`, `lastError` (`connection_refused`, `timeout`, `tls_error`, `http_4xx` or `http_5xx`, or `null`) and `failedSince`. Nothing the endpoint sent reaches a reply, a log or a table.

The route answers `{"enqueued", "gaps", "claimed", "sent", "delivered", "retried", "gave_up": {reason: count}, "superseded", "incomplete"}`. `superseded` counts attempts whose result the store refused because another run owned the delivery, and that run counts its end. `incomplete: true` says that a limit stopped the run with due work left. `/cron/deliver` answers 503 when webhook mode is off.

## Retention and redelivery

Two routes keep the event table small and fill the gaps that a missed webhook leaves. Together with `/cron/deliver` above, a scheduler calls each of them with `GET` and `Authorization: Bearer <CRON_SECRET>`. Vercel Cron does this for a project that has `CRON_SECRET`, from the `crons` entries of its `vercel.json`.

| Variable | Meaning |
| --- | --- |
| `CRON_SECRET` | At least 16 bytes. Unset means every cron route answers 503 and runs nothing. A wrong or missing bearer token is 401. |
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
