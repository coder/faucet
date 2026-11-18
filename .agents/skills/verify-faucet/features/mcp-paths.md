# MCP paths

Faucet serves its GitHub events MCP endpoint at `POST /github/mcp`. `POST /mcp` is an alias for one release, so a client set up before the move keeps working. Faucet will serve one MCP endpoint per service, each at a path that names the service.

## Sub-features

- Both paths run the same handler behind the same bearer check and the same 75 second write deadline. A tool call, `events/list`, `events/poll`, `events/subscribe` and `events/unsubscribe` answer the same on both.
- Without a valid token, both paths are 401 with the body `unauthorized` and `WWW-Authenticate: Bearer`. With `FAUCET_PUBLIC_URL` set, the header names the path's own metadata document instead ([Service accounts](service-accounts.md)).
- Each path is its own resource, so a faucet access token works only on the path that it was issued for. `tests/service-accounts.e2e.ts` and the Go test `TestAFaucetAccessTokenWorksOnlyOnItsOwnResource` pin it.
- Vercel's catch-all rewrite routes both paths to the `faucet` service (`TestVercelRewritesRouteTheMCPPathsToTheServer`).

## How to get to it (user POV)

An agent's MCP client points at `https://<host>/github/mcp` with `Authorization: Bearer <GitHub token>`. An older client still points at `https://<host>/mcp`. The test plays both clients through `faucet.mcp` with `opts.path`.

## Driving it with e2e

`tests/mcp-paths.e2e.ts` sends the 401 cases to each path, compares `events/list` on the two paths, and calls the wait tool on each path for one stored event. Every other MCP test calls `/github/mcp`. Live: not reachable from a browser, use `run_tests {files: ["tests/mcp-paths.e2e.ts"]}`.

## Proof

Response: 200 with the same JSON-RPC `result` on both paths, or 401 with `unauthorized\n`. The Go tests in `cmd/faucet` run the bearer check, the wait tool, the write deadline, `events/poll` and its `Mcp-Name` check on both paths.

## Charters

```
mcp-paths-alias|http|skeptic|Send each MCP method with each bad and good token to /github/mcp and to /mcp, and report any status, header or body that differs between the two paths
```

## Gotchas

- `/mcp` goes away in a later release. A new test calls `/github/mcp`, the default of `faucet.mcp`.
