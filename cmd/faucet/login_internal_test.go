package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth/extauth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/faucet/faucet/pgstore"
	"github.com/coder/quartz"
)

// loginServer is faucet as run() builds it with FAUCET_PUBLIC_URL set to the
// server's own URL, over a scratch database.
type loginServer struct {
	*httptest.Server
	store  *pgstore.Store
	github *githubStub
	logs   *syncBuffer
}

func newLoginServer(t *testing.T) *loginServer {
	t.Helper()
	adminURL := os.Getenv(testDatabaseURLEnv)
	if adminURL == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s must be set in CI", testDatabaseURLEnv)
		}
		t.Skipf("set %s to a Postgres URL", testDatabaseURLEnv)
	}
	db := newScratchDatabase(t, adminURL)
	require.NoError(t, pgstore.Migrate(context.Background(), db))
	store := pgstore.New(db)

	srv := httptest.NewUnstartedServer(nil)
	publicURL, err := parseOrigin("http://" + srv.Listener.Addr().String())
	require.NoError(t, err)
	github := newGitHubStub(t)
	clock := quartz.NewReal()
	logs := &syncBuffer{}
	logger := slog.Make(sloghuman.Sink(logs)).Leveled(slog.LevelDebug)
	cache := faucet.NewAccessCache(faucet.TokenAuthorizer{BaseURL: github.URL, Client: github.Client()}, clock)
	tokens, login, authorizer, err := newLogin(publicURL, store, cache, clock, logger)
	require.NoError(t, err)
	svc, err := faucet.New(faucet.Deps{
		Store: store, Secret: []byte(testSecret), Logger: logger, Authorizer: authorizer, PrincipalFrom: principalFrom, Clock: clock,
		RecordRepositories: true,
	})
	require.NoError(t, err)
	cron := cronRoutes{secret: []byte(loginCronSecret), svc: svc, retention: 24 * time.Hour, login: login, clock: clock, logger: logger}
	srv.Config.Handler = newRouter(svc, tokens, login, cron, logger)
	srv.Start()
	t.Cleanup(srv.Close)
	return &loginServer{Server: srv, store: store, github: github, logs: logs}
}

// client stores a client that an operator made for the endpoint at path,
// granted grants, and returns its secret.
func (s *loginServer) client(t *testing.T, id, path string, scopes []faucet.Scope, grants ...string) string {
	t.Helper()
	c, secret, err := oauth.NewClient(id, s.URL+path, scopes, grants, time.Now())
	require.NoError(t, err)
	require.NoError(t, s.store.InsertClient(context.Background(), c))
	return secret
}

// deliver posts the workflow_run fixture of octo-org/octo-repo, signed, as
// GitHub does.
func (s *loginServer) deliver(t *testing.T, delivery string) {
	t.Helper()
	body, err := os.ReadFile("../../faucet/testdata/workflow_run_completed.json")
	require.NoError(t, err)
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, _ = mac.Write(body)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL+"/webhook/github", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Event", "workflow_run")
	req.Header.Set("X-GitHub-Delivery", delivery)
	resp, err := s.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// connectService connects to the endpoint at path as go-sdk's client
// credentials handler does, with only the URL, the client ID and the secret.
func (s *loginServer) connectService(ctx context.Context, t *testing.T, path, id, secret string) (*mcp.ClientSession, error) {
	t.Helper()
	handler, err := extauth.NewClientCredentialsHandler(&extauth.ClientCredentialsHandlerConfig{
		Credentials: &oauthex.ClientCredentials{ClientID: id, ClientSecretAuth: &oauthex.ClientSecretAuth{ClientSecret: secret}},
		HTTPClient:  s.Client(),
	})
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "service-agent", Version: "0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: s.URL + path, HTTPClient: s.Client(), OAuthHandler: handler, DisableStandaloneSSE: true,
	}, nil)
	if err == nil {
		t.Cleanup(func() { _ = session.Close() })
	}
	return session, err
}

