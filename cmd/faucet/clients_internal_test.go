package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/superblink-events/cmd/faucet/internal/dbconn"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
	"github.com/coder/superblink-events/faucet/pgstore"
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
	hash := oauth.HashToken(secret)
	require.Equal(t, &hash, c.SecretHash, "the store keeps the hash of the secret it printed")
	require.Equal(t, "https://faucet.example.com/github/mcp", c.Resource)
	require.Equal(t, []faucet.Scope{faucet.ScopeEventsRead}, c.Scopes)
	require.Equal(t, []string{"repo:coder/faucet", "repo:coder/coder"}, c.Grants)

	require.NoError(t, store.RecordOwner(ctx, faucet.Owner{ID: 7, Login: "Coder"}))
	_, notice, err = clients(t, getenv, "create", "-resource", "https://faucet.example.com/github/mcp", "-grant", "org:coder", "-grant", "all", "triage")
	require.NoError(t, err)
	require.Contains(t, notice, "The client holds org:Coder#7, all.", "the operator sees which account the org grant names")
	triage, err := store.Client(ctx, "triage")
	require.NoError(t, err)
	require.Equal(t, faucet.AllScopes(), triage.Scopes, "a client holds every scope by default")
	require.Equal(t, []string{"org:Coder#7", "all"}, triage.Grants, "an org grant names the account that holds the login, under the login that faucet holds")

	out, _, err = clients(t, getenv, "list")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3)
	require.Regexp(t, `^CLIENT ID\s+RESOURCE\s+SCOPES\s+GRANTS\s+CREATED\s+DISABLED\s+SECRET\s+TRUST RULES$`, lines[0])
	require.Regexp(t, `^ci-watcher\s+https://faucet.example.com/github/mcp\s+events:read\s+repo:coder/faucet,repo:coder/coder\s+\S+Z\s+-\s+yes\s+0$`, lines[1])
	require.Regexp(t, `^triage\s+\S+\s+\S+\s+org:Coder#7,all\s+\S+Z\s+-\s+yes\s+0$`, lines[2])
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
	require.NotRegexp(t, `(?m)^ci-watcher.*\s-\s+yes\s+0$`, out, "the list shows when it was disabled")

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
		"no resource":                     {"create", "-grant", "repo:coder/faucet", "ci"},
		"a resource of no endpoint":       {"create", "-resource", "https://faucet.example.com/api", "-grant", "repo:coder/faucet", "ci"},
		"a resource with a slash":         {"create", "-resource", "https://faucet.example.com/github/mcp/", "-grant", "repo:coder/faucet", "ci"},
		"a resource of a bad origin":      {"create", "-resource", "http://faucet.example.com/github/mcp", "-grant", "repo:coder/faucet", "ci"},
		"the old /mcp path":               {"create", "-resource", "https://faucet.example.com/mcp", "-grant", "repo:coder/faucet", "ci"},
		"no grant":                        {"create", "-resource", resource, "ci"},
		"an org grant of a repo":          {"create", "-resource", resource, "-grant", "org:coder/faucet", "ci"},
		"an org grant of another account": {"create", "-resource", resource, "-grant", "org:coder#8", "ci"},
		"an org grant of account 07":      {"create", "-resource", resource, "-grant", "org:coder#07", "ci"},
		"an org grant of no account":      {"create", "-resource", resource, "-grant", "org:nobody", "ci"},
		"an unknown scope":                {"create", "-resource", resource, "-scope", "events:read,events:admin", "-grant", "repo:coder/faucet", "ci"},
		"no client ID":                    {"create", "-resource", resource, "-grant", "repo:coder/faucet"},
		"a flag after the ID":             {"create", "-resource", resource, "ci", "-grant", "repo:coder/faucet"},
		"an upper-case ID":                {"create", "-resource", resource, "-grant", "repo:coder/faucet", "CI"},
		"an unknown command":              {"delete", "ci"},
		"no command":                      {},
		"disable with no ID":              {"disable"},
	} {
		out, _, err := clients(t, getenv, args...)
		require.Error(t, err, name)
		require.Empty(t, out, name)
	}
	all, err := store.Clients(context.Background())
	require.NoError(t, err)
	require.Empty(t, all)
}

