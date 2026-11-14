# faucet

Faucet lets agents wait for or subscribe to GitHub events, such as a CI run finishing or a PR approval, through MCP.

It is a Go library first, with a thin HTTP server on top, so coder/coder can embed it in coderd.

## Status

PR 7 of 8. Faucet receives GitHub webhooks, stores them as typed events, and lets an agent wait for them through the `wait_for_github_events` MCP tool, or poll them with the MCP Events extension (SEP-3415). It deletes old events and tells a waiting agent when a purge deleted events after its cursor. An agent can also subscribe a callback URL with `events/subscribe`. Faucet sends each matching event to it as a signed webhook when a scheduler calls `GET /cron/deliver`. The next PR adds the `terminated` notice, renewal edge cases and secret rotation.

- `faucet`: `Event`, `Store`, `Normalize`, `Service.WebhookHandler`, `Service.Register` (the MCP tool and the `events/list`, `events/poll`, `events/subscribe` and `events/unsubscribe` methods), `ServerCapabilities` and `RequireEventName` (the Events extension), and `Principal`, `Authorizer` and `TokenAuthorizer` (the per-repo access check), `Service.Purge` (retention), and `WebhookDeps`, `WebhookKey`, `CallbackVerifier`, `NewCallbackAllowlist` and `NewCallbackHandshake` (webhook subscriptions), `NewHTTPClient` and `SignWebhook` (the guarded client and the signer that deliveries use), and `Service.DeliverDue` (webhook delivery), and `Service.Redeliver`, `HookAdmin`, `GitHubHookAdmin` and `RedeliveryStore` (redelivery of failed webhooks).
- `faucet/pgstore`: the Postgres store, with embedded migrations and sqlc queries.
- `faucet/storetest`: the contract tests every `Store` must pass.
- `cmd/faucet`: the server. It serves `POST /webhook/github`, `/mcp`, `GET /healthz`, `GET /cron/purge` `GET /cron/deliver` and `GET /cron/redeliver`.

## Run the server

```sh
DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' \
GITHUB_WEBHOOK_SECRET='<a random string of at least 16 bytes>' \
go run ./cmd/faucet
```

`PORT` defaults to 8080. The server applies pending migrations at startup. It exits at once when `DATABASE_URL` or `GITHUB_WEBHOOK_SECRET` is missing. When `DATABASE_URL_UNPOOLED` is set, the server connects with it and ignores `DATABASE_URL`. Use it when `DATABASE_URL` goes through a transaction-mode pooler such as Neon's pooled URL, because the Postgres driver (`lib/pq`) sends a query and its parameters in two batches, and a pooler can fail the second one with `unnamed prepared statement does not exist`. The server opens at most 10 connections, and closes an idle one after 10 seconds. It migrates only when `VERCEL_ENV` is unset or `production` (see **Deploy on Vercel**).

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

Webhook mode is off until the operator sets `FAUCET_WEBHOOK_KEY` and a way to verify callback URLs: `FAUCET_WEBHOOK_ALLOWED_ORIGINS` (the allowlist) or `FAUCET_WEBHOOK_CALLBACK_VERIFICATION=handshake`. While it is off, `events/list` offers `poll` only and `events/subscribe` and `events/unsubscribe` answer `Unsupported`.

| Variable | Meaning |
| --- | --- |
| `FAUCET_WEBHOOK_KEY` | The standard base64 of exactly 32 random bytes that are not all zero, for example `openssl rand -base64 32`. It seals subscriber secrets in the database. |
| `FAUCET_WEBHOOK_ALLOWED_ORIGINS` | Comma-separated `https://host[:port]` origins that may receive events. Nothing else is accepted as a callback. |
| `FAUCET_WEBHOOK_CALLBACK_VERIFICATION` | How faucet decides that a callback URL may receive events: `allowlist` (the default) or `handshake`. |

