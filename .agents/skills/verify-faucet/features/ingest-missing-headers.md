# Missing headers

A correctly signed delivery still needs `X-GitHub-Event` and `X-GitHub-Delivery`.

## Sub-features

- A missing `X-GitHub-Event` is 400.
- A missing `X-GitHub-Delivery` is 400.

## How to get to it (user POV)

GitHub always sends both. A hand-made request or a proxy that strips headers does not.

## Driving it with e2e

`tests/ingest-missing-headers.e2e.ts` signs a valid `pull_request.closed` body and omits one header at a time through `faucet.deliver`.

## Proof

Response: 400 with the body `missing event or delivery header\n`. Store: no row for the repository the body names, even though the same body with both headers is stored.

## Charters

```
headers-missing|http|fuzzer|Send correctly signed deliveries with the event header, the delivery header, or both empty, missing or repeated; report any that stores a row or answers 2xx
```

## Gotchas

- The check runs after the signature check, so an unsigned request without headers is 401 (see Bad signature).
- It also runs before the `ping` shortcut, so a ping without a delivery header is 400.