// An operator can name the account of an org grant too, and the command makes
// the client only when that account holds the login now.
func TestTheClientsCommandTakesAnOrgGrantWithTheAccountThatHoldsItsLogin(t *testing.T) {
	t.Parallel()
	getenv, store := newClientsDatabase(t)
	ctx := context.Background()
	require.NoError(t, store.RecordOwner(ctx, faucet.Owner{ID: 7, Login: "coder"}))
	const resource = "https://faucet.example.com/github/mcp"

	_, _, err := clients(t, getenv, "create", "-resource", resource, "-grant", "org:coder#8", "elsewhere")
	require.ErrorContains(t, err, "org:coder#8 names account 8, and faucet's latest delivery gives coder to account 7")
	_, _, err = clients(t, getenv, "create", "-resource", resource, "-grant", "org:Coder#7", "triage")
	require.NoError(t, err)
	triage, err := store.Client(ctx, "triage")
	require.NoError(t, err)
	require.Equal(t, []string{"org:coder#7"}, triage.Grants)
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

// On Vercel production the commands take the public URL as the server does,
// so a client is made for the endpoint that the server serves, unless the
// operator names another with -resource.
func TestTheClientsCommandTakesThePublicURLFromVercelAsTheServerDoes(t *testing.T) {
	t.Parallel()
	withDB, store := newClientsDatabase(t)
	vercel := map[string]string{envVercel: "1", envVercelEnv: "production", envVercelProductionURL: "faucet.example.com"}
	getenv := func(name string) string {
		if v, ok := vercel[name]; ok {
			return v
		}
		return withDB(name)
	}

	_, notice, err := clients(t, getenv, "create", "-grant", "repo:coder/faucet", "ci-watcher")
	require.NoError(t, err)
	require.Contains(t, notice, "public URL taken from VERCEL_PROJECT_PRODUCTION_URL: https://faucet.example.com")
	c, err := store.Client(context.Background(), "ci-watcher")
	require.NoError(t, err)
	require.Equal(t, "https://faucet.example.com/github/mcp", c.Resource)
	// Production can pin FAUCET_PUBLIC_URL to another domain, and an
	// environment pulled from Vercel lacks a sensitive value, so the derived
	// URL is only the default.
	_, _, err = clients(t, getenv, "create", "-resource", "https://events.example.com/github/mcp", "-grant", "repo:coder/faucet", "pinned")
	require.NoError(t, err)
	pinned, err := store.Client(context.Background(), "pinned")
	require.NoError(t, err)
	require.Equal(t, "https://events.example.com/github/mcp", pinned.Resource)
	_, _, err = clients(t, getenv, "create", "-resource", "https://events.example.com/mcp", "-grant", "repo:coder/faucet", "old-path")
	require.ErrorContains(t, err, "-resource must be faucet's public URL and /github/mcp", "the shape is still checked")

	vercel[envVercelEnv] = "preview"
	_, _, err = clients(t, getenv, "create", "-grant", "repo:coder/faucet", "preview")
	require.ErrorContains(t, err, "-resource must be faucet's public URL", "a preview derives nothing, as the server does not")
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

const (
	codercomIssuer = "https://oidc.vercel.com/codercom"
	codercomTeam   = "team_tGkWfhEGGelkkqUUm9nXq17r"
	// The project ID of the example token on vercel.com/docs/oidc/reference.
	digestProject  = "prj_7Gw5ZMBpQA8h9GF832KGp7nwbuh3"
	clusterIssuer  = "https://k8s.wi-test.invalid"
	clusterAccount = "system:serviceaccount:apps:digest-bot"
	clusterJWKS    = "../../faucet/oauth/testdata/k8s-jwks.json"
)

func trustVercel(id string, claims ...string) []string {
	args := []string{"trust", id, "-issuer", codercomIssuer}
	for _, c := range claims {
		args = append(args, "-claim", c)
	}
	return args
}

var vercelPins = []string{"owner_id=" + codercomTeam, "project_id=" + digestProject, "environment=production"}

func TestTheClientsCommandMakesAClientForWorkloadsAndManagesItsTrustRules(t *testing.T) {
	t.Parallel()
	getenv, store := newClientsDatabase(t)
	ctx := context.Background()
	const resource = "https://faucet.example.com/github/mcp"

	out, notice, err := clients(t, getenv, "create", "-resource", resource, "-no-secret", "-grant", "repo:coder/faucet", "digest-bot")
	require.NoError(t, err)
	require.Equal(t, "client_id: digest-bot\n", out, "a client with no secret prints no secret")
	require.Contains(t, notice, "The client has no secret: give it a trust rule with faucet clients trust digest-bot.")
	c, err := store.Client(ctx, "digest-bot")
	require.NoError(t, err)
	require.Nil(t, c.SecretHash)

	out, _, err = clients(t, getenv, trustVercel("digest-bot", vercelPins...)...)
	require.NoError(t, err)
	require.Regexp(t, `^rule_id: [1-9][0-9]*\n$`, out)
	vercelID := strings.TrimSpace(strings.TrimPrefix(out, "rule_id: "))
	again, _, err := clients(t, getenv, trustVercel("digest-bot", "environment=production", "project_id="+digestProject, "owner_id="+codercomTeam)...)
	require.NoError(t, err)
	require.Equal(t, out, again, "the same rule, its claims in another order, is the rule that the client holds")
	pasted, err := os.ReadFile(clusterJWKS)
	require.NoError(t, err)
	var indented bytes.Buffer
	require.NoError(t, json.Indent(&indented, pasted, "", "  "))
	keysFile := filepath.Join(t.TempDir(), "jwks.json")
	require.NoError(t, os.WriteFile(keysFile, []byte(strings.Replace(indented.String(), "{", `{"comment": "the cluster's keys",`, 1)), 0o600))
	out, _, err = clients(t, getenv, "trust", "digest-bot", "-issuer", clusterIssuer, "-claim", "sub="+clusterAccount, "-jwks-file", keysFile)
	require.NoError(t, err)
	clusterID := strings.TrimSpace(strings.TrimPrefix(out, "rule_id: "))
	require.NotEqual(t, vercelID, clusterID)

	rules, err := store.TrustRules(ctx, "digest-bot")
	require.NoError(t, err)
	require.Len(t, rules, 2)
	require.Equal(t, map[string]string{"owner_id": codercomTeam, "project_id": digestProject, "environment": "production"}, rules[0].Rule.Claims)
	require.Empty(t, rules[0].Rule.JWKS)
	require.JSONEq(t, string(pasted), string(rules[1].Rule.JWKS), "the store keeps the keys of the file, and no other member")
	require.NoError(t, oauth.ValidateTrustRule(rules[1].Rule), "the stored rule is the one that trust checked")

	out, _, err = clients(t, getenv, "trusts", "digest-bot")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3)
	require.Regexp(t, `^RULE ID\s+ISSUER\s+CLAIMS\s+KEYS\s+CREATED$`, lines[0])
	require.Regexp(t, `^`+vercelID+`\s+https://oidc\.vercel\.com/codercom\s+\{"environment":"production","owner_id":"`+codercomTeam+`","project_id":"`+digestProject+`"\}\s+discovery\s+\S+Z$`, lines[1])
	require.Regexp(t, `^`+clusterID+`\s+https://k8s\.wi-test\.invalid\s+\{"sub":"system:serviceaccount:apps:digest-bot"\}\s+pasted\s+\S+Z$`, lines[2])
	out, _, err = clients(t, getenv, "list")
	require.NoError(t, err)
	require.Regexp(t, `(?m)^digest-bot\s+.*\s-\s+no\s+2$`, out, "the list shows that the client has no secret and two rules")

	out, _, err = clients(t, getenv, "untrust", "digest-bot", vercelID)
	require.NoError(t, err)
	require.Equal(t, "Trust rule "+vercelID+" of digest-bot is removed. A token that a workload got with it works until it expires.\n", out)
	_, _, err = clients(t, getenv, "untrust", "digest-bot", vercelID)
	require.ErrorContains(t, err, `client "digest-bot" has no trust rule `+vercelID)
	rules, err = store.TrustRules(ctx, "digest-bot")
	require.NoError(t, err)
	require.Len(t, rules, 1)
	require.Equal(t, clusterIssuer, rules[0].Rule.Issuer)
}

// A claim value can hold a comma, an equals sign or a quote, so trusts prints
// the claims of a rule as one JSON object, which no value can end early.
func TestTrustsPrintsTheClaimsOfARuleAsOneJSONObject(t *testing.T) {
	t.Parallel()
	getenv, _ := newClientsDatabase(t)
	_, _, err := clients(t, getenv, "create", "-resource", "https://faucet.example.com/github/mcp", "-no-secret", "-grant", "repo:coder/faucet", "digest-bot")
	require.NoError(t, err)
	_, _, err = clients(t, getenv, "trust", "digest-bot", "-issuer", clusterIssuer,
		"-claim", "sub=system:serviceaccount:apps:bot,environment=production", "-claim", `kubernetes.io/namespace="apps"`)
	require.NoError(t, err)
	out, _, err := clients(t, getenv, "trusts", "digest-bot")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 2)
	require.Equal(t, `{"kubernetes.io/namespace":"\"apps\"","sub":"system:serviceaccount:apps:bot,environment=production"}`, strings.Fields(lines[1])[2])
}