The key and a way to verify callback URLs go together. In `allowlist` mode, which is the default, faucet needs `FAUCET_WEBHOOK_KEY` and `FAUCET_WEBHOOK_ALLOWED_ORIGINS`, and it makes no request to a callback URL. In `handshake` mode faucet needs `FAUCET_WEBHOOK_KEY` and `FAUCET_WEBHOOK_CALLBACK_VERIFICATION=handshake`, and `FAUCET_WEBHOOK_ALLOWED_ORIGINS` must be unset, because an operator who set both could think that the allowlist still limits the destinations. At start the server refuses to run, and names the variable but never the value, when only one of the pair is set in `allowlist` mode, when the key is missing in either mode, when the origins are set in `handshake` mode, when the mode is neither `allowlist` nor `handshake`, or when a value is invalid. Set the key in production only, and keep it when you restart: rows sealed under another key cannot be read, and subscribers seal them again with their next refresh (within 10 minutes).

In `handshake` mode, a first `events/subscribe` for a login and a callback URL makes faucet send one signed `POST` to that URL (SEP-3415 verification option 1), and the subscribe call waits for the answer, for 5 seconds at most. The body is `{"type":"verification","challenge":"<nonce>"}` with the Standard Webhooks headers `webhook-id` (`msg_verification_...`), `webhook-timestamp` and `webhook-signature`, signed with the subscriber's `whsec_` secret, and with `X-MCP-Subscription-Id`. The endpoint must verify the signature and answer `2xx` with a JSON object whose only member is `challenge`, set to the nonce. Any other answer fails the call with `CallbackEndpointError` (`connection_refused`, `timeout`, `tls_error`, `http_4xx`, `http_5xx` or `challenge_failed`) and stores nothing. Success is remembered for the login and URL for 10 minutes. Two buckets limit the handshakes. Each login has one per destination: 5 at once, then one every 12 seconds. Each destination has one for all logins together: 60 at once, then one a second. A login that keeps failing therefore empties only its own bucket and cannot starve another login. It takes about a dozen logins that fail without pause to empty the bucket of a destination, and then every login is limited until it refills, which `allowlist` mode avoids. A host written as an IP address counts by the address itself, so another spelling of it (`::ffff:1.1.1.1` and `1.1.1.1`) shares the buckets, and a host name counts in lower case without a trailing dot or a port. A handshake that the endpoint answers correctly gives both tokens back, so a login that the endpoint answers is never limited, and the answered handshakes do not count against the destination. When a bucket is empty, the call answers `ResourceExhausted` with `limit` `callbackVerifications` and sends nothing.

The handshake and every later request to a callback URL go through the guarded client (`faucet.NewHTTPClient`). It speaks `https` only. It looks the host name up, refuses the request when any address in the answer is not public (loopback, private, link-local, CGNAT, multicast, unspecified, NAT64, 6to4, Teredo and the other special ranges), and then dials the address it checked, so the DNS answer cannot change between the check and the connection. An IPv4-mapped IPv6 address is judged by the IPv4 address inside it, so `::ffff:8.8.8.8` is public and `::ffff:10.0.0.1` is not. The client still verifies the certificate against the host name. It follows no redirect, ignores `HTTPS_PROXY` and the other proxy variables, stops after 5 seconds, and refuses a response body past 64 KiB. Hosts written as a number (`2130706433`, `0x7f.1`) or as one label are refused before any lookup. The error for a host that points inside your network is `connection_refused`, the same as for a closed port, so a caller cannot map your network. Use `allowlist` when a public endpoint can front an internal service.

A binary built with `-tags faucet_e2e` lets the guarded client reach loopback addresses over `http`, so the end-to-end tests can run a receiver on `127.0.0.1`. No variable does this, so a production binary has no switch that turns the guard off. The tagged binary logs a warning at start. Never ship it.

