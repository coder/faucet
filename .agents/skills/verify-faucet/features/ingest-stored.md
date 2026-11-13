# Stored events

A signed delivery of one of five handled event types is normalized and stored as one row in `faucet_events`.

## Sub-features

- `workflow_run` with action `completed` becomes `ci.run_completed`.
- `check_run` with action `completed` becomes `ci.check_completed`.
- `pull_request_review` with action `submitted` becomes `pr.review_submitted`.
- `pull_request` with action `closed` becomes `pr.closed`.
- `issue_comment` with action `created` on a pull request becomes `pr.comment_created`.
- The reply is 200 with the body `stored`.

## How to get to it (user POV)

GitHub POSTs `/webhook/github` with `X-GitHub-Event`, `X-GitHub-Delivery` and `X-Hub-Signature-256: sha256=<hex HMAC of the body>`. The test plays GitHub through `faucet.deliver`.

## Driving it with e2e

`tests/ingest-stored.e2e.ts` runs one test per type from the fixtures in `tests/payloads.ts`, each with its own `newRepoId()`. Live: not reachable from a browser, use `run_tests {files: ["tests/ingest-stored.e2e.ts"]}`.

## Proof

Response: 200 `stored\n`. Store, through `faucet.rows(repoId)`: exactly one row with `source` `github`, the `delivery_id` sent, the normalized `type`, the `repo_id`, `prs` (the pull request numbers, `{7,8}` for the check run), `head_sha` (null for the comment), and `data` equal to the extracted fields.

## Charters

```
stored-types|http|skeptic|Send one signed delivery of each handled event type and read the row back; check that type, repository, pull request numbers, head SHA and data each match what the payload said
stored-prs|http|fuzzer|Send workflow_run and check_run deliveries with zero, one and many pull requests; report rows whose prs column disagrees with the payload
```

## Gotchas

- `data` holds the extracted fields, not the raw payload, so a field the payload carries but faucet does not extract is absent on purpose.
- A `pull_request.closed` with `merged: false` stores `merge_commit_sha` as null in `data`.
- `received_at` and `id` are assigned by Postgres, so the tests never pin them.
