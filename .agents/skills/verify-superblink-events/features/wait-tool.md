# Wait tool

An agent waits for GitHub events on one repository through the `wait_for_github_events` MCP tool on `POST /github/mcp`. The call returns as soon as a matching event exists, or at its deadline with no events and a cursor.

## Sub-features

- A token that Superblink Events minted for `/github/mcp` is required: a service's from the client credentials grant, or the person's from the browser login. No header, a malformed header, a bearer with a Superblink Events prefix (`fct_`, `fcr_` or `fcs_`) that Superblink Events did not mint, or a GitHub token is 401 with the body `unauthorized` and the challenge `Bearer resource_metadata="<origin>/.well-known/oauth-protected-resource/github/mcp", scope="events:read events:subscribe"`. Superblink Events judges every bearer itself, so the GitHub stub gets no request.
- The principal must be able to read the repository. For the person, GitHub decides: a repository the person cannot see and one that does not exist both give the tool error `repository not found or not accessible` and no events. A service reads a repository that a grant covers and a delivery named, and any other is a tool error that says why.
- `repo` is `owner/name` (a service may name `owner` or `all` instead, see [Owner targets](owner-targets.md)), `types` holds at least one of the fifteen typed event names (families or exact types; the tool takes no raw name), `actions` optionally narrows the families in `types` that have each action, leaves a family that has none of them whole, and must hold only actions of one of them, the [field filters](field-filters.md) narrow only the names in `types` that offer them and each must be offered by one, `timeout_s` is 1 to 50 (default 45), `sha` is 40 hex characters, `pr` is not negative, `cursor` is the string that `events/poll` returns (an integer cursor of the build before is refused by the input schema, and the error names `cursor`), and a `pr` with only `ci.jobs` or `ci.statuses` events in `types` is refused, because GitHub names no pull request in them. Anything else is a tool error that names the problem.
- One login can hold 10 waits open at once in a process (`Deps.MaxWaitsPerLogin`). The 11th is a tool error `too many waits are open for this account` at once, and it ends nothing else. Another login is not counted.
- `types`, `actions`, the field filters, `sha` and `pr` narrow the events. Each event's `type` is its exact type, also when `types` named its family. A wait without `cursor` starts at the present, or ten minutes back when `sha` is set. A given `v2` cursor always wins, and one above the head of the repository reads as the head. A `v1.<n>` cursor is never read as a place: the wait starts at the head and its result says `truncated: true`.
- The result is `events` (`id`, `type`, `repo_id`, `prs`, `head_sha`, `data`, `received_at`), `cursor` and `truncated`. An event's `id` is its place on the line of the target, as an `events/poll` cursor counts it (`faucet_events.repo_seq` for a repo, `owner_seq` for an owner, the event ID for `all`), and `cursor` is `v2.<n>` on that line. Passing that cursor to the next call, with the same target and filters, resumes without a gap or a repeat. The cursor stands at the newest event of the target, so an event at or below it that matches only a new filter is not returned. `truncated` is true when a [purge](purge.md) deleted events of the target after the cursor that the call passed, and its test is `tests/purge.e2e.ts`.

## How to get to it (user POV)

An agent's MCP client sends `tools/call` to `/github/mcp` with `Authorization: Bearer <Superblink Events token>`. The test plays GitHub's webhooks through `faucet.deliver` and the agent through `faucet.waitForEvents`, as the Superblink Events server's service, or as the person on a Superblink Events server with the browser login, where the stub plays GitHub and knows the repositories named `verify/r<id>`.

## Driving it with e2e

`tests/wait-tool.e2e.ts`. Live: not reachable from a browser, use `run_tests {files: ["tests/wait-tool.e2e.ts"]}`.

## Proof

Response: 200 with the tool result, or 401 with `unauthorized\n`. Store, through `faucet.rows(repoId)`: the `id` of each returned event equals the `repo_seq` of a row of that repository, in order, and a denied or invalid call returns none. The app log holds `wait denied` and never the token or the secret.

## Charters

```
wait-filters|http|skeptic|Deliver events of every type with different head SHAs and pull requests, then wait with each filter alone and combined, and check every returned event against the rows in Postgres
wait-cursor|http|fuzzer|Chain waits with the cursor of the one before while events arrive, and report any event that is returned twice or never
wait-input|http|fuzzer|Send repo, types, sha, pr, cursor and timeout_s values that are odd, huge, empty, wrongly typed or oddly encoded, and report anything that is not a clear tool error or a normal result
```

## Gotchas

- An empty wait takes its full `timeout_s`, so tests use 1 or 2 seconds.
- The id of a repository `verify/r<id>` is the number in its name, from the harness's ping for a service and from the stub for the person, which is how a test gets a repository nobody else writes to.
- A wait started before its event is delivered must pass a `cursor`. Without one it starts at the present and can miss an event delivered between the call and its first poll.
- A cursor above the head reads as the head at the wait's first read, so an event that arrives between the call and that read is not returned. A test of it waits first and delivers after the wait returned.