`events/subscribe` follows the draft SEP-3415 text. It takes `name`, `arguments` (`repo`, and optionally `sha` and `pr`), `delivery` (`url` and `secret`, and `mode` which may be `webhook` or absent), and optionally `cursor`, `maxAgeMs` and `ttlMs`. The `Mcp-Name` header rule of `events/poll` applies. It returns `id`, `refreshBefore`, `cursor` and `truncated`, and when the call renews a subscription that existed, `deliveryStatus` (below).

- The caller must be authenticated. Faucet checks that the same token can read the repository, as `events/poll` does. A repository the token cannot read gives the same `Forbidden` error as one that does not exist. Faucet stores no token, so it checks again at every renewal.
- `delivery.secret` is `whsec_` followed by the standard base64 of 24 to 64 random bytes, as Standard Webhooks defines. The client makes it. Faucet seals it with AES-256-GCM and never returns or logs it.
- `delivery.url` must be `https`, with a public host name or address, no user info and no fragment, and it must pass the callback verification of the server. With the allowlist, its origin must be on the list, faucet makes no request to it, and an origin that is not listed gives `CallbackEndpointError` with reason `challenge_failed`. With the handshake, faucet sends the signed challenge that the section below describes.
- A subscription is the tuple (login, callback URL, event name, arguments). Subscribing again with the same tuple renews it: same `id`, the secret is replaced (see **Secret rotation**), and the expiry is granted again. Another tuple, or another login, is another subscription.
- The grant is `ttlMs`, never more than 10 minutes and never less than 30 seconds. A missing or `null` `ttlMs`, or any value above 10 minutes, gives 10 minutes. Zero, a negative value of any size, or any value below 30 seconds gives 30 seconds. A `ttlMs` that is not an integer is `InvalidParams`. Renew before `refreshBefore`. A login can hold 100 live subscriptions, then `ResourceExhausted`.
- A call that is refused stores nothing.

`events/unsubscribe` takes `name`, `arguments` and `delivery.url`. It deletes the caller's own subscription and answers `NotFound` for any other, whoever owns it. It needs no repository access.

To embed faucet, set `Deps.Webhook` to a `WebhookDeps`: a `WebhookKey` from `NewWebhookKey` (for example one your own key store hands out), and a `CallbackVerifier`. `NewCallbackAllowlist` is one that makes no request. `NewCallbackHandshake` sends the signed challenge through a client that you build with `NewHTTPClient`. Your own verifier gets a `CallbackRequest` with the login, the canonical URL and the subscription ID. Its `Sign` method signs a message with the subscriber's secret, which the verifier never holds. It returns `ErrCallbackRateLimited` when it chose not to send, or a `*CallbackError` with one of the SEP's categories. Your own `Store` must implement `UpsertSubscription`, `DeleteSubscription`, `DeleteLiveSubscription`, `DeleteExpiredSubscriptions` and the delivery queue (`ScanTargets`, `EnqueueDeliveries`, `ClaimDelivery`, `FinishDelivery` and `ExpireDeliveries`), and `storetest` checks them. `Service.Purge` deletes expired subscriptions as well as old events.

## Deliver webhooks

`GET /cron/deliver` (or `Service.DeliverDue` in a host that embeds faucet) sends the webhooks that are due. A run does four things in this order: it queues the events that live subscriptions have not read yet, ends the unsent deliveries of subscriptions that expired, claims due deliveries, and sends each through the guarded client. A run starts no attempt with less than 10 seconds left on its deadline, claims at most 200 attempts and sends 8 at a time.

**When a webhook arrives.** Faucet sends nothing by itself between runs. On Vercel the cron schedule of the project sets the pace, and a Pro cron runs at most once a minute, so **a webhook can wait up to one minute** (and a retry a minute past its backoff). The `vercel.json` of this repository sets that schedule (see **Deploy on Vercel**). A host that needs seconds calls `DeliverDue` in its own loop, or calls the route more often: extra calls run the same due work earlier and never send a delivery before its time. Delivery is at least once: a request that went out and whose result faucet could not record (a crash, a run that ran out of time) is sent again after the 30 second lease, with the same `webhook-id`, so deduplicate on it.

