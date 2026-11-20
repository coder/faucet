package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
	"github.com/coder/superblink-events/faucet/pgstore"
)

const (
	browserRedirectURI = "http://127.0.0.1:53682/callback"
	browserFixtureRepo = "octo-org/octo-repo"
)

// browserGitHub is GitHub for a user who is signed in and authorized
// faucet's App before: its authorize page sends the browser straight back,
// its token endpoint gives pairs whose refresh token works once, and its API
// knows the user of each access token.
type browserGitHub struct {
	*httptest.Server
	mu       sync.Mutex
	next     int
	codes    map[string]bool
	refresh  map[string]bool
	access   map[string]bool
	apiCalls []string
}

func newBrowserGitHub(t *testing.T) *browserGitHub {
	t.Helper()
	g := &browserGitHub{codes: map[string]bool{}, refresh: map[string]bool{}, access: map[string]bool{}}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	return g
}

func (g *browserGitHub) pair(w http.ResponseWriter) {
	g.next++
	access, refresh := fmt.Sprintf("ghu_browser_%d", g.next), fmt.Sprintf("ghr_browser_%d", g.next)
	g.access[access], g.refresh[refresh] = true, true
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": access, "expires_in": 28800, "refresh_token": refresh, "refresh_token_expires_in": 15897600, "token_type": "bearer",
	})
}