// trust runs ValidateTrustRule first, so its errors say what to pin and why,
// and a rule that the store keeps is one that a workload could match.
func TestTheClientsCommandRefusesATrustRuleThatCannotWork(t *testing.T) {
	t.Parallel()
	getenv, store := newClientsDatabase(t)
	ctx := context.Background()
	const resource = "https://faucet.example.com/github/mcp"
	for _, id := range []string{"digest-bot", "retired"} {
		_, _, err := clients(t, getenv, "create", "-resource", resource, "-no-secret", "-grant", "repo:coder/faucet", id)
		require.NoError(t, err)
	}
	_, _, err := clients(t, getenv, "disable", "retired")
	require.NoError(t, err)
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.json")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	private := filepath.Join(dir, "private.json")
	require.NoError(t, os.WriteFile(private, []byte(`{"keys":[{"kty":"oct","kid":"k","k":"c2VjcmV0"}]}`), 0o600))
	pasted, err := os.ReadFile(clusterJWKS)
	require.NoError(t, err)
	tooLong := filepath.Join(dir, "too-long.json")
	require.NoError(t, os.WriteFile(tooLong, []byte(`{"keys":[],"x":"`+strings.Repeat("a", 32<<10)+`"}`), 0o600))
	withBOM := filepath.Join(dir, "bom.json")
	require.NoError(t, os.WriteFile(withBOM, append([]byte("\xef\xbb\xbf"), pasted...), 0o600))
	nulEscape := filepath.Join(dir, "nul.json")
	require.NoError(t, os.WriteFile(nulEscape, []byte(strings.Replace(string(pasted), `{"use":"sig",`, `{"note":"\u0000","use":"sig",`, 1)), 0o600))
	keysTwice := filepath.Join(dir, "keys-twice.json")
	require.NoError(t, os.WriteFile(keysTwice, []byte(`{"keys":[{"kty":"oct","kid":"k","k":"c2VjcmV0"}],"KEYS":`+strings.TrimPrefix(string(pasted), `{"keys":`)), 0o600))
	cluster := func(extra ...string) []string {
		return append([]string{"trust", "digest-bot", "-issuer", clusterIssuer, "-claim", "sub=" + clusterAccount}, extra...)
	}

	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no production pin":     {trustVercel("digest-bot", vercelPins[:2]...), "a trust rule for https://oidc.vercel.com/codercom must pin environment=production: a preview deployment runs any branch"},
		"a preview":             {trustVercel("digest-bot", vercelPins[0], vercelPins[1], "environment=preview"), "must pin environment=production"},
		"no team":               {trustVercel("digest-bot", vercelPins[1:]...), "must pin owner_id: Vercel signs the tokens of every team with the same keys"},
		"every team's issuer":   {[]string{"trust", "digest-bot", "-issuer", "https://oidc.vercel.com", "-claim", vercelPins[0]}, "faucet never trusts https://oidc.vercel.com, which every Vercel team shares"},
		"no sub":                {[]string{"trust", "digest-bot", "-issuer", clusterIssuer, "-claim", "kubernetes.io/namespace=apps"}, "a trust rule for https://k8s.wi-test.invalid must pin sub"},
		"an http issuer":        {[]string{"trust", "digest-bot", "-issuer", "http://k8s.wi-test.invalid", "-claim", "sub=" + clusterAccount}, "a trust rule's issuer must be an https URL"},
		"no issuer":             {[]string{"trust", "digest-bot", "-claim", "sub=" + clusterAccount}, "a trust rule's issuer must be an https URL"},
		"no claim":              {[]string{"trust", "digest-bot", "-issuer", clusterIssuer}, "a trust rule must pin at least one claim"},
		"an empty value":        {[]string{"trust", "digest-bot", "-issuer", clusterIssuer, "-claim", "sub="}, "neither the name nor the value can be empty"},
		"a claim with no value": {cluster("-claim", "aud"), "a claim is <name>=<value>"},
		"a claim pinned twice":  {cluster("-claim", "sub=system:serviceaccount:apps:other"), "claim sub is pinned twice"},
		"an aud pin":            {cluster("-claim", "aud=https://faucet.example.com"), "a trust rule cannot pin aud"},
		"a key set of no file":  {cluster("-jwks-file", filepath.Join(dir, "missing.json")), "-jwks-file: "},
		"an empty key set":      {cluster("-jwks-file", empty), "-jwks-file " + empty + " is empty"},
		"a key set of no path":  {cluster("-jwks-file", ""), `invalid value "" for flag -jwks-file: it names no file`},
		"a key set over 32 KiB": {cluster("-jwks-file", tooLong), "-jwks-file " + tooLong + " is over 32768 bytes, the most that a key set can hold"},
		"a byte order mark":     {cluster("-jwks-file", withBOM), "-jwks-file " + withBOM + " starts with a UTF-8 byte order mark, which JSON does not allow: save it without one"},
		"a private key":         {cluster("-jwks-file", private), "a trust rule's JWKS must hold no private key"},
		"keys twice":            {cluster("-jwks-file", keysTwice), "a trust rule's JWKS must name its keys array once"},
		"a NUL escape":          {cluster("-jwks-file", nulEscape), "a trust rule's JWKS must be valid UTF-8, with no \\u0000"},
		"a NUL in a claim":      {cluster("-claim", "kubernetes.io/namespace=a\x00b"), "a trust rule's claim names and values must be valid UTF-8 with no control character"},
		"a name with a space":   {cluster("-claim", "ref =x"), `a trust rule's claim names cannot start or end with white space, as "ref " does`},
		"an unknown client":     {[]string{"trust", "nobody", "-issuer", clusterIssuer, "-claim", "sub=" + clusterAccount}, `there is no client "nobody"`},
		"a disabled client":     {[]string{"trust", "retired", "-issuer", clusterIssuer, "-claim", "sub=" + clusterAccount}, `client "retired" is disabled, so no workload can act as it`},
		"an upper-case ID":      {[]string{"trust", "Digest-Bot", "-issuer", clusterIssuer, "-claim", "sub=" + clusterAccount}, "a client ID is"},
		"the ID after flags":    {[]string{"trust", "-issuer", clusterIssuer, "-claim", "sub=" + clusterAccount, "digest-bot"}, "name the client ID before the flags"},
		"an argument after":     {cluster("digest-bot"), "name the client ID before the flags"},
		"no client ID":          {[]string{"trust"}, "usage:"},
	} {
		out, _, err := clients(t, getenv, c.args...)
		require.ErrorContains(t, err, c.want, name)
		require.Empty(t, out, name)
	}
	for _, id := range []string{"digest-bot", "retired"} {
		rules, err := store.TrustRules(ctx, id)
		require.NoError(t, err)
		require.Empty(t, rules, "%s holds no rule", id)
	}
}

