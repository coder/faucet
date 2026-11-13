# Wrong method

`/webhook/github` accepts POST only.

## Sub-features

- `GET` is 405.
- A signed `PUT`, `PATCH` or `DELETE` is 405 and stores nothing.

## How to get to it (user POV)

Someone opens the webhook URL in a browser (GET), or a client uses the wrong verb.

## Driving it with e2e

`tests/ingest-wrong-method.e2e.ts` requests the route with each method. The signed ones carry a valid `pull_request.closed` body, so a handler that wrongly accepted the method would store it. Live: `call navigate /webhook/github` shows the browser's 405 page.

## Proof

Response: status 405. Store: no row for the repository the signed body names.

## Charters

```
method-matrix|http|fuzzer|Request /webhook/github and /healthz with every HTTP method, including HEAD, OPTIONS and TRACE, signed and unsigned; report any 2xx for a method that should be refused and any 5xx
```

## Gotchas

- The 405 comes from the router, before the webhook handler. At PR 2 it has an empty body and `Allow: POST`, also for `HEAD` and `OPTIONS`. The tests pin the status only, because that reply is the router's, not faucet's.
