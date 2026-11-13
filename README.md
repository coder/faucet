# faucet

Faucet lets agents wait for or subscribe to GitHub events, such as a CI run finishing or a PR approval, through MCP.

It is a Go library first, with a thin HTTP server on top, so coder/coder can embed it in coderd.

## Status

PR 3 of 8. Faucet receives GitHub webhooks, stores them as typed events, and lets an agent wait for them through the `wait_for_github_events` MCP tool.

- `faucet`: `Event`, `Store`, `Normalize`, `Service.WebhookHandler`, `Service.Register` (the MCP tool), and `Principal`, `Authorizer` and `TokenAuthorizer` (the per-repo access check).
- `faucet/pgstore`: the Postgres store, with embedded migrations and sqlc queries.
- `faucet/storetest`: the contract tests every `Store` must pass.
- `cmd/faucet`: the server. It serves `POST /webhook/github`, `/mcp` and `GET /healthz`.

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

It returns `events` and a `cursor` as soon as a matching event exists, or no events and a cursor when `timeout_s` runs out. Pass that cursor to the next call to continue without a gap or a repeat. Without a cursor the wait starts at the present, or ten minutes back when `sha` is set.

To embed faucet in another server, build a `faucet.Service` with `faucet.New`, mount `Service.WebhookHandler()` on your own router, and call `Service.Register` on your `*mcp.Server`. Provide your own `Authorizer` and `PrincipalFrom`, or use `TokenAuthorizer`.

## Tests

Start a local Postgres 13 or newer, then run:

```sh
FAUCET_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' go test -race ./...
```

Each test creates and drops its own database. The Postgres tests skip when the variable is unset and fail when `CI` is set. After you change a migration or query, run `go -C tools tool sqlc generate -f ../sqlc.yaml`. sqlc lives in its own module under `tools/`, so it never reaches the `go.mod` of a project that imports faucet.
