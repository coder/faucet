---
name: verify-docs
description: Check and refresh faucet's docs (README.md and docs/*.md) - the tables that internal/docgen generates from the code, the README's map and ways pictures rendered from docs/diagrams/source.html, and the Mermaid diagrams - and look at each picture in light and dark. Use when a change touches the docs, the event catalog, a cmd/faucet setting or route, or docs/diagrams/source.html, or when a docs check fails in CI.
---

# Verify the docs

Three checks keep the docs current. CI runs all three: the first two in the `test` job (`go test ./...`), the third in the `verify` job (`npx e2e run`).

| Check | Run it | It fails when | Refresh |
| --- | --- | --- | --- |
| Tables and names | `go test -count=1 -run 'TestGeneratedTablesAreCurrent\|TestDocsNameWhatExists' ./internal/docgen/` | a `generated:NAME` block differs from what the code gives, or README.md, docs/*.md or source.html name a `FAUCET_*` variable, a route or a relative link (with its `#anchor`) that does not exist | `go run ./internal/docgen` |
| Pictures | `go test -count=1 -run TestPicturesMatchTheirSource ./internal/docgen/` | `docs/images/SOURCE_SHA256` is not the hash of `docs/diagrams/source.html`, or a picture lacks its light or dark PNG, or a PNG belongs to no picture | `cd verify && npm ci && npm run docs:images` |
| Mermaid | `cd verify && npm ci && unset Path && npx e2e run tests/docs-mermaid.e2e.ts` | a ```` ```mermaid ```` block in README.md or docs/*.md does not parse or render (mermaid from `verify/node_modules`, no CDN) | fix the block it names |

The Mermaid run starts the whole verify stack (Postgres, two faucets), so it needs what the `verify-faucet` skill lists. Set `PG_BIN` when `initdb` is not on `PATH`.

## Tables

`go run ./internal/docgen` rewrites every block between `<!-- BEGIN generated:NAME -->` and `<!-- END generated:NAME -->`. Never edit inside a block by hand.

- `events` (README) and `event-actions` (docs/events.md) come from `faucet.EventNames()`.
- `settings` (README) comes from the `settings` registry in `internal/docgen/tables.go`. The generator fails until that registry lists exactly the `env*` constants of cmd/faucet, so a new variable needs a row there. A number or default in a purpose is a `{constName}` or `{faucet.constName}` placeholder, never typed in.

A name check that fails on a path that faucet does not serve but a doc must name (a GitHub API path) goes in `exemptPaths` in `internal/docgen/names_test.go`, with the reason.

## Pictures

`docs/diagrams/source.html` is one page of inline SVG and CSS. Each picture is an element with `data-picture="<name>"`, 960 CSS px wide, and its colours switch on `prefers-color-scheme`. `npm run docs:images` (`verify/docs-images.mjs`) screenshots each picture at twice its size in both schemes to `docs/images/<name>-light.png` and `<name>-dark.png`, then writes `SOURCE_SHA256`. Commit the page, the PNGs and the hash together. The README shows each picture with `<picture>`: a `prefers-color-scheme: dark` source, a light source and a light `<img>` with alt text. A new picture needs that block in the README.

The hash covers only the page. After a change to `docs-images.mjs`, run it again anyway. The PNG bytes depend on the fonts of the host, so another host can give different bytes for the same page.

## Look at them

The checks prove the pictures are current, not that they read well. Before you commit a change to the page:

1. Open all four PNGs (`docs/images/*-light.png`, `*-dark.png`) and check that nothing is clipped, overlaps or is too small at about 830 px wide, GitHub's README width.
2. To compare light and dark while you edit, open `docs/diagrams/source.html` in Chrome and switch DevTools, Rendering, "Emulate CSS media feature prefers-color-scheme".
3. After you push, look at the README as GitHub renders it: `gh api -H 'Accept: application/vnd.github.html' "repos/coder/faucet/readme?ref=<branch>"` shows the `<picture>` markup and the image URLs, and the branch page on github.com shows the picture for your GitHub theme. Switch the theme (Settings, Appearance) to see the other one. GitHub renders the Mermaid blocks in the browser, so look at docs/webhooks.md, docs/redelivery.md and docs/events.md on the branch page too.
