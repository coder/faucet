package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
)

const (
	testSecret  = "whsec-test-0123456789abcdef"
	testTimeout = 30 * time.Second
	testOrigin  = "https://faucet.example.com"
	// aliceClient and bobClient are services that newTestServer knows. Alice
	// can read o/r, bob reads nothing, and nobody can read o/hidden.
	aliceClient = "alice"
	bobClient   = "bob"
)

// testGrants are the grants of the services that newTestServer knows.
var testGrants = map[string][]string{aliceClient: {"repo:o/r"}, "ci-bot": {"repo:o/r"}}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type stubStore struct {
	faucet.Store
	mu     sync.Mutex
	events []faucet.Event
	polls  int
}

func (s *stubStore) PollEvents(_ context.Context, q faucet.PollQuery) (faucet.PollPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polls++
	page := faucet.PollPage{Events: s.events, Next: max(q.After, 7)}
	if len(s.events) > 0 {
		page.Next = q.Target.PositionOf(s.events[len(s.events)-1])
	}
	return page, nil
}

func (*stubStore) PositionAt(context.Context, faucet.Target, time.Time) (faucet.Position, error) {
	return 7, nil
}

func (s *stubStore) pollCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.polls
}

// memTokens is the store of faucet's login server, in memory: it holds the
// access tokens that a test issues and purges nothing.
type memTokens struct {
	oauth.Store
	mu     sync.Mutex
	tokens map[oauth.TokenHash]oauth.AccessToken
	err    error
}

func newMemTokens() *memTokens { return &memTokens{tokens: map[oauth.TokenHash]oauth.AccessToken{}} }

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

func (*memTokens) DeleteExpiredAccessTokens(context.Context, time.Time) (int64, error) { return 0, nil }

func (*memTokens) DeleteExpiredAuthRequests(context.Context, time.Time, time.Time) (int64, error) {
	return 0, nil
}

func (*memTokens) DeleteEndedSessions(context.Context, time.Time, time.Time) (int64, error) {
	return 0, nil
}

func (*memTokens) DeleteUnusedAccessChecks(context.Context, time.Time, time.Time) (int64, error) {
	return 0, nil
}

func (*memTokens) DeleteUnusedConnections(context.Context, time.Time) (int64, error) { return 0, nil }

// newMemLogin is faucet's login server for testOrigin over tokens, with no
// browser login, and the bearer check of its endpoints.
func newMemLogin(t *testing.T, tokens *memTokens, clock quartz.Clock, subjects map[oauth.SubjectKind]oauth.SubjectResolver, logger slog.Logger) (bearers, *oauth.Server) {
	t.Helper()
	verifier, err := oauth.NewVerifier(tokens, clock, subjects)
	require.NoError(t, err)
	b, err := newBearers(verifier, testOrigin)
	require.NoError(t, err)
	login, err := oauth.NewServer(oauth.ServerDeps{
		Store: tokens, Clock: clock, Logger: logger, Issuer: testOrigin, Resources: []string{b.resource("/github/mcp"), b.resource("/mcp")},
	})
	require.NoError(t, err)
	return b, login
}

// grantChecks answers for a service from its grants, with no GitHub call: a
// grant repo:o/r reads o/r, whose ID is 42. It denies every other principal.
type grantChecks struct{}

func (grantChecks) RepoID(_ context.Context, p faucet.Principal, repo string) (int64, error) {
	if client, ok := p.ServiceClient(); ok && slices.Contains(client.Grants, "repo:"+repo) {
		return 42, nil
	}
	return 0, faucet.ErrNoAccess
}

func (g grantChecks) RepoIDFresh(ctx context.Context, p faucet.Principal, repo string) (int64, error) {
	return g.RepoID(ctx, p, repo)
}

type testServer struct {
	*httptest.Server
	store  *stubStore
	tokens *memTokens
	clock  *quartz.Mock
	logs   *syncBuffer
	// mcpPath is the MCP endpoint that the test's clients call.
	mcpPath string
}

// testServerConfig is what newTestServer builds the server from. A test
// changes it before the server starts.
type testServerConfig struct {
	clock    *quartz.Mock
	authz    faucet.Authorizer
	subjects map[oauth.SubjectKind]oauth.SubjectResolver
	webhook  *faucet.WebhookDeps
	// writeTimeout, when set, replaces the server's write deadline.
	writeTimeout time.Duration
}

