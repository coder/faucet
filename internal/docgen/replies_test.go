package main

import (
	"strings"
	"testing"
)

const fixtureCron = `package main

import (
	"net/http"

	"github.com/coder/superblink-events/faucet"
	"github.com/go-chi/chi/v5"
)

type cronRoutes struct {
	svc *faucet.Service
}

func (c cronRoutes) mount(r chi.Router) {
	r.Get("/cron/purge", c.guard(c.purge))
	r.Get("/other", http.NotFound)
}

func (c cronRoutes) guard(next http.HandlerFunc) http.HandlerFunc { return next }

func (c cronRoutes) purge(w http.ResponseWriter, r *http.Request) {
	res, err := c.svc.Purge(r.Context())
	status, failure := http.StatusOK, ""
	if err != nil {
		status, failure = http.StatusInternalServerError, "purge failed"
	}
	type counts struct {
		Limit int ` + "`json:\"limit\"`" + `
		Gone  int ` + "`json:\"gone\"`" + `
	}
	writeJSONStatus(w, status, struct {
		Deleted int    ` + "`json:\"deleted\"`" + `
		Kept    int    ` + "`json:\"kept,omitempty\"`" + `
		Counts  counts ` + "`json:\"counts\"`" + `
		Skipped int    ` + "`json:\"-\"`" + `
		Error   string ` + "`json:\"error,omitempty\"`" + `
	}{int(res.Deleted), res.Kept, counts{res.Limit, res.Gone}, 0, failure})
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {}
`

const fixtureService = `package faucet

import "context"

type Service struct{}

func (s *Service) Purge(ctx context.Context) (PurgeResult, error) { return PurgeResult{}, nil }

type PurgeResult struct {
	// Deleted is the number of events deleted.
	Deleted int64
	Kept    int // Kept is a line comment.
	// Limit and Gone count the deliveries that ended.
	Limit int
	Gone  int
}
`

func TestCronRepliesTable(t *testing.T) {
	t.Parallel()
	doc := "<!-- BEGIN generated:cron-replies -->\n| Route | Reply fields | Documented on |\n| --- | --- | --- |\n" +
		"| `GET /cron/purge` | `deleted`, `kept`, `counts.limit`, `counts.gone`, `error` | `faucet.PurgeResult` |\n<!-- END generated:cron-replies -->\n"
	cases := []struct {
		name     string
		cron     string
		service  string
		wantErrs []string
	}{
		{name: "the table matches the code", cron: fixtureCron, service: fixtureService},
		{
			name:    "fields set by name",
			cron:    strings.Replace(fixtureCron, "{int(res.Deleted), res.Kept, counts{res.Limit, res.Gone}, 0, failure}", "{Error: failure, Deleted: int(res.Deleted), Kept: res.Kept, Counts: counts{Gone: res.Gone, Limit: res.Limit}}", 1),
			service: fixtureService,
		},
		{
			name:     "a result field without a doc comment",
			cron:     fixtureCron,
			service:  strings.Replace(fixtureService, "\t// Limit and Gone count the deliveries that ended.\n", "", 1),
			wantErrs: []string{"counts.limit", "faucet.PurgeResult.Limit", "counts.gone", "faucet.PurgeResult.Gone"},
		},
		{
			name:     "a group comment that does not name the field",
			cron:     fixtureCron,
			service:  strings.Replace(fixtureService, "Limit and Gone count", "Limit counts", 1),
			wantErrs: []string{"faucet.PurgeResult.Gone"},
		},
		{
			name:     "a blank line ends a group",
			cron:     fixtureCron,
			service:  strings.Replace(fixtureService, "\tLimit int\n", "\tLimit int\n\n", 1),
			wantErrs: []string{"faucet.PurgeResult.Gone"},
		},
		{
			name:     "a result whose type the code does not show",
			cron:     strings.Replace(fixtureCron, "c.svc.Purge(r.Context())", "purgeNow(r.Context())", 1),
			service:  fixtureService,
			wantErrs: []string{"the reply field deleted reads res"},
		},
		{
			name:     "no cron handler",
			cron:     strings.Replace(fixtureCron, "c.guard(c.purge)", "http.NotFound", 1),
			service:  fixtureService,
			wantErrs: []string{"cmd/faucet/cron.go:15:2: GET /cron/purge has no handler that is a method of cronRoutes"},
		},
		{
			name:     "an inline cron handler",
			cron:     strings.Replace(fixtureCron, "c.guard(c.purge)", "func(w http.ResponseWriter, r *http.Request) { c.purge(w, r) }", 1),
			service:  fixtureService,
			wantErrs: []string{"GET /cron/purge has an inline handler"},
		},
		{
			name:     "a cron path in a constant",
			cron:     strings.Replace(strings.Replace(fixtureCron, `r.Get("/cron/purge", c.guard(c.purge))`, `r.Get(purgePath, c.guard(c.purge))`, 1), "type cronRoutes struct", "const purgePath = \"/cron/purge\"\n\ntype cronRoutes struct", 1),
			service:  fixtureService,
			wantErrs: []string{"the path of this Get is not a string literal"},
		},
		{
			name:     "a route group",
			cron:     strings.Replace(fixtureCron, `r.Get("/other", http.NotFound)`, `r.Route("/cron", func(r chi.Router) { r.Get("/deliver", http.NotFound) })`, 1),
			service:  fixtureService,
			wantErrs: []string{"Route registers routes that docgen cannot follow"},
		},
		{
			name:     "a method that docgen cannot name",
			cron:     strings.Replace(fixtureCron, `r.Get("/other", http.NotFound)`, `r.Method(verb, "/other", http.NotFound)`, 1),
			service:  fixtureService,
			wantErrs: []string{"cannot read the HTTP method of this route"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := map[string]string{"docs/deploy.md": doc, "cmd/faucet/cron.go": tc.cron, "faucet/service.go": tc.service}
			updates, err := source{fsys: mapFS(files), tables: map[string]table{"cron-replies": cronRepliesTable}}.regenerate()
			if len(tc.wantErrs) == 0 {
				if err != nil || len(updates) > 0 {
					for _, u := range updates {
						t.Logf("%s", u.content)
					}
					t.Fatalf("got %d updates and error %v, want the table to be current", len(updates), err)
				}
				return
			}
			if err == nil {
				t.Fatal("got no error")
			}
			for _, want := range tc.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %s", err, want)
				}
			}
		})
	}
}
