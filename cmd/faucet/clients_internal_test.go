package main

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/cmd/faucet/internal/dbconn"
	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/faucet/faucet/pgstore"
)

// newClientsDatabase is a migrated scratch database and the environment that
// names it.
func newClientsDatabase(t *testing.T) (func(string) string, *pgstore.Store) {
	t.Helper()
	adminURL := os.Getenv(testDatabaseURLEnv)
	if adminURL == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s must be set in CI", testDatabaseURLEnv)
		}
		t.Skipf("set %s to a Postgres URL", testDatabaseURLEnv)
	}
	databaseURL := newScratchDatabaseURL(t, adminURL)
	db, err := dbconn.Open(databaseURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, pgstore.Migrate(context.Background(), db))
	return func(name string) string { return map[string]string{envDatabaseURL: databaseURL}[name] }, pgstore.New(db)
}

func clients(t *testing.T, getenv func(string) string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err = runClients(context.Background(), args, getenv, &out, &errOut)
	return out.String(), errOut.String(), err
}

func TestTheClientsCommandMakesListsAndDisablesClients(t *testing.T) {
	t.Parallel()
	getenv, store := newClientsDatabase(t)
	ctx := context.Background()

	out, notice, err := clients(t, getenv, "create", "-resource", "https://faucet.example.com/github/mcp",
		"-scope", "events:read", "-grant", "repo:coder/faucet", "-grant", "repo:coder/coder", "ci-watcher")
	require.NoError(t, err)
	match := regexp.MustCompile(`^client_id: ci-watcher\nclient_secret: (fcs_[A-Za-z0-9_-]{43})\n$`).FindStringSubmatch(out)
	require.NotNil(t, match, out)
	require.Contains(t, notice, "cannot show it again")
	secret := match[1]

	c, err := store.Client(ctx, "ci-watcher")
	require.NoError(t, err)
	require.Equal(t, oauth.HashToken(secret), c.SecretHash, "the store keeps the hash of the secret it printed")
	require.Equal(t, "https://faucet.example.com/github/mcp", c.Resource)
	require.Equal(t, []faucet.Scope{faucet.ScopeEventsRead}, c.Scopes)
	require.Equal(t, []string{"repo:coder/faucet", "repo:coder/coder"}, c.Grants)

	_, _, err = clients(t, getenv, "create", "-resource", "https://faucet.example.com/mcp", "-grant", "repo:coder/faucet", "triage")
	require.NoError(t, err)
	triage, err := store.Client(ctx, "triage")
	require.NoError(t, err)
	require.Equal(t, faucet.AllScopes(), triage.Scopes, "a client holds every scope by default")

	out, _, err = clients(t, getenv, "list")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3)
	require.Regexp(t, `^CLIENT ID\s+RESOURCE\s+SCOPES\s+GRANTS\s+CREATED\s+DISABLED$`, lines[0])
	require.Regexp(t, `^ci-watcher\s+https://faucet.example.com/github/mcp\s+events:read\s+repo:coder/faucet,repo:coder/coder\s+\S+Z\s+-$`, lines[1])
	require.NotContains(t, out, secret)

	out, _, err = clients(t, getenv, "disable", "ci-watcher")
	require.NoError(t, err)
	require.Contains(t, out, "ci-watcher is disabled since")
	c, err = store.Client(ctx, "ci-watcher")
	require.NoError(t, err)
	require.False(t, c.Live())
	_, _, err = clients(t, getenv, "disable", "ci-watcher")
	require.NoError(t, err, "disabling twice is no error")
	out, _, err = clients(t, getenv, "list")
	require.NoError(t, err)
	require.NotRegexp(t, `(?m)^ci-watcher.*\s-$`, out, "the list shows when it was disabled")

	_, _, err = clients(t, getenv, "create", "-resource", "https://faucet.example.com/github/mcp", "-grant", "repo:coder/faucet", "ci-watcher")
	require.ErrorContains(t, err, "an ID is never given to a second client")
	_, _, err = clients(t, getenv, "disable", "nobody")
	require.ErrorContains(t, err, `there is no client "nobody"`)
}

func TestTheClientsCommandRefusesAClientThatCannotWork(t *testing.T) {
	t.Parallel()
	getenv, store := newClientsDatabase(t)
	const resource = "https://faucet.example.com/github/mcp"

	for name, args := range map[string][]string{
		"no resource":                {"create", "-grant", "repo:coder/faucet", "ci"},
		"a resource of no endpoint":  {"create", "-resource", "https://faucet.example.com/api", "-grant", "repo:coder/faucet", "ci"},
		"a resource with a slash":    {"create", "-resource", "https://faucet.example.com/github/mcp/", "-grant", "repo:coder/faucet", "ci"},
		"a resource of a bad origin": {"create", "-resource", "http://faucet.example.com/github/mcp", "-grant", "repo:coder/faucet", "ci"},
		"no grant":                   {"create", "-resource", resource, "ci"},
		"an org grant":               {"create", "-resource", resource, "-grant", "org:coder", "ci"},
		"an unknown scope":           {"create", "-resource", resource, "-scope", "events:read,events:admin", "-grant", "repo:coder/faucet", "ci"},
		"no client ID":               {"create", "-resource", resource, "-grant", "repo:coder/faucet"},
		"a flag after the ID":        {"create", "-resource", resource, "ci", "-grant", "repo:coder/faucet"},
		"an upper-case ID":           {"create", "-resource", resource, "-grant", "repo:coder/faucet", "CI"},
		"an unknown command":         {"delete", "ci"},
		"no command":                 {},
		"disable with no ID":         {"disable"},
	} {
		out, _, err := clients(t, getenv, args...)
		require.Error(t, err, name)
		require.Empty(t, out, name)
	}
	all, err := store.Clients(context.Background())
	require.NoError(t, err)
	require.Empty(t, all)
}

func TestTheClientsCommandNeedsACurrentDatabase(t *testing.T) {
	t.Parallel()
	_, _, err := clients(t, func(string) string { return "" }, "list")
	require.ErrorContains(t, err, envDatabaseURL)

	adminURL := os.Getenv(testDatabaseURLEnv)
	if adminURL == "" {
		t.Skipf("set %s to a Postgres URL", testDatabaseURLEnv)
	}
	unmigrated := newScratchDatabaseURL(t, adminURL)
	_, _, err = clients(t, func(name string) string { return map[string]string{envDatabaseURL: unmigrated}[name] }, "list")
	require.ErrorIs(t, err, pgstore.ErrSchemaNotCurrent, "the command never migrates")
}