**What a subscription gets.** Only events of the repository that the subscriber could read at its last renewal, of the subscribed event name, that match its `sha` (in any letter case) and `pr` arguments, from the cursor on, and only while it is live. A subscription that holds 100 unsent deliveries reads no more events until some end, and the events wait in the log.

**The request.** `POST <delivery.url>` with `Content-Type: application/json`, `X-MCP-Subscription-Id`, and the Standard Webhooks headers: `webhook-id` (`msg_` and 22 URL-safe characters that an HMAC under the operator's `FAUCET_WEBHOOK_KEY` makes from the subscription and the event, so it is the same on every attempt and on every replay of that event to that subscription, and nobody without the key can compute it; a new key gives a replay new IDs once), `webhook-timestamp` (new on every attempt) and `webhook-signature` (`v1,` and the base64 HMAC-SHA256 over `<webhook-id>.<webhook-timestamp>.<body>`, keyed with the subscriber's secret, and while a rotation window runs, with the previous secret too, see **Secret rotation**). The request has no `Authorization` header and no cookie. The body is `{"eventId", "name", "timestamp", "data", "cursor"}`: `name` and `data` are what `events/poll` returns, `timestamp` is when faucet received the event, and `eventId` is the `webhook-id`. (SEP-3415 describes `eventId` as the upstream event. Faucet uses the message ID so that a receiver that deduplicates on `eventId` without scoping it to the subscription cannot drop the same event on two subscriptions. `events/poll` keeps the GitHub delivery ID, so the two differ.) `cursor` is the safe-to-persist watermark: every event of the subscription at or below it is delivered or given up. Persist it. A `events/subscribe` after the subscription lapsed (it expired, or `events/unsubscribe` deleted it) starts after the cursor you send, so it sends every event after that cursor again, including ones the endpoint already acknowledged. They keep the `webhook-id` they had, so a receiver that deduplicates on it drops them. A renewal of a live subscription keeps its own position and ignores the cursor, as SEP-3415 says.

**Retries.** An answer in 200 to 299 ends the delivery. `410 Gone` and `413` end that delivery and not the subscription. Every other answer, a redirect (never followed), a refused connection, a TLS error and a timeout (5 seconds) fail the attempt, and the next attempt comes after 15 seconds, 1 minute, 3 minutes and 6 minutes. A delivery gets at most 5 attempts in 15 minutes from the queue, and a crash that leaves a claim open costs the attempt after a 30 second lease.

**What ends a delivery.** Every delivery ends as delivered or with one reason, counted in the reply of the route and named in one log line: `endpoint_gone`, `endpoint_payload_too_large`, `event_too_large` (a body past 256 KiB is not sent), `event_malformed`, `attempts_exhausted`, `window_expired`, `event_purged`, `repo_mismatch` (the name now points to another repository), `secret_unreadable` (the current sealed secret does not open, so the subscription is deleted until the next renewal seals it again) and `subscription_expired`. A delivery also ends with its subscription when `events/unsubscribe` deletes it, when `/cron/purge` deletes expired subscriptions, and when an `events/subscribe` removes its login's expired rows. The run counts none of those, unless its own sweep of expired subscriptions comes first and counts `subscription_expired`. The cursor of the last delivery that the subscriber received is behind those events, so a new subscribe with it sends them again. A purge that deletes events before a subscription read them is a gap: the run counts it in `gaps` and logs it. `gap` notices are a later PR.

**Control envelopes.** Faucet also POSTs two bodies that have a top-level `type` and no `eventId` or `data` (SEP-3415, "Non-event webhook bodies"). They carry the same headers as an event and are signed with the subscriber's secret. Their `webhook-id` is `msg_<type>_` and 22 random URL-safe characters, new for every message. Each goes once: one attempt, no queue and no retry, and a send that fails is logged and counted.

