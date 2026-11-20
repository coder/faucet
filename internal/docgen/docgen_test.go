package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/coder/superblink-events/faucet"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := findRoot(wd)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestGeneratedTablesAreCurrent(t *testing.T) {
	t.Parallel()
	updates, err := repoSource(repoRoot(t)).regenerate()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range updates {
		for _, block := range u.blocks {
			t.Errorf("%s: the generated:%s table is stale. Run `go run ./internal/docgen` and commit the result.", u.file, block)
		}
	}
}

func TestDocsNameWhatExists(t *testing.T) {
	t.Parallel()
	problems, err := checkNames(os.DirFS(repoRoot(t)))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

func mapFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return fsys
}

var fixtureEvents = []faucet.EventName{
	{Name: "ci.runs", Family: "ci.runs", GitHubEvent: "workflow_run", Actions: []string{"completed", "requested"}, Filters: []string{"head_branch", "conclusion"}},
	{Name: "ci.jobs", Family: "ci.jobs", GitHubEvent: "workflow_job", Actions: []string{"queued"}},
	{Name: "ci.run_completed", Family: "ci.runs", GitHubEvent: "workflow_run", Exact: true, Actions: []string{"completed"}},
}

const currentReadme = `# Fixture

<!-- BEGIN generated:events -->
| Family | GitHub event | Exact types |
| --- | --- | --- |
| ` + "`ci.runs` | `workflow_run` | `ci.run_completed`" + ` |
| ` + "`ci.jobs` | `workflow_job`" + ` | none |
<!-- END generated:events -->

Prose after the table.
`

const currentEventsDoc = `<!-- BEGIN generated:event-actions -->
| Family | Actions | Filters |
| --- | --- | --- |
| ` + "`ci.runs` | `completed`, `requested` | `head_branch`, `conclusion`" + ` |
| ` + "`ci.jobs` | `queued`" + ` | none |
<!-- END generated:event-actions -->
`

var fixtureTables = map[string]table{"events": eventsTable, "event-actions": eventActionsTable}

