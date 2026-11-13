# faucet

Faucet lets agents wait for or subscribe to GitHub events, such as a CI run finishing or a PR approval, through MCP.

It is a Go library first, with a thin HTTP server on top, so coder/coder can embed it in coderd.

## Status

PR 6 of 8. Faucet receives GitHub webhooks, stores them as typed events, and lets an agent wait for them through the `wait_for_github_events` MCP tool, or poll them with the MCP Events extension (SEP-3415). It deletes old events and tells a waiting agent when a purge deleted events after its cursor. An agent can also subscribe a callback URL with `events/subscribe`. Faucet stores the subscription. It does not send any event yet, which PR 7 adds. The first half of PR 7 adds what delivery needs: a guarded HTTP client, a webhook signer and the handshake that checks a callback URL.

- `faucet`: `Event`, `Store`, `Normalize`, `Service.WebhookHandler`, `Service.Register` (the MCP tool and the `events/list`, `events/poll`, `events/subscribe` and `events/unsubscribe` methods), `ServerCapabilities` and `RequireEventName` (the Events extension), and `Principal`, `Authorizer` and `TokenAuthorizer` (the per-repo access check), `Service.Purge` (retention), and `WebhookDeps`, `WebhookKey`, `CallbackVerifier`, `NewCallbackAllowlist` and `NewCallbackHandshake` (webhook subscriptions), and `NewHTTPClient` and `SignWebhook` (the guarded client and the signer that deliveries use), and `Service.Redeliver`, `HookAdmin`, `GitHubHookAdmin` and `RedeliveryStore` (redelivery of failed webhooks).
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

`events/subscribe` follows the draft SEP-3415 text. It takes `name`, `arguments` (`repo`, and optionally `sha` and `pr`), `delivery` (`url` and `secret`, and `mode` which may be `webhook` or absent), and optionally `cursor`, `maxAgeMs` and `ttlMs`. The `Mcp-Name` header rule of `events/poll` applies. It returns `id`, `refreshBefore`, `cursor` and `truncated`.

- The caller must be authenticated. Faucet checks that the same token can read the repository, as `events/poll` does. A repository the token cannot read gives the same `Forbidden` error as one that does not exist. Faucet stores no token, so it checks again at every renewal.
- `delivery.secret` is `whsec_` followed by the standard base64 of 24 to 64 random bytes, as Standard Webhooks defines. The client makes it. Faucet seals it with AES-256-GCM and never returns or logs it.
- `delivery.url` must be `https`, with a public host name or address, no user info and no fragment, and it must pass the callback verification of the server. With the allowlist, its origin must be on the list, faucet makes no request to it, and an origin that is not listed gives `CallbackEndpointError` with reason `challenge_failed`. With the handshake, faucet sends the signed challenge that the section below describes.
- A subscription is the tuple (login, callback URL, event name, arguments). Subscribing again with the same tuple renews it: same `id`, the secret is replaced, and the expiry is granted again. Another tuple, or another login, is another subscription.
- The grant is `ttlMs`, never more than 10 minutes and never less than 30 seconds. A missing or `null` `ttlMs`, or any value above 10 minutes, gives 10 minutes. Zero, a negative value of any size, or any value below 30 seconds gives 30 seconds. A `ttlMs` that is not an integer is `InvalidParams`. Renew before `refreshBefore`. A login can hold 100 live subscriptions, then `ResourceExhausted`.
- A call that is refused stores nothing.

`events/unsubscribe` takes `name`, `arguments` and `delivery.url`. It deletes the caller's own subscription and answers `NotFound` for any other, whoever owns it. It needs no repository access.

To embed faucet, set `Deps.Webhook` to a `WebhookDeps`: a `WebhookKey` from `NewWebhookKey` (for example one your own key store hands out), and a `CallbackVerifier`. `NewCallbackAllowlist` is one that makes no request. `NewCallbackHandshake` sends the signed challenge through a client that you build with `NewHTTPClient`. Your own verifier gets a `CallbackRequest` with the login, the canonical URL and the subscription ID. Its `Sign` method signs a message with the subscriber's secret, which the verifier never holds. It returns `ErrCallbackRateLimited` when it chose not to send, or a `*CallbackError` with one of the SEP's categories. Your own `Store` must implement `UpsertSubscription`, `DeleteSubscription` and `DeleteExpiredSubscriptions`, and `storetest` checks them. `Service.Purge` deletes expired subscriptions as well as old events.