- `{"type":"gap","cursor":"v1.<n>"}` goes to a subscription that a purge overtook: the purge deleted an event of the subscription's repository and event name that the subscription had not read. (The check ignores the `sha` and `pr` arguments, so a filtered subscription can get a gap for an event that its filter would have dropped.) A purge that deleted none of its repository's events sends it nothing, however long the subscription has been quiet. The client persists `cursor` and treats it as `truncated: true`. `n` is the purge mark, so resuming from it replays every event that still exists. Later events keep flowing. The run queues no event and makes no request for a gap in the same step: it counts the subscription in `gaps` when it queues the next events, and sends the envelope in its send phase, ahead of the deliveries and under the same deadline. It counts the send in `gaps_sent` or `gaps_failed`, and a gap that the run ended before sending is in `gaps_failed` too, with one log line. A run that lost the race to queue for that subscription sends nothing, so one gap sends one envelope.
- `{"type":"terminated","error":{"code":-32024,"message":"Forbidden","data":{"reason":"Access revoked"}}}` goes to a subscription whose renewal failed the repository access check with no access. The renewal deletes the subscription first, and only the renewal whose delete removed the row sends the envelope, signed with the stored secret and not with the one in the renewal. The renewal itself still answers Forbidden. A later renewal is a new subscribe. `Service.Terminations()` counts the sent and the failed envelopes.

Faucet holds no GitHub token of its own, so it sees a revoked grant **only when the subscriber renews**. A subscriber that never renews ends at its expiry (10 minutes at most) with no envelope. A rejected token, and an access check that could not run, end nothing. A GitHub rate limit (a 429, or a 403 with no quota left, a `Retry-After` header or a message that names the limit) is a failed check, not a denial: it ends nothing and the renewal gets a retryable error. Any other 403 is no access.

**Secret rotation.** To rotate, call `events/subscribe` again with a new `delivery.secret`. For a grace window faucet signs every delivery, `gap` envelope and `terminated` envelope with both secrets: `webhook-signature` holds `v1,<new>` and `v1,<old>`, space-delimited, as Standard Webhooks defines, so a receiver that holds only one of them still verifies. The window is 10 minutes at most, and never longer than the TTL that the rotating call was granted. It starts at the refresh that changed the secret and ends on its own: a later refresh with the same secret does not extend it. Faucet keeps one previous secret, so a second rotation inside the window drops the first one. A refresh with the same secret starts no window. The previous secret is sealed like the current one, and `/cron/deliver` removes it from the database when its window ends (and counts them in `previous_secrets_cleared`). A window that ended signs nothing, even before that run. A previous secret that does not open, which happens when you change `FAUCET_WEBHOOK_KEY` and a subscriber renews, is skipped with a log line: the current secret signs alone, and neither the delivery nor the subscription ends. Only the principal that owns the subscription can rotate it. A handshake that a call triggers (a first subscribe, or a renewal after faucet forgot the verified login and URL) is signed with the new secret of that call only, so give your receiver the new secret before you rotate.

**Delivery status.** A renewal that finds the subscription reports `deliveryStatus`: `active` (always true, because faucet never suspends), `lastDeliveryAt`, `lastError` (`connection_refused`, `timeout`, `tls_error`, `http_4xx` or `http_5xx`, or `null`; a redirect counts as `http_4xx`) and `failedSince`. Nothing the endpoint sent reaches a reply, a log or a table.

The route answers `{"enqueued", "gaps", "gaps_sent", "gaps_failed", "claimed", "sent", "delivered", "retried", "gave_up": {reason: count}, "superseded", "previous_secrets_cleared", "incomplete"}`. `superseded` counts attempts whose result the store refused, because another run owns the delivery now (that run counts its end) or because its subscription was deleted meanwhile. `incomplete: true` says that a limit stopped the run with due work left. `/cron/deliver` answers 503 when webhook mode is off.

