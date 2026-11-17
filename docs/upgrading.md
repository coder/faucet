# Upgrading

The server applies every schema change at start (on Vercel, in production only, see [deploy.md](deploy.md#previews-are-off-on-purpose)). This page lists what else an upgrade asks of you.

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

- **Tick the new events on the GitHub hook.** A hook made before them sends only Pull requests, Pull request reviews, Issue comments, Workflow runs and Check runs. Also tick Pull request review comments, Pull request review threads, Workflow jobs, Check suites and Statuses, or their families get no events, and nothing tells you so (see [events.md](events.md#the-github-webhook)).
- **New actions widen old subscriptions.** A family subscription without `actions` picks up every new action at its next renewal, and a `ci.runs` or `ci.checks` one without `actions` now matches runs and checks that have not finished. Pass `actions: ["completed"]` to keep to finished ones. The five exact types match what they did.
- **A rolling deploy drops some events.** Until every instance runs the new build, an old instance answers 202 to a delivery of an action that it does not store. GitHub counts that as delivered, so [redelivery](redelivery.md) never asks for it again, and the event is never stored. An old instance also answers `InvalidParams` to a field filter, `NotFound` to a family name, and its `/cron/deliver` can send a filtered subscription events that its filters drop.