func (s *loginServer) get(t *testing.T, path string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, s.URL+path, nil)
	require.NoError(t, err)
	resp, err := s.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, body
}

var bothScopes = []faucet.Scope{faucet.ScopeEventsRead, faucet.ScopeEventsSubscribe}

const loginCronSecret = "cron-secret-0123456789abcdef"

// An access token is worth nothing once it expires, so the purge deletes it.
func TestThePurgeDeletesExpiredAccessTokens(t *testing.T) {
	t.Parallel()
	srv := newLoginServer(t)
	ctx := context.Background()
	token := func(name string, expires time.Time) oauth.AccessToken {
		return oauth.AccessToken{
			Hash: oauth.HashToken(name), ExpiresAt: expires, Scopes: bothScopes,
			Resource: srv.URL + "/github/mcp", SubjectKind: oauth.SubjectService, Subject: "ci-watcher",
		}
	}
	expired, live := token("fct_expired", time.Now().Add(-time.Minute)), token("fct_live", time.Now().Add(time.Hour))
	for _, at := range []oauth.AccessToken{expired, live} {
		require.NoError(t, srv.store.InsertAccessToken(ctx, at))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/cron/purge", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+loginCronSecret)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var reply map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&reply))
	require.EqualValues(t, 1, reply["expired_access_tokens"])

	_, err = srv.store.AccessToken(ctx, expired.Hash)
	require.ErrorIs(t, err, oauth.ErrUnknownToken)
	_, err = srv.store.AccessToken(ctx, live.Hash)
	require.NoError(t, err)
}

// A service agent logs in with the client credentials that an operator made,
// and no GitHub token. go-sdk's handler finds faucet's login server from the
// 401, sends no resource and asks for every scope of the challenge (R4).
func TestAServiceLogsInWithClientCredentialsAloneAndWaitsForAnEvent(t *testing.T) {
	t.Parallel()
	srv := newLoginServer(t)
	secret := srv.client(t, "ci-watcher", "/github/mcp", []faucet.Scope{faucet.ScopeEventsRead}, "repo:octo-org/octo-repo")
	srv.deliver(t, "d-1")

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	session, err := srv.connectService(ctx, t, "/github/mcp", "ci-watcher", secret)
	require.NoError(t, err)
	res, err := call(ctx, session, "octo-org/octo-repo", map[string]any{"cursor": 0, "timeout_s": 1})
	require.NoError(t, err)
	require.False(t, res.IsError, toolText(t, res))
	var out struct {
		Events []struct {
			Type   string `json:"type"`
			RepoID int64  `json:"repo_id"`
		} `json:"events"`
	}
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.Events, 1)
	require.Equal(t, int64(300029405), out.Events[0].RepoID)

	paths, _ := srv.github.seen()
	require.Empty(t, paths, "a service never reaches GitHub")
	requireNoTokenPiece(t, srv.logs.String(), []string{secret})
}