## Retention

`GET /cron/purge` keeps the event table small. Together with `/cron/deliver` above, a scheduler calls each route with `GET` and `Authorization: Bearer <CRON_SECRET>`. Vercel Cron does this for a project that has `CRON_SECRET`, from the `crons` entries of its `vercel.json`.

| Variable | Meaning |
| --- | --- |
| `CRON_SECRET` | At least 16 bytes. Unset means every cron route answers 503 and runs nothing. A wrong or missing bearer token is 401. |
| `FAUCET_RETENTION_DAYS` | Whole days, 1 to 3650. Default 30. |

**`GET /cron/purge`** deletes the events received more than `FAUCET_RETENTION_DAYS` ago and answers `{"deleted": n, "purged_through": id}`. `purged_through` is the purge mark, the highest event ID any purge has deleted. A wait or poll whose cursor is below the mark reports `truncated: true`. It also deletes the webhook subscriptions that have expired, which the response does not count.

To embed faucet, call `Service.Purge(ctx, before)` on your own schedule. It has no auth. The host decides who may call it. A `Store` of your own must implement `Purge`, and `storetest` checks it.

## Redeliver failed webhooks

GitHub does not send a webhook again when faucet was down or answered with an error. `GET /cron/redeliver` asks GitHub to deliver each failed delivery again, until faucet stores the event. Faucet keeps its place in GitHub's delivery list in Postgres. A run that cannot read all of the list goes on where it stopped, and no failed delivery ends without a counted, logged final state. Redelivery is off until the hook settings are set. It uses the same `CRON_SECRET` guard as the purge. Schedule it every 10 minutes (`*/10 * * * *`), with `GET` and `Authorization: Bearer <CRON_SECRET>`.

| Variable | Meaning |
| --- | --- |
| `FAUCET_HOOK_TOKEN` | A token that can list and redeliver the webhook's deliveries: a classic token with `admin:org_hook` (org hook) or the repository hook permission (repo hook), owned by a machine user. GitHub offers no narrower grant, and the same token can edit or delete the hook, so set it in production only. Faucet sends it only to the GitHub API URL, and only to two endpoints: `GET .../hooks/{id}/deliveries` and `POST .../deliveries/{attempt}/attempts`. |
| `FAUCET_HOOK_ORG` or `FAUCET_HOOK_REPO` | The webhook's owner: an organization, or `owner/name`. Set exactly one. |
| `FAUCET_HOOK_ID` | The webhook's numeric ID. |

The token, the ID and one of org or repo go together. With none set, `/cron/redeliver` answers 503. With some set, the server refuses to start and names the missing variables, never a value.

**What a run does.** A run reads GitHub's deliveries, newest first, down to where the run before it stopped, less a 5 minute overlap for attempts that GitHub wrote late. A stretch that the run cannot read inside its page limit is kept as a range with GitHub's cursor, and later runs read the ranges, newest first. A delivery qualifies when an attempt failed (any answer outside 200 to 299, or none), its event and action are ones that faucet stores, and faucet has no event with its GUID. A run asks for it, and a later run sees the event stored, or a later successful attempt, and counts it recovered. A stored delivery, or one that succeeded later, is never asked for. Two runs never overlap: one holds a lease, and a second answers `busy: true`. The position of a run is a time that GitHub gave: the newest attempt it read, or, when the list is empty, the `Date` header of GitHub's answer less the lookback of 24 hours, so that a `Date` that runs ahead cannot hide the next failure. It is never a time of faucet's clock, so a clock that runs ahead of GitHub's cannot put a new failure below the position. The end of GitHub's retention, where a gap stops, comes from the same `Date`, and a run that has neither a `Date` nor an attempt to take a time from records no gap.

