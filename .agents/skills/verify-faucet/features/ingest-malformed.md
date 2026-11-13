# Malformed payloads

A signed delivery of a handled event whose payload cannot be normalized is refused with 400, so GitHub shows a failed delivery.

## Sub-features

- A body that is not JSON is 400.
- A payload with no `action` is 400.
- A `pull_request.closed` with no pull request number is 400.
- A payload with no `repository.id` is 400.

## How to get to it (user POV)

Only a bug in a sender or a hand-made request reaches this. GitHub itself sends well-formed payloads.

## Driving it with e2e

`tests/ingest-malformed.e2e.ts` sends each case as a signed `pull_request` delivery.

## Proof

Response: 400 whose body starts `malformed payload`. Store: no row for the repository the body names.

## Charters

```
malformed-shapes|http|fuzzer|Send signed deliveries of handled events with wrong JSON types, missing fields, nulls, huge numbers and deep nesting; report any 5xx, any stored row with a nonsense column, and any 2xx that should be 400
```

## Gotchas

- Only handled event names are parsed. A malformed body on an event faucet ignores is 202 (see Ignored events).
- The 400 body names the cause, for example the missing field. That is intended for a failed delivery in GitHub's UI.