// The /mcp alias is its own resource with its own metadata document, so
// go-sdk's check that the document names the URL it called passes (R5).
func TestTheMCPAliasHasItsOwnDiscovery(t *testing.T) {
	t.Parallel()
	srv := newLoginServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	for _, path := range mcpPaths {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, srv.URL+"/.well-known/oauth-protected-resource"+path, srv.URL+path, srv.Client())
		require.NoError(t, err, path)
		require.Equal(t, []string{srv.URL}, prm.AuthorizationServers)
		require.Equal(t, []string{"events:read", "events:subscribe"}, prm.ScopesSupported)

		status, header, body := srv.get(t, "/.well-known/oauth-protected-resource"+path)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "*", header.Get("Access-Control-Allow-Origin"), "a browser client may read it")
		require.JSONEq(t, `{"resource":"`+srv.URL+path+`","authorization_servers":["`+srv.URL+`"],"scopes_supported":["events:read","events:subscribe"]}`, string(body))
	}
	status, _, body := srv.get(t, "/.well-known/oauth-authorization-server")
	require.Equal(t, http.StatusOK, status)
	var meta map[string]any
	require.NoError(t, json.Unmarshal(body, &meta))
	require.Equal(t, srv.URL, meta["issuer"])
	require.Equal(t, srv.URL+"/oauth/token", meta["token_endpoint"])
	require.Equal(t, srv.URL+"/oauth/authorize", meta["authorization_endpoint"])
	status, _, body = srv.get(t, "/oauth/authorize?response_type=code")
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, string(body), "unsupported_response_type")

	secret := srv.client(t, "old-agent", "/mcp", bothScopes, "repo:octo-org/octo-repo")
	srv.deliver(t, "d-1")
	session, err := srv.connectService(ctx, t, "/mcp", "old-agent", secret)
	require.NoError(t, err)
	res, err := call(ctx, session, "octo-org/octo-repo", map[string]any{"cursor": 0, "timeout_s": 1})
	require.NoError(t, err)
	require.False(t, res.IsError, toolText(t, res))

	_, err = srv.connectService(ctx, t, "/github/mcp", "old-agent", secret)
	require.Error(t, err, "a client's tokens are for its one resource")
}

func TestEvery401NamesTheMetadataOfItsPath(t *testing.T) {
	t.Parallel()
	srv := newLoginServer(t)
	for _, path := range mcpPaths {
		ts := &testServer{Server: srv.Server, mcpPath: path}
		want := `Bearer resource_metadata="` + srv.URL + `/.well-known/oauth-protected-resource` + path + `", scope="events:read events:subscribe"`
		for _, header := range []string{"", "Bearer", "Basic YWxpY2U6c2VjcmV0", "Bearer " + staleToken, "Bearer fct_unknown", "Bearer " + newFaucetToken(t, "fct_")} {
			status, challenge, body := rawPost(t, ts, header)
			require.Equal(t, http.StatusUnauthorized, status, "%s %q", path, header)
			require.Equal(t, want, challenge, "%s %q", path, header)
			require.Equal(t, "unauthorized\n", body)
		}
	}
}

func TestAServiceIsCheckedByItsGrantsAndTheReposThatDeliveriesNamed(t *testing.T) {
	t.Parallel()
	srv := newLoginServer(t)
	secret := srv.client(t, "ci-watcher", "/github/mcp", bothScopes, "repo:octo-org/octo-repo", "repo:octo-org/next")
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	session, err := srv.connectService(ctx, t, "/github/mcp", "ci-watcher", secret)
	require.NoError(t, err)

	res, err := call(ctx, session, "octo-org/octo-repo", map[string]any{"cursor": 0, "timeout_s": 1})
	require.NoError(t, err)
	require.True(t, res.IsError, "no delivery has named the repository yet")
	require.Contains(t, toolText(t, res), "faucet has received no webhook delivery from octo-org/octo-repo yet")

	srv.deliver(t, "d-1")
	res, err = call(ctx, session, "Octo-Org/Octo-Repo", map[string]any{"cursor": 0, "timeout_s": 1})
	require.NoError(t, err)
	require.False(t, res.IsError, toolText(t, res))

	res, err = call(ctx, session, "octo-org/other", map[string]any{"cursor": 0, "timeout_s": 1})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, toolText(t, res), "no grant of this service names octo-org/other")
	paths, _ := srv.github.seen()
	require.Empty(t, paths)
}

// A client made for another server's endpoint gets no token here, and the
// server logs a warning that names the client and the endpoint.
func TestAClientForAnotherServerGetsNoToken(t *testing.T) {
	t.Parallel()
	srv := newLoginServer(t)
	c, secret, err := oauth.NewClient("elsewhere", "https://faucet.example.com/github/mcp", bothScopes, []string{"repo:octo-org/octo-repo"}, time.Now())
	require.NoError(t, err)
	require.NoError(t, srv.store.InsertClient(context.Background(), c))
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	_, err = srv.connectService(ctx, t, "/github/mcp", "elsewhere", secret)
	require.ErrorContains(t, err, "which this server does not serve")
	requireLoggedAt(t, srv.logs.String(), "a client is for an endpoint that this server does not serve", "[warn]")
}

