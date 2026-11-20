package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
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
		{"AccessTokenSharesNoSliceWithItsCaller", accessTokenSharesNoSliceWithItsCaller},
		{"DeleteExpiredAccessTokensLeavesTheLiveOnes", deleteExpiredAccessTokensLeavesTheLiveOnes},
		{"DeleteAccessTokenDeletesThatTokenOnly", deleteAccessTokenDeletesThatTokenOnly},
		{"ClientReadsBackAsInserted", clientReadsBackAsInserted},
		{"AClientWithNoSecretReadsBackWithNone", aClientWithNoSecretReadsBackWithNone},
		{"ClientOfAnUnknownIDIsUnknown", clientOfAnUnknownIDIsUnknown},
		{"InsertClientRefusesAnIDThatAnyClientHeld", insertClientRefusesAnIDThatAnyClientHeld},
		{"DisableClientKeepsTheFirstTime", disableClientKeepsTheFirstTime},
		{"InsertClientValidatesInput", insertClientValidatesInput},
		{"ClientSharesNoSliceWithItsCaller", clientSharesNoSliceWithItsCaller},
		{"UpdateClientSetsWhatItNamesOnly", updateClientSetsWhatItNamesOnly},
		{"UpdateClientRefusesAnUnknownOrDisabledClient", updateClientRefusesAnUnknownOrDisabledClient},
		{"UpdateClientValidatesInput", updateClientValidatesInput},
	}
	for _, st := range subtests {
		t.Run(st.name, func(t *testing.T) {
			t.Parallel()
			st.run(t, newStore(t))
		})
	}
	runOAuthLogin(t, newStore)
	runAccessChecks(t, newStore)
	runTrustRules(t, newStore)
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

func accessTokenSharesNoSliceWithItsCaller(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	inserted := accessTokenAt("fct_one", time.Now().Add(time.Minute))
	inserted.Scopes = []faucet.Scope{faucet.ScopeEventsRead}
	want := inserted
	want.Scopes = []faucet.Scope{faucet.ScopeEventsRead}
	require.NoError(t, s.InsertAccessToken(ctx, inserted))
	inserted.Scopes[0] = faucet.ScopeEventsSubscribe

	got, err := s.AccessToken(ctx, want.Hash)
	require.NoError(t, err)
	requireSameAccessToken(t, want, got)
	got.Scopes[0] = faucet.ScopeEventsSubscribe
	again, err := s.AccessToken(ctx, want.Hash)
	require.NoError(t, err)
	requireSameAccessToken(t, want, again)
}

func deleteExpiredAccessTokensLeavesTheLiveOnes(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Microsecond)
	expired := accessTokenAt("fct_expired", now.Add(-time.Minute))
	atNow := accessTokenAt("fct_now", now)
	live := accessTokenAt("fct_live", now.Add(time.Second))
	for _, token := range []oauth.AccessToken{expired, atNow, live} {
		require.NoError(t, s.InsertAccessToken(ctx, token))
	}

	n, err := s.DeleteExpiredAccessTokens(ctx, now)
	require.NoError(t, err)
	require.EqualValues(t, 2, n, "a token that expires at now has expired")
	for _, gone := range []oauth.AccessToken{expired, atNow} {
		_, err := s.AccessToken(ctx, gone.Hash)
		require.ErrorIs(t, err, oauth.ErrUnknownToken)
	}
	got, err := s.AccessToken(ctx, live.Hash)
	require.NoError(t, err)
	requireSameAccessToken(t, live, got)
	n, err = s.DeleteExpiredAccessTokens(ctx, now)
	require.NoError(t, err)
	require.Zero(t, n)
}

func deleteAccessTokenDeletesThatTokenOnly(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Microsecond)
	revoked := accessTokenAt("fct_revoked", now.Add(time.Hour))
	kept := accessTokenAt("fct_kept", now.Add(time.Hour))
	for _, token := range []oauth.AccessToken{revoked, kept} {
		require.NoError(t, s.InsertAccessToken(ctx, token))
	}

	require.NoError(t, s.DeleteAccessToken(ctx, revoked.Hash))
	_, err := s.AccessToken(ctx, revoked.Hash)
	require.ErrorIs(t, err, oauth.ErrUnknownToken)
	got, err := s.AccessToken(ctx, kept.Hash)
	require.NoError(t, err)
	requireSameAccessToken(t, kept, got)
	require.NoError(t, s.DeleteAccessToken(ctx, revoked.Hash), "an unknown hash")
}

// clientAt is a valid live client created at created, which has no time
// below a microsecond, as Postgres keeps it.
func clientAt(id string, created time.Time) oauth.Client {
	secretHash := oauth.HashToken("fcs_" + id)
	return oauth.Client{
		ID:         id,
		SecretHash: &secretHash,
		Resource:   "https://faucet.example.com/github/mcp",
		Grants:     []string{"repo:coder/faucet", "repo:coder/coder"},
		Scopes:     []faucet.Scope{faucet.ScopeEventsRead, faucet.ScopeEventsSubscribe},
		CreatedAt:  created.Truncate(time.Microsecond),
	}
}

