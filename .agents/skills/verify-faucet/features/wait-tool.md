# Wait tool

An agent waits for GitHub events on one repository through the `wait_for_github_events` MCP tool on `POST /mcp`. The call returns as soon as a matching event exists, or at its deadline with no events and a cursor.

## Sub-features

- A bearer token that GitHub accepts is required. No header, a malformed header, or a token GitHub refuses is 401 with the body `unauthorized` and `WWW-Authenticate: Bearer`.
- The token must be able to read the repository. A repository it cannot see and one that does not exist both give the tool error `repository not found or not accessible` and no events.
- `repo` is `owner/name`, `types` holds at least one of the five event types, `timeout_s` is 1 to 50 (default 45), `sha` is 40 hex characters, `pr` and `cursor` are not negative. Anything else is a tool error that names the problem.
- `types`, `sha` and `pr` narrow the events. A wait without `cursor` starts at the present, or ten minutes back when `sha` is set. A given `cursor` always wins.
- The result is `events` (`id`, `type`, `repo_id`, `prs`, `head_sha`, `data`, `received_at`), `cursor` and `truncated`. Passing that cursor to the next call resumes without a gap or a repeat. `truncated` is true when a [purge](purge.md) deleted events after the cursor that the call passed, and its test is `tests/purge.e2e.ts`.

## How to get to it (user POV)

An agent's MCP client sends `tools/call` to `/mcp` with `Authorization: Bearer <GitHub token>`. The stub plays GitHub: it knows one token and the repositories named `verify/r<id>`. The test plays GitHub's webhooks through `faucet.deliver` and the agent through `faucet.waitForEvents`.

## Driving it with e2e

`tests/wait-tool.e2e.ts`. Live: not reachable from a browser, use `run_tests {files: ["tests/wait-tool.e2e.ts"]}`.

## Proof

Response: 200 with the tool result, or 401 with `unauthorized\n`. Store, through `faucet.rows(repoId)`: the `id` of each returned event equals a row of that repository, in order, and a denied or invalid call returns none. The app log holds `wait denied` and never the token or the secret.

## Charters

```
wait-filters|http|skeptic|Deliver events of every type with different head SHAs and pull requests, then wait with each filter alone and combined, and check every returned event against the rows in Postgres
wait-cursor|http|fuzzer|Chain waits with the cursor of the one before while events arrive, and report any event that is returned twice or never
wait-input|http|fuzzer|Send repo, types, sha, pr, cursor and timeout_s values that are odd, huge, empty, wrongly typed or oddly encoded, and report anything that is not a clear tool error or a normal result
```

## Gotchas

- An empty wait takes its full `timeout_s`, so tests use 1 or 2 seconds.
- The stub's repository id is the number in the name, which is how a test gets a repository nobody else writes to.
- A wait started before its event is delivered must pass a `cursor`. Without one it starts at the present and can miss an event delivered between the call and its first poll.