// newTestServer serves faucet with its login server for testOrigin, over a
// store of one event of o/r and access tokens in memory. By default a token's
// subject is a service with the grants that testGrants gives it, checked by
// grantChecks.
func newTestServer(t *testing.T, configure ...func(*testServerConfig)) *testServer {
	t.Helper()
	store := &stubStore{events: []faucet.Event{{
		ID: 8, RepoPosition: 8, Source: "github", DeliveryID: "d", Type: "ci.run_completed", RepoID: 42,
		PRs: []int32{3}, HeadSHA: strings.Repeat("a", 40), Data: json.RawMessage(`{"conclusion":"success"}`),
		ReceivedAt: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
	}}}
	logs := &syncBuffer{}
	logger := slog.Make(sloghuman.Sink(logs)).Leveled(slog.LevelDebug)

	cfg := testServerConfig{
		clock: quartz.NewMock(t),
		authz: grantChecks{},
		subjects: map[oauth.SubjectKind]oauth.SubjectResolver{
			oauth.SubjectService: func(_ context.Context, id string, scopes []faucet.Scope) (faucet.Principal, error) {
				return faucet.NewService(faucet.ServiceClient{ClientID: id, Grants: testGrants[id]}, scopes)
			},
		},
	}
	for _, c := range configure {
		c(&cfg)
	}
	svc, err := faucet.New(faucet.Deps{
		Store: store, Secret: []byte(testSecret), Logger: logger,
		Authorizer: cfg.authz, PrincipalFrom: principalFrom, Clock: cfg.clock, Webhook: cfg.webhook,
	})
	require.NoError(t, err)
	tokens := newMemTokens()
	b, login := newMemLogin(t, tokens, cfg.clock, cfg.subjects, logger)

	srv := httptest.NewUnstartedServer(newRouter(svc, b, login, cronRoutes{}, logger))
	if cfg.writeTimeout != 0 {
		srv.Config.WriteTimeout = cfg.writeTimeout
	}
	srv.Start()
	t.Cleanup(func() { closeServer(srv, cfg.clock) })
	return &testServer{Server: srv, store: store, tokens: tokens, clock: cfg.clock, logs: logs, mcpPath: "/github/mcp"}
}

// issue stores an access token of the service ci-bot for the endpoint at
// s.mcpPath that expires in 15 minutes.
func (s *testServer) issue(t *testing.T, scopes []faucet.Scope, mutate ...func(*oauth.AccessToken)) string {
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

// token issues an access token of the service client, with both scopes, for
// the endpoint at s.mcpPath.
func (s *testServer) token(t *testing.T, client string) string {
	t.Helper()
	return s.issue(t, bothScopes, func(at *oauth.AccessToken) { at.Subject = client })
}

func newFaucetToken(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// challenge is the WWW-Authenticate of a 401 from the endpoint at s.mcpPath.
func (s *testServer) challenge() string {
	return `Bearer resource_metadata="` + testOrigin + `/.well-known/oauth-protected-resource` + s.mcpPath + `", scope="events:read events:subscribe"`
}

// closeServer closes srv and fires the pending timers of clock one at a time
// while it waits, so a handler that waits on one ends. A single Advance of an
// hour fails the test when a timer is due sooner and leaves Close to wait for
// that handler until the package timeout, with no report of what failed.
func closeServer(srv *httptest.Server, clock *quartz.Mock) {
	closed := make(chan struct{})
	go func() {
		srv.Close()
		close(closed)
	}()
	for {
		select {
		case <-closed:
			return
		case <-time.After(10 * time.Millisecond):
			if _, ok := clock.Peek(); ok {
				_, advanced := clock.AdvanceNext()
				<-advanced.Done()
			}
		}
	}
}

// bearerTransport takes the test server's own transport: httptest.Server.Close
// closes the idle connections of http.DefaultTransport, which breaks parallel
// tests that share it.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

func (s *testServer) connect(ctx context.Context, t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             s.URL + s.mcpPath,
		HTTPClient:           &http.Client{Transport: bearerTransport{token, s.Client().Transport}},
		DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func call(ctx context.Context, session *mcp.ClientSession, repo string, extra map[string]any) (*mcp.CallToolResult, error) {
	arguments := map[string]any{"repo": repo, "types": []string{"ci.run_completed"}, "cursor": "v2.0"}
	for k, v := range extra {
		arguments[k] = v
	}
	return session.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_github_events", Arguments: arguments})
}

func toolText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.NotEmpty(t, res.Content)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	return text.Text
}

func rawPost(t *testing.T, srv *testServer, header string) (status int, wwwAuthenticate, body string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+srv.mcpPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if header != "" {
		req.Header["Authorization"] = []string{header}
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate"), string(raw)
}

func TestMCPRejectsRequestsWithoutAGoodBearerToken(t *testing.T) {
	t.Parallel()

	for _, path := range mcpPaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			srv := newTestServer(t)
			srv.mcpPath = path
			refused := []string{
				"", "Basic YWxpY2U6c2VjcmV0", "Bearer", "Bearer ", "Bearer two tokens", "Token " + srv.token(t, aliceClient),
				"Bearer " + strings.Repeat("a", 1025), "Bearer ghp_alice0123456789abcdefghijklmnopqrstu", "Bearer " + newFaucetToken(t, "fct_"),
			}
			for _, header := range refused {
				status, challenge, body := rawPost(t, srv, header)
				require.Equal(t, http.StatusUnauthorized, status, "%.12q", header)
				require.Equal(t, srv.challenge(), challenge, "%.12q", header)
				require.Equal(t, "unauthorized\n", body, "a token that does not verify reads like a malformed header")
			}
		})
	}
}

