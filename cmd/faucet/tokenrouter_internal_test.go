package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/quartz"
)

const testOrigin = "https://faucet.example.com"

func newFaucetToken(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

type memTokens struct {
	mu     sync.Mutex
	tokens map[oauth.TokenHash]oauth.AccessToken
	err    error
}

func (m *memTokens) InsertAccessToken(_ context.Context, t oauth.AccessToken) error {
	if err := t.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[t.Hash] = t
	return nil
}

func (m *memTokens) AccessToken(_ context.Context, hash oauth.TokenHash) (oauth.AccessToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return oauth.AccessToken{}, m.err
	}
	t, ok := m.tokens[hash]
	if !ok {
		return oauth.AccessToken{}, oauth.ErrUnknownToken
	}
	return t, nil
}

// byKind answers for a service from its grants, with no GitHub call, and
// sends every other principal to GitHub.
type byKind struct{ github faucet.Authorizer }

func (k byKind) RepoID(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	if client, ok := p.ServiceClient(); ok {
		return granted(client, repo)
	}
	return k.github.RepoID(ctx, p, repo)
}

func (k byKind) RepoIDFresh(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	if client, ok := p.ServiceClient(); ok {
		return granted(client, repo)
	}
	return k.github.RepoIDFresh(ctx, p, repo)
}

func granted(client faucet.ServiceClient, repo string) (int64, error) {
	if slices.Contains(client.Grants, "repo:"+repo) {
		return 42, nil
	}
	return 0, faucet.ErrNoAccess
}

type tokenServer struct {
	*testServer
	tokens *memTokens
}

// newTokenServer is newTestServer with a faucet verifier whose service
// subjects are live, for an origin of testOrigin.
func newTokenServer(t *testing.T) *tokenServer {
	t.Helper()
	github := newGitHubStub(t)
	store := &stubStore{events: []faucet.Event{{
		ID: 8, Source: "github", DeliveryID: "d", Type: "ci.run_completed", RepoID: 42,
		PRs: []int32{3}, HeadSHA: strings.Repeat("a", 40), Data: json.RawMessage(`{"conclusion":"success"}`),
		ReceivedAt: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
	}}}
	clock := quartz.NewMock(t)
	logs := &syncBuffer{}
	logger := slog.Make(sloghuman.Sink(logs)).Leveled(slog.LevelDebug)

	authz := faucet.TokenAuthorizer{BaseURL: github.URL, Client: github.Client()}
	svc, err := faucet.New(faucet.Deps{
		Store: store, Secret: []byte(testSecret), Logger: logger,
		Authorizer: byKind{github: authz}, PrincipalFrom: principalFrom, Clock: clock,
	})
	require.NoError(t, err)
	tokens := &memTokens{tokens: map[oauth.TokenHash]oauth.AccessToken{}}
	verifier, err := oauth.NewVerifier(tokens, clock, map[oauth.SubjectKind]oauth.SubjectResolver{
		oauth.SubjectService: func(_ context.Context, id string, scopes []faucet.Scope) (faucet.Principal, error) {
			return faucet.NewService(faucet.ServiceClient{ClientID: id, Grants: []string{"repo:o/r"}}, scopes)
		},
	})
	require.NoError(t, err)

	srv := httptest.NewServer(newRouter(svc, bearers{github: authz, faucet: verifier, origin: testOrigin}, cronRoutes{}, logger))
	t.Cleanup(func() { closeServer(srv, clock) })
	return &tokenServer{
		testServer: &testServer{Server: srv, github: github, store: store, clock: clock, logs: logs},
		tokens:     tokens,
	}
}

// issue stores a service access token for the /mcp endpoint that expires in
// 15 minutes.
func (s *tokenServer) issue(t *testing.T, scopes []faucet.Scope, mutate ...func(*oauth.AccessToken)) string {
	t.Helper()
	token := newFaucetToken(t, "fct_")
	at := oauth.AccessToken{
		Hash: oauth.HashToken(token), ExpiresAt: s.clock.Now().Add(15 * time.Minute), Scopes: scopes,
		Resource: testOrigin + "/mcp", SubjectKind: oauth.SubjectService, Subject: "ci-bot",
	}
	for _, m := range mutate {
		m(&at)
	}
	require.NoError(t, s.tokens.InsertAccessToken(context.Background(), at))
	return token
}

func requireRefused(t *testing.T, srv *testServer, token string) {
	t.Helper()
	status, challenge, body := rawPost(t, srv, "Bearer "+token)
	require.Equal(t, http.StatusUnauthorized, status, "%.8s...", token)
	require.Equal(t, "Bearer", challenge)
	require.Equal(t, "unauthorized\n", body)
}