func (g *browserGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/login/oauth/authorize":
		g.next++
		code := fmt.Sprintf("gh-code-%d", g.next)
		g.codes[code] = true
		q := r.URL.Query()
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+url.Values{"code": {code}, "state": {q.Get("state")}}.Encode(), http.StatusFound)
	case "/login/oauth/access_token":
		_ = r.ParseForm()
		switch {
		case r.PostForm.Get("grant_type") == "refresh_token" && g.refresh[r.PostForm.Get("refresh_token")]:
			delete(g.refresh, r.PostForm.Get("refresh_token"))
			g.pair(w)
		case r.PostForm.Get("grant_type") == "refresh_token":
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad_refresh_token"})
		case g.codes[r.PostForm.Get("code")]:
			delete(g.codes, r.PostForm.Get("code"))
			g.pair(w)
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code"})
		}
	default:
		g.apiCalls = append(g.apiCalls, r.URL.Path)
		switch token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); {
		case !g.access[token]:
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/user":
			_, _ = io.WriteString(w, `{"id": 583231, "login": "octocat"}`)
		case r.URL.Path == "/repos/"+browserFixtureRepo:
			_, _ = io.WriteString(w, `{"id": 300029405}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (g *browserGitHub) calls(path string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, p := range g.apiCalls {
		if p == path {
			n++
		}
	}
	return n
}

// browserServer is faucet as run() builds it with the browser login on, over
// TLS and a scratch database. configure changes the service's Deps.
type browserServer struct {
	*httptest.Server
	store    *pgstore.Store
	github   *browserGitHub
	clientID string
	logs     *syncBuffer
}

func newBrowserServer(t *testing.T, configure ...func(*faucet.Deps)) *browserServer {
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
	github := newBrowserGitHub(t)

	var clientID string
	docs := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id": clientID, "client_name": "Coding Agent", "redirect_uris": []string{"http://127.0.0.1/callback"},
			"token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"},
		})
	}))
	t.Cleanup(docs.Close)
	clientID = docs.URL + "/client.json"
	roots := x509.NewCertPool()
	roots.AddCert(docs.Certificate())

	srv := httptest.NewUnstartedServer(nil)
	publicURL, err := parseOrigin("https://" + srv.Listener.Addr().String())
	require.NoError(t, err)
	browser, err := browserLoginFor(github.URL, roots)
	require.NoError(t, err)
	clock := quartz.NewReal()
	logs := &syncBuffer{}
	logger := slog.Make(sloghuman.Sink(logs)).Leveled(slog.LevelDebug)
	githubAPI := faucet.TokenAuthorizer{BaseURL: github.URL, Client: github.Client()}
	tokens, login, checks, err := newLogin(publicURL, browser, store, githubAPI, clock, logger)
	require.NoError(t, err)
	deps := faucet.Deps{
		Store: store, Secret: []byte(testSecret), Logger: logger, Authorizer: checks.authorizer, ServiceGrants: checks.grants,
		PrincipalFrom: principalFrom, Clock: clock,
	}
	for _, c := range configure {
		c(&deps)
	}
	svc, err := faucet.New(deps)
	require.NoError(t, err)
	srv.Config.Handler = newRouter(svc, tokens, login, cronRoutes{}, logger)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &browserServer{Server: srv, store: store, github: github, clientID: clientID, logs: logs}
}

// browserLoginFor is the browser login that loadConfig makes, for a GitHub
// at githubURL and a document client that trusts roots.
func browserLoginFor(githubURL string, roots *x509.CertPool) (*oauth.BrowserLogin, error) {
	cfg, err := loadWith(map[string]string{
		envPublicURL: "https://faucet.example.com", envGitHubAppClientID: "Iv1.faucet", envGitHubAppClientSecret: "app-secret-value",
		envTokenKey: testTokenKey(), envGitHubURL: githubURL, envAPIURL: githubURL,
	})
	if err != nil {
		return nil, err
	}
	cfg.browserLogin.Documents = faucet.HTTPClientOptions{AllowInsecureLoopback: true, RootCAs: roots}
	return cfg.browserLogin, nil
}

// A GitHub token is not a faucet token, so it gets the 401 that starts the
// login, as a request with no token does, and faucet never sends it to
// GitHub. The token here is one that GitHub knows and that can read the
// repository: GitHub would vouch for it, and still it does not get in,
// because no GitHub API says what a fine-grained token may read.
func TestAGitHubTokenGetsTheLoginChallengeAndNeverReachesGitHub(t *testing.T) {
	t.Parallel()
	srv := newBrowserServer(t)
	srv.deliver(t, "d-pasted")
	githubTokens := []string{
		"ghp_pasted0123456789abcdefghijklmnopqrstu", "github_pat_11ABCDEFG0_pasted0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRS",
		"gho_pasted0123456789abcdefghijklmnopqrstu", "ghu_pasted0123456789abcdefghijklmnopqrstu", "ghs_pasted0123456789abcdefghijklmnopqrstu",
		strings.Repeat("0123456789abcdef", 2) + "01234567",
	}
	srv.github.mu.Lock()
	for _, token := range githubTokens {
		srv.github.access[token] = true
	}
	srv.github.mu.Unlock()
	for _, path := range mcpPaths {
		ts := &testServer{Server: srv.Server, mcpPath: path}
		want := `Bearer resource_metadata="` + srv.URL + `/.well-known/oauth-protected-resource` + path + `", scope="events:read events:subscribe"`
		for _, token := range githubTokens {
			status, challenge, body := rawPost(t, ts, "Bearer "+token)
			require.Equal(t, http.StatusUnauthorized, status, "%s %.8s...", path, token)
			require.Equal(t, want, challenge, "%s %.8s...", path, token)
			require.Equal(t, "unauthorized\n", body)
		}
	}
	srv.github.mu.Lock()
	defer srv.github.mu.Unlock()
	require.Empty(t, srv.github.apiCalls, "faucet sends no bearer to GitHub")
	require.NotContains(t, srv.logs.String(), "pasted0123456789", "no log line holds the token")
}

func (s *browserServer) deliver(t *testing.T, delivery string) {
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

var browserFormField = regexp.MustCompile(`<input type="hidden" name="(request|login|token)" value="([^"]+)"`)

