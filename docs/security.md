# Security model

## Three kinds of caller

Faucet returns events only for repositories that the caller may read. A caller of `/github/mcp` is one of three kinds:

| Caller | Its bearer token | Who decides what it may read | GitHub token that faucet keeps for it |
| --- | --- | --- | --- |
| A pasted GitHub token | The caller's GitHub token | GitHub, asked with that token | None. The token lives only for the request. |
| A [browser login](browser-login.md) | A faucet access token | GitHub, asked with the user's GitHub App token | The user's access token and refresh token, sealed |
| A [service](service-accounts.md) | A faucet access token | The operator's grants. Faucet asks GitHub nothing. | None |

For a pasted token and for a service, faucet holds no GitHub token of the caller. For a browser login, faucet keeps the user's GitHub App token, sealed with AES-256-GCM under `FAUCET_TOKEN_KEY`, and gives the client its own short-lived token. The client never sees the GitHub token. Whoever holds both faucet's database and `FAUCET_TOKEN_KEY` can open those tokens and act as each of those users within the App's permissions, and with the App's client secret can also refresh them. That is why the App gets read-only permissions.

## Access to events

Every `/github/mcp` request carries its bearer token as `Authorization: Bearer <token>`. A bearer that starts with `fct_`, `fcr_` or `fcs_` is a faucet token. Faucet checks it itself and never sends it to GitHub. Faucet mints access tokens for service clients and for the sessions of browser logins, each valid for 15 minutes on the one endpoint it was issued for, and every other faucet token is 401.

Any other bearer is a GitHub token. Faucet checks it with `GET /user`, then checks that the same token can read the repository with `GET /repos/{owner}/{name}`. The token lives only for that request, and faucet sends it only to `FAUCET_GITHUB_API_URL`. A repository the token can't read gives the same `Forbidden` error as one that doesn't exist.

For a GitHub token, faucet remembers a successful check for 60 seconds per token and repository, to save GitHub rate limit. The cache is keyed by the SHA-256 of the token, never the token itself. Failed checks aren't cached, so a new grant works at once. So **a revocation can take up to 60 seconds to reach** `events/poll` and the start of a wait. A wait that starts in that window can run for up to 50 more seconds. `events/subscribe` always checks fresh.

A browser login is checked with the same `GET /repos/{owner}/{name}`, sent with the GitHub token that faucet keeps for the user. Faucet refreshes that token first when it is about to expire. When GitHub rejects it, faucet refreshes it once and asks again. Faucet stores each check that passes, per GitHub user and repository, and a session takes a stored check without asking GitHub only when faucet made it after that session's last refresh. At each refresh faucet asks GitHub again about the 50 repositories that the user used last, and deletes those that GitHub denies. A denial is never stored, and it deletes the user's stored check of that repository. An agent must refresh to keep calling once its 15-minute access token expires, so a browser login's `events/poll` and the start of its wait **see a revocation within 15 minutes**. `events/subscribe` always asks GitHub.

A GitHub rate-limit answer counts as a failed check that the caller can retry, not as a denial.

One login can hold 10 waits open at once per process. Embedders can change that with `Deps.MaxWaitsPerLogin`.

## Principals and scopes

A host that embeds faucet tells it who calls. Its `PrincipalFrom` returns a `Principal` from `NewPrincipal` (a caller with a GitHub token, which holds every scope), `NewOAuthUser` (a user of a browser login) or `NewService` (a service). The wait tool and `events/poll` need `events:read`, `events/subscribe` and `events/unsubscribe` need `events:subscribe`, and `events/list` needs none. A `Principal` built by hand, or one whose `Login` changed after a constructor built it, is no kind of caller: every method but `events/list` answers it `authentication required`.

An `Authorizer` answers `RepoID`, which may remember, and `RepoIDFresh`, which every renewal calls and which must ask again. One that sends each kind of principal to another `Authorizer` passes both calls on. `TokenAuthorizer` answers a caller without a GitHub token with `ErrNoAccess` and asks GitHub nothing.

## The browser login

These checks keep another site, another browser and another client from finishing a user's login:

