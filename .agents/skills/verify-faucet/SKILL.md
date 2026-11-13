---
name: verify-faucet
description: Launch, check, drive, prove and bug bash the faucet server (cmd/faucet, GitHub webhook ingest into Postgres, the wait_for_github_events MCP tool, the events/list and events/poll MCP methods, and the /cron/purge and /cron/redeliver routes) over HTTP with e2e. Use before claiming a faucet server change works, to prove a PR against the running server, or to bug bash it.
---

# Verify faucet

Read the e2e skill first (`npx e2e guide`, run in `verify/`, prints it). This skill only says what is true of faucet. faucet serves `GET /healthz`, signed webhooks at `POST /webhook/github`, MCP at `POST /mcp` (the `wait_for_github_events` tool and the `events/list` and `events/poll` methods), and the scheduler routes `GET /cron/purge` and `GET /cron/redeliver`. Everything lives in `verify/`: config `verify/e2e.config.ts` (one `web()` target named `http`, tests `tests/**/*.e2e.ts`), the app command `verify/app.mjs`, and the tests. faucet has no UI, so the tests call the server with `fetch` and read Postgres with `psql`. They take no model.

## Launch

Nothing to start by hand. The target's `app.command` runs `node app.mjs --port {port}` on a free port. `app.mjs`:

1. finds the PostgreSQL server binaries: `PG_BIN`, else the `initdb` on `PATH`, else `/usr/lib/postgresql/*/bin`;
2. runs `initdb` into `$TMPDIR/faucet-verify-<port>/pgdata` and starts that Postgres on a Unix socket in the private (mode 0700) `$TMPDIR/faucet-verify-<port>` directory, with TCP off (database and user `postgres`, trust auth on the socket only);
3. runs `go build ./cmd/faucet` in the checkout under test;
4. starts a GitHub API stub on a free `127.0.0.1` port. It knows one random token. `GET /user` answers `verify-user`, `GET /repos/verify/r<id>` answers repository id `<id>`, and every other repository is 404. It also plays the deliveries API of the org webhook `verify/7` for a second random token, the hook token, and plays GitHub's redelivery: on `POST .../deliveries/<id>/attempts` it posts the stored payload, signed, to the cron faucet and records a new attempt. Tests drive it through `/__stub/`, which needs a third random token, the control token;
5. generates random secrets and writes them with the tokens, the socket directory, the number that names the socket file (`5432`), the `psql` path, the built binary's path and the cron faucet's port and database to `$TMPDIR/faucet-verify-<port>/state.json` (mode 0600);
6. starts the cron faucet on a free port. It has its own database `faucet_cron` (a purge moves the purge mark of its whole table), its own webhook secret, and the variables `CRON_SECRET`, `FAUCET_HOOK_TOKEN`, `FAUCET_HOOK_ORG=verify` and `FAUCET_HOOK_ID=7`;
7. starts the main faucet on the runner's port with only `DATABASE_URL`, `GITHUB_WEBHOOK_SECRET`, `PORT` and `FAUCET_GITHUB_API_URL` (the stub). It has no `CRON_SECRET`, so its cron routes answer 503.

The runner waits for `GET /healthz` of the main faucet to answer 200, and `app.mjs` waits for the cron faucet's first. On SIGTERM, or once its parent process is gone (a runner killed with SIGKILL), `app.mjs` stops both faucets and the stub, stops Postgres and deletes the directory within seconds. `--detach` turns the parent watch off. Every secret and token is random per run and reaches no log, report or output.

Needs on the host: Node `^22.22.3 || >=24.8.0` (the `engines` of `e2e` and `@e2e-dev/web`), Go as `go.mod` asks (`GOTOOLCHAIN` and other `GO*` variables pass through), PostgreSQL server binaries and `psql` (written against 18), and network on the first run (`npm ci`, then Playwright downloads Chromium into `~/.cache/ms-playwright`, because the `web()` engine opens a browser context even for API tests; CI uses `npx e2e-web install chromium --with-deps`). `E2E_TELEMETRY_DISABLED=1` turns off e2e's anonymous telemetry.

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