func TestTheClientsCommandRemovesATrustRuleOfThatClientOnly(t *testing.T) {
	t.Parallel()
	getenv, store := newClientsDatabase(t)
	ctx := context.Background()
	for _, id := range []string{"digest-bot", "ci-watcher"} {
		_, _, err := clients(t, getenv, "create", "-resource", "https://faucet.example.com/github/mcp", "-no-secret", "-grant", "repo:coder/faucet", id)
		require.NoError(t, err)
	}
	out, _, err := clients(t, getenv, trustVercel("digest-bot", vercelPins...)...)
	require.NoError(t, err)
	ruleID := strings.TrimSpace(strings.TrimPrefix(out, "rule_id: "))

	_, _, err = clients(t, getenv, "untrust", "ci-watcher", ruleID)
	require.ErrorContains(t, err, `client "ci-watcher" has no trust rule `+ruleID)
	_, _, err = clients(t, getenv, "untrust", "digset-bot", ruleID)
	require.ErrorContains(t, err, `there is no client "digset-bot"`, "a typo in the client ID is no missing rule")
	for name, args := range map[string][]string{
		"no rule ID":       {"untrust", "digest-bot"},
		"a rule ID of 0":   {"untrust", "digest-bot", "0"},
		"a negative ID":    {"untrust", "digest-bot", "-" + ruleID},
		"a name":           {"untrust", "digest-bot", "vercel"},
		"an unknown ID":    {"untrust", "digest-bot", "999999"},
		"an unknown owner": {"untrust", "nobody", ruleID},
		"trusts of nobody": {"trusts", "nobody"},
		"trusts of two":    {"trusts", "digest-bot", "ci-watcher"},
	} {
		out, _, err := clients(t, getenv, args...)
		require.Error(t, err, name)
		require.Empty(t, out, name)
	}
	rules, err := store.TrustRules(ctx, "digest-bot")
	require.NoError(t, err)
	require.Len(t, rules, 1, "the rule stays")
}

