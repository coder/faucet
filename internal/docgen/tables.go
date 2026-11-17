package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/coder/faucet/faucet"
)

// tables renders each generated block by name, header rows included.
var tables = map[string]func(source) ([]string, error){
	"events":        eventsTable,
	"event-actions": eventActionsTable,
	"settings":      settingsTable,
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
// a number or a value of the code as {constName} for a constant of cmd/faucet
// and {faucet.constName} for one of package faucet, so the table cannot drift
// from the code.
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
	{"VERCEL", "set by Vercel", "Marks a Vercel deployment. There the server never migrates without `VERCEL_ENV`."},
	{"VERCEL_ENV", "set by Vercel", "When set, the server migrates only if it is `production`, and otherwise starts only on a current schema."},
}

func settingsTable(s source) ([]string, error) {
	cmd, err := readConsts(s.fsys, "cmd/faucet")
	if err != nil {
		return nil, err
	}
	core, err := readConsts(s.fsys, "faucet")
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	var envs []string
	for name, c := range cmd {
		values[name] = c.value
		if c.str && strings.HasPrefix(name, "env") {
			envs = append(envs, c.value)
		}
	}
	for name, c := range core {
		values["faucet."+name] = c.value
	}
	if err := compareSettings(s.settings, envs); err != nil {
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

// compareSettings fails unless the registry lists exactly the variables that
// cmd/faucet names in its env* constants.
func compareSettings(registry []setting, envs []string) error {
	listed := map[string]bool{}
	for _, st := range registry {
		listed[st.name] = true
	}
	var unlisted, unread []string
	for _, env := range envs {
		if !listed[env] {
			unlisted = append(unlisted, env)
		}
	}
	for _, st := range registry {
		if !slices.Contains(envs, st.name) {
			unread = append(unread, st.name)
		}
	}
	var problems []string
	if len(unlisted) > 0 {
		slices.Sort(unlisted)
		problems = append(problems, "cmd/faucet reads "+strings.Join(unlisted, ", ")+", which the settings registry lacks")
	}
	if len(unread) > 0 {
		problems = append(problems, "the settings registry lists "+strings.Join(unread, ", ")+", which no env* constant of cmd/faucet names")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

var placeholderRE = regexp.MustCompile(`\{([A-Za-z][A-Za-z0-9_.]*)\}`)

func fill(text string, values map[string]string) (string, error) {
	var unknown []string
	out := placeholderRE.ReplaceAllStringFunc(text, func(m string) string {
		v, ok := values[m[1:len(m)-1]]
		if !ok {
			unknown = append(unknown, m)
		}
		return v
	})
	if len(unknown) > 0 {
		return "", fmt.Errorf("no constant of cmd/faucet or package faucet for %s", strings.Join(unknown, ", "))
	}
	return out, nil
}