// person is the user at the browser: it opens the authorization URL, reads
// the consent page, presses Continue, and follows the browser through GitHub
// and back until faucet sends it to the agent's redirect URI.
func (s *browserServer) person(t *testing.T) auth.AuthorizationCodeFetcher {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	browser := *s.Client()
	browser.Jar = jar
	browser.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasPrefix(req.URL.String(), "http://127.0.0.1:53682/") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		get, err := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
		if err != nil {
			return nil, err
		}
		page, err := browser.Do(get)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(page.Body)
		_ = page.Body.Close()
		form := url.Values{}
		for _, m := range browserFormField.FindAllStringSubmatch(string(body), -1) {
			form.Set(m[1], m[2])
		}
		if page.StatusCode != http.StatusOK || len(form) != 3 {
			return nil, fmt.Errorf("the consent page answered %d: %s", page.StatusCode, body)
		}
		post, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+oauth.AuthorizePath, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		post.Header.Set("Origin", s.URL)
		back, err := browser.Do(post)
		if err != nil {
			return nil, err
		}
		_ = back.Body.Close()
		loc, err := url.Parse(back.Header.Get("Location"))
		if err != nil || back.StatusCode != http.StatusFound || !strings.HasPrefix(loc.String(), browserRedirectURI) {
			return nil, fmt.Errorf("the login ended at %d %s", back.StatusCode, back.Request.URL.Path)
		}
		return &auth.AuthorizationResult{Code: loc.Query().Get("code"), State: loc.Query().Get("state"), Iss: loc.Query().Get("iss")}, nil
	}
}

// connect connects an agent that knows only the endpoint's URL and its own
// client ID metadata document, as go-sdk's authorization code handler does.
func (s *browserServer) connect(ctx context.Context, t *testing.T, path string) (*mcp.ClientSession, error) {
	t.Helper()
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		ClientIDMetadataDocumentConfig: &auth.ClientIDMetadataDocumentConfig{URL: s.clientID},
		RedirectURL:                    browserRedirectURI,
		Client:                         s.Client(),
		AuthorizationCodeFetcher:       s.person(t),
	})
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: s.URL + path, HTTPClient: s.Client(), OAuthHandler: handler, DisableStandaloneSSE: true,
	}, nil)
	if err == nil {
		t.Cleanup(func() { _ = session.Close() })
	}
	return session, err
}

// The whole way in: an agent with no token gets a 401, finds faucet's login
// server, sends the person to the consent page and through GitHub, redeems
// its code with go-sdk's Basic-first token request (R6), and waits for an
// event of a repository that the person's GitHub login can read.
func TestAnAgentLogsInThroughABrowserAndWaitsForAnEvent(t *testing.T) {
	t.Parallel()
	srv := newBrowserServer(t)
	srv.deliver(t, "d-1")

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	session, err := srv.connect(ctx, t, "/github/mcp")
	require.NoError(t, err, srv.logs.String())
	res, err := call(ctx, session, browserFixtureRepo, map[string]any{"cursor": "v2.0", "timeout_s": 1})
	require.NoError(t, err)
	require.False(t, res.IsError, toolText(t, res))
	var out struct {
		Events []struct {
			RepoID int64 `json:"repo_id"`
		} `json:"events"`
	}
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.Events, 1)
	require.Equal(t, int64(300029405), out.Events[0].RepoID)
	require.Equal(t, 1, srv.github.calls("/repos/"+browserFixtureRepo), "GitHub answered for the repository with the token that faucet keeps")

	hidden, err := call(ctx, session, "octo-org/hidden", map[string]any{"cursor": "v2.0", "timeout_s": 1})
	require.NoError(t, err)
	require.True(t, hidden.IsError, "a repository that the person's GitHub login cannot read is refused")

	for _, piece := range []string{"ghu_browser_", "ghr_browser_", "app-secret-value", "fct_", "fcr_"} {
		require.NotContains(t, srv.logs.String(), piece, "the log holds no token")
	}
	c, err := srv.store.Connection(ctx, "github:583231")
	require.NoError(t, err)
	require.Equal(t, "octocat", c.Login)
	require.NotContains(t, string(c.AccessToken), "ghu_browser_", "the GitHub token is sealed at rest")
	require.NotContains(t, string(c.RefreshToken), "ghr_browser_")
}

