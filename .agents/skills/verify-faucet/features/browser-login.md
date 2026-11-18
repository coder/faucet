# Browser login

A person's coding agent connects to `/github/mcp` with no token, gets a 401, and logs the person in through a browser: faucet shows a consent page, sends the browser to GitHub through faucet's GitHub App, keeps the GitHub user token that it gets there sealed, and gives the agent a faucet token and a refresh token. Nobody makes a GitHub token by hand. The agent's client ID is the https URL of its client ID metadata document. It is on only when `FAUCET_GITHUB_APP_CLIENT_ID`, `FAUCET_GITHUB_APP_CLIENT_SECRET` and `FAUCET_TOKEN_KEY` are set on top of `FAUCET_PUBLIC_URL`.

## Sub-features

- The metadata at `/.well-known/oauth-authorization-server` adds response type `code`, the grants `authorization_code` and `refresh_token`, `client_id_metadata_document_supported` and `authorization_response_iss_parameter_supported`, which is how go-sdk finds the login.
- `GET /oauth/authorize` fetches the client's document, checks the request, sets a `__Host-` cookie and shows the consent page: the host of the client ID, the name that the document gives, the endpoint, the scopes and where the browser returns to. It never redirects.
- `POST /oauth/authorize` (Continue with GitHub) needs the Origin of faucet, the request's cookie and the form token of that cookie. Anything else is a 403 page and ends the login, so a page of another site that posts the form of a login it started gets nothing (review finding R1).
- `GET /oauth/github/callback` takes GitHub's code in the browser that pressed Continue, trades it with the App's secret and the PKCE verifier, reads the user, seals both GitHub tokens into `faucet_connections` and sends the browser to the redirect URI with `code`, `state` and `iss`.
- `POST /oauth/token` redeems the code (`authorization_code`, PKCE, public client) for a 15-minute `fct_` token and an `fcr_` refresh token. `refresh_token` rotates the pair, refreshes the GitHub token when it expires within 10 minutes, and a replaced refresh token presented again is `invalid_grant` and ends the session.
- A person's token waits on any repository that GitHub lets their GitHub login read: faucet asks GitHub with the token that it keeps. Any other repository is refused.
- Without the three settings, `/oauth/github/callback` is 404 and the metadata lists `client_credentials` alone.

## How to get to it (user POV)

An operator makes a GitHub App with expiring user tokens, sets its client ID and secret, a token key and the public URL. The person's agent (here go-sdk's `auth.AuthorizationCodeHandler` with `ClientIDMetadataDocumentConfig`) needs the endpoint URL and its own document URL. It reads the 401 and the metadata, opens the authorization URL in the person's browser, and redeems the code that comes back to its loopback redirect URI.

## Driving it with e2e

`tests/browser-login.e2e.ts` starts a faucet with `withFaucet(..., { browserLogin: true })`: the e2e build, the stub as GitHub (`FAUCET_GITHUB_URL`) with the App from the run state, a random token key and `SSL_CERT_FILE` naming the certificate that `app.mjs` wrote, so faucet's document client trusts the `documents` fixture, which serves the client documents over https on 127.0.0.1. `Faucet.loginAgent` runs `cmd/faucet/internal/loginagent`, a go-sdk agent whose person plays the browser without one: it reads the consent page, posts its form with faucet's Origin and the cookies, and follows GitHub back to the redirect URI. `started.appLog()` lists the tokens that the stub's App issued and the token requests that it answered, and `started.connectionToken(id, field)` opens a sealed token of `faucet_connections` with the token key, as an independent reading of the seal. Two tests use the real Chromium of `@e2e-dev/web`: one walks the consent page and ends on a redirect URI that the `site` fixture serves, the other opens a page of `site` at `http://localhost:<port>`, another site than faucet's, that auto-posts the consent form of a login that the test's server started. The last test checks the main faucet and one with only `FAUCET_PUBLIC_URL`. Live: `run_tests {files: ["tests/browser-login.e2e.ts"]}`.

## Proof

The agent exits 0 with the delivered `ci.run_completed` event after one consent page, its call for a repository that the stub's GitHub denies is refused, and the stub's log shows faucet asked GitHub with the person's token. Neither faucet's output nor the app log holds a `ghu_`, `ghr_`, `fct_` or `fcr_` token, the App secret or the token key. The connection's sealed tokens open, with the token key, to the last pair GitHub issued, and no row holds one in clear. With `-refresh` the agent's second call works after a refresh on the same session, the stub saw faucet refresh the GitHub token, and the replaced refresh token is `invalid_grant` and ends the session. In Chromium, Continue ends on the redirect URI with a code that redeems, the state and `iss`, and the cross-site post is a 403 that never reaches the stub's authorize page. The Go tests in `cmd/faucet/browserlogin_flow_internal_test.go` and `faucet/oauth/` pin the same flows in-process.

## Charters

```
browser-consent|http|skeptic|Open /oauth/authorize with every mix of missing, repeated and odd client_id, redirect_uri, resource, scope, state and PKCE values, and report any request that shows a consent page it should refuse or redirects anywhere
browser-csrf|http|fuzzer|Post the consent form of a login from other origins, with Origin null or absent, without the cookie, with another login's cookie or form token, and report any post that reaches GitHub or leaves the login usable
```

## Gotchas

- The client ID must be an https URL with a path. On loopback the e2e build of faucet fetches it, and only from a server whose certificate it trusts: `SSL_CERT_FILE` replaces Go's system roots for that process.
- faucet's cookies are `__Host-` and Secure. Chromium keeps and sends them on `http://127.0.0.1`, which it counts as secure. Go's cookie jar sends a Secure cookie over https only, so the agent's person carries the cookies itself.
- The stub's GitHub access token lasts 8 minutes, under faucet's 10-minute margin, so every refresh grant also refreshes it at GitHub. GitHub's lasts 8 hours.