func requireSameClient(t *testing.T, want, got oauth.Client) {
	t.Helper()
	require.True(t, want.CreatedAt.Equal(got.CreatedAt), "created_at: want %v, got %v", want.CreatedAt, got.CreatedAt)
	require.True(t, want.DisabledAt.Equal(got.DisabledAt), "disabled_at: want %v, got %v", want.DisabledAt, got.DisabledAt)
	got.CreatedAt, got.DisabledAt = want.CreatedAt, want.DisabledAt
	require.Equal(t, want, got)
}

func clientReadsBackAsInserted(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	at := time.Now()
	first := clientAt("ci-watcher", at)
	second := clientAt("triage", at.Add(time.Second))
	second.Resource = "https://faucet.example.com/mcp"
	second.Grants = []string{"repo:coder/faucet"}
	second.Scopes = []faucet.Scope{faucet.ScopeEventsRead}
	// Inserted out of order, listed by creation.
	require.NoError(t, s.InsertClient(ctx, second))
	require.NoError(t, s.InsertClient(ctx, first))

	got, err := s.Client(ctx, "ci-watcher")
	require.NoError(t, err)
	requireSameClient(t, first, got)
	require.True(t, got.Live())
	all, err := s.Clients(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	requireSameClient(t, first, all[0])
	requireSameClient(t, second, all[1])
}

// A client made with no secret reads back with no hash, never with the zero
// hash, which would make it a client with a secret.
func aClientWithNoSecretReadsBackWithNone(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	at := time.Now()
	workload := clientAt("digest-bot", at)
	workload.SecretHash = nil
	require.NoError(t, s.InsertClient(ctx, workload))
	require.NoError(t, s.InsertClient(ctx, clientAt("ci-watcher", at.Add(time.Second))))

	got, err := s.Client(ctx, "digest-bot")
	require.NoError(t, err)
	requireSameClient(t, workload, got)
	all, err := s.Clients(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	requireSameClient(t, workload, all[0])
	require.NotNil(t, all[1].SecretHash, "the other client keeps its secret")
}

func clientOfAnUnknownIDIsUnknown(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	require.NoError(t, s.InsertClient(ctx, clientAt("ci-watcher", time.Now())))
	_, err := s.Client(ctx, "ci-watcher2")
	require.ErrorIs(t, err, oauth.ErrUnknownClient)
	_, err = s.DisableClient(ctx, "ci-watcher2", time.Now())
	require.ErrorIs(t, err, oauth.ErrUnknownClient)
	all, err := s.Clients(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
}

// A service's login is "svc:" and its client ID, and the login owns its
// subscriptions, so an ID that a client ever held is never given to another.
func insertClientRefusesAnIDThatAnyClientHeld(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	first := clientAt("ci-watcher", time.Now())
	require.NoError(t, s.InsertClient(ctx, first))
	again := clientAt("ci-watcher", time.Now())
	another := oauth.HashToken("fcs_another")
	again.SecretHash = &another
	require.ErrorIs(t, s.InsertClient(ctx, again), oauth.ErrClientExists)

	_, err := s.DisableClient(ctx, "ci-watcher", time.Now())
	require.NoError(t, err)
	require.ErrorIs(t, s.InsertClient(ctx, again), oauth.ErrClientExists, "a disabled client keeps its ID")
	got, err := s.Client(ctx, "ci-watcher")
	require.NoError(t, err)
	require.Equal(t, first.SecretHash, got.SecretHash)
}

func disableClientKeepsTheFirstTime(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	at := time.Now().Truncate(time.Microsecond)
	c := clientAt("ci-watcher", at)
	require.NoError(t, s.InsertClient(ctx, c))
	require.NoError(t, s.InsertClient(ctx, clientAt("triage", at)))

	disabled, err := s.DisableClient(ctx, "ci-watcher", at.Add(time.Minute))
	require.NoError(t, err)
	want := c
	want.DisabledAt = at.Add(time.Minute)
	requireSameClient(t, want, disabled)
	require.False(t, disabled.Live())

	again, err := s.DisableClient(ctx, "ci-watcher", at.Add(time.Hour))
	require.NoError(t, err)
	requireSameClient(t, want, again)
	got, err := s.Client(ctx, "ci-watcher")
	require.NoError(t, err)
	requireSameClient(t, want, got)
	other, err := s.Client(ctx, "triage")
	require.NoError(t, err)
	require.True(t, other.Live(), "disabling one client leaves the others live")
}

func insertClientValidatesInput(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	for name, mutate := range map[string]func(*oauth.Client){
		"no ID":              func(c *oauth.Client) { c.ID = "" },
		"an upper-case ID":   func(c *oauth.Client) { c.ID = "CI" },
		"an ID with a colon": func(c *oauth.Client) { c.ID = "ci:bot" },
		"a zero secret hash": func(c *oauth.Client) { c.SecretHash = &oauth.TokenHash{} },
		"no resource":        func(c *oauth.Client) { c.Resource = "" },
		"no scope":           func(c *oauth.Client) { c.Scopes = nil },
		"an unknown scope":   func(c *oauth.Client) { c.Scopes = []faucet.Scope{"events:everything"} },
		"a bad grant":        func(c *oauth.Client) { c.Grants = []string{"org:coder/faucet"} },
		"no creation time":   func(c *oauth.Client) { c.CreatedAt = time.Time{} },
	} {
		c := clientAt("ci-watcher", time.Now())
		mutate(&c)
		require.Error(t, s.InsertClient(ctx, c), name)
	}
	all, err := s.Clients(ctx)
	require.NoError(t, err)
	require.Empty(t, all, "nothing is stored")
}

func clientSharesNoSliceWithItsCaller(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	inserted := clientAt("ci-watcher", time.Now())
	want := clientAt("ci-watcher", inserted.CreatedAt)
	require.NoError(t, s.InsertClient(ctx, inserted))
	inserted.Grants[0], inserted.Scopes[0], *inserted.SecretHash = "repo:evil/repo", faucet.ScopeEventsSubscribe, oauth.HashToken("fcs_evil")

	got, err := s.Client(ctx, "ci-watcher")
	require.NoError(t, err)
	requireSameClient(t, want, got)
	got.Grants[0], got.Scopes[0], *got.SecretHash = "repo:evil/repo", faucet.ScopeEventsSubscribe, oauth.HashToken("fcs_evil")
	again, err := s.Client(ctx, "ci-watcher")
	require.NoError(t, err)
	requireSameClient(t, want, again)
}

// An update sets only what it names, so an update of the scopes alone keeps
// grants that another update set meanwhile.
func updateClientSetsWhatItNamesOnly(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	at := time.Now()
	c := clientAt("ci-watcher", at)
	require.NoError(t, s.InsertClient(ctx, c))
	require.NoError(t, s.InsertClient(ctx, clientAt("triage", at)))

	got, err := s.UpdateClient(ctx, "ci-watcher", oauth.ClientUpdate{Grants: []string{"org:coder#95932066"}})
	require.NoError(t, err)
	want := c
	want.Grants = []string{"org:coder#95932066"}
	requireSameClient(t, want, got)

	got, err = s.UpdateClient(ctx, "ci-watcher", oauth.ClientUpdate{Scopes: []faucet.Scope{faucet.ScopeEventsSubscribe}})
	require.NoError(t, err)
	want.Scopes = []faucet.Scope{faucet.ScopeEventsSubscribe}
	requireSameClient(t, want, got)

	got, err = s.UpdateClient(ctx, "ci-watcher", oauth.ClientUpdate{Grants: []string{"all", "repo:coder/faucet"}, Scopes: []faucet.Scope{faucet.ScopeEventsRead}})
	require.NoError(t, err)
	want.Grants, want.Scopes = []string{"all", "repo:coder/faucet"}, []faucet.Scope{faucet.ScopeEventsRead}
	requireSameClient(t, want, got)
	again, err := s.Client(ctx, "ci-watcher")
	require.NoError(t, err)
	requireSameClient(t, want, again)
	untouched, err := s.Client(ctx, "triage")
	require.NoError(t, err)
	requireSameClient(t, clientAt("triage", at), untouched)
}

func updateClientRefusesAnUnknownOrDisabledClient(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	at := time.Now()
	require.NoError(t, s.InsertClient(ctx, clientAt("ci-watcher", at)))
	update := oauth.ClientUpdate{Grants: []string{"all"}}
	_, err := s.UpdateClient(ctx, "ci-watcher2", update)
	require.ErrorIs(t, err, oauth.ErrUnknownClient)

	disabled, err := s.DisableClient(ctx, "ci-watcher", at.Add(time.Minute))
	require.NoError(t, err)
	_, err = s.UpdateClient(ctx, "ci-watcher", update)
	require.ErrorIs(t, err, oauth.ErrUnknownClient, "a disabled client keeps what it held")
	got, err := s.Client(ctx, "ci-watcher")
	require.NoError(t, err)
	requireSameClient(t, disabled, got)
	all, err := s.Clients(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1, "an update makes no client")
}

func updateClientValidatesInput(t *testing.T, s oauth.Store) {
	ctx := context.Background()
	c := clientAt("ci-watcher", time.Now())
	require.NoError(t, s.InsertClient(ctx, c))
	for name, u := range map[string]oauth.ClientUpdate{
		"nothing":          {},
		"no grant":         {Grants: []string{}},
		"no scope":         {Scopes: []faucet.Scope{}},
		"an unknown scope": {Scopes: []faucet.Scope{"events:everything"}},
		"a bad grant":      {Grants: []string{"org:coder/faucet"}},
		"an unbound grant": {Grants: []string{"org:coder"}},
	} {
		_, err := s.UpdateClient(ctx, "ci-watcher", u)
		require.Error(t, err, name)
	}
	got, err := s.Client(ctx, "ci-watcher")
	require.NoError(t, err)
	requireSameClient(t, c, got)
}
