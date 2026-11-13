# Duplicate deliveries

GitHub redelivers. A second delivery with the same `X-GitHub-Delivery` stores nothing new.

## Sub-features

- The repeat answers 200 with the body `duplicate`.
- The first row wins: a replay with the same delivery ID and a different body leaves the stored row as it was.

## How to get to it (user POV)

GitHub's "Redeliver" button, or a retry after a timeout, sends the same delivery ID again.

## Driving it with e2e

`tests/ingest-duplicate.e2e.ts`: two deliveries of the same body, then one with a changed `conclusion`. Both read `faucet.rows(repoId)`.

## Proof

Response: first 200 `stored\n`, repeat 200 `duplicate\n`. Store: one row for the repository, equal (id included) to the row read after the first delivery. After the changed replay its `data.conclusion` is still the first one.

## Charters

```
duplicate-replay|http|skeptic|Deliver the same event twice, then a third time with a changed body and the same delivery ID; check that the row count stays one and the first body wins
duplicate-ids|http|fuzzer|Reuse one delivery ID across different event types and repositories; report any case that stores a second row or overwrites the first
```

## Gotchas

- Uniqueness is on the delivery ID alone, not on the body or the repository. The same ID sent for another repository is a duplicate too (checked at PR 2, not pinned).