**Final states.** Every failed delivery ends once, in one of these, and the run that ends it counts it in the reply and logs it. A run that fails still answers with its counts (see the reply below), so each outcome that it recorded reaches you:

- `recovered`: faucet stored the event, or a later attempt succeeded.
- `gave_up` with `limit`: every ask was spent. A delivery is asked for 7 times, at 0, 10, 30, 70, 150, 310 and 630 minutes after the run that first read it. The count is raised before the request is sent, so a crash or a refused request costs an ask, and two runs cannot send the same one.
- `gave_up` with `expired`: GitHub keeps a delivery for 72 hours, counted from its first attempt (the earliest attempt that a run read), and the delivery was still not stored. A delivery of a webhook that you replaced ends here too: an attempt ID of one webhook means nothing to another, so a run asks only for the deliveries of the webhook it was configured with. Those deliveries stay pending until faucet stores the event or 72 hours pass.
- `gap`: a time range of attempts that no run read. It names no delivery, because the attempts were never read. A range whose old end left GitHub's retention before a run read it is a gap, and so is the part of GitHub's 72 hours before the first run's lookback of 24 hours, and the open ranges of a webhook that the operator replaced.

The reply is JSON: `busy`, `pages`, `attempts`, `tracked`, `asked`, `recovered`, `gave_up` (`limit` and `expired`), `gaps`, `pending` and `ranges`. A run that fails answers 500 with the same JSON and an `error` member, so the outcomes that it recorded before it failed are in a reply as well as in the log. The first reply of a new database says `gaps: 1`, for the time before its lookback. Faucet deletes a final delivery and a recorded gap 72 hours after its last attempt, so a give-up is logged once. A pending delivery and an open range stay until they end, and the scan position is one row that stays.

**Capacity.** A run reads at most 20 pages of 100 attempts, which is 2,000 attempts, and sends at most 50 requests. A run every 10 minutes reads 15 minutes of the list: 10 minutes of new attempts and the 5 minute overlap. It also needs one attempt beyond them, to learn that it has reached the end of that window, and it starts from the newest attempt of the run before it, so a run holds at most 1,998 attempts of its window. That is 7,992 attempts an hour at best, and the sweep keeps up with 7,900 attempts an hour, rounded down for a run that starts late, and sends at most 300 requests an hour. These numbers hold only when GitHub answers each page of the list within 0.6 seconds, because a run reads the list for half of its 25 seconds (see Time). A slower API reads fewer pages: at 1.3 seconds a page a run reads 9 pages, and the sweep keeps up with about 3,500 attempts an hour. Faucet has no measurement of GitHub's page time, so size a hook with that margin. The list counts every attempt of the hook: attempts of events that faucet does not store, and the redeliveries that faucet asked for. A hook that records more than that does not lose anything silently: the part of the list that a run could not read stays a range. Later runs read the ranges, newest first, with the pages that the newest attempts leave them, and every run reads the overlap again, because GitHub lists some attempts late. A burst that holds more attempts in the 5 minute overlap than a run has pages can keep a quiet hook from reading its older ranges: each run reads the same newest pages again. No failure is lost silently then. A range that no run read when its old end left GitHub's retention is counted and logged as a gap, so every failure in it ends recovered or inside that gap. A failure of a request, GitHub's rate limit or a slow API makes a run read fewer pages. The simulations in `faucet/redeliver_sim_test.go` run 4,200 attempts an hour with one failure in 100, and outages of up to an hour at 150, 300 and 600 attempts an hour, and no failure is lost. They also check the law above at a tenth of the scale: the sweep holds at its stated capacity, and ranges grow when the load is a tenth above it. An outage of faucet that ends within 630 minutes of the first run that reads a failure is recovered by one of its asks.

**Time.** The cron route stops a run after 25 seconds. A run that has such a deadline spends it so that the asks and the purge always get their turn: it reads the list for half of the time, drops a page that is not back by then and leaves the rest as an open range (as at the page limit), settles, asks until 80 percent of the time has passed, and purges. A slow GitHub costs pages and never the asks. A first page that is not back in half of the time ends the run with an error, and it asks for nothing.

