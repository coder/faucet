# Bad signature

A delivery whose HMAC-SHA256 signature does not match the webhook secret is refused before faucet reads any header.

## Sub-features

- No `X-Hub-Signature-256` header is 401.
- A signature made with another secret, or over another body, is 401.
- A header that is not `sha256=` plus 64 hex characters (a `sha1=` prefix, non-hex digits, a short digest) is 401.
- The signature is checked before the event and delivery headers, so an unsigned delivery without them is 401, not 400.

## How to get to it (user POV)

Anyone who can reach the server can POST `/webhook/github`. Only GitHub, which holds the secret, can sign.

## Driving it with e2e

`tests/ingest-bad-signature.e2e.ts` sends a valid `pull_request.closed` body with each bad signature through `faucet.deliver({ signature })`. `signature: null` omits the header.

## Proof

Response: 401 with the body `invalid signature\n`. Store: `faucet.rows(repoId)` is empty for the repository the body names. The app log has a `webhook rejected` line with `status` 401.

## Charters

```
signature-forgery|http|skeptic|Sign deliveries with the wrong secret, over a modified body and with a truncated digest; report any 2xx or any stored row
signature-shape|http|fuzzer|Send X-Hub-Signature-256 values with extra whitespace, repeated headers, empty values and a sha256= prefix in odd casing; report any answer other than 401 for a wrong signature
```

## Gotchas

- A sha1 header and a short digest are rejected by shape, before the HMAC runs, so the mutation of removing the HMAC check does not fail those two tests. That is expected.
- The reply never says which part was wrong: every refusal is `invalid signature`.
- Hex digits are case-insensitive: a correct digest in uppercase is accepted (checked at PR 2, not pinned by a test). GitHub sends lowercase.
