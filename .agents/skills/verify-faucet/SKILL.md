---
name: verify-faucet
description: Launch, check, drive, prove and bug bash the faucet server (cmd/faucet, GitHub webhook ingest into Postgres) over HTTP with e2e. Use before claiming a faucet server change works, to prove a PR against the running server, or to bug bash it.
---

# Verify faucet

Read the e2e skill first (`npx e2e guide`, run in `verify/`, prints it). This skill only says what is true of faucet. Everything lives in `verify/`: config `verify/e2e.config.ts` (one `web()` target named `http`, tests `tests/**/*.e2e.ts`), the app command `verify/app.mjs`, and the tests. faucet has no UI, so the tests call the server with `fetch` and read Postgres with `psql`. They take no model.

## Launch

Nothing to start by hand. The target's `app.command` runs `node app.mjs --port {port}` on a free port. `app.mjs`:

1. finds the PostgreSQL server binaries: `PG_BIN`, else the `initdb` on `PATH`, else `/usr/lib/postgresql/*/bin`;
2. runs `initdb` into `$TMPDIR/faucet-verify-<port>/pgdata` and starts that Postgres on a Unix socket in the private (mode 0700) `$TMPDIR/faucet-verify-<port>` directory, with TCP off (database and user `postgres`, trust auth on the socket only);
3. runs `go build ./cmd/faucet` in the checkout under test;
4. generates a webhook secret, writes it with the socket directory, the number that names the socket file (`5432`), the `psql` path and the built binary's path to `$TMPDIR/faucet-verify-<port>/state.json` (mode 0600), and starts faucet with only `DATABASE_URL`, `GITHUB_WEBHOOK_SECRET` and `PORT`.

The runner waits for `GET /healthz` to answer 200. On SIGTERM `app.mjs` stops faucet, stops Postgres and deletes the directory. The secret is random per run and reaches no log, report or output.

Needs on the host: Node 22.22.3 or newer (or 24.8+), Go as `go.mod` asks (`GOTOOLCHAIN` and other `GO*` variables pass through), PostgreSQL server binaries and `psql` (written against 18), and network on the first run (`npm ci`, then Playwright downloads Chromium into `~/.cache/ms-playwright`, because the `web()` engine opens a browser context even for API tests; CI uses `npx @e2e-dev/web install chromium --with-deps`). `E2E_TELEMETRY_DISABLED=1` turns off e2e's anonymous telemetry.

