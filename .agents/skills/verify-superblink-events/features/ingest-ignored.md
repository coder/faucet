# Ignored events

Superblink Events acknowledges every delivery it has no rule for with 202 and stores nothing, so GitHub does not mark the webhook failing.

## Sub-features

- An event name with no typed rule (`push`) is 202 while no live subscription or poll lease wants it. With one it is stored, see [Raw events](raw-events.md).
- A handled event name with an unhandled action (`pull_request` with an action GitHub does not document, `check_run` `rerequested`, `check_suite` `requested`) is 202. Repository and organization webhooks never receive those two CI actions, so Superblink Events has no rule for them.
- A comment on a plain issue, as opposed to a pull request, is 202.
- A comment on a pull request is not ignored.

## How to get to it (user POV)

A webhook subscribed to "all events" sends dozens of types. Only ten are handled, and of `check_run` and `check_suite` only the actions that repository and organization webhooks receive.

## Driving it with e2e

`tests/ingest-ignored.e2e.ts` sends each ignored case signed and valid, then one pull request comment as the control.

## Proof

Response: 202 with the body `ignored\n`. Store: no row for the repository the body names. The control comment answers 200.

## Charters

```
ignored-events|http|skeptic|Send signed deliveries of many event names and actions Superblink Events should skip, such as push, star, release, check_run created and workflow_run in_progress; check that each is 202 and leaves no row
```

## Gotchas

- 202 is the contract for "valid but not wanted". Do not report it as a failed delivery.
- An unknown event name is acknowledged without parsing the body, so a non-JSON body on an unknown event is 202, not 400 (checked at PR 2, not pinned).
