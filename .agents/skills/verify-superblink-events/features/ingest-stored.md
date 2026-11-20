# Stored events

A signed delivery of a handled event and action is normalized and stored as one row in `faucet_events`. The README's **Event names** lists every handled action.

## Sub-features

- Every action of `workflow_run` becomes `ci.run_<action>` and every action of `workflow_job` `ci.job_<action>`, for example `ci.run_requested` and `ci.job_queued`.
- `check_run` with action `created` or `completed` becomes `ci.check_created` or `ci.check_completed`, and `check_suite` with action `completed` becomes `ci.suite_completed`.
- `status`, which has no action, becomes `ci.status_<state>`, for example `ci.status_success`.
- Every action of `pull_request` becomes `pr.<action>`, for example `closed` becomes `pr.closed` and `synchronize` becomes `pr.synchronize`.
- Every action of `pull_request_review` becomes `pr.review_<action>`, for example `pr.review_submitted` and `pr.review_dismissed`.
- Every action of `issue_comment` on a pull request becomes `pr.comment_<action>`, for example `pr.comment_created` and `pr.comment_deleted`.
- Every action of `pull_request_review_comment` becomes `pr.review_comment_<action>`, and every action of `pull_request_review_thread` becomes `pr.review_thread_<action>`.
- `workflow_run`, `check_run` and `check_suite` store only the pull requests whose base is this repository. A pull request whose base is another repository, such as a fork's pull request to upstream, is dropped from `prs`. `workflow_job` and `status` name no pull request, so their `prs` is empty.
- The reply is 200 with the body `stored`.

## How to get to it (user POV)

GitHub POSTs `/webhook/github` with `X-GitHub-Event`, `X-GitHub-Delivery` and `X-Hub-Signature-256: sha256=<hex HMAC of the body>`. The test plays GitHub through `faucet.deliver`.

## Driving it with e2e

`tests/ingest-stored.e2e.ts` runs one test per sample in `tests/payloads.ts`, each with its own `newRepoId()`: the hand-built `samples` of the five exact types, and the `recordedSamples` that send recorded GitHub payloads from `faucet/testdata` for one new action of each pull request family (`opened`, `synchronize`, `dismissed`, `deleted`, a review comment `created` and a thread `resolved`) and for the CI families (`workflow_run` `requested`, `workflow_job` `queued` and `completed`, `check_run` `created`, `check_suite` `completed` and a `status`). Live: not reachable from a browser, use `run_tests {files: ["tests/ingest-stored.e2e.ts"]}`.

## Proof

Response: 200 `stored\n`. Store, through `faucet.rows(repoId)`: exactly one row with `source` `github`, the `delivery_id` sent, the normalized `type`, the `repo_id`, `prs` (the numbers of pull requests based on this repository: `{7}` for the workflow run and `{7,8}` for the check run, though each payload also lists fork pull request 99, which must be absent), `head_sha` (null for the issue comments), and `data` equal to the extracted fields.

## Charters

```
stored-types|http|skeptic|Send one signed delivery of each handled event type and read the row back; check that type, repository, pull request numbers, head SHA and data each match what the payload said
stored-prs|http|fuzzer|Send workflow_run and check_run deliveries with zero, one and many pull requests, some based on another repository; report rows whose prs column differs from the payload's pull requests based on its own repository
```

## Gotchas

- `data` holds the extracted fields, not the raw payload, so a field the payload carries but Superblink Events does not extract is absent on purpose. No text body is stored: no pull request body, comment body, review body or `changes`.
- `workflow_run` and `check_run` list every pull request GitHub ties to the commit, fork ones included. Superblink Events keeps a number only when the entry's `base.repo.id` equals the payload's `repository.id`. A fixture entry that lacks `base.repo.id` is dropped too, so build entries with `pullRequestRef` in `tests/payloads.ts`.
- `merge_commit_sha` is stored for `pull_request.closed` only, though GitHub sends it with every action. It is GitHub's value as sent. Before a merge it is the test-merge commit or null, so a closed pull request with `merged: false` can carry a SHA. Read `merged` to know whether it merged.
- `received_at` and `id` are assigned by Postgres, so the tests never pin them.
