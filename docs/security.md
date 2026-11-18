# Security model

## Access to events

Faucet reads events for a caller with the caller's own GitHub token, or, for a service, from the grants that an operator gave it (see [service-accounts.md](service-accounts.md)). Its only GitHub token, `FAUCET_HOOK_TOKEN`, serves redelivery and nothing else (see [Secrets](#secrets)). Every `/github/mcp` request carries the caller's GitHub token as `Authorization: Bearer <token>`. Faucet checks it with `GET /user`, then checks that the same token can read the repository with `GET /repos/{owner}/{name}`. The token lives only for that request, and faucet sends it only to `FAUCET_GITHUB_API_URL`. A repository the token can't read gives the same `Forbidden` error as one that doesn't exist.

To save GitHub rate limit, faucet remembers a successful check for 60 seconds per token and repository. The cache is keyed by the SHA-256 of the token, never the token itself. Failed checks aren't cached, so a new grant works at once. The flip side is that **a revocation can take up to 60 seconds to reach** `events/poll` and the start of a wait. A wait that starts in that window can run for up to 50 more seconds. `events/subscribe` always checks fresh.

A GitHub rate-limit answer counts as a failed check that the caller can retry, not as a denial.

A bearer that starts with `fct_`, `fcr_` or `fcs_` is a faucet token: faucet checks it itself and never sends it to GitHub. Faucet mints access tokens for service clients only, each valid on the one endpoint it was issued for, and every other faucet token is 401.

One login can hold 10 waits open at once per process. Embedders can change that with `Deps.MaxWaitsPerLogin`.

## Principals and scopes

A host that embeds faucet tells it who calls. Its `PrincipalFrom` returns a `Principal` from `NewPrincipal` (a caller with a GitHub token, which holds every scope), `NewOAuthUser` or `NewService`. The wait tool and `events/poll` need `events:read`, `events/subscribe` and `events/unsubscribe` need `events:subscribe`, and `events/list` needs none. A `Principal` built by hand, or one whose `Login` changed after a constructor built it, is no kind of caller: every method but `events/list` answers it `authentication required`.

An `Authorizer` answers `RepoID`, which may remember, and `RepoIDFresh`, which every renewal calls and which must ask again. One that sends each kind of principal to another `Authorizer` passes both calls on. `TokenAuthorizer` answers a caller without a GitHub token with `ErrNoAccess` and asks GitHub nothing.

## Callback URLs

In webhook mode faucet makes outbound requests to URLs that clients choose, so it guards them against SSRF:

- **`allowlist` mode** (the default) only sends to origins that the operator listed.
- **Every outbound request**, including handshakes, goes through a guarded client (`faucet.NewHTTPClient`). It speaks `https` only and resolves the host. It refuses the request if any resolved address is not public (loopback, private, link-local, CGNAT, multicast, NAT64 and similar ranges), then dials the address it checked, so DNS can't change between the check and the connection. It follows no redirects, ignores proxy variables, and times out after 5 seconds. It refuses a response that declares a body over 64 KiB or that has headers over 16 KiB, and a delivery attempt that gets one counts as `http_5xx` and is retried. A delivery closes the body unread. A handshake reads its answer, and fails one that is longer than a challenge answer needs.
- A host that resolves to an internal address fails as `connection_refused`, the same as a closed port, so a caller can't map your network.

In `handshake` mode, any public endpoint that answers the challenge can receive events. Use `allowlist` when a public endpoint could front an internal service.

A binary built with `-tags faucet_e2e` lets the guarded client reach loopback over `http`, for the end-to-end tests. No environment variable does this, and the tagged binary logs a warning at start. **Never ship it.**

## Secrets

- **Subscriber secrets** are sealed with AES-256-GCM under `FAUCET_WEBHOOK_KEY`, and never returned or logged.
- **`CRON_SECRET`** guards the `/cron/*` routes. With it unset, they answer 503 and run nothing.
- **Client secrets** of service clients are shown once, when an operator makes the client, and kept only as SHA-256 hashes. So are the access tokens that faucet issues to them, and `/cron/purge` deletes each one once it expires.
- **`FAUCET_HOOK_TOKEN`** is the one GitHub credential that faucet holds. GitHub offers no grant narrower than one that can also edit or delete the hook, so give it to a machine user and keep it in production only. Faucet uses it for exactly two endpoints: listing a hook's deliveries and asking for a redelivery.
- Startup errors name a bad variable but never its value.
