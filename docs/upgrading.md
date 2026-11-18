# Upgrading

The server applies every schema change at start (on Vercel, in production only, see [deploy.md](deploy.md#previews-are-off-on-purpose)). This page lists what else an upgrade asks of you.

## To the browser login (migration `000013_browser_login`)

This came in [#54](https://github.com/coder/faucet/pull/54). Nothing that a client sees changes until you set `FAUCET_GITHUB_APP_CLIENT_ID`, `FAUCET_GITHUB_APP_CLIENT_SECRET` and `FAUCET_TOKEN_KEY` next to `FAUCET_PUBLIC_URL` (see [browser-login.md](browser-login.md)). With only some of them set, the server refuses to start and names the missing ones. Then:

- `/oauth/authorize` shows the consent page and takes its form, the new route `/oauth/github/callback` takes GitHub's answer, `/oauth/token` also takes the `authorization_code` and `refresh_token` grants, and the new route `/oauth/revoke` revokes a client's own tokens (RFC 7009), service clients' included. The authorization server metadata lists the new grants and the revocation endpoint, and says that faucet takes client ID metadata documents.
- Faucet keeps the GitHub App tokens of the users who log in, sealed under `FAUCET_TOKEN_KEY` (see [security.md](security.md#three-kinds-of-caller)).

Whether or not you set them:

- The migration adds the tables of the browser login, which stay empty until you do.
- `/cron/purge` also deletes login requests once they expired, or a day after for those with a code, sessions that ended or went unused for 180 days, and a user's sealed GitHub tokens a day after no session and no login request names them. It counts them in `expired_auth_requests`, `ended_sessions` and `unused_connections`, which are 0 while the browser login is off.

A host that embeds faucet: an `oauth.Store` of your own must also implement `oauth.LoginStore` and pass the new `storetest.RunOAuth` subtests. An Instant Rollback past this migration cannot start (see [deploy.md](deploy.md#rolling-back)), and its down migration drops every session and every GitHub token that faucet keeps, so every user must log in again.

## To service accounts (migration `000011_service_clients_and_repos`)

This came in [#50](https://github.com/coder/faucet/pull/50). Nothing changes until you set `FAUCET_PUBLIC_URL` (see [service-accounts.md](service-accounts.md)). Then:

- Every signed delivery that names a repository writes a row first, ping and ignored events too. A failed write is logged, and the delivery goes on. Tick Repositories on the GitHub hook so that renames reach faucet.
- A 401 from `/github/mcp` or `/mcp` names the path's metadata document and both scopes in `WWW-Authenticate`, for a GitHub token too. A client that reads only the status sees no change.
- `/cron/purge` also deletes expired access tokens and counts them in `expired_access_tokens`, which the reply leaves out without `FAUCET_PUBLIC_URL`.

A host that embeds faucet: a `Store` of your own must pass the new `storetest` subtests, and an `Authorizer` may now return an error that wraps `ErrNoAccess` and says why, because the caller reads its text. An Instant Rollback past this migration cannot start (see [deploy.md](deploy.md#rolling-back)).

## To principal kinds and scopes

This came in [#49](https://github.com/coder/faucet/pull/49). It matters to a host that embeds faucet (see [security.md](security.md#principals-and-scopes)).

- **An `Authorizer` of your own** needs a second method, `RepoIDFresh`, which `events/subscribe` calls for every renewal. `AccessCache.RepoIDUncached` is now `AccessCache.RepoIDFresh`, and `TokenAuthorizer.RepoIDFresh` is its `RepoID`.
- **A `Principal` from `NewPrincipal`** holds both scopes, as before, and `NewPrincipal` refuses a login with a colon, which GitHub never gives. One built by hand, as `faucet.Principal{Login: ...}`, or one whose `Login` changed after a constructor built it, is no kind of caller (`Principal.Authenticated` says so): every method but `events/list` answers it `authentication required`.
- **`TokenAuthorizer`** answers a caller without a GitHub token with `ErrNoAccess` and asks GitHub nothing. Before, the caller saw `could not verify repository access, try again`.
- **Migration `000010_access_tokens`** adds the table `faucet_access_tokens`, which nothing writes yet.

## To `/github/mcp`

This came in [#48](https://github.com/coder/faucet/pull/48). The MCP endpoint is now `/github/mcp`, because faucet will serve one MCP endpoint per service and this one serves GitHub events. Point MCP clients at `https://<your host>/github/mcp`. `/mcp` still serves the same endpoint with the same token check, and a later release removes it. An Instant Rollback to a build before this one answers 404 to clients that already call `/github/mcp`.

## To event families, every PR and CI action, and field filters

These came in [#44](https://github.com/coder/faucet/pull/44), [#45](https://github.com/coder/faucet/pull/45), [#46](https://github.com/coder/faucet/pull/46) and [#47](https://github.com/coder/faucet/pull/47).

- **Tick the new events on the GitHub hook.** A hook made before them may lack Pull request review comments, Pull request review threads, Workflow jobs, Check suites and Statuses. Tick them, or their families get no events, and nothing tells you so (see [events.md](events.md#the-github-webhook)).
- **New actions widen old subscriptions.** A family subscription without `actions` picks up every new action at its next renewal, and a `ci.runs` or `ci.checks` one without `actions` now matches runs and checks that have not finished. Pass `actions: ["completed"]` to keep to finished ones. The five exact types match what they did.
- **A rolling deploy drops some events.** Until every instance runs the new build, an old instance answers 202 to a delivery of an action that it does not store. GitHub counts that as delivered, so [redelivery](redelivery.md) never asks for it again, and the event is never stored. An old instance also answers `InvalidParams` to a field filter, and its `/cron/deliver` can send a filtered subscription events that its filters drop.
- **Migration `000009_subscription_event_types`.** A subscription now stores the exact types that its name matched. The migration fills each row with the row's event name, the one type that a name of an earlier build matches, so a subscription keeps its events and its key, and a trigger does the same for rows that an old instance inserts. The old build does not know families. Until a deploy has replaced every old instance, and again after a rollback to that build:
  - it answers `NotFound` to a family name;
  - its `/cron/deliver` and `/cron/purge` read a family subscription as one that matches no event, so its delivery run leaves the subscription's events to the runs of the new build;
  - when a purge has overtaken a family subscription, its delivery run moves the subscription to the head, past events it never sent, and records the gap, so the subscriber gets a gap envelope;
  - its purge can move a family subscription past events that the same purge deletes unread, and then no gap envelope goes out;
  - it sends a family subscription's event under its exact type, with no `event` and no `action` in `data`.

  Subscriptions to the five exact types are not affected.

## To the redelivery position (migration `000008_redelivery_clear`)

- **Old instances fail their runs.** Until a deploy has replaced every instance of the build before the migration, their `/cron/redeliver` runs fail with `column "top_at" does not exist` and write no position, and so does a run that was in flight during the migration. Their other routes keep working, and the runs of the new build read the list in their place.
- **The first run after the migration is a first run.** It reads its lookback of the list again and logs one more `before tracking began` gap. On a hook near the [capacity](redelivery.md#capacity), the sweep cannot read that stretch again before GitHub deletes it, and the unread part ends as `expired unread` gaps. A delivery that a run tracked before keeps its row and its ask count. A position that an old build took from a `Date` far ahead of GitHub's list carries the first risk in [known limits](redelivery.md#known-limits).
- **A `redelivery.Store` of your own** must move the position it holds into `Clear` once, as the migration does. Until then every run fails with `the store returned a top without a clear` and asks for nothing. `storetest.RunRedelivery` checks it.

## To a stricter `CRON_SECRET`

The server now stops at start, and names the variable, when `CRON_SECRET` holds a space, a control character such as a newline, a byte outside printable ASCII, or more bytes than the bearer limit in the [settings table](../README.md#configuration). Before, it started and answered 401 to every cron call. A value from `openssl rand -hex 32` passes. A secret that you read from a file can end in a newline that you must remove.
