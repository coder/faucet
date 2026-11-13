# faucet

Faucet lets agents wait for or subscribe to GitHub events, such as a CI run finishing or a PR approval, through MCP.

It is a Go library first, with a thin HTTP server on top, so coder/coder can embed it in coderd.

## Status

PR 1 of 8. Only the event store exists. There is no webhook handler, MCP tool, or server binary yet.

- `faucet`: the `Event` type and the `Store` interface.
- `faucet/pgstore`: the Postgres store, with embedded migrations and sqlc queries.
- `faucet/storetest`: the contract tests every `Store` must pass.

## Tests

Start a local Postgres 13 or newer, then run:

```sh
FAUCET_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' go test -race ./...
```

Each test creates and drops its own database. The Postgres tests skip when the variable is unset and fail when `CI` is set. After you change a migration or query, run `go tool sqlc generate`.
