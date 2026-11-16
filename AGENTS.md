# Notes for agents

Faucet is a Go library with a thin server (`cmd/faucet`). Read `README.md` first.

- Run the tests with a local Postgres 13 or newer: `FAUCET_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' go test -race ./...`. They fail in CI when the variable is unset.
- After you change a migration or a query, run `go -C tools tool sqlc generate -f ../sqlc.yaml` and commit the result.
- The end-to-end checks live in `verify/` and are described by the `verify-faucet` skill in `.agents/skills/`.
- Redelivery selection keeps persisted state; no stateless sliding-window sweep (see #10).
- Redelivery never derives a read position or bound from faucet's clock: the two clocks differ, and a position ahead of GitHub's drops the next failure unseen. It uses times that GitHub gave (attempt times, the `Date` header). A run also keeps to its context deadline by construction: the scan has its own time budget, so the asks and the purge always run.
- The scan takes no read bound and no gap from faucet's clock (see #26). That clock is only the time of record that a step gives the store (`ScanStep.Now`). A read bound comes from an attempt time, and a `Date` can only lower it (the first run after an empty list or the migration); the types in `faucet/redelivertime.go` enforce it. A gap ends at a page `Date` that passed the check (not more than S = 1h behind the newest attempt known), and a gap after a silence starts at the stored `Date` less M = 6h. M and S are the trust window (see README, redelivery limits); a change to them needs a design review first.
- No selection shortcut that skips, suppresses or merges an overlap read or an open range (see #22).
