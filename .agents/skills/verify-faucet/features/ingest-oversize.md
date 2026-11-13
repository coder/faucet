# Oversize body

GitHub caps a webhook payload at 25 MiB. faucet refuses a larger body with 413.

## Sub-features

- A signed body of exactly 25 MiB (26 214 400 bytes) is accepted and stored.
- A signed body one byte larger is 413 and stores nothing.

## How to get to it (user POV)

GitHub never sends more than 25 MiB. A hand-made request can.

## Driving it with e2e

`tests/ingest-oversize.e2e.ts` pads a `pull_request.closed` payload to the exact size with a `padding` string field and signs the whole body. Each test sends about 25 MiB, so the file takes around a second.

## Proof

Response at the limit: 200 `stored\n`, and one row (the stored `data` holds the extracted fields, so the padding does not bloat the row). One byte over: 413 `body too large\n` and no row.

## Charters

```
oversize-limit|http|fuzzer|Send signed bodies at 25 MiB minus one, exactly 25 MiB, plus one and well past it, with and without a Content-Length header; report any size that is mishandled or that leaves a row behind
```

## Gotchas

- The size check runs after the signature header shape check and before the HMAC, so an oversized body with a well-formed but wrong signature is 413, not 401 (checked at PR 2, not pinned).
- Sending much more than the limit can end in a connection reset instead of a 413 body on the client. That is the server closing early, not a bug.
