# Stored events

A signed delivery of one of five handled event types is normalized and stored as one row in `faucet_events`.

## Sub-features

- `workflow_run` with action `completed` becomes `ci.run_completed`.
- `check_run` with action `completed` becomes `ci.check_completed`.
- `pull_request_review` with action `submitted` becomes `pr.review_submitted`.
- `pull_request` with action `closed` becomes `pr.closed`.
- `issue_comment` with action `created` on a pull request becomes `pr.comment_created`.
- `workflow_run` and `check_run` store only the pull requests whose base is this repository. A pull request whose base is another repository, such as a fork's pull request to upstream, is dropped from `prs`.
- The reply is 200 with the body `stored`.

## How to get to it (user POV)

GitHub POSTs `/webhook/github` with `X-GitHub-Event`, `X-GitHub-Delivery` and `X-Hub-Signature-256: sha256=<hex HMAC of the body>`. The test plays GitHub through `faucet.deliver`.

## Driving it with e2e

`tests/ingest-stored.e2e.ts` runs one test per type from the fixtures in `tests/payloads.ts`, each with its own `newRepoId()`. Live: not reachable from a browser, use `run_tests {files: ["tests/ingest-stored.e2e.ts"]}`.

## Proof

Response: 200 `stored\n`. Store, through `faucet.rows(repoId)`: exactly one row with `source` `github`, the `delivery_id` sent, the normalized `type`, the `repo_id`, `prs` (the numbers of pull requests based on this repository: `{7}` for the workflow run and `{7,8}` for the check run, though each payload also lists fork pull request 99, which must be absent), `head_sha` (null for the comment), and `data` equal to the extracted fields.

## Charters

```
stored-types|http|skeptic|Send one signed delivery of each handled event type and read the row back; check that type, repository, pull request numbers, head SHA and data each match what the payload said
stored-prs|http|fuzzer|Send workflow_run and check_run deliveries with zero, one and many pull requests, some based on another repository; report rows whose prs column differs from the payload's pull requests based on its own repository
```

## Gotchas

- `data` holds the extracted fields, not the raw payload, so a field the payload carries but faucet does not extract is absent on purpose.
- `workflow_run` and `check_run` list every pull request GitHub ties to the commit, fork ones included. faucet keeps a number only when the entry's `base.repo.id` equals the payload's `repository.id`. A fixture entry that lacks `base.repo.id` is dropped too, so build entries with `pullRequestRef` in `tests/payloads.ts`.
- A `pull_request.closed` with `merged: false` stores `merge_commit_sha` as null in `data`.
- `received_at` and `id` are assigned by Postgres, so the tests never pin them.