- The `faucet` fixture is the main faucet. The `cron` fixture is the cron faucet, a `Faucet` with the same methods, on its own database. The `hooks` fixture drives the stub (below).
- `faucet.deliver({ event, delivery, body, signature? })` posts to `/webhook/github`. It signs `body` with the run's secret unless `signature` is a string (sent as is) or `null` (header omitted).
- `faucet.waitForEvents(args, token?)` calls the `wait_for_github_events` tool on `/mcp` as a stateless 2026-07-28 client would, with the run's valid GitHub token unless `token` is a string (sent as is) or `null` (no header). It returns the HTTP reply plus `isError`, `text`, `events`, `cursor` and `truncated`.
- `faucet.mcp(method, params, opts)` sends one JSON-RPC request to `/mcp` as a stateless 2026-07-28 client would, for `server/discover`, `events/list` and `events/poll`. It uses the run's valid GitHub token unless `opts.token` is a string (sent as is) or `null` (no header). `opts.name` sets `Mcp-Name` (default `params.name`, `null` omits it) and `opts.id` sets the request id. It returns the HTTP reply plus `id`, `result` and `error`.
- `faucet.request(path, init)` is a plain `fetch` returning `{ status, body, headers }`.
- `faucet.sign(body, secret?)` returns `sha256=<hex>`.
- `faucet.rows(repoId)` returns the `faucet_events` rows of one repository through `psql`; `faucet.sql(query)` runs any query.
- `faucet.cronJob('purge' | 'redeliver', { method?, send? })` calls `/cron/<job>` with the run's cron secret as a bearer token. `send` is a function of the secret that returns what the request carries instead (`authorization`, `query`), so no test names the secret.
- `faucet.backdate(deliveryId, days)` makes a stored event look that many days old. `faucet.purgeMark()` reads `faucet_purge_mark`.
- `hooks.set(deliveries, { pageSize?, redeliveryStatus? })` replaces what the stub lists for the webhook and clears its request log. `hooks.requests()` returns the requests it got (`method`, `path`, `query`, and `auth`, which names the kind of credential: `hook`, `user`, `cron`, `other` or `none`). `stubDelivery(repoId, { id, guid, ageMs, status })` in `tests/payloads.ts` builds an entry that carries a payload.
- `faucet.withFaucet(env, fn)` starts one more faucet on a new database with a random `CRON_SECRET` and no hook settings, runs `fn(started, output)`, then stops it and drops the database. Use it for a test that needs another configuration.
- `faucet.leaksSecret(text)` says whether `text` holds a webhook secret, the GitHub token, the cron secret, the hook token or the stub's control token, so none leaves `support.ts`.
- `faucet.binary` is the path of the faucet executable the run built, for a test that starts its own process (`tests/startup.e2e.ts`) with an explicit environment.

The tests of one faucet share its server and database. Each test makes its own `newRepoId()` and `newDeliveryId()` and asserts on that repository's rows only, never on table-wide counts. The purge tests are the exception: they take their expected counts from Postgres. The wait tests name their repository `verify/r<newRepoId()>`, so the stub answers with that id and each test waits on events nobody else posted. Payload fixtures are in `tests/payloads.ts`. Run one test by `file:line`: `npx e2e run tests/ingest-ping.e2e.ts:4`.