// TestMCPServesAPublicHostOnALoopbackListener models Vercel, whose runtime
// hands each request to the function's server on a loopback address with the
// Host that the client called.
func TestMCPServesAPublicHostOnALoopbackListener(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+srv.mcpPath, strings.NewReader(body))
	require.NoError(t, err)
	req.Host = "faucet.example.com"
	req.Header.Set("Authorization", "Bearer "+srv.token(t, aliceClient))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", got)
	require.Contains(t, string(got), `"serverInfo"`)
}

func TestMCPToolWaitsForAllowedRepoOverHTTP(t *testing.T) {
	t.Parallel()

	for _, path := range mcpPaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()
			srv := newTestServer(t)
			srv.mcpPath = path
			token := srv.token(t, aliceClient)
			session := srv.connect(ctx, t, token)

			tools, err := session.ListTools(ctx, nil)
			require.NoError(t, err)
			require.Len(t, tools.Tools, 1)
			require.Equal(t, "wait_for_github_events", tools.Tools[0].Name)

			res, err := call(ctx, session, "o/r", nil)
			require.NoError(t, err)
			require.False(t, res.IsError, "%v", res.Content)
			text := toolText(t, res)
			require.Contains(t, text, `"type":"ci.run_completed"`)
			require.Contains(t, text, `"repo_id":42`)
			require.Contains(t, text, `"cursor":"v2.8"`)
			requireNoTokenPiece(t, text+srv.logs.String(), []string{token})
		})
	}
}

func TestMCPToolDeniesARepoTheCallerCannotSee(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTestServer(t)

	tokens := []string{srv.token(t, bobClient), srv.token(t, aliceClient)}
	bob := srv.connect(ctx, t, tokens[0])
	res, err := call(ctx, bob, "o/r", nil)
	require.NoError(t, err)
	require.True(t, res.IsError)
	denied := toolText(t, res)
	require.Contains(t, denied, "not found or not accessible")
	require.Nil(t, res.StructuredContent)

	alice := srv.connect(ctx, t, tokens[1])
	res, err = call(ctx, alice, "o/hidden", nil)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Equal(t, denied, toolText(t, res), "a repository alice cannot see reads like one that does not exist")

	require.Zero(t, srv.store.pollCount(), "no events were read for a denied call")
	requireNoTokenPiece(t, denied+srv.logs.String(), tokens)
}

func TestMCPEachRequestUsesItsOwnPrincipal(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTestServer(t)
	alice := srv.connect(ctx, t, srv.token(t, aliceClient))
	bob := srv.connect(ctx, t, srv.token(t, bobClient))

	for range 3 {
		res, err := call(ctx, alice, "o/r", nil)
		require.NoError(t, err)
		require.False(t, res.IsError)
		res, err = call(ctx, bob, "o/r", nil)
		require.NoError(t, err)
		require.True(t, res.IsError, "bob stays denied after alice was allowed")
	}
}

func TestMCPWaitStopsWhenTheClientDisconnects(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTestServer(t)
	srv.store.events = nil
	session := srv.connect(ctx, t, srv.token(t, aliceClient))

	timers := srv.clock.Trap().NewTimer()
	defer timers.Close()
	stops := srv.clock.Trap().TimerStop()
	defer stops.Close()

	callCtx, cancelCall := context.WithCancel(ctx)
	defer cancelCall()
	done := make(chan error, 1)
	go func() {
		_, err := call(callCtx, session, "o/r", map[string]any{"timeout_s": 50})
		done <- err
	}()
	timers.MustWait(ctx).MustRelease(ctx)
	pollsBefore := srv.store.pollCount()

	cancelCall()
	stops.MustWait(ctx).MustRelease(ctx)
	require.Error(t, <-done)

	srv.clock.Advance(time.Minute).MustWait(ctx)
	require.Equal(t, pollsBefore, srv.store.pollCount(), "the loop stopped polling")
}

