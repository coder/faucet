# Upgrading

The server applies every schema change at start (on Vercel, in production only, see [deploy.md](deploy.md#previews-are-off-on-purpose)). This page lists what else an upgrade asks of you.

## To `/github/mcp`

This came in [#48](https://github.com/coder/faucet/pull/48). The MCP endpoint is now `/github/mcp`, because faucet will serve one MCP endpoint per service and this one serves GitHub events. Point MCP clients at `https://<your host>/github/mcp`. `/mcp` still serves the same endpoint with the same token check, and a later release removes it. An Instant Rollback to a build before this one answers 404 to clients that already call `/github/mcp`.

## To event families, every PR and CI action, and field filters

These came in [#44](https://github.com/coder/faucet/pull/44), [#45](https://github.com/coder/faucet/pull/45), [#46](https://github.com/coder/faucet/pull/46) and [#47](https://github.com/coder/faucet/pull/47).

- **Tick the new events on the GitHub hook.** A hook made before them sends only Pull requests, Pull request reviews, Issue comments, Workflow runs and Check runs. Also tick Pull request review comments, Pull request review threads, Workflow jobs, Check suites and Statuses, or their families get no events, and nothing tells you so (see [events.md](events.md#the-github-webhook)).
- **New actions widen old subscriptions.** A family subscription without `actions` picks up every new action at its next renewal, and a `ci.runs` or `ci.checks` one without `actions` now matches runs and checks that have not finished. Pass `actions: ["completed"]` to keep to finished ones. The five exact types match what they did.
- **A rolling deploy drops some events.** Until every instance runs the new build, an old instance answers 202 to a delivery of an action that it does not store. GitHub counts that as delivered, so [redelivery](redelivery.md) never asks for it again, and the event is never stored. An old instance also answers `InvalidParams` to a field filter, `NotFound` to a family name, and its `/cron/deliver` can send a filtered subscription events that its filters drop.
