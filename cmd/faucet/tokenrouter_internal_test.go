package main

import (
	"bytes"
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

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	oauth.Store
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

// tokenServerConfig is what newTokenServer builds the server from. A test
// changes it before the server starts.
type tokenServerConfig struct {
	github   faucet.TokenAuthorizer
	clock    *quartz.Mock
	authn    authenticator
	authz    faucet.Authorizer
	subjects map[oauth.SubjectKind]oauth.SubjectResolver
	webhook  *faucet.WebhookDeps
}

// newTokenServer is newTestServer with a faucet verifier for an origin of
// testOrigin. By default its service subjects are live and hold the grant
// repo:o/r.
func newTokenServer(t *testing.T, configure ...func(*tokenServerConfig)) *tokenServer {
	t.Helper()
	github := newGitHubStub(t)
	store := &stubStore{events: []faucet.Event{{
		ID: 8, Source: "github", DeliveryID: "d", Type: "ci.run_completed", RepoID: 42,
		PRs: []int32{3}, HeadSHA: strings.Repeat("a", 40), Data: json.RawMessage(`{"conclusion":"success"}`),
		ReceivedAt: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
	}}}
	logs := &syncBuffer{}
	logger := slog.Make(sloghuman.Sink(logs)).Leveled(slog.LevelDebug)

	cfg := tokenServerConfig{
		github: faucet.TokenAuthorizer{BaseURL: github.URL, Client: github.Client()},
		clock:  quartz.NewMock(t),
		subjects: map[oauth.SubjectKind]oauth.SubjectResolver{
			oauth.SubjectService: func(_ context.Context, id string, scopes []faucet.Scope) (faucet.Principal, error) {
				return faucet.NewService(faucet.ServiceClient{ClientID: id, Grants: []string{"repo:o/r"}}, scopes)
			},
		},
	}
	cfg.authn, cfg.authz = cfg.github, byKind{github: cfg.github}
	for _, c := range configure {
		c(&cfg)
	}
	svc, err := faucet.New(faucet.Deps{
		Store: store, Secret: []byte(testSecret), Logger: logger,
		Authorizer: cfg.authz, PrincipalFrom: principalFrom, Clock: cfg.clock, Webhook: cfg.webhook,
	})
	require.NoError(t, err)
	tokens := &memTokens{tokens: map[oauth.TokenHash]oauth.AccessToken{}}
	verifier, err := oauth.NewVerifier(tokens, cfg.clock, cfg.subjects)
	require.NoError(t, err)
	b, err := newBearers(cfg.authn, verifier, testOrigin)
	require.NoError(t, err)

	srv := httptest.NewServer(newRouter(svc, b, nil, cronRoutes{}, logger))
	t.Cleanup(func() { closeServer(srv, cfg.clock) })
	return &tokenServer{
		testServer: &testServer{Server: srv, github: github, store: store, clock: cfg.clock, logs: logs, mcpPath: "/github/mcp"},
		tokens:     tokens,
	}
}

// issue stores a service access token for the endpoint at s.mcpPath that
// expires in 15 minutes.
func (s *tokenServer) issue(t *testing.T, scopes []faucet.Scope, mutate ...func(*oauth.AccessToken)) string {
	t.Helper()
	token := newFaucetToken(t, "fct_")
	at := oauth.AccessToken{
		Hash: oauth.HashToken(token), ExpiresAt: s.clock.Now().Add(15 * time.Minute), Scopes: scopes,
		Resource: testOrigin + s.mcpPath, SubjectKind: oauth.SubjectService, Subject: "ci-bot",
	}
	for _, m := range mutate {
		m(&at)
	}
	require.NoError(t, s.tokens.InsertAccessToken(context.Background(), at))
	return token
}

// randomPart is a piece of the random part of a faucet token, which a log
// that printed any of the token would hold.
func randomPart(token string) string { return token[8:24] }

func requireNoTokenPiece(t *testing.T, logs string, tokens []string) {
	t.Helper()
	for _, token := range tokens {
		if len(token) >= 24 {
			require.Zero(t, strings.Count(logs, randomPart(token)), "the log never holds a piece of a token")
		}
	}
}

// requireLoggedAt checks that every log line with message has level, as the
// human sink writes it, such as [debu] or [warn].
func requireLoggedAt(t *testing.T, logs, message, level string) {
	t.Helper()
	found := false
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "]  "+message) {
			found = true
			require.Contains(t, line, level, "%q is logged at %s", message, level)
		}
	}
	require.True(t, found, "the log has no line %q", message)
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
		tokens := refused(t)
		for _, token := range tokens {
			requireRefused(t, srv, token)
		}
		paths, _ := srv.github.seen()
		require.Empty(t, paths, "a token with a faucet prefix is faucet's own, so GitHub never sees it")
		requireLoggedAt(t, srv.logs.String(), "faucet token rejected", "[debu]")
		requireNoTokenPiece(t, srv.logs.String(), tokens)
	})
	t.Run("with a faucet verifier", func(t *testing.T) {
		t.Parallel()
		srv := newTokenServer(t)
		refresh := newFaucetToken(t, "fcr_")
		srv.issue(t, []faucet.Scope{faucet.ScopeEventsRead}, func(at *oauth.AccessToken) { at.Hash = oauth.HashToken(refresh) })
		tokens := append(refused(t), refresh)
		for _, token := range tokens {
			requireRefused(t, srv.testServer, token)
		}
		paths, _ := srv.github.seen()
		require.Empty(t, paths)
		logs := srv.logs.String()
		requireLoggedAt(t, logs, "faucet token rejected", "[debu]")
		require.Contains(t, logs, "not an access token", "the debug log says why")
		require.Contains(t, logs, "unknown access token")
		requireNoTokenPiece(t, logs, tokens)
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

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTokenServer(t)
	read := []faucet.Scope{faucet.ScopeEventsRead}
	issueFor := func(resource string) string {
		return srv.issue(t, read, func(at *oauth.AccessToken) { at.Resource = resource })
	}
	for _, path := range mcpPaths {
		srv.mcpPath = path
		res, err := call(ctx, srv.connect(ctx, t, issueFor(testOrigin+path)), "o/r", nil)
		require.NoError(t, err, path)
		require.False(t, res.IsError, "%s: %v", path, res.Content)

		other := "/mcp"
		if path == "/mcp" {
			other = "/github/mcp"
		}
		for _, resource := range []string{
			testOrigin + other, "https://other.example.com" + path, testOrigin + path + "/", "http://faucet.example.com" + path,
		} {
			requireRefused(t, srv.testServer, issueFor(resource))
		}
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
	requireLoggedAt(t, srv.logs.String(), "could not check the faucet token", "[warn]")
	require.Zero(t, strings.Count(srv.logs.String(), randomPart(token)), "the log never holds the token, nor a piece of it")
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

func TestAFaucetVerifierNeedsABareOrigin(t *testing.T) {
	t.Parallel()

	verifier, err := oauth.NewVerifier(&memTokens{}, quartz.NewMock(t), nil)
	require.NoError(t, err)
	for _, origin := range []string{
		"https://faucet.example.com", "https://faucet.example.com:8443", "http://127.0.0.1:8080", "http://localhost:3000", "http://[::1]:8080", "https://faucet.example.com:65535",
	} {
		_, err := newBearers(nil, verifier, origin)
		require.NoError(t, err, origin)
	}
	for _, origin := range []string{
		"", "faucet.example.com", "http://faucet.example.com", "https://faucet.example.com/", "https://faucet.example.com/github",
		"https://faucet.example.com?x=1", "https://faucet.example.com#top", "https://user@faucet.example.com", "https://",
		"ftp://faucet.example.com", "http://127.0.0.1.example.com", "https://faucet.example.com/../x",
		"https://faucet.example.com:", "https://faucet.example.com:0", "https://faucet.example.com:65536", "https://faucet.example.com:0443",
		"https://faucet.example.com:443", "http://localhost:80", "http://127.0.0.1:80", "https://Faucet.example.com", "http://LOCALHOST:3000",
		"https://faucet.example.com.", "http://localhost.:3000",
	} {
		_, err := newBearers(nil, verifier, origin)
		require.Error(t, err, "%q", origin)
	}
	b, err := newBearers(nil, nil, "")
	require.NoError(t, err, "without a verifier no origin is needed")
	require.Nil(t, b.faucet)
}

type subscribeParams struct {
	mcp.ParamsBase
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Delivery  map[string]any `json:"delivery"`
}

func connectSubscriber(ctx context.Context, t *testing.T, srv *testServer, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	require.NoError(t, mcp.AddSendingCustomMethod[*eventsParams, *eventsResult](client, "events/poll"))
	require.NoError(t, mcp.AddSendingCustomMethod[*subscribeParams, *eventsResult](client, "events/subscribe"))
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             srv.URL + srv.mcpPath,
		HTTPClient:           &http.Client{Transport: eventNameTransport{next: bearerTransport{token, srv.Client().Transport}}},
		DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// The finding (R12): an events:read token must read and must not subscribe.
func TestAReadOnlyFaucetTokenPollsButCannotSubscribe(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTokenServer(t, func(c *tokenServerConfig) {
		key, err := faucet.NewWebhookKey(bytes.Repeat([]byte{1}, 32))
		require.NoError(t, err)
		allow, err := faucet.NewCallbackAllowlist([]string{"https://hooks.example.com"}, false)
		require.NoError(t, err)
		c.webhook = &faucet.WebhookDeps{Key: key, Callbacks: allow}
	})
	session := connectSubscriber(ctx, t, srv.testServer, srv.issue(t, []faucet.Scope{faucet.ScopeEventsRead}))

	polled, err := callEvents(ctx, session, "events/poll", pollParams("o/r", nil))
	require.NoError(t, err, "events:read covers events/poll")
	require.Contains(t, polled, "cursor")

	_, err = mcp.CallCustomMethod[*subscribeParams, *eventsResult](ctx, session, "events/subscribe", &subscribeParams{
		Name: "ci.run_completed", Arguments: map[string]any{"repo": "o/r"},
		Delivery: map[string]any{"mode": "webhook", "url": "https://hooks.example.com/cb", "secret": "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))},
	})
	var rpcErr *jsonrpc.Error
	require.ErrorAs(t, err, &rpcErr, "a token without events:subscribe cannot subscribe")
	require.EqualValues(t, -32024, rpcErr.Code)
	require.Contains(t, rpcErr.Message, "events:subscribe")
}

// sessionChecks answers for OAuth users as their access checks will: a
// repository that passed a check at or after the caller's own session last
// refreshed passes with no GitHub call, and any other repository asks GitHub,
// which has revoked the access.
type sessionChecks struct {
	mu          sync.Mutex
	checkedAt   time.Time
	refreshedAt map[string]time.Time
	asked       []string
}

func (a *sessionChecks) RepoID(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	session, ok := p.OAuthSession()
	if !ok {
		return 0, faucet.ErrNoAccess
	}
	a.mu.Lock()
	fresh := repo == "o/r" && !a.checkedAt.Before(a.refreshedAt[session.SessionID])
	a.mu.Unlock()
	if fresh {
		return 42, nil
	}
	return a.RepoIDFresh(ctx, p, repo)
}

func (a *sessionChecks) RepoIDFresh(_ context.Context, p faucet.Principal, _ string) (int64, error) {
	session, _ := p.OAuthSession()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, session.SessionID)
	return 0, faucet.ErrNoAccess
}

// The finding (R13): an access check that one session made fresh must not be
// fresh for another session of the same connection.
func TestAnAccessCheckIsFreshOnlyForTheSessionOfTheToken(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	checks := &sessionChecks{refreshedAt: map[string]time.Time{}}
	srv := newTokenServer(t, func(c *tokenServerConfig) {
		t0 := c.clock.Now()
		checks.refreshedAt["sess_idle"] = t0
		checks.checkedAt = t0.Add(time.Minute)
		checks.refreshedAt["sess_active"] = t0.Add(15 * time.Minute)
		c.authz = checks
		c.subjects[oauth.SubjectUser] = func(_ context.Context, id string, scopes []faucet.Scope) (faucet.Principal, error) {
			return faucet.NewOAuthUser("alice", faucet.OAuthSession{SessionID: id, ConnectionID: "conn_alice"}, scopes)
		}
	})
	asUser := func(session string) func(*oauth.AccessToken) {
		return func(at *oauth.AccessToken) { at.SubjectKind, at.Subject = oauth.SubjectUser, session }
	}
	read := []faucet.Scope{faucet.ScopeEventsRead}

	res, err := call(ctx, srv.connect(ctx, t, srv.issue(t, read, asUser("sess_idle"))), "o/r", nil)
	require.NoError(t, err)
	require.False(t, res.IsError, "the idle session has not refreshed since the check: %v", res.Content)

	res, err = call(ctx, srv.connect(ctx, t, srv.issue(t, read, asUser("sess_active"))), "o/r", nil)
	require.NoError(t, err)
	require.True(t, res.IsError, "the active session refreshed after the check, so it asks GitHub, which says no")
	require.Contains(t, toolText(t, res), "not found or not accessible")
	checks.mu.Lock()
	defer checks.mu.Unlock()
	require.Equal(t, []string{"sess_active"}, checks.asked)
}

func TestTheGitHubPathKeepsItsMinuteOfMemoryBesideAFaucetVerifier(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTokenServer(t, func(c *tokenServerConfig) {
		cache := faucet.NewAccessCache(c.github, c.clock)
		c.authn, c.authz = cache, byKind{github: cache}
	})
	count := func(path string) int {
		paths, _ := srv.github.seen()
		n := 0
		for _, p := range paths {
			if p == path {
				n++
			}
		}
		return n
	}
	session := srv.connect(ctx, t, aliceToken)
	for range 3 {
		res, err := call(ctx, session, "o/r", nil)
		require.NoError(t, err)
		require.False(t, res.IsError, "%v", res.Content)
	}
	require.Equal(t, 1, count("/user"), "the login is remembered for a minute")
	require.Equal(t, 1, count("/repos/o/r"), "and so is the repository")

	requireRefused(t, srv.testServer, newFaucetToken(t, "fcr_"))
	srv.clock.Advance(time.Minute)
	res, err := call(ctx, session, "o/r", nil)
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Equal(t, 2, count("/user"), "after a minute GitHub is asked again")
	require.Equal(t, 2, count("/repos/o/r"))
}

func TestAResolverThatAnswersForAnotherPrincipalIsAServerError(t *testing.T) {
	t.Parallel()

	srv := newTokenServer(t, func(c *tokenServerConfig) {
		c.subjects[oauth.SubjectService] = func(_ context.Context, _ string, scopes []faucet.Scope) (faucet.Principal, error) {
			return faucet.NewService(faucet.ServiceClient{ClientID: "other-bot", Grants: []string{"repo:o/r"}}, scopes)
		}
	})
	token := srv.issue(t, []faucet.Scope{faucet.ScopeEventsRead})
	status, challenge, body := rawPost(t, srv.testServer, "Bearer "+token)
	require.Equal(t, http.StatusInternalServerError, status, "faucet's own bug is not the caller's bad token")
	require.Empty(t, challenge)
	require.Equal(t, "could not check the token\n", body)
	requireLoggedAt(t, srv.logs.String(), "could not check the faucet token", "[warn]")
	requireNoTokenPiece(t, srv.logs.String(), []string{token})
}
