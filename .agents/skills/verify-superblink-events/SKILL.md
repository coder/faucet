---
name: verify-superblink-events
description: Launch, check, drive, prove and bug bash the Superblink Events server (cmd/faucet, GitHub webhook ingest into Postgres, the wait_for_github_events MCP tool, the events/list, events/poll, events/subscribe and events/unsubscribe MCP methods, the /cron/purge, /cron/deliver and /cron/redeliver routes, with the signed webhooks that /cron/deliver sends) over HTTP with e2e. Use before claiming a Superblink Events server change works, to prove a PR against the running server, or to bug bash it.
---

# Verify Superblink Events

Read the e2e skill first (`npx e2e guide`, run in `verify/`, prints it). This skill only says what is true of Superblink Events. Superblink Events serves `GET /healthz`, signed webhooks at `POST /webhook/github`, MCP at `POST /github/mcp` (the `wait_for_github_events` tool and the `events/list`, `events/poll`, `events/subscribe` and `events/unsubscribe` methods), and the scheduler routes `GET /cron/purge`, `GET /cron/deliver` and `GET /cron/redeliver`. Everything lives in `verify/`: config `verify/e2e.config.ts` (one `web()` target named `http`, tests `tests/**/*.e2e.ts`), the app command `verify/app.mjs`, and the tests. Superblink Events has no UI, so the tests call the server with `fetch` and read Postgres with `psql`. They take no model.

## Launch

Nothing to start by hand. The target's `app.command` runs `node app.mjs --port {port}` on a free port. `app.mjs`:

1. finds the PostgreSQL server binaries: `PG_BIN`, else the `initdb` on `PATH`, else `/usr/lib/postgresql/*/bin`;
2. runs `initdb` into `$TMPDIR/faucet-verify-<port>/pgdata` and starts that Postgres on a Unix socket in the private (mode 0700) `$TMPDIR/faucet-verify-<port>` directory, with TCP off (database and user `postgres`, trust auth on the socket only);
3. runs `go build ./cmd/faucet` in the checkout under test, and a second build with `-tags faucet_e2e`, whose callback client also reaches loopback over `http`. Only a Superblink Events server that a test starts with `{ e2eBuild: true }` or `{ browserLogin: true }` runs it. It also builds the agents of the tests, `cmd/faucet/internal/serviceagent` and `cmd/faucet/internal/loginagent`, and the login agent writes a self-signed certificate for 127.0.0.1 and localhost into `<dir>/tls`;
4. starts a GitHub API stub on a free `127.0.0.1` port. It plays GitHub for the browser login: Superblink Events' GitHub App, with a random client ID and secret, and one person, `verify-person` (see [Browser login](features/browser-login.md)). For the person's access tokens alone, `GET /user` answers `verify-person`, `GET /repos/verify/r<id>` answers repository id `<id>`, and every other repository is 404. Any other credential is 401. It also serves the deliveries API of the org webhook `verify/7` to a random token, the hook token (see [Redelivery](features/redeliver.md)), and tests drive it through `/__stub/`, which needs a control token of its own. A callback receiver on another free `127.0.0.1` port plays the endpoint of a subscriber (see [Webhook handshake](features/webhook-handshake.md)), and tests drive it through `/__receiver/`, which needs its own random control token;
5. generates random secrets and the webhook key (32 random bytes in standard base64), and writes them with the socket directory, the number that names the socket file (`5432`), the `psql` path, the built binaries' paths, the receiver's URL and control token, and the cron Superblink Events server's port and database to `$TMPDIR/faucet-verify-<port>/state.json` (mode 0600);
6. starts the cron Superblink Events server on a free port. It has its own database `faucet_cron` (a purge moves the purge mark of its whole table), its own webhook secret, `CRON_SECRET` and its own URL as `FAUCET_PUBLIC_URL`;
7. starts the main Superblink Events server on the runner's port with only `DATABASE_URL`, `GITHUB_WEBHOOK_SECRET`, `PORT`, `FAUCET_PUBLIC_URL` (its own URL, `http://127.0.0.1:<port>`), `FAUCET_GITHUB_API_URL` (the stub), `FAUCET_RAW_EVENTS`, `FAUCET_WEBHOOK_KEY` (the webhook key) and `FAUCET_WEBHOOK_ALLOWED_ORIGINS=https://hooks.verify.test`, so webhook mode is on. It has no `CRON_SECRET`, so its cron route answers 503. No server answers at `hooks.verify.test`: the allowlist makes no request to a callback URL.