To embed faucet, set `Deps.Redelivery` to a `RedeliveryDeps`: a `RedeliveryStore` (`pgstore.Store` is one, and `storetest.RunRedelivery` checks yours), and a `HookAdmin`. `NewGitHubHookAdmin` is the one for GitHub's REST API. Call `Service.Redeliver(ctx)` on your own schedule. It has no auth. The host decides who may call it.

## Deploy on Vercel

`vercel.json` deploys `cmd/faucet` as a Vercel Go service (Go runtime, Beta) behind one catch-all rewrite, lets a request run for 90 seconds at most (an `/mcp` wait lasts 50 seconds at most, and the server's write deadline for it is 75), and schedules the cron routes: `/cron/deliver` every minute, `/cron/redeliver` every 10 minutes and `/cron/purge` every hour. A cron every minute needs the Pro plan. Vercel runs crons on production deployments only, and sends `Authorization: Bearer $CRON_SECRET`.

**Previews are off, on purpose.** A preview runs the code of any branch. The Neon integration gives a preview the same database as production, and the server migrates at start. A preview of an unmerged migration would therefore change the production schema before anyone reviewed it, and every production instance that started afterwards would exit. `vercel.json` sets `git.deploymentEnabled` to `false` for every branch except `main`, and it holds for Git deployments only (a `vercel deploy` from a terminal skips it). Two more guards back it up:

- The server migrates only when `VERCEL_ENV` is unset or `production`. In any other environment it checks the schema, and exits at start unless the database is already at the newest migration.
- Keep the database variables and every secret below set for Production only. A preview must not read them.

1. Create a Vercel project from this repository and attach a Neon Postgres database to it with the Vercel integration. The integration sets `DATABASE_URL` (pooled) and `DATABASE_URL_UNPOOLED` (direct), and the `POSTGRES_*` and `PG*` variables. Remove the Preview target from all of them. The server uses the direct URL, for the reason that **Run the server** gives.
2. Set these variables for Production only, as sensitive:

   | Variable | Value |
   | --- | --- |
   | `GITHUB_WEBHOOK_SECRET` | The secret of the GitHub webhook. |
   | `CRON_SECRET` | A random string, for example `openssl rand -hex 32`. |
   | `FAUCET_WEBHOOK_KEY` | `openssl rand -base64 32`. |
   | `FAUCET_WEBHOOK_CALLBACK_VERIFICATION` | `handshake`. To use an allowlist instead, delete this variable and set `FAUCET_WEBHOOK_ALLOWED_ORIGINS`. |

   Redelivery stays off, and `/cron/redeliver` answers 503, until the GitHub webhook exists and you set `FAUCET_HOOK_TOKEN`, `FAUCET_HOOK_ORG` or `FAUCET_HOOK_REPO`, and `FAUCET_HOOK_ID` (see **Redeliver failed webhooks**).
3. Deploy by pushing to `main`. Do not run `vercel deploy` by hand. On a project with no production deployment, a plain `vercel deploy` created a production deployment.
4. Set the GitHub webhook's payload URL to `https://<production domain>/webhook/github`. Do not put Vercel Deployment Protection on that domain, because GitHub and MCP clients cannot pass it.

The server applies pending migrations when a production instance starts, over `DATABASE_URL_UNPOOLED`, before it serves a request.

## Tests

Start a local Postgres 13 or newer, then run:

```sh
FAUCET_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' go test -race ./...
```

Each test creates and drops its own database. The Postgres tests skip when the variable is unset and fail when `CI` is set. After you change a migration or query, run `go -C tools tool sqlc generate -f ../sqlc.yaml`. sqlc lives in its own module under `tools/`, so it never reaches the `go.mod` of a project that imports faucet.
