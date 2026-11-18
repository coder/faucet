package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

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

	require.NoError(t, store.RecordOwner(ctx, faucet.Owner{ID: 7, Login: "Coder"}))
	_, _, err = clients(t, getenv, "create", "-resource", "https://faucet.example.com/github/mcp", "-grant", "org:coder", "-grant", "all", "triage")
	require.NoError(t, err)
	triage, err := store.Client(ctx, "triage")
	require.NoError(t, err)
	require.Equal(t, faucet.AllScopes(), triage.Scopes, "a client holds every scope by default")
	require.Equal(t, []string{"org:Coder#7", "all"}, triage.Grants, "an org grant names the account that holds the login, under the login that faucet holds")

	out, _, err = clients(t, getenv, "list")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3)
	require.Regexp(t, `^CLIENT ID\s+RESOURCE\s+SCOPES\s+GRANTS\s+CREATED\s+DISABLED$`, lines[0])
	require.Regexp(t, `^ci-watcher\s+https://faucet.example.com/github/mcp\s+events:read\s+repo:coder/faucet,repo:coder/coder\s+\S+Z\s+-$`, lines[1])
	require.Regexp(t, `^triage\s+\S+\s+\S+\s+org:Coder#7,all\s+\S+Z\s+-$`, lines[2])
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

	require.NoError(t, store.RecordOwner(context.Background(), faucet.Owner{ID: 7, Login: "coder"}))
	for name, args := range map[string][]string{
		"no resource":                {"create", "-grant", "repo:coder/faucet", "ci"},
		"a resource of no endpoint":  {"create", "-resource", "https://faucet.example.com/api", "-grant", "repo:coder/faucet", "ci"},
		"a resource with a slash":    {"create", "-resource", "https://faucet.example.com/github/mcp/", "-grant", "repo:coder/faucet", "ci"},
		"a resource of a bad origin": {"create", "-resource", "http://faucet.example.com/github/mcp", "-grant", "repo:coder/faucet", "ci"},
		"the old /mcp path":          {"create", "-resource", "https://faucet.example.com/mcp", "-grant", "repo:coder/faucet", "ci"},
		"no grant":                   {"create", "-resource", resource, "ci"},
		"an org grant of a repo":     {"create", "-resource", resource, "-grant", "org:coder/faucet", "ci"},
		"an org grant of an account": {"create", "-resource", resource, "-grant", "org:coder#7", "ci"},
		"an org grant of no account": {"create", "-resource", resource, "-grant", "org:nobody", "ci"},
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

// An org grant names the account that holds its login when the client is
// made, so faucet must have heard of the login from a delivery.
func TestTheClientsCommandRefusesAnOrgGrantOfALoginThatFaucetHasNotSeen(t *testing.T) {
	t.Parallel()
	getenv, store := newClientsDatabase(t)
	_, _, err := clients(t, getenv, "create", "-resource", "https://faucet.example.com/github/mcp", "-grant", "org:coder", "triage")
	require.ErrorContains(t, err, "faucet has received no webhook delivery from coder or a repository of it, so it cannot tell which account org:coder names")
	_, err = store.Client(context.Background(), "triage")
	require.ErrorIs(t, err, oauth.ErrUnknownClient, "no client is made")
}

// With FAUCET_PUBLIC_URL set, as on the server, a client can be made only for
// that server's endpoint, and -resource defaults to it.
func TestTheClientsCommandTakesTheEndpointOfFAUCETPUBLICURL(t *testing.T) {
	t.Parallel()
	withURL, store := newClientsDatabase(t)
	getenv := func(name string) string {
		if name == envPublicURL {
			return "https://faucet.example.com"
		}
		return withURL(name)
	}

	_, _, err := clients(t, getenv, "create", "-grant", "repo:coder/faucet", "ci-watcher")
	require.NoError(t, err)
	c, err := store.Client(context.Background(), "ci-watcher")
	require.NoError(t, err)
	require.Equal(t, "https://faucet.example.com/github/mcp", c.Resource)

	_, _, err = clients(t, getenv, "create", "-resource", "https://other.example.com/github/mcp", "-grant", "repo:coder/faucet", "elsewhere")
	require.ErrorContains(t, err, "https://faucet.example.com/github/mcp")
	_, _, err = clients(t, getenv, "create", "-resource", "https://faucet.example.com/mcp", "-grant", "repo:coder/faucet", "old")
	require.Error(t, err)

	bad := func(name string) string {
		if name == envPublicURL {
			return "https://faucet.example.com/"
		}
		return withURL(name)
	}
	_, _, err = clients(t, bad, "list")
	require.ErrorContains(t, err, envPublicURL)
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

func subscriptionOf(principal, callbackURL string, expiresAt time.Time) faucet.Subscription {
	arguments := json.RawMessage(`{"repo":"coder/faucet"}`)
	return faucet.Subscription{
		KeyHash: faucet.NewSubscriptionKeyHash(principal, callbackURL, "ci.run_completed", arguments), Principal: principal,
		CallbackURL: callbackURL, EventType: "ci.run_completed", EventTypes: []string{"ci.run_completed"}, Arguments: arguments,
		Target: faucet.RepoTarget(42), SecretEnc: []byte("sealed"), ExpiresAt: expiresAt,
	}
}

// Faucet owns the subscriptions of a service, so disabling its client ends
// them and the deliveries they queued, and leaves every other caller's.
func TestDisablingAClientDeletesItsSubscriptions(t *testing.T) {
	t.Parallel()
	getenv, store := newClientsDatabase(t)
	ctx := context.Background()
	_, _, err := clients(t, getenv, "create", "-resource", "https://faucet.example.com/github/mcp", "-grant", "repo:coder/faucet", "ci-watcher")
	require.NoError(t, err)
	now := time.Now()
	mine := []faucet.Subscription{
		subscriptionOf("svc:ci-watcher", "https://hooks.example.test/1", now.Add(time.Hour)),
		subscriptionOf("svc:ci-watcher", "https://hooks.example.test/2", now.Add(time.Hour)),
	}
	other := subscriptionOf("alice", "https://hooks.example.test/1", now.Add(time.Hour))
	for _, sub := range append(mine, other) {
		_, err := store.UpsertSubscription(ctx, sub, now, 10)
		require.NoError(t, err)
	}

	out, _, err := clients(t, getenv, "disable", "ci-watcher")
	require.NoError(t, err)
	require.Contains(t, out, "deleted its 2 subscriptions")
	for _, sub := range mine {
		deleted, err := store.DeleteSubscription(ctx, sub.Principal, sub.KeyHash)
		require.NoError(t, err)
		require.False(t, deleted, "the client's subscription is gone")
	}
	deleted, err := store.DeleteSubscription(ctx, other.Principal, other.KeyHash)
	require.NoError(t, err)
	require.True(t, deleted, "another caller's subscription stays")
}

// DATABASE_URL is Neon's pooled URL on Vercel, so the commands open it as
// the server does, or every statement with a parameter fails behind the
// pooler.
func TestTheClientsCommandWorksThroughATransactionModePooler(t *testing.T) {
	t.Parallel()
	direct, _ := newClientsDatabase(t)
	pooled := throughPooler(t, direct(envDatabaseURL))
	getenv := func(name string) string { return map[string]string{envDatabaseURL: pooled}[name] }

	_, _, err := clients(t, getenv, "create", "-resource", "https://faucet.example.com/github/mcp", "-grant", "repo:coder/faucet", "ci-watcher")
	require.NoError(t, err)
	out, _, err := clients(t, getenv, "list")
	require.NoError(t, err)
	require.Contains(t, out, "ci-watcher")
	_, _, err = clients(t, getenv, "disable", "ci-watcher")
	require.NoError(t, err)
}

// The pooler of the test above fails a statement that is parsed and bound
// in two round trips, as a real one does.
func TestTheTestPoolerFailsAStatementBoundInASecondRoundTrip(t *testing.T) {
	t.Parallel()
	direct, _ := newClientsDatabase(t)
	db, err := dbconn.Open(throughPooler(t, direct(envDatabaseURL)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	var one int
	err = db.QueryRowContext(context.Background(), "SELECT $1::int", 1).Scan(&one)
	require.ErrorContains(t, err, "unnamed prepared statement does not exist")
}

// A client ID that no client can have is refused before the database sees it,
// which would fail on a NUL byte or bytes that are not UTF-8.
func TestDisablingAnImpossibleClientIDIsRefused(t *testing.T) {
	t.Parallel()
	getenv, _ := newClientsDatabase(t)
	for _, id := range []string{"a\x00b", "\xff", "CI"} {
		_, _, err := clients(t, getenv, "disable", id)
		require.ErrorContains(t, err, "a client ID is", "%q", id)
	}
}