The runner waits for `GET /healthz` of the main Superblink Events server to answer 200, and `app.mjs` waits for the cron Superblink Events server's first. `app.mjs` is a watchdog. It starts one process that runs the stack (both Superblink Events servers, the stub, Postgres) in a process group of its own, and the config hands it the runner's PID in `FAUCET_VERIFY_PARENT`. On SIGTERM, or once the runner is gone (killed with SIGKILL, also before `app.mjs` started), the stack stops both Superblink Events servers and the stub, stops Postgres and deletes the directory within seconds. A SIGKILL of `app.mjs` itself, or of the stack process, ends the stack too, because each of the two ends the other's group. `--detach` turns the parent watch off. Every secret, token and key is random per run and reaches no log, report or output.

Needs on the host: Node `^22.22.3 || >=24.8.0` (the `engines` of `e2e` and `@e2e-dev/web`), Go as `go.mod` asks (`GOROOT`, `GOPATH`, `GOCACHE`, `GOMODCACHE`, `GOFLAGS` and `GOTOOLCHAIN` pass through, and no other variable of your host does: keep a setting such as `GOPROXY` in `go env -w`), PostgreSQL server binaries and `psql` (written against 18), and network on the first run (`npm ci`, then Playwright downloads Chromium into `~/.cache/ms-playwright`, because the `web()` engine opens a browser context even for API tests; CI uses `npx e2e-web install chromium --with-deps`). `E2E_TELEMETRY_DISABLED=1` turns off e2e's anonymous telemetry.

