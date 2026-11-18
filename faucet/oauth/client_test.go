package oauth_test

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
)

var bothScopes = []faucet.Scope{faucet.ScopeEventsRead, faucet.ScopeEventsSubscribe}

// A client sends its secret in HTTP Basic authentication, URL-encoded first
// as RFC 6749 section 2.3.1 says. A secret in standard base64 holds '+', '/'
// and '=', which that encoding changes, so the server would see another
// secret. The secret is base64url, which it leaves alone.
func TestNewClientMintsASecretThatBasicEncodingLeavesAlone(t *testing.T) {
	t.Parallel()

	form := regexp.MustCompile(`^fcs_[A-Za-z0-9_-]{43}$`)
	seen := map[string]bool{}
	for range 2000 {
		c, secret, err := oauth.NewClient("ci-watcher", "https://faucet.example.com/github/mcp", bothScopes, []string{"repo:coder/faucet"}, time.Now())
		require.NoError(t, err)
		require.Regexp(t, form, secret)
		require.Equal(t, secret, url.QueryEscape(secret))
		require.Equal(t, oauth.HashToken(secret), c.SecretHash, "the client keeps the hash of the secret")
		require.False(t, seen[secret])
		seen[secret] = true
	}
}

func TestNewClientRefusesAClientThatCannotWork(t *testing.T) {
	t.Parallel()

	const resource = "https://faucet.example.com/github/mcp"
	grants := []string{"repo:coder/faucet"}
	for name, mk := range map[string]func() (oauth.Client, string, error){
		"no grant": func() (oauth.Client, string, error) {
			return oauth.NewClient("ci", resource, bothScopes, nil, time.Now())
		},
		"an org grant of a repository": func() (oauth.Client, string, error) {
			return oauth.NewClient("ci", resource, bothScopes, []string{"org:coder/faucet"}, time.Now())
		},
		"an org grant of no login": func() (oauth.Client, string, error) {
			return oauth.NewClient("ci", resource, bothScopes, []string{"org:"}, time.Now())
		},
		"an org grant of no account": func() (oauth.Client, string, error) {
			return oauth.NewClient("ci", resource, bothScopes, []string{"org:coder"}, time.Now())
		},
		"an org grant of account 0": func() (oauth.Client, string, error) {
			return oauth.NewClient("ci", resource, bothScopes, []string{"org:coder#0"}, time.Now())
		},
		"an org grant of account 07": func() (oauth.Client, string, error) {
			return oauth.NewClient("ci", resource, bothScopes, []string{"org:coder#07"}, time.Now())
		},
		"an org grant of an account and no login": func() (oauth.Client, string, error) {
			return oauth.NewClient("ci", resource, bothScopes, []string{"org:#7"}, time.Now())
		},
		"all of a repository": func() (oauth.Client, string, error) {
			return oauth.NewClient("ci", resource, bothScopes, []string{"all:coder/faucet"}, time.Now())
		},
		"a bare repo": func() (oauth.Client, string, error) {
			return oauth.NewClient("ci", resource, bothScopes, []string{"coder/faucet"}, time.Now())
		},
		"a grant of ..": func() (oauth.Client, string, error) {
			return oauth.NewClient("ci", resource, bothScopes, []string{"repo:coder/.."}, time.Now())
		},
		"no scope": func() (oauth.Client, string, error) { return oauth.NewClient("ci", resource, nil, grants, time.Now()) },
		"an unknown one": func() (oauth.Client, string, error) {
			return oauth.NewClient("ci", resource, []faucet.Scope{"events:all"}, grants, time.Now())
		},
		"no resource": func() (oauth.Client, string, error) { return oauth.NewClient("ci", "", bothScopes, grants, time.Now()) },
		"an empty ID": func() (oauth.Client, string, error) {
			return oauth.NewClient("", resource, bothScopes, grants, time.Now())
		},
		"a hyphen at end": func() (oauth.Client, string, error) {
			return oauth.NewClient("ci-", resource, bothScopes, grants, time.Now())
		},
		"a percent": func() (oauth.Client, string, error) {
			return oauth.NewClient("ci%2D1", resource, bothScopes, grants, time.Now())
		},
		"65 bytes": func() (oauth.Client, string, error) {
			return oauth.NewClient(strings.Repeat("a", 65), resource, bothScopes, grants, time.Now())
		},
	} {
		_, secret, err := mk()
		require.Error(t, err, name)
		require.Empty(t, secret, name)
	}
	_, _, err := oauth.NewClient(strings.Repeat("a", 64), resource, bothScopes, grants, time.Now())
	require.NoError(t, err, "64 bytes is the longest ID")
	c, _, err := oauth.NewClient("ci", resource, bothScopes, []string{"repo:coder/faucet", "org:coder#7", "all"}, time.Now())
	require.NoError(t, err, "a grant names a repository, an account or every repository")
	require.Equal(t, []string{"repo:coder/faucet", "org:coder#7", "all"}, c.Grants)
}

// An operator writes an org grant by login, and the client holds it for the
// account that holds the login now, under the login that faucet holds.
func TestBindGrantsNamesTheAccountThatHoldsTheLoginNow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	owners := fakeRepos{owners: map[string]faucet.Owner{"coder": {ID: 7, Login: "Coder"}}}

	bound, err := oauth.BindGrants(ctx, owners, []string{"repo:coder/faucet", "org:coder", "all"})
	require.NoError(t, err)
	require.Equal(t, []string{"repo:coder/faucet", "org:Coder#7", "all"}, bound)

	_, err = oauth.BindGrants(ctx, owners, []string{"org:octo"})
	require.ErrorIs(t, err, faucet.ErrUnknownOwner)
	require.ErrorContains(t, err, "faucet has received no webhook delivery from octo or a repository of it, so it cannot tell which account org:octo names")
	for _, grant := range []string{"org:coder#7", "org:coder/faucet", "repo:coder", "everything"} {
		_, err = oauth.BindGrants(ctx, owners, []string{grant})
		require.ErrorContains(t, err, "is not repo:<owner>/<name>, org:<login> or all", "%s: an operator names no account ID", grant)
	}
}
