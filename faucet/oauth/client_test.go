package oauth_test

import (
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
	c, _, err := oauth.NewClient("ci", resource, bothScopes, []string{"repo:coder/faucet", "org:coder", "all"}, time.Now())
	require.NoError(t, err, "a grant names a repository, an owner or every repository")
	require.Equal(t, []string{"repo:coder/faucet", "org:coder", "all"}, c.Grants)
}
