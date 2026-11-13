# Ignored events

faucet acknowledges every delivery it has no rule for with 202 and stores nothing, so GitHub does not mark the webhook failing.

## Sub-features

- An event name with no rule at all (`push`) is 202.
- A handled event name with an unhandled action (`pull_request` `opened`, `check_run` `created`, `workflow_run` `requested`) is 202.
- A comment on a plain issue, as opposed to a pull request, is 202.
- A comment on a pull request is not ignored.

## How to get to it (user POV)

A webhook subscribed to "all events" sends dozens of types. Only five are handled.

## Driving it with e2e

`tests/ingest-ignored.e2e.ts` sends each ignored case signed and valid, then one pull request comment as the control.

## Proof

Response: 202 with the body `ignored\n`. Store: no row for the repository the body names. The control comment answers 200.

## Charters

```
ignored-events|http|skeptic|Send signed deliveries of many event names and actions faucet should skip, such as push, star, release, pull_request opened and synchronize; check that each is 202 and leaves no row
```

## Gotchas

- 202 is the contract for "valid but not wanted". Do not report it as a failed delivery.
- An unknown event name is acknowledged without parsing the body, so a non-JSON body on an unknown event is 202, not 400 (checked at PR 2, not pinned).
