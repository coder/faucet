package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
)

// RunOAuth runs the oauth.Store contract against stores returned by
// newStore. Every call must return an empty store that does not share state
// with the others, because the subtests run in parallel.
func RunOAuth(t *testing.T, newStore func(t *testing.T) oauth.Store) {
	subtests := []struct {
		name string
		run  func(t *testing.T, s oauth.Store)
	}{
		{"AccessTokenReadsBackAsInserted", accessTokenReadsBackAsInserted},
		{"AccessTokenOfAnUnknownHashIsUnknown", accessTokenOfAnUnknownHashIsUnknown},
		{"AccessTokenReadsBackAfterItExpires", accessTokenReadsBackAfterItExpires},
		{"InsertAccessTokenRefusesASecondTokenWithTheSameHash", insertAccessTokenRefusesASecondTokenWithTheSameHash},
		{"InsertAccessTokenValidatesInput", insertAccessTokenValidatesInput},
	}
	for _, st := range subtests {
		t.Run(st.name, func(t *testing.T) {
			t.Parallel()
			st.run(t, newStore(t))
		})
	}
}

// accessTokenAt is a valid token that expires at expires. Postgres keeps
// microseconds, so the time has none below them.
func accessTokenAt(token string, expires time.Time) oauth.AccessToken {
	return oauth.AccessToken{
		Hash:        oauth.HashToken(token),
		ExpiresAt:   expires.Truncate(time.Microsecond),
		Scopes:      []faucet.Scope{faucet.ScopeEventsSubscribe, faucet.ScopeEventsRead},
		Resource:    "https://faucet.example.com/github/mcp",
		SubjectKind: oauth.SubjectService,
		Subject:     "ci-bot",
	}
}

func requireSameAccessToken(t *testing.T, want, got oauth.AccessToken) {
	t.Helper()
	require.True(t, want.ExpiresAt.Equal(got.ExpiresAt), "expires_at: want %v, got %v", want.ExpiresAt, got.ExpiresAt)
	got.ExpiresAt = want.ExpiresAt
	require.Equal(t, want, got)
}

func accessTokenReadsBackAsInserted(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	service := accessTokenAt("fct_one", time.Now().Add(15*time.Minute))
	user := accessTokenAt("fct_two", time.Now().Add(time.Hour))
	user.Scopes = []faucet.Scope{faucet.ScopeEventsRead}
	user.Resource = "https://faucet.example.com/mcp"
	user.SubjectKind, user.Subject = oauth.SubjectUser, "sess_1"
	require.NoError(t, s.InsertAccessToken(ctx, service))
	require.NoError(t, s.InsertAccessToken(ctx, user))

	got, err := s.AccessToken(ctx, oauth.HashToken("fct_one"))
	require.NoError(t, err)
	requireSameAccessToken(t, service, got)
	got, err = s.AccessToken(ctx, oauth.HashToken("fct_two"))
	require.NoError(t, err)
	requireSameAccessToken(t, user, got)
}

func accessTokenOfAnUnknownHashIsUnknown(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	require.NoError(t, s.InsertAccessToken(ctx, accessTokenAt("fct_one", time.Now().Add(time.Minute))))
	_, err := s.AccessToken(ctx, oauth.HashToken("fct_other"))
	require.ErrorIs(t, err, oauth.ErrUnknownToken)
}

// accessTokenReadsBackAfterItExpires: the verifier, not the store, decides
// what an expiry means.
func accessTokenReadsBackAfterItExpires(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	expired := accessTokenAt("fct_old", time.Now().Add(-time.Hour))
	require.NoError(t, s.InsertAccessToken(ctx, expired))
	got, err := s.AccessToken(ctx, expired.Hash)
	require.NoError(t, err)
	requireSameAccessToken(t, expired, got)
}

func insertAccessTokenRefusesASecondTokenWithTheSameHash(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	first := accessTokenAt("fct_one", time.Now().Add(time.Minute))
	require.NoError(t, s.InsertAccessToken(ctx, first))
	second := first
	second.Subject = "someone-else"
	require.Error(t, s.InsertAccessToken(ctx, second))

	got, err := s.AccessToken(ctx, first.Hash)
	require.NoError(t, err)
	requireSameAccessToken(t, first, got)
}

func insertAccessTokenValidatesInput(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	for name, mutate := range map[string]func(*oauth.AccessToken){
		"no hash":              func(a *oauth.AccessToken) { a.Hash = oauth.TokenHash{} },
		"no expiry":            func(a *oauth.AccessToken) { a.ExpiresAt = time.Time{} },
		"no scope":             func(a *oauth.AccessToken) { a.Scopes = nil },
		"an unknown scope":     func(a *oauth.AccessToken) { a.Scopes = []faucet.Scope{"events:everything"} },
		"no resource":          func(a *oauth.AccessToken) { a.Resource = "" },
		"an unknown kind":      func(a *oauth.AccessToken) { a.SubjectKind = "robot" },
		"no subject":           func(a *oauth.AccessToken) { a.Subject = "" },
		"no kind and no scope": func(a *oauth.AccessToken) { a.SubjectKind, a.Scopes = "", []faucet.Scope{} },
	} {
		token := accessTokenAt("fct_"+name, time.Now().Add(time.Minute))
		mutate(&token)
		require.Error(t, s.InsertAccessToken(ctx, token), name)
		for _, hash := range []oauth.TokenHash{oauth.HashToken("fct_" + name), {}} {
			_, err := s.AccessToken(ctx, hash)
			require.ErrorIs(t, err, oauth.ErrUnknownToken, "%s: nothing is stored", name)
		}
	}
}