func TestFaucetTokensNeverReachGitHub(t *testing.T) {
	t.Parallel()

	refused := func(t *testing.T) []string {
		return []string{
			newFaucetToken(t, "fcr_"), newFaucetToken(t, "fcs_"), newFaucetToken(t, "fct_"),
			"fct_short", "fcr_", "fcs_x", newFaucetToken(t, "fct_") + "A",
		}
	}
	t.Run("without a faucet verifier", func(t *testing.T) {
		t.Parallel()
		srv := newTestServer(t)
		for _, token := range refused(t) {
			requireRefused(t, srv, token)
		}
		paths, _ := srv.github.seen()
		require.Empty(t, paths, "a token with a faucet prefix is faucet's own, so GitHub never sees it")
	})
	t.Run("with a faucet verifier", func(t *testing.T) {
		t.Parallel()
		srv := newTokenServer(t)
		refresh := newFaucetToken(t, "fcr_")
		srv.issue(t, []faucet.Scope{faucet.ScopeEventsRead}, func(at *oauth.AccessToken) { at.Hash = oauth.HashToken(refresh) })
		for _, token := range append(refused(t), refresh) {
			requireRefused(t, srv.testServer, token)
		}
		paths, _ := srv.github.seen()
		require.Empty(t, paths)
	})
}

func TestAFaucetAccessTokenReachesTheEndpointWithoutGitHub(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTokenServer(t)
	token := srv.issue(t, []faucet.Scope{faucet.ScopeEventsRead})

	res, err := call(ctx, srv.connect(ctx, t, token), "o/r", nil)
	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)
	require.Contains(t, toolText(t, res), `"repo_id":42`)
	paths, _ := srv.github.seen()
	require.Empty(t, paths, "a service token is checked by faucet and its grants alone")

	srv.clock.Advance(15 * time.Minute)
	requireRefused(t, srv.testServer, token)
}

func TestAFaucetAccessTokenWorksOnlyOnItsOwnResource(t *testing.T) {
	t.Parallel()

	srv := newTokenServer(t)
	for _, resource := range []string{testOrigin + "/github/mcp", "https://other.example.com/mcp", testOrigin + "/mcp/", "http://faucet.example.com/mcp"} {
		requireRefused(t, srv.testServer, srv.issue(t, []faucet.Scope{faucet.ScopeEventsRead}, func(at *oauth.AccessToken) { at.Resource = resource }))
	}
}

func TestAFaucetTokenHoldsOnlyItsScopesOverHTTP(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTokenServer(t)
	session := srv.connect(ctx, t, srv.issue(t, []faucet.Scope{faucet.ScopeEventsSubscribe}))

	res, err := call(ctx, session, "o/r", nil)
	require.NoError(t, err)
	require.True(t, res.IsError, "the wait tool needs events:read")
	require.Contains(t, toolText(t, res), "events:read")
	require.Zero(t, srv.store.pollCount())
}

func TestAFaucetTokenCheckThatCannotRunIsAServerError(t *testing.T) {
	t.Parallel()

	srv := newTokenServer(t)
	token := srv.issue(t, []faucet.Scope{faucet.ScopeEventsRead})
	srv.tokens.mu.Lock()
	srv.tokens.err = xerrors.New("connection refused")
	srv.tokens.mu.Unlock()

	status, challenge, body := rawPost(t, srv.testServer, "Bearer "+token)
	require.Equal(t, http.StatusInternalServerError, status)
	require.Empty(t, challenge, "a check that could not run is not a rejected token")
	require.Equal(t, "could not check the token\n", body)
	require.Contains(t, srv.logs.String(), "could not check the faucet token")
	require.Zero(t, strings.Count(srv.logs.String(), token), "the log never holds the token")
	paths, _ := srv.github.seen()
	require.Empty(t, paths)
}

func TestAGitHubTokenTakesTheGitHubPathBesideAFaucetVerifier(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTokenServer(t)
	res, err := call(ctx, srv.connect(ctx, t, aliceToken), "o/r", nil)
	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)
	paths, _ := srv.github.seen()
	require.Contains(t, paths, "/user")

	srv.github.mu.Lock()
	srv.github.userErr = http.StatusInternalServerError
	srv.github.mu.Unlock()
	status, _, body := rawPost(t, srv.testServer, "Bearer "+bobToken)
	require.Equal(t, http.StatusBadGateway, status)
	require.Equal(t, "could not check the token with GitHub\n", body)
}
