# Service accounts

A service agent, such as one that analyzes GitHub events, logs in to faucet with a client ID and secret that an operator made ahead of time, with preset repo grants and no GitHub token. Every MCP client that connects without a token finds faucet's login server by standard discovery. All of it is on only when `FAUCET_PUBLIC_URL` is set.

## Sub-features

- `faucet clients create -resource <url> [-scope <scopes>] -grant repo:<owner>/<name> <client-id>` prints the client ID and a `fcs_` secret once; `faucet clients list` shows every client without a secret; `faucet clients disable <client-id>` ends one. The commands read `DATABASE_URL`. An ID is never given to a second client.
- Every 401 from `/github/mcp` and `/mcp` carries `WWW-Authenticate: Bearer resource_metadata="<origin>/.well-known/oauth-protected-resource<path>", scope="events:read events:subscribe"`, and each path serves its own metadata document, whose `resource` is the path's URL.
- `/.well-known/oauth-authorization-server` names `/oauth/authorize`, which answers 400 `unsupported_response_type` until a grant uses it (the TypeScript MCP SDK needs the field), and `POST /oauth/token`, which takes `client_credentials` with HTTP Basic client authentication. A token request without `resource` gets the client's endpoint, and the token holds the scopes asked for that the client holds. The token lasts 15 minutes and works only on the client's endpoint.
- A service may wait on a repository that a grant names, matched by name, once a signed delivery named it: every signed delivery records the repository it names, stored or ignored. Before that the wait tool says faucet has received no delivery from it yet. A repository no grant names is refused with that reason.
- A disabled client gets no new token, and its token fails on its next request.
- Without `FAUCET_PUBLIC_URL`, none of these routes exist (404) and the 401 is the bare `Bearer` of [MCP paths](mcp-paths.md).

## How to get to it (user POV)

An operator runs `faucet clients create` against the production database and gives the service the endpoint URL, the client ID and the secret. The service's MCP client (here go-sdk's `extauth.ClientCredentialsHandler`) needs nothing else: it reads the 401, the metadata and the token endpoint on its own.

## Driving it with e2e

`tests/service-accounts.e2e.ts` starts a faucet with `FAUCET_PUBLIC_URL` set to its own URL (`withFaucet(..., { publicUrl: true })`), makes clients with the real `faucet clients` command (`Faucet.clients`), delivers a workflow_run whose payload names the repository in full, and runs `cmd/faucet/internal/serviceagent`, a go-sdk client built by `app.mjs`, with the URL, the client ID and the secret (`Faucet.serviceAgent`). A second test logs in with the TypeScript MCP SDK's `ClientCredentialsProvider` (`@modelcontextprotocol/sdk`, pinned in `package.json`). The last test checks that the main faucet, which has no `FAUCET_PUBLIC_URL`, serves none of the routes. Live: not reachable from a browser, use `run_tests {files: ["tests/service-accounts.e2e.ts"]}`.

## Proof

The agent exits 0 with a wait result that holds the delivered `ci.run_completed` event, and exits 1 once the client is disabled. The 401 header and the metadata documents match byte for byte. The Go tests in `cmd/faucet/login_internal_test.go` run the same client against the router as `run()` builds it; `faucet/oauth/server_test.go` pins the token endpoint's answers.

## Charters

```
service-token-endpoint|http|skeptic|Send POST /oauth/token with every mix of Basic and form credentials, URL-encoded and not, wrong secrets, disabled clients, scopes and resource values, and report any answer that differs from RFC 6749 section 5 or issues a token it should not
service-discovery|http|skeptic|Fetch every 401 and metadata document on /github/mcp and /mcp with and without FAUCET_PUBLIC_URL, and report any resource, issuer or URL that a client could not match byte for byte
```

## Gotchas

- `FAUCET_PUBLIC_URL` must be the URL that clients call, in the form they compute: no trailing slash, no default port, a lower-case host. faucet refuses to start otherwise.
- A grant matches a repository by name. After a rename, the grant must name the new name.