Live, over `e2e mcp` (the e2e plugin's server, or `npx e2e mcp` started in `verify/`): `open_session {config: "<checkout>/verify/e2e.config.ts"}`, then `call {session, tool: "navigate", args: {url: "/healthz"}}` and `call {session, tool: "observe"}`, because `navigate` and `observe` are catalog tools reached through `call`. A browser can only GET, so the live lane shows `/healthz` and the 405 page of `GET /webhook/github`. Webhook POSTs and `/mcp` calls go through `run_tests {files: [...]}` or the CLI. To look at the database by hand during a session, connect through the socket (the app port is in the `open_session` output):

```sh
psql -h "${TMPDIR:-/tmp}/faucet-verify-<port>" -p 5432 -U postgres -c 'select id, type, repo_id, prs from faucet_events order by id desc limit 5'
```

Never print `state.json`: it holds every secret of the run.

## Evidence

Every run writes `.e2e/report.json`, a short `trace.md` per test under `.e2e/results/` (`trace: 'on'` in the config), and the app's own log at `.e2e/logs/app.log`. faucet logs one `webhook stored` line per accepted delivery (`event_id`, `inserted`), one `webhook rejected` line per refusal (`status`, `reason`), `events purged` per purge (`deleted`, `purged_through`, `before`), `redelivery requested` per request (`delivery_id`, `attempt_id`, `status`), `redelivery sweep done` per sweep (`examined`, `pending`, `requested`, `incomplete`) and one `wait denied` or `poll denied` line per `wait_for_github_events` or `events/poll` call for a repository the token cannot see (`repo` and `login`, and `event` on a poll). The next run clears `.e2e/results/`, and the app log is appended to across runs. A proof that must outlive them starts a fresh log, runs with its own output directory and keeps the log beside it:

```sh
rm -f .e2e/logs/app.log
npx e2e run --output .e2e/proof/<name>; status=$?
mkdir -p .e2e/proof/<name>; cp .e2e/logs/app.log .e2e/proof/<name>/app.log
(exit $status)
```

The copy runs whatever the exit code is, and the last line returns the test's own exit code, so a failed proof keeps its app log.

`run.summary` in the proof's `report.json` holds the counts (`selected`, `passed`, `failed`).

API tests record no screens, so the trace pages only say pass or fail. The proof of a side effect is the test's own assertion on the `psql` rows, and the report says which ones passed.

Proof standards: drive the real HTTP route with a really signed delivery, or the real `/mcp` route with the token the stub knows. Check the response status and body, and the rows in Postgres. A rejected delivery proves its row count is still zero for that delivery's repository. Mock nothing in faucet: the secret, the database and the server are the real ones, private to the run. The GitHub API is the one thing stubbed, because faucet calls it with the caller's token.

## Bug bash

Unproven. No model was configured when this skill was written, so no charter has run and the setup below is a design, not a result.

The charters are in [features/](features/), one fan-out line `slug|target|agent|charter` each (target `http`). The cron features have none. They need a bug-bash config that is not committed, `verify/e2e.bugbash.config.ts`, that spreads `e2e.config.ts` and adds:

- a model, the personas `skeptic` and `fuzzer`, and a `context` saying what is not a bug here: a 202 for an event faucet ignores, the `404 page not found` on `GET /`, text/plain replies with a trailing newline, and that the secret and database are throwaway;
- agent tools `deliver` and `count_rows` that wrap `Faucet.main(...)` from `tests/support.ts`, because the explorer drives a browser and a browser cannot POST a signed webhook;
- a target with `app.url` and no `app.command`. `app.mjs` brings up its own Postgres, so the first explorer to finish would remove it under the others. Start it once, `node app.mjs --port <free port> --detach &` (without `--detach` it stops as soon as the shell that started it exits), save the PID, and stop it with `kill <that PID>` when the bash ends.

`.e2e/` is gitignored, so `.e2e/bugbash/<slug>` is too. A finding is a bug once `tests/bugbash/<slug>.e2e.ts` fails on the reported defect (topic `bug-bash`, step 6). Anything else is the explorer's claim.

## Cleanup

The runner stops the app it started, and `close_session` stops the one a session started. Both run `app.mjs`'s SIGTERM path, which stops faucet and Postgres and deletes `$TMPDIR/faucet-verify-<port>`. If the runner is killed with SIGKILL, `app.mjs` notices within seconds and runs the same teardown (`tests/lifecycle.e2e.ts` pins it, also for a kill while initdb runs). Never `pkill` by name.

Wait ten seconds after a kill, then run `ls -d "${TMPDIR:-/tmp}"/faucet-verify-*`. A directory you know is yours (the port is in its name) that is still listed, `<dir>` below, needs these steps in order, with `ps -eo pid,args | grep -F '<text>' | grep -v grep` to find a pid:

1. Find `node app.mjs --port <port>` and send that pid SIGTERM (`kill <pid>`). It tears the stack down itself. A stack started with `--detach` outlives its launcher by design, so this is also how it stops.
2. Only when no such process is left: find `<dir>/faucet` (two processes, the main and the cron faucet) and `kill` each pid, run `pg_ctl -D <dir>/pgdata stop -m fast` (from the PostgreSQL bin directory), then delete that one directory.

Keep `.e2e/report.json`, `.e2e/results/`, `.e2e/proof/` and `.e2e/logs/app.log`. That is the proof, and all of it is gitignored.

## Helpers

`verify/app.mjs` starts and stops the stack. The runner calls it through the config. Run it by hand only for the bug bash above: `node app.mjs --port <port> --detach`.
