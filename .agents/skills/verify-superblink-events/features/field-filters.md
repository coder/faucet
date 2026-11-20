# Field filters

An agent asks for only the events it wants by a field of their data: `base_branch`, `head_branch` or `conclusion`, in `events/poll`, `events/subscribe` and the [wait tool](wait-tool.md). Each filter is named after the data field it matches. A list means any of its values, and different filters must all match.

## Sub-features

- `events/list` offers a filter only on a family whose data has the field: `ci.runs` takes `head_branch` and `conclusion`, `ci.jobs`, `ci.checks` and `ci.suites` take `conclusion`, and `pr.activity` takes `base_branch` and `head_branch`. `ci.statuses` and the exact types offer none. A null field, such as the `conclusion` of a job that has not completed, matches no value. A filter that the name does not offer, an empty list or a value that is not a list of strings is `InvalidParams` for a poll and for a subscribe, and the subscribe stores nothing.
- "CI failed on main" is `ci.runs` with `actions: ["completed"]`, `head_branch: ["main"]` and `conclusion: ["failure", "timed_out"]`. "Pull requests into main to review" is `pr.activity` with the actions that ask for a review and `base_branch: ["main"]`.
- The store applies the filters before the page limit, so `maxEvents` and `hasMore` count only the matching events, a full page's cursor stands at its last matching event, and a webhook subscription is sent only them.
- `head_branch` never matches a branch of a fork. `ci.runs` and `pr.activity` events carry `from_fork`, true when the head repository is another one or, for a pull request, a fork that was deleted, and the filter matches only events whose `from_fork` is false.
- The subscription key sorts each list and drops repeats, so reordered or repeated values address the same subscription.
- The wait tool takes the same filters. Each narrows only the names in `types` that offer it and leaves the others whole, as `actions` does, and a filter that no name in `types` offers is a tool error that names the families that do.

## How to get to it (user POV)

As for [events/poll](events-poll.md), [events/subscribe](events-subscribe.md) and the [wait tool](wait-tool.md), with the filters in `arguments` (or in the tool's arguments). The test sends recorded GitHub payloads from `faucet/testdata`, edited to the branch and the conclusion that a case needs. octokit/webhooks records no payload from a fork, so the fork cases edit the head repository of a recorded one.

## Driving it with e2e

`tests/field-filters.e2e.ts`. Live: not reachable from a browser, use `run_tests {files: ["tests/field-filters.e2e.ts"]}`.

## Proof

Response: the `eventId` of each polled event is the `delivery_id` of a matching row, in order, and the receiver gets one signed request for each matching event and none for the others (`GET /cron/deliver` counts two). A refused call is -32602, and `faucet.subscriptions(marker)` stays empty.
