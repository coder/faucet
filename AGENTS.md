# Notes for agents

Faucet is a Go library with a thin server (`cmd/faucet`). Read `README.md` first.

- Run the tests with a local Postgres 13 or newer: `FAUCET_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' go test -race ./...`. They fail in CI when the variable is unset.
- After you change a migration or a query, run `go -C tools tool sqlc generate -f ../sqlc.yaml` and commit the result.
- The end-to-end checks live in `verify/` and are described by the `verify-faucet` skill in `.agents/skills/`.
- Redelivery selection keeps persisted state; no stateless sliding-window sweep (see #10).
- Redelivery never derives a read position or bound from faucet's clock: the two clocks differ, and a position ahead of GitHub's drops the next failure unseen. It uses times that GitHub gave (attempt times, the `Date` header). A run also keeps to its context deadline by construction: the scan has its own time budget, so the asks and the purge always run.