// accessToken logs the person in for the endpoint at path, as go-sdk's
// authorization code handler does, and returns the agent's access token.
func (s *browserServer) accessToken(ctx context.Context, t *testing.T, path string) string {
	t.Helper()
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		ClientIDMetadataDocumentConfig: &auth.ClientIDMetadataDocumentConfig{URL: s.clientID},
		RedirectURL:                    browserRedirectURI,
		Client:                         s.Client(),
		AuthorizationCodeFetcher:       s.person(t),
	})
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: s.URL + path, HTTPClient: s.Client(), OAuthHandler: handler, DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err, s.logs.String())
	_ = session.Close()
	source, err := handler.TokenSource(ctx)
	require.NoError(t, err)
	token, err := source.Token()
	require.NoError(t, err)
	return token.AccessToken
}

// A person who logged in through a browser is checked through the
// Authorizer that run() composes. Once GitHub answered for a repository, the
// next wait and poll answer from faucet's access checks, and every
// subscription, which renews, asks GitHub again (R3).
func TestABrowserUserAsksGitHubOnceForReadsAndAtEveryRenewal(t *testing.T) {
	t.Parallel()
	key, err := faucet.NewWebhookKey(bytes.Repeat([]byte{1}, 32))
	require.NoError(t, err)
	allow, err := faucet.NewCallbackAllowlist([]string{"https://hooks.example.com"}, false)
	require.NoError(t, err)
	srv := newBrowserServer(t, func(d *faucet.Deps) { d.Webhook = &faucet.WebhookDeps{Key: key, Callbacks: allow} })
	srv.deliver(t, "d-1")
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	agent := connectSubscriber(ctx, t, &testServer{Server: srv.Server, mcpPath: "/github/mcp"}, srv.accessToken(ctx, t, "/github/mcp"))
	lookups := func() int { return srv.github.calls("/repos/" + browserFixtureRepo) }

	for i := range 2 {
		res, err := call(ctx, agent, browserFixtureRepo, map[string]any{"cursor": "v2.0", "timeout_s": 1})
		require.NoError(t, err)
		require.False(t, res.IsError, toolText(t, res))
		_, err = callEvents(ctx, agent, "events/poll", pollParams(browserFixtureRepo, nil))
		require.NoError(t, err)
		require.Equal(t, 1, lookups(), "round %d: GitHub answered once for the reads", i)
	}
	for i := range 2 {
		require.NoError(t, subscribeTo(ctx, agent, browserFixtureRepo))
		require.Equal(t, 2+i, lookups(), "subscription %d asked GitHub", i)
	}
}

// A token of the browser login is for the endpoint that the person logged in
// for, and the other endpoint refuses it.
func TestATokenOfABrowserLoginWorksOnItsEndpointOnly(t *testing.T) {
	t.Parallel()
	srv := newBrowserServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		ClientIDMetadataDocumentConfig: &auth.ClientIDMetadataDocumentConfig{URL: srv.clientID},
		RedirectURL:                    browserRedirectURI,
		Client:                         srv.Client(),
		AuthorizationCodeFetcher:       srv.person(t),
	})
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: srv.URL + "/mcp", HTTPClient: srv.Client(), OAuthHandler: handler, DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err, srv.logs.String())
	t.Cleanup(func() { _ = session.Close() })
	source, err := handler.TokenSource(ctx)
	require.NoError(t, err)
	token, err := source.Token()
	require.NoError(t, err)
	require.NotEmpty(t, token.RefreshToken, "the login gives a refresh token")

	list := func(path string) int {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		return resp.StatusCode
	}
	require.Equal(t, http.StatusOK, list("/mcp"))
	require.Equal(t, http.StatusUnauthorized, list("/github/mcp"))
}
