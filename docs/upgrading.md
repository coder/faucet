# Upgrading

The server applies every schema change at start (on Vercel, in production only, see [deploy.md](deploy.md#previews-are-off-on-purpose)). This page lists what else an upgrade asks of you.

## To renames taken once (migration `000019_repo_renames`)

This came in [#83](https://github.com/coder/faucet/pull/83). It fixes how a host that records repositories (with `FAUCET_PUBLIC_URL`) moves a repository's name, which a `repo:` grant resolves through (see [service-accounts.md](service-accounts.md#grants)).

- **A rename or transfer whose write fails is answered 500**, so GitHub records the failure and redelivery asks for it again. Every other delivery whose repository write fails goes on as before.
- **A rename or transfer from a name that faucet does not hold now unbinds the repository**, and logs `a rename or transfer of the repository never reached faucet`: faucet quarantines it, and no grant reads it by name until a fresh rename or transfer from the name that faucet holds, or until you write its current name to its row in `faucet_repos` (see [security.md](security.md#repository-names)). Before, it changed nothing and left the old name bound. One that redelivery asked for still changes nothing then.
- **The migration** adds `faucet_repo_renames`, which keeps the SHA-256 of each rename or transfer that faucet took, so the same body never moves a name twice, and `faucet_repos.quarantined_at`. A body that faucet took before the migration has no row (see [security.md](security.md#repository-names)). The down file drops both, which gives a quarantined repository its stale name back.
- **Bodies signed before the migration:** someone who kept one, such as a hook admin, can send it again once, and faucet takes it as new. To make those bodies useless, give the hook a new secret and `GITHUB_WEBHOOK_SECRET` the same value after the deploy. GitHub signs with the new secret at once, so deliveries that arrive before faucet runs with it fail and need a redelivery.
- **A rolling deploy:** an instance of the build before that is already running keeps taking renames as before, with no row of their bodies. One that starts after the migration can't (`no migration found for version 19`).
- **A host that embeds faucet:** `Store.RenameRepository` takes a `RepositoryRename` and returns a `RenameOutcome`, and a `Store` of your own must pass the new `storetest` subtests. Give `Service.RedeliveryWants` to `redelivery.Deps.Wants` instead of `Service.RawWants`, so that redelivery asks for a failed rename again.

## To cursors per target (migration `000018_target_lines`)

This came in [#78](https://github.com/coder/faucet/pull/78). A cursor used to be faucet's event ID, which counts the events of every repository, so a caller of one repository could tell from its cursors how many events, and when, other repositories received. A cursor is now a place among the events of the target that the call names, written `v2.<n>` (see [events.md](events.md#cursors)).

- **A `v1.` cursor restarts once.** `events/poll`, the wait tool and `events/subscribe` answer it with `truncated: true` and start at the newest event of the target, never with an error. A client loses at most the events between its stored cursor and the upgrade, and hears so. Webhook bodies and gap envelopes carry `v2.` cursors from the first run of this build.
- **The wait tool's `cursor` is a string**, in its input and its result, the same string that `events/poll` returns, and each event's `id` is its place among the target's events. The input schema refuses the old integer cursor, and the error names `cursor`. A client that stores the integer must store the string instead.
- **The migration** numbers every row of `faucet_events` per repository and per owner and builds two unique indexes, while it holds the lock of the migration and a lock on the table, so webhook deliveries wait for it. Its time grows with the rows that the retention keeps. It also adds `faucet_event_lines`, the purge mark of each target, and the start position of every subscription. The down file drops all of them.
- **A rolling deploy:** an instance of the build before that is already running keeps working, but one that starts after the migration can't (`no migration found for version 18`). Triggers number the events that it stores and raise the purge marks of what it purges, and a subscription that it stores gets the position of its cursor. Its answers and webhooks still carry `v1.` cursors, so a client that reaches both builds during the deploy may restart once with `truncated: true`.
- **The build before refuses `v2.` cursors.** Its `events/poll` and `events/subscribe`, renewals included, answer -32602 (`cursor is not one this server returned`), and its wait tool takes only an integer cursor. During the deploy, a client that gets this error retries the same call, which reaches the new build once the deploy ends.
- **A rollback** past this migration must also apply its down file (see [deploy.md](deploy.md#rolling-back)), because the build before can't start against it. From then on every `v2.` cursor gets that error, so a client must call again without it. A renewal of a live subscription keeps the subscription's own start and loses nothing. A poll or a wait starts at the newest event, so it misses the events after its cursor. Deploying this build again restarts each `v1.` cursor once more.
- **A host that embeds faucet:** a `faucet.Store` of your own must number each target's events (`Event.RepoPosition`, `Event.OwnerPosition`), serve `PollEvents` by position and `ScanEvents` by event ID, replace `CursorAt` with `PositionAt`, and implement `StartSubscription`, which works out a subscription's start in one snapshot. It must keep `Subscription.Position`, count it on the new target's line when a renewal changes the target, count in the watermark only the unsent events of the current target, and return the watermark and gap cursor as positions. It must pass the new `storetest.Run` subtests.

## GitHub tokens are refused

This came in [#76](https://github.com/coder/faucet/pull/76). It needs no migration. **Turn on the [browser login](browser-login.md) in production before you deploy it**: from this release a person has no other way in, and a caller that sends a GitHub token gets 401.

- **A GitHub token as the bearer token is refused.** `/github/mcp` and `/mcp` answer it as they answer a request with no token: 401, with the `WWW-Authenticate` challenge that names the endpoint's metadata. Faucet sends it nowhere. GitHub's `GET /repos/{owner}/{name}` answers a fine-grained token that holds only the `Metadata` permission, or a user token of another GitHub App, for a private repository whose pull requests and checks that token cannot read, and no GitHub API says what a fine-grained token may read (see [security.md](security.md#access-to-events)).
- **What a caller does instead.** A person's coding agent drops its token and connects with none, and its user logs in through a browser (see [browser-login.md](browser-login.md#what-the-user-sees)). A service agent logs in as a client that an operator makes, with its secret or with its workload's own token (see [service-accounts.md](service-accounts.md)).
- **The server needs a public URL.** It refuses to start without `FAUCET_PUBLIC_URL`, and the error names it. On Vercel production an unset one is still `https://` and `VERCEL_PROJECT_PRODUCTION_URL`. A preview or development deployment never takes that URL, so it exits at start unless `FAUCET_PUBLIC_URL` is set for it (see [deploy.md](deploy.md#the-public-url)). For a local run, `http://localhost:8080` will do.
- **`/cron/purge`** always answers `expired_access_tokens`, `expired_auth_requests`, `ended_sessions`, `unused_connections` and `unused_access_checks`. Before, it left them out without `FAUCET_PUBLIC_URL`.
- **A rolling deploy:** until every instance runs this build, an instance of the build before still takes a GitHub token.
- **A host that embeds faucet:** `TokenAuthorizer.Authenticate` and `AccessCache` are gone. `TokenAuthorizer` still answers `RepoID` and `RepoIDFresh` for a principal from `NewPrincipal`, for a GitHub token that the host got itself, as the browser login of `faucet/oauth` does (see [security.md](security.md#principals-and-scopes)).

## To clients updated in place

This came in [#74](https://github.com/coder/faucet/pull/74). It needs no migration.

- **A token of a service holds only the scopes that its client holds now.** Before, a token kept every scope that it was issued with until it expired. No client could lose a scope before, so this changes nothing for one made earlier.
- **A rolling deploy:** until every instance runs this build, an instance of the build before lets a token use every scope that it was issued with, so a narrowed `-scope` reaches it only when the token expires, 15 minutes at most. Grants reach every instance at once.
- **A host that embeds faucet:** an `oauth.Store` of your own must also implement `UpdateClient`, which takes an `oauth.ClientUpdate`, and pass the new `storetest.RunOAuth` subtests.

## To workload logins (migration `000017_workload_trusts`)

This came in [#63](https://github.com/coder/faucet/pull/63) and, with the token exchange, [#65](https://github.com/coder/faucet/pull/65). A Vercel function or a Kubernetes pod can now log in as a service client with the token that its platform signs for it (see [service-accounts.md](service-accounts.md#log-in-with-a-workload-identity)). Nothing changes until an operator runs `faucet clients trust`.

- **`faucet clients trust`** refuses the issuers of GitHub Actions (github.com and GHE.com) and gitlab.com, which the build of #63 took. A rule for them that it stored matches no token: remove it with `faucet clients untrust`.
- **The migration** lets a client have no secret and adds `faucet_workload_trusts`, which holds the trust rules. The down file deletes every trust rule, of every client, and gives each client without a secret an all-zero secret hash, which no known secret hashes to, so the build before gives it no token. The up file turns that hash back into no secret, but the rules are gone: add them again with `faucet clients trust`.
- **The metadata** lists `urn:ietf:params:oauth:grant-type:token-exchange` in `grant_types_supported` and `none` in `token_endpoint_auth_methods_supported`, also with the browser login off. A service client logs in with its secret as before, and the revocation endpoint takes the same methods as before.
- **A rolling deploy:** until every instance runs this build, an instance of the build before answers a token exchange `unsupported_grant_type`.
- **A host that embeds faucet:** an `oauth.Store` of your own must also implement `AddTrustRule`, `TrustRules` and `RemoveTrustRule` and pass the new `storetest.RunOAuth` subtests, and `oauth.Client.SecretHash` is now a pointer, nil for a client with no secret. `oauth.ServerDeps.WorkloadKeys` sets up the client that fetches the keys of issuers, which faucet builds with `faucet.NewHTTPClient`. Leave it at its zero value in production: `AllowInsecureLoopback` is for tests, and `oauth.NewServer` logs a warning when it is set.

## To a public URL from Vercel

This came in [#60](https://github.com/coder/faucet/pull/60). On Vercel production with `FAUCET_PUBLIC_URL` unset, faucet now takes `https://` and `VERCEL_PROJECT_PRODUCTION_URL` as its public URL, so the routes of its login server exist there from this deploy on: the metadata documents, `/oauth/token`, `/oauth/authorize` and `/oauth/revoke`, and a 401 from `/github/mcp` names the metadata. A preview or development deployment, and every host outside Vercel, sees no change. Set `FAUCET_PUBLIC_URL` to pin the URL, because a shorter custom domain added later changes it (see [deploy.md](deploy.md#the-public-url)).

## To org grants bound to their account (migration `000016_org_grant_accounts`)

This came in [#60](https://github.com/coder/faucet/pull/60). An `org:` grant now names the account that held its login when `faucet clients create` made it, so it follows the account through a rename, and neither a later holder of the login nor a delivery that a hook admin sends again by hand points it at another account (see [service-accounts.md](service-accounts.md#grants)).

- **The migration binds the `org:` grants of existing clients** to the account that holds each login at the migration, which is the account that each grant reads at that moment, so it gives no client more than it had. After the migration, check the account ID after each `#` in `faucet clients list`. A grant whose login no account holds then keeps its form, reads nothing from then on, and shows in `faucet clients list` without `#<account ID>`. Make a new client for it once a delivery has named the account. The down file turns each bound grant back into the login that its account holds then, so the build before reads the same account. A grant whose account holds no login then keeps its bound form, which the build before reads as no grant.
- **`faucet clients create` refuses an `org:` grant of a login that no delivery has named.** Make the client after GitHub has sent a delivery from the account or one of its repositories.
- **A rolling deploy:** until every instance runs this build, an instance of the build before reads a bound grant as no grant and refuses what only that grant allows.
- **A host that embeds faucet:** `oauth.NewClient` takes an `org:` grant only as `org:<login>#<account ID>`. `oauth.BindGrants` turns the `org:<login>` that an operator writes into that form.

## To faucet's own document client

This came in [#60](https://github.com/coder/faucet/pull/60). It matters to a host that embeds faucet. `oauth.BrowserLogin.Documents` is now a `faucet.HTTPClientOptions`, not an `*http.Client`: faucet builds the client that fetches client ID metadata documents itself, with `faucet.NewHTTPClient`, because the client of the login chooses their URL. Leave it at its zero value in production. Before, a host that passed a client of its own, such as `http.DefaultClient`, let anyone who opened `/oauth/authorize` make faucet fetch from the host's network and follow redirects. `AllowInsecureLoopback` exists for tests only, and `oauth.NewServer` logs a warning when it is set.

## To stored repository checks (migration `000015_access_checks`)

This came in [#56](https://github.com/coder/faucet/pull/56). Faucet stores the repository checks of browser logins, so a poll or a wait of a browser login asks GitHub only when no check newer than its session's last refresh answers (see [browser-login.md](browser-login.md#how-faucet-checks-a-repository)). Each refresh of a browser login asks GitHub again about up to 50 repositories, which can make the refresh take up to 5 seconds longer. The migration adds `faucet_access_checks`, which stays empty until a browser login checks a repository. `/cron/purge` deletes a stored check a day after a session last used it, and a stored denial 15 minutes after its check, and counts both in `unused_access_checks`, which is 0 while the browser login is off.

A host that embeds faucet: an `oauth.Store` of your own must also implement `oauth.AccessCheckStore` and pass the new `storetest.RunOAuth` subtests, and an `oauth.ServerDeps` with a `Login` now needs `Repos`. The down migration drops every stored check, and faucet then asks GitHub at the next use of each repository.

## To owner and all targets

This came in [#55](https://github.com/coder/faucet/pull/55). A service can now read every repository of an owner or every repository (see [service-accounts.md](service-accounts.md#grants) and [events.md](events.md#targets)). A caller that names `repo` sees no change, and a subscription made before keeps its key.

- **Grant a service an owner or all repositories** with `faucet clients create -grant org:<login>` or `-grant all`. A client made before keeps its `repo:` grants.
- **Grant again a repository whose owner was renamed before the upgrade.** Faucet now finds a repository by the login that its owner's latest delivery carried. A `repo:<old login>/<name>` grant of an owner renamed before the upgrade works until that owner's next delivery and then finds nothing. Make a client with `repo:<current login>/<name>` instead.
- **Migration `000014_owner_targets`** adds the owner and the name of the repository to each event, the kind of target to subscriptions and raw leases, the table `faucet_owners` of each account's latest login (filled from `faucet_repos`), and indexes for each kind of target. It rewrites no row, but it builds `faucet_events_owner` over the events table, which blocks event writes while it runs. An event stored before it records no owner, so an `owner` target never reads it, while an `all` target does. Its down file deletes every owner and all subscription and lease, and the raw events of deliveries that named no repository.
- **Issue no `org:` or `all` grant until every instance runs this build.** Only a service with one of them can make an owner or all subscription or lease, and an instance of the build before cannot serve one:
  - Its delivery run fails at such a subscription when it sits below the purge mark, and queues no event for the subscriptions after it.
  - Its purge moves such a subscription past the events it lost, so the subscriber gets no gap.
  - It gives up a queued event of such a subscription, typed or raw, as `repo_mismatch`.
  - It stores no raw event that only an owner or all interest wants.
  - It refuses the renewal, poll or wait of an owner or all target as invalid arguments, so a subscription whose renewals reach only such instances expires.
- **Redelivery asks again for more.** An owner or all interest wants every failed delivery of its raw event, whatever the repository, because GitHub's list of deliveries names no owner.
- **A host that embeds faucet** turns owner and all targets on for services with `Deps.ServiceGrants`, which `oauth.ServiceAccess` implements. Without it faucet refuses them. `Subscription`, `PollQuery`, `ScanTarget`, `DeliveryClaim` and the raw lease, read, claim and want types hold a `Target` in place of `RepoID`.
- **A host that embeds faucet and sets `Deps.RecordRepositories`** must now also set `Deps.Redelivered`, or `New` refuses: pass `pgstore.Store.Redelivered`, or a func that answers false when the host runs no redelivery. A delivery that it reports moves no login. There is no default, because a func that always answered false would let the host's own redeliveries give renamed accounts their old logins back, and one that always answered true would keep every login as first seen.
- **A `Store` or `RawStore` of your own** must pass the new `storetest` subtests. `Store` gains `RecordOwner`, `AddOwner`, `OwnerByID` and `OwnerByLogin`; `RecordRepository` and `RenameRepository` no longer record the owner's login, which the webhook records first; and `RenameRepository` also matches `from` under the login that the row's owner holds now.

## To the browser login (migration `000013_browser_login`)

This came in [#54](https://github.com/coder/faucet/pull/54). Nothing that a client sees changes until you set `FAUCET_GITHUB_APP_CLIENT_ID`, `FAUCET_GITHUB_APP_CLIENT_SECRET` and `FAUCET_TOKEN_KEY` next to `FAUCET_PUBLIC_URL` (see [browser-login.md](browser-login.md)). With only some of them set, the server refuses to start and names the missing ones. Then:

- `/oauth/authorize` shows the consent page and takes its form, the new route `/oauth/github/callback` takes GitHub's answer, `/oauth/token` also takes the `authorization_code` and `refresh_token` grants, and the new route `/oauth/revoke` revokes a client's own tokens (RFC 7009), service clients' included. The authorization server metadata lists the new grants and the revocation endpoint, and says that faucet takes client ID metadata documents.
- Faucet keeps the GitHub App tokens of the users who log in, sealed under `FAUCET_TOKEN_KEY` (see [security.md](security.md#three-kinds-of-caller)).

Whether or not you set them:

- The migration adds the tables of the browser login, which stay empty until you do.
- `/cron/purge` also deletes login requests once they expired, or a day after for those with a code, sessions that ended or went unused for 180 days, and a user's sealed GitHub tokens a day after no session and no login request names them. It counts them in `expired_auth_requests`, `ended_sessions` and `unused_connections`, which are 0 while the browser login is off.

A host that embeds faucet: an `oauth.Store` of your own must also implement `oauth.LoginStore` and pass the new `storetest.RunOAuth` subtests. An Instant Rollback past this migration cannot start (see [deploy.md](deploy.md#rolling-back)), and its down migration drops every session and every GitHub token that faucet keeps, so every user must log in again.

## To raw events

This came in [#51](https://github.com/coder/faucet/pull/51). The server offers no [raw event](events.md#raw-events) until you list it in `FAUCET_RAW_EVENTS`, and stores none until someone polls or subscribes to one.

- **List the events you want raw in `FAUCET_RAW_EVENTS`, and tick them on the GitHub hook.** Faucet can only offer what the hook sends. The server refuses to start on an event whose payload GitHub shows only to people with more than read access.
- **A host that embeds faucet** turns raw events on with `Deps.RawStore`, which must be the same database as `Deps.Store`, and `Deps.RawEvents`, which lists the events. `pgstore.Store` is both stores. Without them nothing changes. To have failed raw deliveries asked for again, give `Service.RawWants` to `redelivery.Deps.Wants`.
- **A `Store` of your own** needs no change. A `RawStore` of your own must pass `storetest.RunRaw`.
- **Migration `000012_raw_events`** adds five tables and one index on `faucet_subscriptions`. An instance of the build before it never reads them, and an Instant Rollback past it cannot start (see [deploy.md](deploy.md#rolling-back)).
- **A rolling deploy drops some raw events.** Until every instance runs the new build, an old instance answers 202 to a delivery that a new instance would have stored, and `NotFound` to a raw name.
- **`/cron/purge`** answers four more members, and `/cron/deliver` one more (see [deploy.md](deploy.md#scheduled-routes)).

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