- **Another site cannot press Continue.** The consent page sets a cookie for its own login, and its form carries a token derived from that cookie. Faucet goes on only when the post carries the cookie, the matching form token and an `Origin` that equals `FAUCET_PUBLIC_URL`. Any other post is refused and stores nothing, so a page of another site that posts the form makes nothing. Continue works only once the page has been visible and in focus for 500 milliseconds, so that a page of another site cannot open the consent page under a click that the user meant for that site, such as the second click of a double-click. A press that began before then does not count.
- **Each login has its own cookies.** Their names end with the login's ID, so two logins in one browser do not mix. They are `__Host-` cookies, which only faucet's own origin can set, over `https`.
- **GitHub's answer counts only in the browser that pressed Continue.** The state that faucet sends to GitHub, and the PKCE verifier of faucet's own exchange with GitHub, both derive from a random value in a second cookie. A callback in another browser is refused.
- **Faucet redirects to a client only at the end.** `/oauth/authorize` answers every error with a page and never with a redirect, because nothing proves yet that the browser is the user's. Only the callback redirects to a client, and the redirect names faucet in `iss` (RFC 9207).
- **A code works once, for 60 seconds.** A code that comes back after its redemption ends the session that it made.
- **PKCE runs both ways.** A client must send an S256 challenge and redeem its code with the verifier. Faucet uses PKCE with GitHub too.
- **Refresh tokens rotate.** Each refresh gives a new refresh token. A refresh token that comes back 30 seconds or more after its first use ends its session, because the token then has two holders. Within those 30 seconds it trades again, because the parallel requests of one agent send one token at once. Of the tokens that such requests get, the next refresh retires the ones that the agent did not keep, and one of those that comes ends the session too. Faucet keeps the hash of every refresh token of a session until the session goes, so a stolen token ends the session however late its holder sends it.
- **Client documents come through the guarded client** of [callback URLs](#callback-urls): `https` and public addresses only, and no redirects. A fetch must end within 3 seconds, and the document must be 5 KiB at most. A document that holds a client secret is refused, because anyone can read it.
- **The consent page shows the host, not the name.** The host of the client ID is the large text, and the name that the document gives is marked as unchecked, because anyone can write any name in a document. The host must be ASCII, with an internationalized name in its `xn--` form, and the name may hold no control or format character, so that neither can pass for another.
- **Redirect URIs match exactly.** A request's redirect URI must equal one of the document's byte for byte, and a loopback `http` one may differ in its port only (RFC 8252). The token request must name the same redirect URI. Schemes that a browser runs, such as `javascript`, are refused.
- **The pages load nothing and run one script,** the wait of Continue, which their Content-Security-Policy allows by its hash. No other site can frame them.

Anyone who serves a valid document can open `/oauth/authorize`, so faucet bounds what that costs. It keeps up to 64 documents, each for as long as its cache headers let a shared cache keep it and 5 minutes at most, and fetches no client ID on its own host. The consent page stores nothing: its form carries the request, sealed with AES-256-GCM for the cookie of the browser that got the page, under a key that HKDF-SHA256 derives from `FAUCET_TOKEN_KEY` for consent forms alone, and faucet stores the request only when that browser presses Continue. So no number of requests uses up anything that another user's login needs. A script can still store rows, one for each GET and POST pair, because it can keep the cookie and send faucet's `Origin` itself. Put a per-IP rate limit on `POST /oauth/authorize` at the edge, such as a Vercel Firewall rule; `/cron/purge` deletes such a row once it expires, 10 minutes after the GET. Faucet keeps the failure of a fetch for 1 minute, so a client ID whose fetch failed costs one fetch a minute however often it comes, and logins that start at once share one fetch. A client ID with a new path, or a document sent with `no-store`, still costs one fetch per request. Faucet sets no limit per host, because bad client IDs on a host could use it up and lock out that host's real client.

## Callback URLs

In webhook mode faucet makes outbound requests to URLs that clients choose, so it guards them against SSRF:

- **`allowlist` mode** (the default) only sends to origins that the operator listed.
- **Every outbound request**, including handshakes, goes through a guarded client (`faucet.NewHTTPClient`). It speaks `https` only and resolves the host. It refuses the request if any resolved address is not public (loopback, private, link-local, CGNAT, multicast, NAT64 and similar ranges), then dials the address it checked, so DNS can't change between the check and the connection. It follows no redirects, ignores proxy variables, and times out after 5 seconds. It refuses a response that declares a body over 64 KiB or that has headers over 16 KiB, and a delivery attempt that gets one counts as `http_5xx` and is retried. A delivery closes the body unread. A handshake reads its answer, and fails one that is longer than a challenge answer needs.
- A host that resolves to an internal address fails as `connection_refused`, the same as a closed port, so a caller can't map your network.

In `handshake` mode, any public endpoint that answers the challenge can receive events. Use `allowlist` when a public endpoint could front an internal service.

A binary built with `-tags faucet_e2e` lets the guarded client reach loopback over `http`, for the end-to-end tests. No environment variable does this, and the tagged binary logs a warning at start. **Never ship it.**

## Secrets

- **Subscriber secrets** are sealed with AES-256-GCM under `FAUCET_WEBHOOK_KEY`, and never returned or logged.
- **The GitHub tokens of browser logins** are sealed with AES-256-GCM under `FAUCET_TOKEN_KEY`, and never returned or logged. Once no session and no login request names a user's pair, `/cron/purge` deletes it a day after its last update. A sealed token names its user and its field in the data that the seal covers, so a token copied to another row or field does not open. Faucet sends the access token only to `FAUCET_GITHUB_API_URL`, and the refresh token only to GitHub's token endpoint at `FAUCET_GITHUB_URL`, and follows no redirect. A token that does not open fails its user's checks and ends no session (see [rotating the key](browser-login.md#rotate-the-token-key)).
- **`FAUCET_GITHUB_APP_CLIENT_SECRET`** goes only to GitHub's token endpoint, with a user's code or refresh token. Keep it in production only.
- **`CRON_SECRET`** guards the `/cron/*` routes. With it unset, they answer 503 and run nothing.
- **Client secrets** of service clients are shown once, when an operator makes the client, and kept only as SHA-256 hashes.
- **Faucet's own tokens** are kept only as SHA-256 hashes too: access tokens, refresh tokens, codes, and the cookie values and GitHub state of a login in progress. `/cron/purge` deletes each access token once it expires, and each session with its refresh tokens once the session ended or went unused for 180 days. A client revokes a token of its own at `POST /oauth/revoke` (RFC 7009): a refresh token ends its session.
- **`FAUCET_HOOK_TOKEN`** is the only GitHub token that faucet holds for itself. GitHub offers no grant narrower than one that can also edit or delete the hook, so give it to a machine user and keep it in production only. Faucet uses it for exactly two endpoints: listing a hook's deliveries and asking for a redelivery.
- Startup errors name a bad variable but never its value.
