# faucet

Faucet lets agents wait for or subscribe to GitHub events, such as a CI run finishing or a PR approval, through MCP.

It is a Go library first, with a thin HTTP server on top, so coder/coder can embed it in coderd.

## Status

PR 5 of 8. Faucet receives GitHub webhooks, stores them as typed events, and lets an agent wait for them through the `wait_for_github_events` MCP tool, or poll them with the MCP Events extension (SEP-3415). It deletes old events and asks GitHub again for the deliveries it missed.

- `faucet`: `Event`, `Store`, `Normalize`, `Service.WebhookHandler`, `Service.Register` (the MCP tool and the `events/list` and `events/poll` methods), `ServerCapabilities` and `RequireEventName` (the Events extension), and `Principal`, `Authorizer` and `TokenAuthorizer` (the per-repo access check), and `Service.Purge`, `Service.Redeliver`, `HookAdmin` and `GitHubHookAdmin` (retention and redelivery).
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

`/mcp` also speaks the draft Events extension, SEP-3415, in poll mode. A client finds it in the `capabilities.extensions` of `server/discover`, under `io.modelcontextprotocol/events`. Webhook and push delivery are not offered, so every event type lists `delivery: ["poll"]`.

- `events/list` returns the five event types with their `inputSchema` and `payloadSchema`.
- `events/poll` takes `name` (an event type), `arguments` (`repo`, and optionally `sha` and `pr`) and `cursor`. It returns `events`, a `cursor` in every result, `hasMore`, `truncated` and `nextPollMs`. `truncated` is true when `maxAgeMs` skipped events or when a retention purge deleted events after the cursor. Each event has `eventId` (the GitHub delivery GUID), `name`, `timestamp` and `data`. Without a cursor the poll returns no events and the cursor of the present. `maxEvents` and `maxAgeMs` bound a replay.
- Every `events/poll` request over HTTP must carry the header `Mcp-Name` with the event name, or faucet answers 400. The SDK client does not set it for `events/*` yet.
- The access check is the one `wait_for_github_events` uses. A repository the token cannot read gives the same `Forbidden` error as one that does not exist.

To embed faucet in another server, build a `faucet.Service` with `faucet.New`, mount `Service.WebhookHandler()` on your own router, and call `Service.Register` on your `*mcp.Server`. Create that server with `mcp.ServerOptions{Capabilities: faucet.ServerCapabilities()}` so clients see the Events extension, and put `faucet.RequireEventName` in front of its HTTP handler. Provide your own `Authorizer` and `PrincipalFrom`, or use `TokenAuthorizer`.

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

**`GET /cron/purge`** deletes the events received more than `FAUCET_RETENTION_DAYS` ago and answers `{"deleted": n, "purged_through": id}`. `purged_through` is the purge mark, the highest event ID any purge has deleted. A wait or poll whose cursor is below the mark reports `truncated: true`.

**`GET /cron/redeliver`** lists the webhook's deliveries from the last two hours and asks GitHub to deliver again each one that failed (any answer outside 200 to 299, or none), that faucet has not stored, and that has no later successful attempt. It answers `{"examined", "pending", "gave_up", "requested", "incomplete"}`. The first request that GitHub refuses stops the run with a 502. A run has two bounds, and it says `incomplete: true` when one stopped it:

- It sends at most 50 requests, the deliveries with the fewest redeliveries first and then the oldest. The next run goes on with the rest, because a delivery that faucet stored is skipped.
- It reads at most 10 pages of 100 attempts, the newest first. Faucet keeps no state, so the next run reads the newest 1000 attempts again and does not go on where this one stopped, and `incomplete: true` says that the window held more. A request for a delivery needs its first attempt among the newest 1000, and the n-th request comes less than n schedules after that attempt. At a 30 minute schedule the third request is therefore safe while the hook records at most 1000 / (3 × 0.5 h) = 666 attempts an hour, redeliveries included, and the first request while it records at most 1000 / 0.5 h = 2000 an hour. A delivery that falls out of view is given up and logged. A shorter schedule raises both limits: at 10 minutes they are 2000 and 6000 an hour.

- Schedule the sweep every 30 minutes or more often. The window is two hours because faucet asks at most 3 times for a delivery, the third request comes up to 3 schedules after the first attempt, and the sweep after that must still see the first attempt. A slower schedule lets the first attempt leave the window before faucet has asked 3 times, so a delivery that a later request would have recovered is given up. GitHub keeps deliveries for three days, so a sweep cannot see older ones.
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