type rulesUnread struct{ clientStore }

func (rulesUnread) TrustRules(context.Context, string) ([]oauth.StoredTrustRule, error) {
	return nil, xerrors.New("the command read the rules of a client")
}

// list counts the rules of every client in one query, and reads no rule: a
// key set can be 32 KiB, and the database can be far away.
func TestListCountsTheTrustRulesWithoutReadingThem(t *testing.T) {
	t.Parallel()
	getenv, store := newClientsDatabase(t)
	for _, id := range []string{"digest-bot", "ci-watcher"} {
		_, _, err := clients(t, getenv, "create", "-resource", "https://faucet.example.com/github/mcp", "-no-secret", "-grant", "repo:coder/faucet", id)
		require.NoError(t, err)
	}
	for _, pins := range [][]string{vercelPins, {"owner_id=" + codercomTeam, "project_id=prj_other", "environment=production"}} {
		_, _, err := clients(t, getenv, trustVercel("digest-bot", pins...)...)
		require.NoError(t, err)
	}
	var out, notice bytes.Buffer
	require.NoError(t, clientsCommand(context.Background(), []string{"list"}, rulesUnread{store}, nil, nil, time.Now(), &out, &notice))
	require.Regexp(t, `(?m)^digest-bot\s+.*\s-\s+no\s+2$`, out.String())
	require.Regexp(t, `(?m)^ci-watcher\s+.*\s-\s+no\s+0$`, out.String())
}