## Retention

`GET /cron/purge` keeps the event table small. A scheduler calls it with `GET` and `Authorization: Bearer <CRON_SECRET>`. Vercel Cron does this for a project that has `CRON_SECRET`, from the `crons` entries of its `vercel.json`.

| Variable | Meaning |
| --- | --- |
| `CRON_SECRET` | At least 16 bytes. Unset means the route answers 503 and runs nothing. A wrong or missing bearer token is 401. |
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

**What a run does.** A run reads GitHub's deliveries, newest first, down to where the run before it stopped, less a 5 minute overlap for attempts that GitHub wrote late. A stretch that the run cannot read inside its page limit is kept as a range with GitHub's cursor, and later runs read the ranges, newest first. A delivery qualifies when an attempt failed (any answer outside 200 to 299, or none), its event and action are ones that faucet stores, and faucet has no event with its GUID. A run asks for it, and a later run sees the event stored, or a later successful attempt, and counts it recovered. A stored delivery, or one that succeeded later, is never asked for. Two runs never overlap: one holds a lease, and a second answers `busy: true`.

**Final states.** Every failed delivery ends once, in one of these, and the run that ends it counts it in the reply and logs it:

- `recovered`: faucet stored the event, or a later attempt succeeded.
- `gave_up` with `limit`: every ask was spent. A delivery is asked for 7 times, at 0, 10, 30, 70, 150, 310 and 630 minutes after the run that first read it. The count is raised before the request is sent, so a crash or a refused request costs an ask, and two runs cannot send the same one.
- `gave_up` with `expired`: GitHub keeps a delivery for 72 hours, counted from its first attempt (the earliest attempt that a run read), and the delivery was still not stored.
- `gap`: a time range of attempts that no run read. It names no delivery, because the attempts were never read. A range whose old end left GitHub's retention before a run read it is a gap, and so is the part of GitHub's 72 hours before the first run's lookback of 24 hours, and the open ranges of a webhook that the operator replaced.

The reply is JSON: `busy`, `pages`, `attempts`, `tracked`, `asked`, `recovered`, `gave_up` (`limit` and `expired`), `gaps`, `pending` and `ranges`. The first reply of a new database says `gaps: 1`, for the time before its lookback. Rows of the three redelivery tables are deleted 72 hours after their last attempt, so a give-up is logged once.

**Capacity.** A run reads at most 20 pages of 100 attempts, which is 2,000 attempts, and sends at most 50 requests. A run every 10 minutes covers 10 minutes of new attempts and the 5 minute overlap, so it keeps up with 8,000 attempts an hour and sends at most 300 requests an hour. A hook that records more than that does not lose anything silently: the part of the list that a run could not read stays a range, and the ranges are read when the load drops, or they expire as a gap. A failure of a request, GitHub's rate limit or a slow API makes a run read fewer pages. The simulations in `faucet/redeliver_sim_test.go` run 4,200 attempts an hour with one failure in 100, and outages of up to an hour at 150, 300 and 600 attempts an hour, and no failure is lost. An outage of faucet that ends within 630 minutes of the first run that reads a failure is recovered by one of its asks.

To embed faucet, set `Deps.Redelivery` to a `RedeliveryDeps`: a `RedeliveryStore` (`pgstore.Store` is one, and `storetest.RunRedelivery` checks yours), and a `HookAdmin`. `NewGitHubHookAdmin` is the one for GitHub's REST API. Call `Service.Redeliver(ctx)` on your own schedule. It has no auth. The host decides who may call it.

## Tests

Start a local Postgres 13 or newer, then run:

```sh
FAUCET_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' go test -race ./...
```

Each test creates and drops its own database. The Postgres tests skip when the variable is unset and fail when `CI` is set. After you change a migration or query, run `go -C tools tool sqlc generate -f ../sqlc.yaml`. sqlc lives in its own module under `tools/`, so it never reaches the `go.mod` of a project that imports faucet.
