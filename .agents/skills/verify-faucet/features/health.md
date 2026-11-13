# Health

`GET /healthz` is the liveness probe. It answers without touching the database.

## Sub-features

- `GET /healthz` answers 200 with the body `ok` and a newline.
- `POST /healthz` is 405 and an unknown path is 404.

## How to get to it (user POV)

An operator or load balancer requests `/healthz`. No account, no header. The runner also uses it as the readiness probe (`readyUrl`).

## Driving it with e2e

`tests/health.e2e.ts` pins the three behaviors. `tests/smoke.e2e.ts` is the Doctor test: it pins the 200 and checks that `faucet_events` exists, so a green smoke run means faucet started and migrated. Live: `open_session`, `call navigate /healthz`, then `call observe` shows one text node `ok`.

## Proof

Response: status 200, body `ok\n`. Store: none for this feature, except that the smoke test reads `to_regclass('public.faucet_events')` over `psql` and gets a table. The app log has the `listening` line and no secret.

## Charters

```
health-methods|http|fuzzer|Starting at /healthz, request it with each HTTP method and with trailing slashes, query strings and encoded paths; report any answer that is not 200 ok for GET or a clean 404 or 405
```

## Gotchas

- `ok` does not prove the database is reachable: the handler never queries it.
- The probe is the only route that answers GET with 200. `GET /webhook/github` is 405 by design.
