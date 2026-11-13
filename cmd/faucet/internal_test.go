package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/faucet/faucet"
	"github.com/coder/quartz"
)

const (
	testSecret  = "whsec-test-0123456789abcdef"
	aliceToken  = "ghp_alice0123456789abcdefghijklmnopqrstu"
	bobToken    = "ghp_bobby0123456789abcdefghijklmnopqrstu"
	staleToken  = "ghp_stale0123456789abcdefghijklmnopqrstu"
	testTimeout = 30 * time.Second
)

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

type githubStub struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	auth     []string
	userErr  int
}

// newGitHubStub knows alice and bob. Alice can read o/r and nobody can see
// o/hidden.
func newGitHubStub(t *testing.T) *githubStub {
	t.Helper()
	stub := &githubStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.requests = append(stub.requests, r.URL.Path)
		stub.auth = append(stub.auth, r.Header.Get("Authorization"))
		userErr := stub.userErr
		stub.mu.Unlock()

		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		login := map[string]string{aliceToken: "alice", bobToken: "bob"}[token]
		switch {
		case r.URL.Path == "/user" && userErr != 0:
			w.WriteHeader(userErr)
		case login == "":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/user":
			_, _ = fmt.Fprintf(w, `{"login": %q}`, login)
		case r.URL.Path == "/repos/o/r" && login == "alice":
			_, _ = fmt.Fprint(w, `{"id": 42}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(stub.Close)
	return stub
}

func (s *githubStub) seen() (paths, auth []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...), append([]string(nil), s.auth...)
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
		page.Next = s.events[len(s.events)-1].ID
	}
	return page, nil
}

func (*stubStore) CursorAt(context.Context, time.Time) (int64, error) { return 7, nil }

func (s *stubStore) pollCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.polls
}

type testServer struct {
	*httptest.Server
	github *githubStub
	store  *stubStore
	clock  *quartz.Mock
	logs   *syncBuffer
}

func newTestServer(t *testing.T, configure ...func(*httptest.Server)) *testServer {
	t.Helper()
	github := newGitHubStub(t)
	store := &stubStore{events: []faucet.Event{{
		ID: 8, Source: "github", DeliveryID: "d", Type: "ci.run_completed", RepoID: 42,
		PRs: []int32{3}, HeadSHA: strings.Repeat("a", 40), Data: json.RawMessage(`{"conclusion":"success"}`),
		ReceivedAt: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
	}}}
	clock := quartz.NewMock(t)
	logs := &syncBuffer{}
	logger := slog.Make(sloghuman.Sink(logs))

	authz := faucet.TokenAuthorizer{BaseURL: github.URL, Client: github.Client()}
	svc, err := faucet.New(faucet.Deps{
		Store: store, Secret: []byte(testSecret), Logger: logger,
		Authorizer: authz, PrincipalFrom: principalFrom, Clock: clock,
	})
	require.NoError(t, err)

	srv := httptest.NewUnstartedServer(newRouter(svc, authz, cronRoutes{}, logger))
	for _, c := range configure {
		c(srv)
	}
	srv.Start()
	t.Cleanup(srv.Close)
	t.Cleanup(func() { clock.Advance(time.Hour) })
	return &testServer{Server: srv, github: github, store: store, clock: clock, logs: logs}
}

func (s *testServer) requireNoToken(t *testing.T, what string, texts ...string) {
	t.Helper()
	for _, token := range []string{aliceToken, bobToken, staleToken} {
		for _, text := range append(texts, s.logs.String()) {
			require.Zero(t, strings.Count(text, token), "a token appears in %s", what)
		}
	}
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func (s *testServer) connect(ctx context.Context, t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             s.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerTransport{token}},
		DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	t.Cleanup(func() { s.clock.Advance(time.Hour) })
	return session
}

func call(ctx context.Context, session *mcp.ClientSession, repo string, extra map[string]any) (*mcp.CallToolResult, error) {
	arguments := map[string]any{"repo": repo, "types": []string{"ci.run_completed"}, "cursor": 0}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if header != "" {
		req.Header["Authorization"] = []string{header}
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate"), string(raw)
}

func TestMCPRejectsRequestsWithoutAGoodBearerToken(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	malformed := []string{
		"", "Basic YWxpY2U6c2VjcmV0", "Bearer", "Bearer ", "Bearer two tokens", "Token " + aliceToken,
		"Bearer " + strings.Repeat("a", 1025),
	}
	for _, header := range malformed {
		status, challenge, body := rawPost(t, srv, header)
		require.Equal(t, http.StatusUnauthorized, status, "%q", header)
		require.Equal(t, "Bearer", challenge)
		require.Equal(t, "unauthorized\n", body)
	}
	paths, _ := srv.github.seen()
	require.Empty(t, paths, "a malformed header must not reach GitHub")

	status, _, body := rawPost(t, srv, "Bearer "+staleToken)
	require.Equal(t, http.StatusUnauthorized, status)
	require.Equal(t, "unauthorized\n", body, "GitHub's refusal looks like a malformed header")
	paths, _ = srv.github.seen()
	require.Equal(t, []string{"/user"}, paths)
	srv.requireNoToken(t, "a response", body)
}

func TestMCPGitHubFailureIsABadGateway(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	srv.github.mu.Lock()
	srv.github.userErr = http.StatusInternalServerError
	srv.github.mu.Unlock()

	status, _, body := rawPost(t, srv, "Bearer "+aliceToken)
	require.Equal(t, http.StatusBadGateway, status)
	require.NotContains(t, body, "500")
	srv.requireNoToken(t, "a response or the log", body)
	require.Contains(t, srv.logs.String(), "could not check the GitHub token")
}

func TestMCPToolWaitsForAllowedRepoOverHTTP(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTestServer(t)
	session := srv.connect(ctx, t, aliceToken)

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
	require.Contains(t, text, `"cursor":8`)

	paths, auth := srv.github.seen()
	require.Contains(t, paths, "/user")
	require.Contains(t, paths, "/repos/o/r")
	for _, a := range auth {
		require.Equal(t, "Bearer "+aliceToken, a, "GitHub got the caller's own token")
	}
	srv.requireNoToken(t, "a result", text)
}

func TestMCPToolDeniesARepoTheCallerCannotSee(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTestServer(t)

	bob := srv.connect(ctx, t, bobToken)
	res, err := call(ctx, bob, "o/r", nil)
	require.NoError(t, err)
	require.True(t, res.IsError)
	denied := toolText(t, res)
	require.Contains(t, denied, "not found or not accessible")
	require.Nil(t, res.StructuredContent)

	alice := srv.connect(ctx, t, aliceToken)
	res, err = call(ctx, alice, "o/hidden", nil)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Equal(t, denied, toolText(t, res), "a repository alice cannot see reads like one that does not exist")

	require.Zero(t, srv.store.pollCount(), "no events were read for a denied call")
	srv.requireNoToken(t, "a denial", denied)
}

func TestMCPEachRequestUsesItsOwnPrincipal(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	srv := newTestServer(t)
	alice := srv.connect(ctx, t, aliceToken)
	bob := srv.connect(ctx, t, bobToken)

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
	session := srv.connect(ctx, t, aliceToken)

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

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	const serverWriteTimeout = 300 * time.Millisecond
	srv := newTestServer(t, func(s *httptest.Server) { s.Config.WriteTimeout = serverWriteTimeout })
	srv.store.events = nil
	session := srv.connect(ctx, t, aliceToken)

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
}

func TestHealthAndWebhookDoNotNeedABearerToken(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/healthz", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
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
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	paths, _ := srv.github.seen()
	require.Empty(t, paths)
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