func TestMCPWaitOutlastsTheServerWriteTimeout(t *testing.T) {
	t.Parallel()

	for _, path := range mcpPaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()
			const serverWriteTimeout = 300 * time.Millisecond
			srv := newTestServer(t, func(c *testServerConfig) { c.writeTimeout = serverWriteTimeout })
			srv.mcpPath = path
			srv.store.events = nil
			session := srv.connect(ctx, t, srv.token(t, aliceClient))

			timers := srv.clock.Trap().NewTimer()
			defer timers.Close()
			done := make(chan error, 1)
			go func() {
				res, err := call(ctx, session, "o/r", map[string]any{"timeout_s": 2})
				if err == nil && res.IsError {
					err = fmt.Errorf("tool error: %s", toolText(t, res))
				}
				done <- err
			}()
			armed := timers.MustWait(ctx)
			time.Sleep(2 * serverWriteTimeout)
			armed.MustRelease(ctx)
			srv.clock.Advance(2 * time.Second).MustWait(ctx)

			require.NoError(t, <-done, "the response arrives after the server's own write timeout")
		})
	}
}

func TestHealthAndWebhookDoNotNeedABearerToken(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/healthz", nil)
	require.NoError(t, err)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := []byte(`{}`)
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, _ = mac.Write(body)
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/webhook/github", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-GitHub-Delivery", "d-1")
	resp, err = srv.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestBearerToken(t *testing.T) {
	t.Parallel()

	for header, want := range map[string]string{
		"Bearer abc":                          "abc",
		"bearer abc":                          "abc",
		"BEARER abc.def-ghi_jkl~+/=":          "abc.def-ghi_jkl~+/=",
		"Bearer " + strings.Repeat("a", 1024): strings.Repeat("a", 1024),
	} {
		got, ok := bearerToken(header)
		require.True(t, ok, header)
		require.Equal(t, want, got)
	}
	for _, header := range []string{"", "abc", "Bearer", "Bearer ", "Bearer  abc", "Bearer a b", "Bearer a\tb", "Bearer a\x7f", "Bearer é", "Bearer " + strings.Repeat("a", 1025)} {
		_, ok := bearerToken(header)
		require.False(t, ok, "%q", header)
	}
}

func TestLoadConfigGitHubAPIURL(t *testing.T) {
	t.Parallel()

	env := func(url string) func(string) string {
		return func(name string) string {
			return map[string]string{
				envDatabaseURL: "postgres://localhost/faucet",
				envSecret:      testSecret,
				envPublicURL:   testOrigin,
				envAPIURL:      url,
			}[name]
		}
	}

	cfg, err := loadConfig(env(""))
	require.NoError(t, err)
	require.Equal(t, "https://api.github.com", cfg.githubAPI)

	for _, url := range []string{"http://127.0.0.1:9999", "https://ghe.example.com/api/v3"} {
		cfg, err = loadConfig(env(url))
		require.NoError(t, err, url)
		require.Equal(t, url, cfg.githubAPI)
	}

	const userinfo = "hunter2-not-a-real-secret"
	for _, url := range []string{"http://api.github.com", "ftp://x", "api.github.com", "https://u:" + userinfo + "@api.github.com", "https://api.github.com?x=1"} {
		_, err = loadConfig(env(url))
		require.ErrorContains(t, err, envAPIURL, url)
		require.NotContains(t, err.Error(), userinfo)
		require.NotContains(t, err.Error(), url)
	}
}

// Neon's pooled URL serves the requests, with the option that keeps lib/pq
// working behind its pooler. The direct URL applies the migrations, because
// the migration lock does not survive a transaction-mode pooler.
func TestLoadConfigServesOverTheDatabaseURLAndMigratesOverTheDirectOne(t *testing.T) {
	t.Parallel()

	const (
		pooled = "postgres://pooled.example.com/faucet"
		direct = "postgres://direct.example.com/faucet"
	)
	load := func(unpooled string) (config, error) {
		return loadConfig(func(name string) string {
			return map[string]string{
				envDatabaseURL:         pooled,
				envDatabaseURLUnpooled: unpooled,
				envSecret:              testSecret,
				envPublicURL:           testOrigin,
			}[name]
		})
	}

	cfg, err := load("")
	require.NoError(t, err)
	require.Equal(t, pooled, cfg.databaseURL)
	require.Equal(t, pooled, cfg.migrateURL)
	require.Equal(t, envDatabaseURL, cfg.migrateVar)

	cfg, err = load(direct)
	require.NoError(t, err)
	require.Equal(t, pooled, cfg.databaseURL)
	require.Equal(t, direct, cfg.migrateURL)
	require.Equal(t, envDatabaseURLUnpooled, cfg.migrateVar)
}
