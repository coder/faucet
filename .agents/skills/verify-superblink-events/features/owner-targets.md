# Owner targets

A service agent reads the events of every repository of one owner, or of every repository, through one wait, poll or subscription, instead of naming one repository. The operator grants it with `faucet clients create -grant org:<login>` or `-grant all`. The arguments name the target: `{"owner": "<login>"}` or `{"all": true}` in place of `{"repo": "<owner>/<name>"}`, with an optional `repos` list of full names. The person of the browser login keeps naming one repository.

## Sub-features

- `-grant` takes `repo:<owner>/<name>`, `org:<login>` or `all`. `faucet clients create` binds an `org:` grant to the account that holds the login, and `faucet clients list` shows it as `org:<login>#<account ID>`. Before any delivery from the owner or a repository of it, the command refuses the grant with `faucet has received no webhook delivery from <login> or a repository of it`. An `org:` grant covers an `owner` target of whatever login its account holds now, ignoring case, and a `repo` target of any repository of that account, matched by owner ID. `all` covers every target. A service whose grants do not cover a target gets -32024.
- Superblink Events finds the owner's ID through the repositories that signed deliveries named. For an `all` grant, before any delivery from a repository of the owner, an `owner` target is -32024 with `faucet has received no webhook delivery from a repository of <owner> yet`. After one, the target also covers a repository of the owner that has never sent a delivery.
- `faucet_subscriptions` stores the target as `target_kind` (`repo`, `owner` or `all`), `owner_id` (the owner's GitHub ID) and `repo_id` (0 for `owner` and `all`).
- One `owner` subscription gets the events of every repository of the owner that arrive after it, once each, and no event of another owner. `repos` keeps the repositories it names, matched ignoring case against the full name that each delivery carried.
- The wait tool and `events/poll` read `owner` and `all` targets as they read `repo`.
- A person of the browser login who names `owner` or `all` gets -32024 from `events/poll` and `events/subscribe`, and a tool error from the wait tool, with `owner and all targets are for service clients; name one repository with repo`. A refused subscribe writes no row.
- An `owner` target reads only the typed events stored since the migration that added `faucet_events.owner_id`, because older rows hold 0 there. An `all` target reads every event.
- A raw delivery of an organization that names no repository reaches `owner` and `all` interests only, and never one that names `repos`.

## How to get to it (user POV)

An operator runs `faucet clients create -grant org:<login> <client-id>` and gives the service the endpoint URL, the client ID and the secret. The service logs in with client credentials and calls `events/subscribe` with `{"owner": "<login>"}`. Superblink Events then delivers the events of every repository of that owner to its callback URL.

## Driving it with e2e

`tests/owner-targets.e2e.ts`, with the helpers of `tests/services.ts` and `tests/delivery.ts`. The subscription case runs on a delivery Superblink Events server (`withDeliveryFaucet(faucet, fn)`) and the receiver. It tries to make a client with `-grant org:verify`, which is refused while no repository of `verify` has sent a delivery, delivers a run of one repository (owner `verify`, ID 1), makes the client, then subscribes once with `{"owner": "verify"}` and once with `repos` naming a second repository that has never sent a delivery. It delivers a run of each repository and one of another owner, runs `/cron/deliver`, and waits with the same target. The refusals of the person run on a delivery Superblink Events server with the browser login (`withDeliveryFaucet(faucet, fn, { browserLogin: true })`), and the grant refusals on a Superblink Events server of their own (`withFaucet(...)`).

## Proof

Receiver: the `owner` subscription gets two requests with a `valid` signature, the runs of both repositories after the subscribe, and the `repos` subscription gets one, the run of the repository it names. `/cron/deliver` counts 3 enqueued, sent and delivered. Store: the row holds `owner|1|0` as `target_kind`, `owner_id` and `repo_id`. The wait returns the three runs of the owner in order. The refusals are -32024 with the message above, and no row has the marker of the refused subscribe. The app log holds no secret and no token.

## Charters

```
owner-target-scope|http|skeptic|Make clients with repo:, org: and all grants, deliver from repositories of two owners, then wait, poll and subscribe with repo, owner and all targets, with and without repos and in every case of the login, and report any event that reaches a target that no grant of the client covers, or a covered target that is refused
owner-target-identity|http|skeptic|Rename and transfer repositories and owners through repository deliveries and payloads whose owner or full name changes, and report any owner subscription that gets an event of another owner, or loses one of its own owner's after the change
```

## Gotchas

- `deliverRun` gives every `verify/r<id>` repository the owner `verify` with ID 1, so a test that reads an `owner` target runs on a Superblink Events server of its own, which has its own database. On the main Superblink Events server, the events of owner `verify` that other tests deliver would reach it.
- An event stores its owner only when the payload names `repository.owner.id`, which the samples of `tests/payloads.ts` lack. `deliverRun` of `tests/services.ts` sends the repository in full.
- A subscription starts at the newest stored event, so the run delivered to make the owner known is not sent.