func TestRegenerate(t *testing.T) {
	t.Parallel()
	emptied := strings.Replace(currentReadme, "| `ci.runs` | `workflow_run` | `ci.run_completed` |\n| `ci.jobs` | `workflow_job` | none |\n", "", 1)
	cases := []struct {
		name string
		docs map[string]string
		// stale lists "file block" for every block that a run rewrites.
		stale []string
		// wantErr is the file:line that the error must name.
		wantErr string
	}{
		{name: "current tables", docs: map[string]string{"README.md": currentReadme, "docs/events.md": currentEventsDoc}},
		{name: "a doc without blocks", docs: map[string]string{"README.md": currentReadme, "docs/events.md": currentEventsDoc, "docs/other.md": "# Other\n"}},
		{name: "a table without a block", docs: map[string]string{"README.md": currentReadme}, wantErr: "generated:event-actions has no block"},
		{name: "no blocks at all", docs: map[string]string{"docs/other.md": "# Other\n"}, wantErr: "generated:event-actions, generated:events has no block"},
		{
			name:  "a row edited by hand",
			docs:  map[string]string{"README.md": strings.Replace(currentReadme, "| `workflow_job` | none |", "| `workflow_job` | `ci.job_queued` |", 1), "docs/events.md": currentEventsDoc},
			stale: []string{"README.md events"},
		},
		{name: "an empty block", docs: map[string]string{"README.md": emptied, "docs/events.md": currentEventsDoc}, stale: []string{"README.md events"}},
		{
			name:  "a stale block in docs",
			docs:  map[string]string{"README.md": currentReadme, "docs/events.md": strings.Replace(currentEventsDoc, "`queued`", "`waiting`", 1)},
			stale: []string{"docs/events.md event-actions"},
		},
		{name: "an unknown name", docs: map[string]string{"README.md": "# x\n<!-- BEGIN generated:nope -->\n<!-- END generated:nope -->\n"}, wantErr: "README.md:2:"},
		{name: "a BEGIN without its END", docs: map[string]string{"README.md": "# x\n\n<!-- BEGIN generated:events -->\n| old |\n"}, wantErr: "README.md:3:"},
		{name: "an END without its BEGIN", docs: map[string]string{"docs/a.md": "<!-- END generated:events -->\n"}, wantErr: "docs/a.md:1:"},
		{
			name:    "a nested block",
			docs:    map[string]string{"README.md": "<!-- BEGIN generated:events -->\n<!-- BEGIN generated:event-actions -->\n<!-- END generated:event-actions -->\n<!-- END generated:events -->\n"},
			wantErr: "README.md:2:",
		},
		{
			name:    "a repeated block",
			docs:    map[string]string{"README.md": currentReadme + "\n<!-- BEGIN generated:events -->\n<!-- END generated:events -->\n"},
			wantErr: "README.md:12:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := mapFS(tc.docs)
			s := source{fsys: fsys, tables: fixtureTables, events: fixtureEvents}
			updates, err := s.regenerate()
			if tc.wantErr != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
					t.Fatalf("got error %v, want one that starts with %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var stale []string
			for _, u := range updates {
				for _, block := range u.blocks {
					stale = append(stale, u.file+" "+block)
				}
				fsys[u.file].Data = u.content
			}
			if !slices.Equal(stale, tc.stale) {
				t.Errorf("stale blocks: got %q, want %q", stale, tc.stale)
			}
			if again, err := s.regenerate(); err != nil || len(again) > 0 {
				t.Errorf("a second run must change nothing, got %d updates and error %v", len(again), err)
			}
			for file, content := range map[string]string{"README.md": currentReadme, "docs/events.md": currentEventsDoc} {
				if f, ok := fsys[file]; ok && string(f.Data) != content {
					t.Errorf("%s after the run:\n%s\nwant:\n%s", file, f.Data, content)
				}
			}
		})
	}
}

