package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"golang.org/x/xerrors"

	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
)

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
	require.Equal(t, srv.challenge(), challenge)
	require.Equal(t, "unauthorized\n", body)
}

// A token with a faucet prefix that does not verify gets the 401 of a
// request with no token, and the debug log says why in one line.
func TestAFaucetTokenThatDoesNotVerifyIsRefused(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	refresh := newFaucetToken(t, "fcr_")
	srv.issue(t, []faucet.Scope{faucet.ScopeEventsRead}, func(at *oauth.AccessToken) { at.Hash = oauth.HashToken(refresh) })
	tokens := []string{
		newFaucetToken(t, "fcr_"), newFaucetToken(t, "fcs_"), newFaucetToken(t, "fct_"),
		"fct_short", "fcr_", "fcs_x", newFaucetToken(t, "fct_") + "A", refresh,
	}
	for _, token := range tokens {
		requireRefused(t, srv, token)
	}
	logs := srv.logs.String()
	requireLoggedAt(t, logs, "faucet token rejected", "[debu]")
	require.Contains(t, logs, "not an access token", "the debug log says why")
	require.Contains(t, logs, "unknown access token")
	require.NotContains(t, logs, ".go:", "each rejection is one line, with no stack frame")
	requireNoTokenPiece(t, logs, tokens)
}

func TestAFaucetAccessTokenReachesTheEndpointUntilItExpires(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTestServer(t)
	token := srv.issue(t, []faucet.Scope{faucet.ScopeEventsRead})

	res, err := call(ctx, srv.connect(ctx, t, token), "o/r", nil)
	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)
	require.Contains(t, toolText(t, res), `"repo_id":42`)

	srv.clock.Advance(15 * time.Minute)
	requireRefused(t, srv, token)
}

func TestAFaucetAccessTokenWorksOnlyOnItsOwnResource(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTestServer(t)
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
			requireRefused(t, srv, issueFor(resource))
		}
	}
}

func TestAFaucetTokenHoldsOnlyItsScopesOverHTTP(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTestServer(t)
	session := srv.connect(ctx, t, srv.issue(t, []faucet.Scope{faucet.ScopeEventsSubscribe}))

	res, err := call(ctx, session, "o/r", nil)
	require.NoError(t, err)
	require.True(t, res.IsError, "the wait tool needs events:read")
	require.Contains(t, toolText(t, res), "events:read")
	require.Zero(t, srv.store.pollCount())
}

func TestAFaucetTokenCheckThatCannotRunIsAServerError(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	token := srv.issue(t, []faucet.Scope{faucet.ScopeEventsRead})
	srv.tokens.mu.Lock()
	srv.tokens.err = xerrors.New("connection refused")
	srv.tokens.mu.Unlock()

	status, challenge, body := rawPost(t, srv, "Bearer "+token)
	require.Equal(t, http.StatusInternalServerError, status)
	require.Empty(t, challenge, "a check that could not run is not a rejected token")
	require.Equal(t, "could not check the token\n", body)
	requireLoggedAt(t, srv.logs.String(), "could not check the faucet token", "[warn]")
	require.Zero(t, strings.Count(srv.logs.String(), randomPart(token)), "the log never holds the token, nor a piece of it")
}

func TestAFaucetVerifierNeedsABareOrigin(t *testing.T) {
	t.Parallel()

	verifier, err := oauth.NewVerifier(&memTokens{}, quartz.NewMock(t), nil)
	require.NoError(t, err)
	for _, origin := range []string{
		"https://faucet.example.com", "https://faucet.example.com:8443", "http://127.0.0.1:8080", "http://localhost:3000", "http://[::1]:8080", "https://faucet.example.com:65535",
	} {
		_, err := newBearers(verifier, origin)
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
		_, err := newBearers(verifier, origin)
		require.Error(t, err, "%q", origin)
	}
	_, err = newBearers(nil, testOrigin)
	require.Error(t, err, "the bearers need a verifier")
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
	srv := newTestServer(t, func(c *testServerConfig) {
		key, err := faucet.NewWebhookKey(bytes.Repeat([]byte{1}, 32))
		require.NoError(t, err)
		allow, err := faucet.NewCallbackAllowlist([]string{"https://hooks.example.com"}, false)
		require.NoError(t, err)
		c.webhook = &faucet.WebhookDeps{Key: key, Callbacks: allow}
	})
	session := connectSubscriber(ctx, t, srv, srv.issue(t, []faucet.Scope{faucet.ScopeEventsRead}))

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
	srv := newTestServer(t, func(c *testServerConfig) {
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

func TestAResolverThatAnswersForAnotherPrincipalIsAServerError(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, func(c *testServerConfig) {
		c.subjects[oauth.SubjectService] = func(_ context.Context, _ string, scopes []faucet.Scope) (faucet.Principal, error) {
			return faucet.NewService(faucet.ServiceClient{ClientID: "other-bot", Grants: []string{"repo:o/r"}}, scopes)
		}
	})
	token := srv.issue(t, []faucet.Scope{faucet.ScopeEventsRead})
	status, challenge, body := rawPost(t, srv, "Bearer "+token)
	require.Equal(t, http.StatusInternalServerError, status, "faucet's own bug is not the caller's bad token")
	require.Empty(t, challenge)
	require.Equal(t, "could not check the token\n", body)
	requireLoggedAt(t, srv.logs.String(), "could not check the faucet token", "[warn]")
	requireNoTokenPiece(t, srv.logs.String(), []string{token})
}
