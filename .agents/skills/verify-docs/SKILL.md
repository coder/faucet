---
name: verify-docs
description: Check and refresh Superblink Events' docs (README.md and docs/*.md) - the tables that internal/docgen generates from the code, the README's map and ways pictures rendered from docs/diagrams/source.html, and the Mermaid diagrams - and look at each picture in light and dark. Use when a change touches the docs, the event catalog, a cmd/faucet setting or route, or docs/diagrams/source.html, or when a docs check fails in CI.
---

# Verify the docs

Four checks keep the docs current. CI runs the first three in the `test` job (`go test ./...`) and the Mermaid check in the `verify` job (`npx e2e run`).

| Check | Run it | It fails when | Refresh |
| --- | --- | --- | --- |
| Tables and names | `go test -count=1 -run 'TestGeneratedTablesAreCurrent\|TestDocsNameWhatExists' ./internal/docgen/` | a `generated:NAME` block differs from what the code gives, a table has no block, or README.md, docs/*.md or source.html name an environment variable that no non-test file reads, a route (also inside a `https://<host>/...` URL) or a relative link (with its `#anchor`) that does not exist | `go run ./internal/docgen` |
| Numbers | `go test -count=1 -run 'DocsState\|RedeliveryDoc\|CallbackReason' ./faucet/... ./cmd/faucet/` | a number that the docs or the pictures repeat (timeouts, limits, the retry schedule, the handshake buckets, redelivery's budget) differs from its constant | change the prose; each failure names the phrase it wants |
| Pictures | `go test -count=1 -run TestPicturesMatchTheirSource ./internal/docgen/`, or `sha256sum -c docs/images/SOURCE_SHA256` | a hash in `docs/images/SOURCE_SHA256` does not match `docs/diagrams/source.html` or a PNG (so a changed page, a swapped or a truncated image fails), or a picture lacks its light or dark PNG, or a PNG is unlisted, or the README's `<picture>` for a picture does not use its dark PNG as the dark source and its light PNG as the light source and the `<img>` | `cd verify && npm ci && npx playwright-core install chromium && npm run docs:images` |
| Mermaid | `cd verify && npm ci && unset Path && npx e2e run tests/docs-mermaid.e2e.ts` | a ```` ```mermaid ```` block in README.md or docs/*.md does not parse or render (mermaid from `verify/node_modules`, no CDN) | fix the block it names |

The Mermaid run starts the whole verify stack (Postgres, two Superblink Events servers), so it needs what the `verify-superblink-events` skill lists. Set `PG_BIN` when `initdb` is not on `PATH`.

## Tables

`go run ./internal/docgen` rewrites every block between `<!-- BEGIN generated:NAME -->` and `<!-- END generated:NAME -->`. Never edit inside a block by hand.

- `events` (README) and `event-actions` (docs/events.md) come from `faucet.EventNames()`.
- `settings` (README) comes from the `settings` registry in `internal/docgen/tables.go`. The generator fails until that registry lists exactly the variables that cmd/faucet reads, so a new variable needs a row there.
- `give-up-reasons` and `callback-reasons` (docs/webhooks.md) come from registries in `internal/docgen/reasons.go`, which must list every `faucet.GiveUpReason` and `faucet.CallbackReason` constant. `TestWebhookDocSaysWhereEachCallbackReasonComesFrom` checks the lastError and data.reason columns against sample errors.
- `cron-replies` (docs/deploy.md) reads each `/cron/*` handler's reply struct in cmd/faucet with go/ast, and the result type whose doc comments explain the fields.
- A number or default in a registry text is a `{constName}` or `{faucet.constName}` placeholder, never typed in.

A name check that fails on a path or a variable that Superblink Events does not serve or read but a doc must name (a GitHub API path, Neon's `POSTGRES_*`) goes in `exemptPaths` or `exemptVariables` in `internal/docgen/names_test.go`, with the reason.

## Pictures

`docs/diagrams/source.html` is one page of inline SVG and CSS. Each picture is an element with `data-picture="<name>"`, 960 CSS px wide, and its colours switch on `prefers-color-scheme`. `npm run docs:images` (`verify/docs-images.mjs`) screenshots each picture at twice its size in both schemes to `docs/images/<name>-light.png` and `<name>-dark.png`, then writes `SOURCE_SHA256`: one sha256sum line for the page and one for each PNG. Commit the page, the PNGs and the hash together. The README shows each picture with `<picture>`: a `prefers-color-scheme: dark` source, a light source and a light `<img>` with alt text. A new picture needs that block in the README.

The page follows the Coder Kit product look: zinc neutrals, one accent (the link blue) that marks Superblink Events itself, and Geist and Geist Mono from `docs/diagrams/fonts/` (SIL Open Font License). Give a new part a neutral role from the page's `:root` block, not a new colour. The PNG bytes still depend on the host's font rasteriser, so another host can give different bytes for the same page; commit the PNGs and the hash file from one run. Keep text at 20 px or more at 960 px width, and borders, arrows and lifelines at 3:1 contrast or more against their background in both schemes (text 4.5:1).

## Look at them

The checks prove that the PNGs match `SOURCE_SHA256`, not that they read well. Before you commit a change to the page:

1. Open all four PNGs (`docs/images/*-light.png`, `*-dark.png`) and check that nothing is clipped, overlaps or is too small at about 830 px wide, GitHub's README width.
2. To compare light and dark while you edit, open `docs/diagrams/source.html` in Chrome and switch DevTools, Rendering, "Emulate CSS media feature prefers-color-scheme".
3. After you push, look at the README as GitHub renders it: `gh api -H 'Accept: application/vnd.github.html' "repos/coder/superblink-events/readme?ref=<branch>"` shows the `<picture>` markup and the image URLs, and the branch page on github.com shows the picture for your GitHub theme. Switch the theme (Settings, Appearance) to see the other one. GitHub renders the Mermaid blocks in the browser, so look at docs/webhooks.md, docs/redelivery.md and docs/events.md on the branch page too.