func TestSettingsTable(t *testing.T) {
	t.Parallel()
	code := map[string]string{
		// A constant that names a variable is no read.
		"cmd/faucet/main.go": "package main\n\nimport \"os\"\n\nconst (\n\tenvPort = \"PORT\"\n\tdefaultPort = \"8080\"\n\tmaxDays = 30\n\tenvUnread = \"NOT_READ\"\n)\n\n" +
			"func main() { load(os.Getenv) }\n\nfunc load(getenv func(string) string) {\n\t_ = getenv(envPort)\n\t_, _ = os.LookupEnv(\"HOOK_KEY\")\n\tfor _, name := range []string{envPort, \"HOOK_KEY\"} {\n\t\t_ = getenv(name)\n\t}\n}\n",
		// A file behind a build tag and a test file read no variable of a default build.
		"cmd/faucet/e2e.go":       "//go:build faucet_e2e\n\npackage main\n\nfunc e2e(getenv func(string) string) { getenv(\"E2E_ONLY\") }\n",
		"cmd/faucet/main_test.go": "package main\n\nimport \"os\"\n\nvar testOnly = os.Getenv(\"TEST_ONLY\")\n",
		"faucet/key.go":           "package faucet\n\nconst keyBytes = 32\n",
	}
	block := "<!-- BEGIN generated:settings -->\n| Variable | Required | Purpose |\n| --- | --- | --- |\n" +
		"| `PORT` | no | Listen port. Default 8080. |\n| `HOOK_KEY` | for webhooks | 32 bytes, kept 30 days. |\n<!-- END generated:settings -->\n"
	registry := []setting{{"PORT", "no", "Listen port. Default {defaultPort}."}, {"HOOK_KEY", "for webhooks", "{faucet.keyBytes} bytes, kept {maxDays} days."}}
	cases := []struct {
		name     string
		registry []setting
		// extra is one more file of cmd/faucet.
		extra string
		// wantErr are the names that the error must mention.
		wantErr []string
	}{
		{name: "the registry matches the code", registry: registry},
		{name: "a variable missing from the registry", registry: registry[:1], wantErr: []string{"HOOK_KEY"}},
		{name: "a variable the code does not read", registry: append(slices.Clone(registry), setting{"GONE", "no", "x"}), wantErr: []string{"GONE"}},
		{name: "a constant that no read takes", registry: append(slices.Clone(registry), setting{"NOT_READ", "no", "x"}), wantErr: []string{"NOT_READ"}},
		{name: "a variable that only a test or a tagged build reads", registry: append(slices.Clone(registry), setting{"TEST_ONLY", "no", "x"}, setting{"E2E_ONLY", "no", "x"}), wantErr: []string{"TEST_ONLY", "E2E_ONLY"}},
		{name: "both ways at once", registry: []setting{registry[0], {"GONE", "no", "x"}}, wantErr: []string{"HOOK_KEY", "GONE"}},
		{
			name: "a read of a variable that holds the name", registry: registry,
			extra:   "package main\n\nfunc other(getenv func(string) string) {\n\tname := envUnread\n\t_ = getenv(name)\n}\n",
			wantErr: []string{"cmd/faucet/extra.go:5:6: cannot name the variable that this read takes"},
		},
		{
			name: "a wrapper that reads its parameter", registry: registry,
			extra:   "package main\n\nimport \"os\"\n\nfunc envOr(name, fallback string) string {\n\tif v := os.Getenv(name); v != \"\" {\n\t\treturn v\n\t}\n\treturn fallback\n}\n\nvar _ = envOr(\"FAUCET_X\", \"x\")\n",
			wantErr: []string{"cmd/faucet/extra.go:6:10: cannot name the variable that this read takes"},
		},
		{
			name: "a wrapper whose parameter shares a loop variable's name", registry: registry,
			extra:   "package main\n\nfunc first(getenv func(string) string) {\n\tfor _, name := range []string{envPort} {\n\t\t_ = getenv(name)\n\t}\n}\n\nfunc envOr(getenv func(string) string, name, fallback string) string {\n\tif v := getenv(name); v != \"\" {\n\t\treturn v\n\t}\n\treturn fallback\n}\n",
			wantErr: []string{"cmd/faucet/extra.go:10:10: cannot name the variable that this read takes"},
		},
		{
			name: "a name read after its loop", registry: registry,
			extra:   "package main\n\nfunc other(getenv func(string) string) {\n\tfor _, name := range []string{envPort} {\n\t\t_ = getenv(name)\n\t}\n\tname := envUnread + \"x\"\n\t_ = getenv(name)\n}\n",
			wantErr: []string{"cmd/faucet/extra.go:8:6: cannot name the variable that this read takes"},
		},
		{
			name: "a reader under another name", registry: registry,
			extra:   "package main\n\nfunc other(env func(string) string) { _ = env(\"FAUCET_X\") }\n",
			wantErr: []string{"cmd/faucet/extra.go:3:12: the parameter env can read the environment"},
		},
		{name: "an unknown placeholder", registry: []setting{registry[0], {"HOOK_KEY", "no", "Default {nope}."}}, wantErr: []string{"{nope}"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := map[string]string{"README.md": block}
			for name, src := range code {
				files[name] = src
			}
			if tc.extra != "" {
				files["cmd/faucet/extra.go"] = tc.extra
			}
			updates, err := source{fsys: mapFS(files), tables: map[string]table{"settings": settingsTable}, settings: tc.registry}.regenerate()
			if len(tc.wantErr) == 0 {
				if err != nil || len(updates) > 0 {
					t.Fatalf("got %d updates and error %v, want the table to be current", len(updates), err)
				}
				return
			}
			if err == nil {
				t.Fatal("got no error")
			}
			for _, name := range tc.wantErr {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error %q does not name %s", err, name)
				}
			}
		})
	}
}