Against a checkout that has no `verify/` (for example another PR's head), run the skill from a checkout that does and point it at the code under test:

```sh
cd <checkout with verify/>/verify && npm ci && FAUCET_ROOT=<checkout under test> npx e2e run
```

For the checkout that holds `verify/`, drop `FAUCET_ROOT`. `FAUCET_ROOT` must hold `cmd/faucet/main.go`, else the run stops at once.

## Doctor

```sh
cd verify
npx e2e list tests/smoke.e2e.ts    # loads the config, starts nothing; exit 0, two tests
npx e2e run tests/smoke.e2e.ts     # builds and starts Superblink Events and Postgres; exit 0
```

`list` failing is the config or `npm ci` not run. `run` failing with `APP_UNREACHABLE` means the app never became ready: read `.e2e/logs/app.log`. It names a failed `go build`, `no PostgreSQL server binaries found` (install Postgres or set `PG_BIN`), or `<dir> exists, so an earlier run was killed` (see Cleanup). A failing assertion on the migrated table means Superblink Events started but did not migrate. Run Doctor first whenever anything looks off.

## Drive

Scripted: every feature in [features/](features/README.md) has a test under `verify/tests/`. They import `test` from `tests/support.ts`, which adds a `faucet` fixture:

- The `faucet` fixture is the main Superblink Events server. The `cron` fixture is the cron Superblink Events server, a `Faucet` with the same methods, on its own database.
- `faucet.deliver({ event, delivery, body, signature? })` posts to `/webhook/github`. It signs `body` with the run's secret unless `signature` is a string (sent as is) or `null` (header omitted).
- Every MCP call goes as a principal, with a token that Superblink Events minted. A `Faucet` calls as `service` unless `faucet.as(principal)` returned it: `faucet.as('other')` calls as a second service and `started.as('person')` as the person of the browser login. Each service is a client that the harness makes on that Superblink Events server with `faucet clients create` the first time a test needs it, with the grant `all` and the scopes `events:read` and `events:subscribe`, and logs in with the client credentials grant. Its client ID is `verify-` (or `verify-other-`) and a random part per test process, so a process that the runner starts again after a failed test makes new clients. `faucet.login` is the login that Superblink Events records for the principal, `svc:<client ID>` for a service. The person logs in through the browser login, as a browser and a public client do (consent page, Continue, the stub's GitHub, Superblink Events' callback, the code with PKCE), only on a Superblink Events server started with `{ browserLogin: true }`, and the stub decides what it reads, so `receiver.revoke` takes its access away. The harness keeps one token per Superblink Events, principal and path, and mints a new one when less than a minute of its 15 minutes is left. A service has tokens for `/github/mcp` alone, because `/mcp` takes no new client, so a test of `/mcp` calls as the person.
- A service reads only a repository that a delivery named. Before a call of a service whose `repo` is `verify/r<id>`, the harness sends that Superblink Events server one signed `ping` that names the repository with its owner `verify` (ID 1), as GitHub's first delivery from a repository does. It does that once per Superblink Events and repository, and never for an explicit token.
- `faucet.waitForEvents(args, token?)` calls the `wait_for_github_events` tool on `/github/mcp` as a stateless 2026-07-28 client would, as the principal unless `token` is a string (sent as is) or `null` (no header). It returns the HTTP reply plus `isError`, `text`, `events`, `cursor` and `truncated`.
- `faucet.mcp(method, params, opts)` sends one JSON-RPC request to `/github/mcp` as a stateless 2026-07-28 client would, for `server/discover`, `events/list`, `events/poll`, `events/subscribe` and `events/unsubscribe`. It goes as the principal unless `opts.token` is a string (sent as is) or `null` (no header). `opts.name` sets `Mcp-Name` (default `params.name`, `null` omits it), `opts.id` sets the request id and `opts.path: '/mcp'` sends it to the alias instead. It returns the HTTP reply plus `id`, `result` and `error`.
- `faucet.challenge(path?)` is the `WWW-Authenticate` header of a 401 on the MCP endpoint at `path` (default `/github/mcp`): `Bearer` with the `resource_metadata` of that path and `scope="events:read events:subscribe"`. Superblink Events takes only its own tokens (`fct_`, `fcr_`, `fcs_`) and answers any other bearer, a GitHub token too, with it.
- `faucet.request(path, init)` is a plain `fetch` returning `{ status, body, headers }`.
- `faucet.sign(body, secret?)` returns `sha256=<hex>`.
- `faucet.rows(repoId)` returns the `faucet_events` rows of one repository through `psql`, each with its place on its repository's line (`repo_seq`) and its owner's (`owner_seq`), which a cursor and a waited event's `id` count; `faucet.sql(query)` runs any query.
- `faucet.cronJob('purge' | 'deliver', { method?, send? })` calls `/cron/<job>` with the run's cron secret as a bearer token. `send` is a function of the secret that returns what the request carries instead (`authorization`, `query`), so no test names the secret.
- `faucet.backdate(deliveryId, days)` makes a stored event look that many days old. `faucet.purgeMark()` reads the global `faucet_purge_mark`, and `faucet.repoMark(repoId)` the purge mark of one repository's line.
- `faucet.subscriptions(marker)` returns the `faucet_subscriptions` rows whose callback URL holds `marker` (times in epoch milliseconds, `key_hash` and `secret_enc` in hex). `faucet.secretCopies(secret)` counts the rows that hold a subscriber secret in any column, as text, without its prefix or as decoded bytes. `faucet.expireSubscriptions(marker)` moves the expiry of those rows into the past. `faucet.openSecret(row, key?)` opens a sealed secret in Node with `aes-256-gcm`, the run's key (or `key`) and the row's `key_hash`, and throws when either is wrong. `faucet.openPreviousSecret(row)` opens `previous_secret_enc` the same way, and `faucet.endRotationWindows(marker)` moves the end of those rows' rotation window into the past.
- `tests/subscriptions.ts` holds what the subscribe tests share: `newMarker()` (the part of a test's callback URLs that no other test uses), `callbackUrl(marker, suffix?)` (a URL on the allowlisted origin with the marker in its path and a token in its query), `newSecret(bytes?)`, `subscribeParams(repoId, marker, over?)`, `subscribe`, `subscribed` (must succeed), `unsubscribe` and the log patterns `subscribedLine` and `unsubscribedLine`.
- The `receiver` fixture drives the callback receiver: `receiver.register(id, secret)` gives it the secret that it checks signatures with, `receiver.callback(behavior, id, { port?, query? })` builds a callback URL, `receiver.requests(id)` returns what it got, with its verdict on the signature, and `receiver.revoke(repoId)` makes the GitHub stub answer 404 for that repository from then on, which is what the person sees after losing access. The behaviors for the handshake are listed in [Webhook handshake](features/webhook-handshake.md) and the ones for events in [Webhook delivery](features/delivery.md).
- `faucet.withFaucet(env, fn, { e2eBuild?, browserLogin? })` starts one more Superblink Events server on a new database with a random `CRON_SECRET`, its own URL as `FAUCET_PUBLIC_URL` and webhook mode off, runs `fn(started, output)`, then stops it and drops the database. `FAUCET_PUBLIC_URL: ''` in `env` leaves it unset, so the Superblink Events server starts only when `env` also holds the Vercel production variables, and then takes `https://` and `VERCEL_PROJECT_PRODUCTION_URL`; without them it exits at start. Use it for a test that needs another configuration. `env` can hold `FAUCET_WEBHOOK_KEY` and `FAUCET_WEBHOOK_ALLOWED_ORIGINS`, and `started.openSecret` then uses that key.
- `faucet.deliveries()` returns the `faucet_deliveries` rows (the deliveries that have not ended, times in epoch milliseconds). `faucet.makeDeliveriesDue()` moves their next attempt into the past and ends their leases, and `faucet.leaseDeliveries(seconds, attempts)` plays a run that claimed them and died. `tests/delivery.ts` holds what the delivery tests share, listed in [Webhook delivery](features/delivery.md).
- `tests/services.ts` holds what the service-account tests share: `createClient(started, id, path, grants, scope?)` runs `faucet clients create` and returns the secret, `accessToken(started, id, secret)` logs in at `POST /oauth/token`, and `deliverRun(started, repoId, { runId?, owner? })` delivers a workflow_run whose repository has its full name and its owner (`verify`, ID 1, unless `owner` says otherwise). Their Superblink Events server starts with `withFaucet(env, fn)`, or with `withDeliveryFaucet(faucet, fn)` to subscribe through the receiver. `withDeliveryFaucet(faucet, fn, { browserLogin: true })` also turns on the browser login, for a test that needs the person. See [Owner targets](features/owner-targets.md).
- `faucet.hookEnv()` is the environment that turns on redelivery for the stub's webhook (pass it to `withFaucet`), and the `hooks` fixture drives the stub: `hooks.arm(faucet, deliveries, opts)`, `hooks.add`, `hooks.requests()` and `hooks.attempts()`. A redelivery test runs on its own Superblink Events server, because the position of a run is state. `faucet.cronJob('redeliver')` calls the route.
- `faucet.leaksSecret(text)` says whether `text` holds a webhook secret, the hook token, the cron secret, the receiver's and the stub's control tokens, the webhook key, the GitHub App's secret, the started Superblink Events' token key, or a client secret or token that the harness minted, so none leaves `support.ts`.
- The browser login's helpers (`loginAgent`, `appLog`, `connectionToken` and the `documents` and `site` fixtures) are listed in [Browser login](features/browser-login.md).
- `faucet.binary` is the path of the Superblink Events executable the run built, for a test that starts its own process (`tests/startup.e2e.ts`) with an explicit environment.

The tests of one Superblink Events server share its server and database. Each test makes its own `newRepoId()` and `newDeliveryId()` and asserts on that repository's rows only, never on table-wide counts. The purge tests are the exception: they take their expected counts from Postgres. A subscribe test finds its rows by the marker in its own callback URLs, and the 100 live subscriptions that one principal may hold are shared by every test of the main Superblink Events server, so the limit test runs on its own Superblink Events server. The wait tests name their repository `verify/r<newRepoId()>`, so the ping of the harness (or, for the person, the stub) gives Superblink Events that id and each test waits on events nobody else posted. Payload fixtures are in `tests/payloads.ts`. Run one test by `file:line`: `npx e2e run tests/ingest-ping.e2e.ts:4`.

Live, over `e2e mcp` (the e2e plugin's server, or `npx e2e mcp` started in `verify/`): `open_session {config: "<checkout>/verify/e2e.config.ts"}`, then `call {session, tool: "navigate", args: {url: "/healthz"}}` and `call {session, tool: "observe"}`, because `navigate` and `observe` are catalog tools reached through `call`. A browser can only GET, so the live lane shows `/healthz` and the 405 page of `GET /webhook/github`. Webhook POSTs and `/github/mcp` calls go through `run_tests {files: [...]}` or the CLI. To look at the database by hand during a session, connect through the socket (the app port is in the `open_session` output):

```sh
psql -h "${TMPDIR:-/tmp}/faucet-verify-<port>" -p 5432 -U postgres -c 'select id, type, repo_id, prs from faucet_events order by id desc limit 5'
```

Never print `state.json`: it holds every secret of the run.

## Evidence

Every run writes `.e2e/report.json`, a short `trace.md` per test under `.e2e/results/` (`trace: 'on'` in the config), and the app's own log at `.e2e/logs/app.log`. Superblink Events logs one `webhook stored` line per accepted delivery (`event_id`, `inserted`), one `webhook rejected` line per refusal (`status`, `reason`), `events purged` per purge (`deleted`, `purged_through`, `before`), `redelivery requested`, `redelivery recovered`, `redelivery gave up` and `redelivery gap` per outcome of a redelivery run (`delivery_id`, `reason`, `cause`, `from`, `to`) and `redelivery run done` per run and one `wait denied`, `poll denied` or `subscribe denied` line per `wait_for_github_events`, `events/poll` or `events/subscribe` call for a repository the principal cannot read (`repo` and `login`, and `event` on a poll or subscribe). A purge also logs `expired_subscriptions_deleted` on its `events purged` line. A subscribe logs `subscribed` (`repo`, `event`, `login`, `subscription`, `expires_at`), an unsubscribe logs `unsubscribed` (`repo`, `event`, `login`, `subscription`), and a callback origin that is not on the allowlist logs `callback not verified` (`repo`, `event`, `login`, `reason`). None of these lines holds a subscriber secret, the webhook key, or any part of the callback URL: the tests count 0 copies of the secret and of the URL's path and query. The next run clears `.e2e/results/`, and the app log is appended to across runs. A proof that must outlive them starts a fresh log, runs with its own output directory and keeps the log beside it:

```sh
rm -f .e2e/logs/app.log
npx e2e run --output .e2e/proof/<name>; status=$?
mkdir -p .e2e/proof/<name>; cp .e2e/logs/app.log .e2e/proof/<name>/app.log
(exit $status)
```

The copy runs whatever the exit code is, and the last line returns the test's own exit code, so a failed proof keeps its app log.

`run.summary` in the proof's `report.json` holds the counts (`selected`, `passed`, `failed`).

API tests record no screens, so the trace pages only say pass or fail. The proof of a side effect is the test's own assertion on the `psql` rows, and the report says which ones passed.

Proof standards: drive the real HTTP route with a really signed delivery, or the real `/github/mcp` route with a token that Superblink Events minted. Check the response status and body, and the rows in Postgres. A stored secret is proved by opening the sealed bytes independently with the key from the run state. A rejected delivery proves its row count is still zero for that delivery's repository. Mock nothing in Superblink Events: the secret, the database and the server are the real ones, private to the run. The GitHub API is the one thing stubbed, because Superblink Events calls it with the GitHub token that it keeps for the person and with the hook token.

## Bug bash

Unproven. No model was configured when this skill was written, so no charter has run and the setup below is a design, not a result.

The charters are in [features/](features/), one fan-out line `slug|target|agent|charter` each (target `http`). The cron feature has none. They need a bug-bash config that is not committed, `verify/e2e.bugbash.config.ts`, that spreads `e2e.config.ts` and adds:

- a model, the personas `skeptic` and `fuzzer`, and a `context` saying what is not a bug here: a 202 for an event Superblink Events ignores, the `404 page not found` on `GET /`, text/plain replies with a trailing newline, and that the secret and database are throwaway;
- agent tools `deliver` and `count_rows` that wrap `Faucet.main(...)` from `tests/support.ts`, because the explorer drives a browser and a browser cannot POST a signed webhook;
- a target with `app.url` and no `app.command`. `app.mjs` brings up its own Postgres, so the first explorer to finish would remove it under the others. Start it once, `node app.mjs --port <free port> --detach &` (without `--detach` it stops as soon as the shell that started it exits), save the PID, and stop it with `kill <that PID>` when the bash ends.

`.e2e/` is gitignored, so `.e2e/bugbash/<slug>` is too. A finding is a bug once `tests/bugbash/<slug>.e2e.ts` fails on the reported defect (topic `bug-bash`, step 6). Anything else is the explorer's claim.

## Cleanup

The runner stops the app it started, and `close_session` stops the one a session started. Both run `app.mjs`'s SIGTERM path, which stops Superblink Events and Postgres and deletes `$TMPDIR/faucet-verify-<port>`. If the runner is killed with SIGKILL, `app.mjs` notices within seconds and runs the same teardown. If `app.mjs` or the stack process is killed, the other one runs it. `tests/lifecycle.e2e.ts` pins each case, also for a kill while initdb runs and for a runner that died before `app.mjs` started. Never `pkill` by name.

Wait ten seconds after a kill, then run `ls -d "${TMPDIR:-/tmp}"/faucet-verify-*`. A directory you know is yours (the port is in its name) that is still listed, `<dir>` below, needs these steps in order, with `ps -eo pid,args | grep -F '<text>' | grep -v grep` to find a pid:

1. Find `node app.mjs --port <port>` and send that pid SIGTERM (`kill <pid>`). It tears the stack down itself. A stack started with `--detach` outlives its launcher by design, so this is also how it stops.
2. Only when no such process is left: find `<dir>/faucet` (two processes, the main and the cron Superblink Events server) and `kill` each pid, run `pg_ctl -D <dir>/pgdata stop -m fast` (from the PostgreSQL bin directory), then delete that one directory.

Keep `.e2e/report.json`, `.e2e/results/`, `.e2e/proof/` and `.e2e/logs/app.log`. That is the proof, and all of it is gitignored.

## Helpers

`verify/app.mjs` starts and stops the stack. The runner calls it through the config. Run it by hand only for the bug bash above: `node app.mjs --port <port> --detach`.
