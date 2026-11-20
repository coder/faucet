package main

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"

	"github.com/coder/superblink-events/faucet"
)

// tables renders each generated block by name.
var tables = map[string]table{
	"events":           eventsTable,
	"event-actions":    eventActionsTable,
	"settings":         settingsTable,
	"give-up-reasons":  giveUpTable,
	"callback-reasons": callbackTable,
	"cron-replies":     cronRepliesTable,
	"raw-events":       rawEventsTable,
}

func eventsTable(s source) ([]string, error) {
	rows := []string{"| Family | GitHub event | Exact types |", "| --- | --- | --- |"}
	for _, family := range families(s.events) {
		var exact []string
		for _, e := range s.events {
			if e.Exact && e.Family == family.Name {
				exact = append(exact, e.Name)
			}
		}
		rows = append(rows, fmt.Sprintf("| `%s` | `%s` | %s |", family.Name, family.GitHubEvent, codeList(exact)))
	}
	return rows, nil
}

func eventActionsTable(s source) ([]string, error) {
	rows := []string{"| Family | Actions | Filters |", "| --- | --- | --- |"}
	for _, family := range families(s.events) {
		rows = append(rows, fmt.Sprintf("| `%s` | %s | %s |", family.Name, codeList(family.Actions), codeList(family.Filters)))
	}
	return rows, nil
}

// rawEventsTable lists the GitHub events that FAUCET_RAW_EVENTS may name,
// and the ones that faucet refuses.
func rawEventsTable(source) ([]string, error) {
	return []string{
		"| `FAUCET_RAW_EVENTS` | GitHub events |",
		"| --- | --- |",
		"| may list | " + codeList(faucet.RawEventNames()) + " |",
		"| refuses | " + codeList(faucet.RefusedRawEvents()) + " |",
	}, nil
}

func families(events []faucet.EventName) []faucet.EventName {
	return slices.DeleteFunc(slices.Clone(events), func(e faucet.EventName) bool { return e.Name != e.Family })
}

func codeList(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return "`" + strings.Join(items, "`, `") + "`"
}

// setting is one environment variable that cmd/faucet reads. A purpose names
// a number or a value of the code as a placeholder (see fill), so the table
// cannot drift from the code.
type setting struct {
	name     string
	required string
	purpose  string
}

var settings = []setting{
	{"DATABASE_URL", "yes", "Postgres connection for requests. A transaction-mode pooler works."},
	{"DATABASE_URL_UNPOOLED", "no", "Direct connection for migrations, when `DATABASE_URL` is a pooler."},
	{"GITHUB_WEBHOOK_SECRET", "yes", "The GitHub webhook's secret, at least {faucet.minSecretBytes} bytes."},
	{"PORT", "no", "Listen port. Default {defaultPort}."},
	{"FAUCET_GITHUB_API_URL", "no", "GitHub API base URL. Default `{defaultAPIURL}`."},
	{"CRON_SECRET", "for cron routes", "Bearer token for `/cron/*`: {minCronSecretBytes} to {maxTokenBytes} printable ASCII characters, no spaces. Unset turns the cron routes off."},
	{"FAUCET_RETENTION_DAYS", "no", "How many days events are kept, from 1 to {maxRetentionDays}. Default {defaultRetentionDays}."},
	{"FAUCET_WEBHOOK_KEY", "for webhooks", "{faucet.webhookKeyBytes} random bytes, base64. Seals subscriber secrets at rest."},
	{"FAUCET_WEBHOOK_CALLBACK_VERIFICATION", "no", "How webhook mode checks callback URLs: `{verifyAllowlist}` (default) or `{verifyHandshake}`."},
	{"FAUCET_WEBHOOK_ALLOWED_ORIGINS", "in `{verifyAllowlist}` mode", "Comma-separated `https://host[:port]` origins that may receive webhooks. Must be unset in `{verifyHandshake}` mode."},
	{"FAUCET_HOOK_TOKEN", "for redelivery", "GitHub token that lists and redelivers the hook's deliveries."},
	{"FAUCET_HOOK_ID", "for redelivery", "Numeric ID of the GitHub webhook to watch for failed deliveries."},
	{"FAUCET_HOOK_ORG", "for an org hook", "The organization that owns the hook. Set this or `FAUCET_HOOK_REPO`, not both."},
	{"FAUCET_HOOK_REPO", "for a repo hook", "The repository that owns the hook, as `owner/name`."},
	{"FAUCET_PUBLIC_URL", "yes", "Superblink Events' public URL, such as `https://superblink-events.example.com`, or `http` on a loopback host such as `http://localhost:8080`, with no path or trailing slash. Superblink Events' login server lives there, and every caller logs in there: service clients (see [docs/service-accounts.md](docs/service-accounts.md)) and the browser login. The server refuses to start without it, except on Vercel production, where an unset one is `https://` and `VERCEL_PROJECT_PRODUCTION_URL`. Set it there anyway to pin it: a shorter custom domain added later changes that URL, which ends every browser session and access token (see [docs/deploy.md](docs/deploy.md#the-public-url))."},
	{"FAUCET_GITHUB_APP_CLIENT_ID", "for the browser login", "The client ID of Superblink Events' GitHub App. With the secret and the token key it turns on the browser login of coding agents (see [docs/browser-login.md](docs/browser-login.md))."},
	{"FAUCET_GITHUB_APP_CLIENT_SECRET", "for the browser login", "A client secret of Superblink Events' GitHub App."},
	{"FAUCET_TOKEN_KEY", "for the browser login", "The keys that seal the GitHub tokens of users at rest: one or more `<key_id>:<32 random bytes, base64>`, comma-separated. The first seals, every one opens (see [rotating the key](docs/browser-login.md#rotate-the-token-key))."},
	{"FAUCET_GITHUB_URL", "no", "GitHub's web host, which serves the App's login page. Default `{defaultGitHubURL}`."},
	{"FAUCET_RAW_EVENTS", "for raw events", "Comma-separated GitHub events, such as `push,release,issues`, that Superblink Events offers raw as `github.<event>` (see [docs/events.md](docs/events.md#raw-events)). Unset offers none. The server refuses to start on an event whose payload GitHub shows only to people with more than read access."},
	{"VERCEL", "set by Vercel", "Marks a Vercel deployment. There the server never migrates without `VERCEL_ENV`."},
	{"VERCEL_ENV", "set by Vercel", "When set, the server migrates only if it is `production`, and otherwise starts only on a current schema."},
	{"VERCEL_PROJECT_PRODUCTION_URL", "set by Vercel", "The host of the project's production deployment: its shortest custom domain, or its `vercel.app` domain. Vercel sets it in every environment, and only in production does Superblink Events take it for an unset `FAUCET_PUBLIC_URL`."},
}