func TestDisablingAClientFailsItsTokenOnTheNextRequest(t *testing.T) {
	t.Parallel()
	srv := newLoginServer(t)
	secret := srv.client(t, "ci-watcher", "/github/mcp", bothScopes, "repo:octo-org/octo-repo")
	srv.deliver(t, "d-1")
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	session, err := srv.connectService(ctx, t, "/github/mcp", "ci-watcher", secret)
	require.NoError(t, err)
	_, err = call(ctx, session, "octo-org/octo-repo", map[string]any{"cursor": 0, "timeout_s": 1})
	require.NoError(t, err)

	_, err = srv.store.DisableClient(ctx, "ci-watcher", time.Now())
	require.NoError(t, err)
	_, err = call(ctx, session, "octo-org/octo-repo", map[string]any{"cursor": 0, "timeout_s": 1})
	require.Error(t, err, "the next request is refused, and the handler gets no new token")
}

// With the login server on, a GitHub token takes the path it always took.
func TestAGitHubTokenWorksBesideTheLoginServer(t *testing.T) {
	t.Parallel()
	srv := newLoginServer(t)
	ts := &testServer{Server: srv.Server, github: srv.github, mcpPath: "/github/mcp"}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	session := ts.connect(ctx, t, aliceToken)
	res, err := call(ctx, session, "o/r", map[string]any{"cursor": 0, "timeout_s": 1})
	require.NoError(t, err)
	require.False(t, res.IsError, toolText(t, res))
	paths, _ := srv.github.seen()
	require.Equal(t, []string{"/user", "/repos/o/r"}, paths)
}

// Without FAUCET_PUBLIC_URL, none of the login routes exist and a 401 is the
// one faucet sent before it had them.
func TestTheLoginRoutesAreOffWithoutAPublicURL(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource/github/mcp", "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server", "/oauth/authorize"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
		require.NoError(t, err)
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusNotFound, resp.StatusCode, path)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/oauth/token", bytes.NewReader([]byte("grant_type=client_credentials")))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	for _, path := range mcpPaths {
		srv.mcpPath = path
		status, challenge, body := rawPost(t, srv, "")
		require.Equal(t, http.StatusUnauthorized, status)
		require.Equal(t, "Bearer", challenge)
		require.Equal(t, "unauthorized\n", body)
	}
}

// R16: a client computes the resource from the URL it was given, byte for
// byte, so faucet refuses to start with a public URL in any other form.
func TestLoadConfigTakesThePublicURLOnlyInTheFormAClientComputes(t *testing.T) {
	t.Parallel()
	load := func(publicURL string) (config, error) {
		return loadConfig(func(name string) string {
			return map[string]string{envDatabaseURL: "postgres://localhost/faucet", envSecret: testSecret, envPublicURL: publicURL}[name]
		})
	}
	cfg, err := load("")
	require.NoError(t, err)
	require.Nil(t, cfg.publicURL, "the login server is off without it")

	for _, good := range []string{"https://faucet.example.com", "https://faucet.example.com:8443", "http://127.0.0.1:8080", "http://localhost:3000"} {
		cfg, err := load(good)
		require.NoError(t, err, good)
		require.Equal(t, good, cfg.publicURL.String())
	}
	for _, bad := range []string{
		"https://faucet.example.com/", "https://faucet.example.com/api", "http://faucet.example.com", "https://Faucet.example.com",
		"https://faucet.example.com:443", "https://faucet.example.com.", "faucet.example.com", "https://faucet.example.com?x=1",
	} {
		_, err := load(bad)
		require.ErrorContains(t, err, envPublicURL, bad)
	}
}