Against a checkout that has no `verify/` (for example another PR's head), run the skill from a checkout that does and point it at the code under test:

```sh
cd <checkout with verify/>/verify && npm ci && FAUCET_ROOT=<checkout under test> npx e2e run
```

For the checkout that holds `verify/`, drop `FAUCET_ROOT`. `FAUCET_ROOT` must hold `cmd/faucet/main.go`, else the run stops at once.

## Doctor

```sh
cd verify
npx e2e list tests/smoke.e2e.ts    # loads the config, starts nothing; exit 0, two tests
npx e2e run tests/smoke.e2e.ts     # builds and starts faucet and Postgres; exit 0
```

`list` failing is the config or `npm ci` not run. `run` failing with `APP_UNREACHABLE` means the app never became ready: read `.e2e/logs/app.log`. It names a failed `go build`, `no PostgreSQL server binaries found` (install Postgres or set `PG_BIN`), or `<dir> exists, so an earlier run was killed` (see Cleanup). A failing assertion on the migrated table means faucet started but did not migrate. Run Doctor first whenever anything looks off.

## Drive

Scripted: every feature in [features/](features/README.md) has a test under `verify/tests/`. They import `test` from `tests/support.ts`, which adds a `faucet` fixture:

- `faucet.deliver({ event, delivery, body, signature? })` posts to `/webhook/github`. It signs `body` with the run's secret unless `signature` is a string (sent as is) or `null` (header omitted).
- `faucet.request(path, init)` is a plain `fetch` returning `{ status, body, headers }`.
- `faucet.sign(body, secret?)` returns `sha256=<hex>`.
- `faucet.rows(repoId)` returns the `faucet_events` rows of one repository through `psql`; `faucet.sql(query)` runs any query.
- `faucet.leaksSecret(text)` says whether `text` holds the secret, so the secret itself never leaves `support.ts`.

All tests share one server and one database. Each test makes its own `newRepoId()` and `newDeliveryId()` and asserts on that repository's rows only, never on table-wide counts. Payload fixtures are in `tests/payloads.ts`. Run one test by `file:line`: `npx e2e run tests/ingest-ping.e2e.ts:4`.

Live, over `e2e mcp` (the e2e plugin's server, or `npx e2e mcp` started in `verify/`): `open_session {config: "<checkout>/verify/e2e.config.ts"}`, then `call {session, tool: "navigate", args: {url: "/healthz"}}` and `observe`. A browser can only GET, so the live lane shows `/healthz` and the 405 page of `GET /webhook/github`. Webhook POSTs go through `run_tests {files: [...]}` or the CLI. To look at the database by hand during a session, connect through the socket (the app port is in the `open_session` output):

```sh
psql -h "${TMPDIR:-/tmp}/faucet-verify-<port>" -p 5432 -U postgres -c 'select id, type, repo_id, prs from faucet_events order by id desc limit 5'
```

Never print `state.json`: it holds the secret.

## Evidence

Every run writes `.e2e/report.json`, a short `trace.md` per test under `.e2e/results/` (`trace: 'on'` in the config), and the app's own log at `.e2e/logs/app.log`. faucet logs one `webhook stored` line per accepted delivery (`event_id`, `inserted`) and one `webhook rejected` line per refusal (`status`, `reason`). The next run clears `.e2e/results/`, and the app log is appended to across runs. A proof that must outlive them starts a fresh log, runs with its own output directory and keeps the log beside it:

```sh
rm -f .e2e/logs/app.log && npx e2e run --output .e2e/proof/<name> && cp .e2e/logs/app.log .e2e/proof/<name>/app.log
```

`run.summary` in the proof's `report.json` holds the counts (`selected`, `passed`, `failed`).

API tests record no screens, so the trace pages only say pass or fail. The proof of a side effect is the test's own assertion on the `psql` rows, and the report says which ones passed.

Proof standards: drive the real HTTP route with a really signed delivery. Check the response status and body, and the rows in Postgres. A rejected delivery proves its row count is still zero for that delivery's repository. Mock nothing: the secret, the database and the server are the real ones, private to the run.

## Bug bash

Unproven. No model was configured when this skill was written, so no charter has run and the setup below is a design, not a result.

The charters are in [features/](features/), one fan-out line `slug|target|agent|charter` each (target `http`). They need a bug-bash config that is not committed, `verify/e2e.bugbash.config.ts`, that spreads `e2e.config.ts` and adds:

- a model, the personas `skeptic` and `fuzzer`, and a `context` saying what is not a bug here: a 202 for an event faucet ignores, the `404 page not found` on `GET /`, text/plain replies with a trailing newline, and that the secret and database are throwaway;
- agent tools `deliver` and `count_rows` that wrap `Faucet` from `tests/support.ts`, because the explorer drives a browser and a browser cannot POST a signed webhook;
- a target with `app.url` and no `app.command`. `app.mjs` brings up its own Postgres, so the first explorer to finish would remove it under the others. Start it once, `node app.mjs --port <free port> &`, save the PID, and stop it with `kill <that PID>` when the bash ends.

`.e2e/` is gitignored, so `.e2e/bugbash/<slug>` is too. A finding is a bug once `tests/bugbash/<slug>.e2e.ts` fails on the reported defect (topic `bug-bash`, step 6). Anything else is the explorer's claim.

## Cleanup

The runner stops the app it started, and `close_session` stops the one a session started. Both run `app.mjs`'s SIGTERM path, which stops faucet and Postgres and deletes `$TMPDIR/faucet-verify-<port>`. Never `pkill` by name.

A run killed with SIGKILL leaves that directory and its Postgres. List `ls -d "${TMPDIR:-/tmp}"/faucet-verify-*`, pick one you know is yours, run `pg_ctl -D <dir>/pgdata stop -m fast` (from the PostgreSQL bin directory), then delete that one directory.

Keep `.e2e/report.json`, `.e2e/results/`, `.e2e/proof/` and `.e2e/logs/app.log`. That is the proof, and all of it is gitignored.

## Helpers

`verify/app.mjs` starts and stops the stack. The runner calls it through the config. Run it by hand only for the bug bash above: `node app.mjs --port <port>`.