func TestTheTrustCommandsPrintTheirUsageForHelp(t *testing.T) {
	t.Parallel()
	getenv, _ := newClientsDatabase(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"trust", "--help"}, "usage: faucet clients trust <client-id> -issuer <url> -claim <name>=<value>"},
		{[]string{"trust", "-h"}, "usage: faucet clients trust <client-id>"},
		{[]string{"trust", "digest-bot", "-help"}, "usage: faucet clients trust <client-id>"},
		{[]string{"trusts", "--help"}, "usage: faucet clients trusts <client-id>"},
		{[]string{"untrust", "-h"}, "usage: faucet clients untrust <client-id> <rule-id>"},
		{[]string{"update", "-h"}, "usage: faucet clients update <client-id> [-scope <scopes>] [-grant <grant> [-grant ...]]"},
		{[]string{"update", "superblink", "--help"}, "usage: faucet clients update <client-id>"},
	} {
		out, notice, err := clients(t, getenv, c.args...)
		require.ErrorIs(t, err, flag.ErrHelp, c.args)
		require.Empty(t, out, c.args)
		require.Contains(t, notice, c.want, c.args)
		if c.args[0] == "trust" {
			require.Contains(t, notice, "-jwks-file", "trust lists its flags")
		}
	}
}

// update sets the scopes, the grants or both of a client in place, so the
// service keeps its ID, its secret and its trust rules.
func TestTheClientsCommandUpdatesAClientInPlace(t *testing.T) {
	t.Parallel()
	getenv, store := newClientsDatabase(t)
	ctx := context.Background()
	const resource = "https://faucet.example.com/github/mcp"
	require.NoError(t, store.RecordOwner(ctx, faucet.Owner{ID: 7, Login: "Coder"}))
	_, _, err := clients(t, getenv, "create", "-resource", resource, "-no-secret", "-scope", "events:read", "-grant", "org:coder", "superblink")
	require.NoError(t, err)
	_, _, err = clients(t, getenv, trustVercel("superblink", vercelPins...)...)
	require.NoError(t, err)
	before, err := store.Client(ctx, "superblink")
	require.NoError(t, err)

	out, notice, err := clients(t, getenv, "update", "superblink", "-scope", "events:read,events:subscribe")
	require.NoError(t, err)
	require.Equal(t, "superblink holds org:Coder#7, with the scopes events:read,events:subscribe.\n", out)
	require.Contains(t, notice, "the service needs a new token")
	after, err := store.Client(ctx, "superblink")
	require.NoError(t, err)
	want := before
	want.Scopes = []faucet.Scope{faucet.ScopeEventsRead, faucet.ScopeEventsSubscribe}
	require.Equal(t, want, after, "the client keeps its grants, its resource, its creation time and its lack of a secret")
	rules, err := store.TrustRules(ctx, "superblink")
	require.NoError(t, err)
	require.Len(t, rules, 1, "the client keeps its trust rule")

	out, _, err = clients(t, getenv, "update", "superblink", "-grant", "repo:coder/faucet", "-grant", "org:coder")
	require.NoError(t, err)
	require.Equal(t, "superblink holds repo:coder/faucet, org:Coder#7, with the scopes events:read,events:subscribe.\n", out, "an org grant is bound as create binds it, and the scopes stay")

	out, _, err = clients(t, getenv, "update", "superblink", "-scope", "events:read")
	require.NoError(t, err)
	require.Equal(t, "superblink holds repo:coder/faucet, org:Coder#7, with the scopes events:read.\nIt holds no events:subscribe, so faucet deleted its 0 subscriptions.\n", out)

	out, _, err = clients(t, getenv, "update", "superblink", "-scope", "events:subscribe", "-grant", "all")
	require.NoError(t, err)
	require.Equal(t, "superblink holds all, with the scopes events:subscribe.\n", out)
	after, err = store.Client(ctx, "superblink")
	require.NoError(t, err)
	require.Equal(t, []string{"all"}, after.Grants)
	require.Equal(t, []faucet.Scope{faucet.ScopeEventsSubscribe}, after.Scopes)
}