func settingsTable(s source) ([]string, error) {
	cmd, err := parsePackage(s.fsys, "cmd/faucet")
	if err != nil {
		return nil, err
	}
	listed := make([]string, len(s.settings))
	for i, st := range s.settings {
		listed[i] = st.name
	}
	reads, err := packageEnvReads(cmd)
	if err != nil {
		return nil, err
	}
	if err := compareNames(listed, reads, "cmd/faucet reads %s, which the settings registry lacks", "the settings registry lists %s, which cmd/faucet does not read"); err != nil {
		return nil, err
	}
	values, err := constValues(s.fsys)
	if err != nil {
		return nil, err
	}
	rows := []string{"| Variable | Required | Purpose |", "| --- | --- | --- |"}
	for _, st := range s.settings {
		required, err := fill(st.required, values)
		if err != nil {
			return nil, fmt.Errorf("setting %s: %w", st.name, err)
		}
		purpose, err := fill(st.purpose, values)
		if err != nil {
			return nil, fmt.Errorf("setting %s: %w", st.name, err)
		}
		rows = append(rows, fmt.Sprintf("| `%s` | %s | %s |", st.name, required, purpose))
	}
	return rows, nil
}

// compareNames fails unless listed, the names of a registry, are exactly
// found, the names in the code. lacks and extra describe the names that only
// the code has and those that only the registry has, as a format for the
// list of them.
func compareNames(listed, found []string, lacks, extra string) error {
	var onlyCode, onlyListed []string
	for _, name := range found {
		if !slices.Contains(listed, name) {
			onlyCode = append(onlyCode, name)
		}
	}
	for _, name := range listed {
		if !slices.Contains(found, name) {
			onlyListed = append(onlyListed, name)
		}
	}
	var problems []string
	if len(onlyCode) > 0 {
		slices.Sort(onlyCode)
		problems = append(problems, fmt.Sprintf(lacks, strings.Join(onlyCode, ", ")))
	}
	if len(onlyListed) > 0 {
		slices.Sort(onlyListed)
		problems = append(problems, fmt.Sprintf(extra, strings.Join(onlyListed, ", ")))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// constValues returns the constants that a registry may name: {name} for one
// of cmd/faucet and {faucet.name} for one of package faucet.
func constValues(fsys fs.FS) (map[string]goConst, error) {
	values := map[string]goConst{}
	for prefix, dir := range map[string]string{"": "cmd/faucet", "faucet.": "faucet"} {
		pkg, err := parsePackage(fsys, dir)
		if err != nil {
			return nil, err
		}
		for name, c := range readConsts(pkg) {
			values[prefix+name] = c
		}
	}
	return values, nil
}

// placeholderRE matches {name} and {name:KiB}. A constant fills in as its
// text, and :KiB writes a byte count as a whole number of KiB.
var placeholderRE = regexp.MustCompile(`\{([A-Za-z][A-Za-z0-9_.]*)(:KiB)?\}`)

func fill(text string, values map[string]goConst) (string, error) {
	var problems []string
	out := placeholderRE.ReplaceAllStringFunc(text, func(m string) string {
		sub := placeholderRE.FindStringSubmatch(m)
		c, ok := values[sub[1]]
		switch {
		case !ok:
			problems = append(problems, "no constant of cmd/faucet or package faucet for "+m)
			return ""
		case sub[2] == ":KiB":
			kib, err := c.kibText()
			if err != nil {
				problems = append(problems, m+": "+err.Error())
			}
			return kib
		default:
			return c.text()
		}
	})
	if len(problems) > 0 {
		return "", errors.New(strings.Join(problems, "; "))
	}
	return out, nil
}