func TestCheckNames(t *testing.T) {
	t.Parallel()
	tree := map[string]string{
		"cmd/faucet/main.go": "package main\n\nimport \"os\"\n\nconst (\n\tenvKey = \"FAUCET_KEY\"\n\tliteral = \"FAUCET_LITERAL\"\n)\n\n" +
			"func load(getenv func(string) string) {\n\tgetenv(envKey)\n\tgetenv(\"DATABASE_URL\")\n\tos.Getenv(\"PORT\")\n\tos.LookupEnv(\"CRON_SECRET\")\n}\n\n" +
			"func routes(r chi.Router) {\n\tr.Get(\"/cron/purge\", nil)\n\tr.Method(http.MethodPost, \"/webhook/github\", nil)\n\tr.Handle(\"/mcp\", nil)\n}\n",
		"cmd/faucet/e2e.go":                "//go:build faucet_e2e\n\npackage main\n\nfunc e2e(r chi.Router) { r.Get(\"/e2e\", nil) }\n",
		"faucet/db_test.go":                "package faucet_test\n\nimport \"os\"\n\nvar dbVar = os.Getenv(\"FAUCET_TEST_ONLY\")\n",
		"faucet/internal/pgtest/pgtest.go": "package pgtest\n\nimport \"os\"\n\nfunc URL() string { return os.Getenv(\"FAUCET_TEST_DATABASE_URL\") }\n",
		"verify/go.mod":                    "module github.com/coder/superblink-events/verify\n",
		"verify/run.go":                    "package verify\n\nimport \"os\"\n\nvar v = os.Getenv(\"FAUCET_VERIFY_ONLY\")\n",
		"docs/a.md":                        "# A\n\n## Second part\n\n## Twice\n\n## Twice\n\n```sh\n## Not a heading\n```\n",
		"docs/images/map.png":              "",
		"faucet/redelivery/x.go":           "package redelivery\n",
	}
	cases := []struct {
		name   string
		readme string
		// want lists "line name" for every problem.
		want []string
	}{
		{
			name: "names that exist",
			readme: "# R\n\nSet `FAUCET_KEY`, `FAUCET_TEST_DATABASE_URL` or any `FAUCET_*`. Call `GET /cron/purge`, `POST /webhook/github`,\n" +
				"`DELETE /mcp`, `/mcp` and `/cron/*`. GitHub answers `GET /user`.\n" +
				"Set `PORT`, `DATABASE_URL`, `Authorization: Bearer $CRON_SECRET` or `<CRON_SECRET>`. Neon sets `POSTGRES_*` and `PG*`. Send one `POST`.\n" +
				"Point the hook at `https://<your host>/webhook/github` and clients at https://<production domain>/mcp, on https://<your host>.\n" +
				"```sh\nDATABASE_URL=x FAUCET_KEY=y go run .\n```\n" +
				"[a](docs/a.md#second-part) [b](docs/a.md) [c](#r) [d](faucet/redelivery) [e](docs/a.md#twice-1) [f](https://example.com/nope.md#x) [g](mailto:x@example.com)\n" +
				`<picture><source srcset="docs/images/map.png 2x, docs/images/map.png"><img src="docs/images/map.png" alt="map"></picture>` + "\n" +
				"Code is skipped: `[x](nope.md)`\n```md\n[y](nope.md)\n```\n",
		},
		{name: "an unknown FAUCET_ name", readme: "Set `FAUCET_NOPE`.\n", want: []string{"1 FAUCET_NOPE"}},
		{name: "a FAUCET_ name that only verify/ has", readme: "x\nFAUCET_VERIFY_ONLY\n", want: []string{"2 FAUCET_VERIFY_ONLY"}},
		{name: "a FAUCET_ name that the code has but does not read", readme: "Set `FAUCET_LITERAL`.\n", want: []string{"1 FAUCET_LITERAL"}},
		{name: "a FAUCET_ name that only a test reads", readme: "FAUCET_TEST_ONLY\n", want: []string{"1 FAUCET_TEST_ONLY"}},
		{name: "an unknown variable in a code span", readme: "Set `CRON_SECRETS`, `Bearer $NOPE_TOKEN` and `HOST`, not CRON_SECRETS.\n", want: []string{"1 CRON_SECRETS", "1 HOST", "1 NOPE_TOKEN"}},
		{name: "an unknown variable that a code fence sets", readme: "```sh\nNOPE_URL=x go run .\n```\n", want: []string{"2 NOPE_URL"}},
		{
			name:   "a placeholder URL whose path is no route",
			readme: "Use `https://<your host>/webhook/githubx`.\nOr https://<production domain>/mcpx.\n```\ncurl https://<your host>/cron/nope\n```\n",
			want:   []string{"1 https://<your host>/webhook/githubx", "2 https://<production domain>/mcpx", "4 https://<your host>/cron/nope"},
		},
		{name: "an unknown FAUCET_ prefix", readme: "`FAUCET_NOPE_*`\n", want: []string{"1 FAUCET_NOPE_*"}},
		{name: "an unknown route", readme: "Call `GET /cron/nope`.\n", want: []string{"1 GET /cron/nope"}},
		{name: "an unknown route in a code fence", readme: "```sh\ncurl\nGET /cron/nope\n```\n", want: []string{"3 GET /cron/nope"}},
		{name: "a wrong method", readme: "`POST /cron/purge`\n", want: []string{"1 POST /cron/purge"}},
		{name: "a route behind a build tag", readme: "`GET /e2e`\n", want: []string{"1 GET /e2e"}},
		{name: "an unknown path", readme: "`/cron/nope` and `/nope/*`\n", want: []string{"1 /cron/nope", "1 /nope/*"}},
		{
			name:   "a missing file",
			readme: "[x](docs/nope.md)\n<img src=\"docs/images/nope.png\">\n<source srcset=\"docs/images/map.png 1x, docs/images/gone.png 2x\">\n",
			want:   []string{"1 docs/nope.md", "2 docs/images/nope.png", "3 docs/images/gone.png"},
		},
		{
			name:   "a missing anchor",
			readme: "# R\n[x](docs/a.md#third) [y](#nowhere) [z](docs/a.md#twice-2) [w](docs/a.md#not-a-heading)\n",
			want:   []string{"2 docs/a.md#third", "2 #nowhere", "2 docs/a.md#twice-2", "2 docs/a.md#not-a-heading"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := map[string]string{"README.md": tc.readme}
			for name, content := range tree {
				files[name] = content
			}
			problems, err := checkNames(mapFS(files))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, p := range problems {
				if p.file == "README.md" {
					got = append(got, fmt.Sprintf("%d %s", p.line, p.name))
				} else {
					t.Errorf("unexpected problem in a fixture doc: %s", p)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnvReadsOutsideCmdFaucet(t *testing.T) {
	t.Parallel()
	read := func(pkg, name string) string {
		return "package " + pkg + "\n\nimport \"os\"\n\nvar _ = os.Getenv(\"" + name + "\")\n"
	}
	_, err := envReads(mapFS(map[string]string{
		"faucet/internal/pgtest/pgtest.go": read("pgtest", "CI"),
		"cmd/faucet/main.go":               read("main", "PORT"),
	}))
	if err != nil {
		t.Fatalf("got %v, want the listed reads to pass", err)
	}
	_, err = envReads(mapFS(map[string]string{
		"faucet/other.go":                  read("faucet", "FAUCET_OTHER"),
		"faucet/internal/pgtest/pgtest.go": read("pgtest", "FAUCET_NEW"),
	}))
	for _, want := range []string{"faucet reads FAUCET_OTHER", "faucet/internal/pgtest reads FAUCET_NEW"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not say %q", err, want)
		}
	}
}