func TestTheClientsCommandRefusesAnUpdateThatCannotWork(t *testing.T) {
	t.Parallel()
	getenv, store := newClientsDatabase(t)
	ctx := context.Background()
	const resource = "https://faucet.example.com/github/mcp"
	require.NoError(t, store.RecordOwner(ctx, faucet.Owner{ID: 7, Login: "coder"}))
	for _, id := range []string{"ci", "gone"} {
		_, _, err := clients(t, getenv, "create", "-resource", resource, "-scope", "events:read", "-grant", "repo:coder/faucet", id)
		require.NoError(t, err)
	}
	_, _, err := clients(t, getenv, "disable", "gone")
	require.NoError(t, err)
	ci, err := store.Client(ctx, "ci")
	require.NoError(t, err)
	gone, err := store.Client(ctx, "gone")
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		args []string
		says string
	}{
		"no flag":                         {[]string{"update", "ci"}, "name -scope, -grant or both"},
		"no client ID":                    {[]string{"update", "-scope", "events:read"}, "name the client ID before the flags"},
		"a flag before the ID":            {[]string{"update", "-scope", "events:read", "ci"}, "name the client ID before the flags"},
		"an argument after the flags":     {[]string{"update", "ci", "-scope", "events:read", "more"}, "nothing after them"},
		"an upper-case ID":                {[]string{"update", "CI", "-scope", "events:read"}, "a client ID is"},
		"an unknown client":               {[]string{"update", "nobody", "-scope", "events:read"}, `there is no client "nobody"`},
		"a disabled client":               {[]string{"update", "gone", "-scope", "events:read,events:subscribe"}, "an update cannot bring it back"},
		"an empty scope list":             {[]string{"update", "ci", "-scope", ""}, "-scope"},
		"an unknown scope":                {[]string{"update", "ci", "-scope", "events:read,events:admin"}, "-scope"},
		"an org grant of a repo":          {[]string{"update", "ci", "-grant", "org:coder/faucet"}, "org:coder/faucet"},
		"an org grant of another account": {[]string{"update", "ci", "-grant", "org:coder#8"}, "names account 8"},
		"an org grant of no account":      {[]string{"update", "ci", "-grant", "org:nobody"}, "cannot tell which account"},
		"an empty grant":                  {[]string{"update", "ci", "-grant", ""}, `grant "" is not`},
	} {
		out, _, err := clients(t, getenv, tc.args...)
		require.Error(t, err, name)
		require.ErrorContains(t, err, tc.says, name)
		require.Empty(t, out, name)
	}
	got, err := store.Client(ctx, "ci")
	require.NoError(t, err)
	require.Equal(t, ci, got, "a refused update changes nothing")
	got, err = store.Client(ctx, "gone")
	require.NoError(t, err)
	require.Equal(t, gone, got, "a disabled client keeps what it held")
}

// disabledMeanwhile disables the client just before the update reaches the
// store, as a disable that races the command does.
type disabledMeanwhile struct{ clientStore }

func (s disabledMeanwhile) UpdateClient(ctx context.Context, id string, u oauth.ClientUpdate) (oauth.Client, error) {
	if _, err := s.DisableClient(ctx, id, time.Now()); err != nil {
		return oauth.Client{}, err
	}
	return s.clientStore.UpdateClient(ctx, id, u)
}

func TestAnUpdateThatADisableOvertakesChangesNothing(t *testing.T) {
	t.Parallel()
	getenv, store := newClientsDatabase(t)
	ctx := context.Background()
	_, _, err := clients(t, getenv, "create", "-resource", "https://faucet.example.com/github/mcp", "-scope", "events:read", "-grant", "repo:coder/faucet", "ci")
	require.NoError(t, err)
	var out, notice bytes.Buffer
	err = clientsCommand(ctx, []string{"update", "ci", "-grant", "all"}, disabledMeanwhile{store}, nil, nil, time.Now(), &out, &notice)
	require.ErrorContains(t, err, `client "ci" was disabled before the update, which changed nothing`)
	require.Empty(t, out.String())
	c, err := store.Client(ctx, "ci")
	require.NoError(t, err)
	require.False(t, c.Live())
	require.Equal(t, []string{"repo:coder/faucet"}, c.Grants)
}

// An org grant that the client holds stays bound to its account through the
// command, and org:<login> cannot move it to the login's new holder unseen.
func TestAnUpdateKeepsTheAccountOfAHeldOrgGrant(t *testing.T) {
	t.Parallel()
	getenv, store := newClientsDatabase(t)
	ctx := context.Background()
	require.NoError(t, store.RecordOwner(ctx, faucet.Owner{ID: 7, Login: "coder"}))
	_, _, err := clients(t, getenv, "create", "-resource", "https://faucet.example.com/github/mcp", "-grant", "org:coder", "ci")
	require.NoError(t, err)
	require.NoError(t, store.RecordOwner(ctx, faucet.Owner{ID: 8, Login: "coder"}), "another account takes the login")

	out, _, err := clients(t, getenv, "update", "ci", "-grant", "org:coder#7", "-grant", "repo:coder/faucet")
	require.NoError(t, err, "a grant copied from the list needs no lookup")
	require.Equal(t, "ci holds org:coder#7, repo:coder/faucet, with the scopes events:read,events:subscribe.\n", out)
	_, _, err = clients(t, getenv, "update", "ci", "-grant", "org:coder")
	require.ErrorContains(t, err, "the client holds org:coder#7, and coder names account 8 now: write org:coder#7 to keep that grant, or org:coder#8 for the account that holds the login now")
	c, err := store.Client(ctx, "ci")
	require.NoError(t, err)
	require.Equal(t, []string{"org:coder#7", "repo:coder/faucet"}, c.Grants)
}
