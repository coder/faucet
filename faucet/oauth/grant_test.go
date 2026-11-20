package oauth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/quartz"
)

const (
	grantClientID    = "https://agent.example.com/client.json"
	grantRedirectURI = "http://127.0.0.1:33418/callback"
	// grantGitHubLatency is how long the GitHub stub takes to answer a token
	// request, so that refreshes which race overlap there.
	grantGitHubLatency = 5 * time.Millisecond
)

// grantGitHub stands in for GitHub: its token endpoint, GET /user and GET
// /repos. Each refresh token works once, as GitHub's do.
type grantGitHub struct {
	*httptest.Server
	mu sync.Mutex
	// logins, access and refresh hold the users and their live tokens.
	logins  map[int64]string
	access  map[string]int64
	refresh map[string]int64
	issued  int
	// refreshes counts the token requests that GitHub received.
	refreshes int
	// repoLookups are the repositories, owner/name, that GitHub was asked
	// about, in the order the lookups arrived.
	repoLookups []string
	// tokenStatus and repoStatus, when not zero, answer every token request
	// or every repository lookup.
	tokenStatus, repoStatus int
	// denied are the repositories, owner/name, that no token can read.
	denied map[string]bool
	// renamed maps the old name of a renamed repository, owner/name, to its
	// new one: GitHub answers the old name with the repository of the new.
	renamed map[string]string
	// onRefresh runs when a token request arrives, before the answer.
	onRefresh func()
	// onRepo runs when a repository lookup arrives, before the answer.
	onRepo func(r *http.Request)
	// userAs, when not zero, is the user ID that GET /user gives for every
	// live token.
	userAs int64
}

func newGrantGitHub(t *testing.T) *grantGitHub {
	t.Helper()
	g := &grantGitHub{
		logins: map[int64]string{}, access: map[string]int64{}, refresh: map[string]int64{},
		denied: map[string]bool{}, renamed: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", g.token)
	mux.HandleFunc("GET /user", g.user)
	mux.HandleFunc("GET /repos/{owner}/{name}", g.repo)
	g.Server = httptest.NewServer(mux)
	t.Cleanup(g.Close)
	return g
}

// issue gives the user with this ID and login a new pair of tokens.
func (g *grantGitHub) issue(userID int64, login string) (access, refresh string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.logins[userID] = login
	return g.issueLocked(userID)
}

func (g *grantGitHub) issueLocked(userID int64) (access, refresh string) {
	g.issued++
	access, refresh = fmt.Sprintf("ghu_%d_%d", userID, g.issued), fmt.Sprintf("ghr_%d_%d", userID, g.issued)
	g.access[access], g.refresh[refresh] = userID, userID
	return access, refresh
}

// revoke makes GitHub reject every token of the user with this ID, as when
// the user revokes the App.
func (g *grantGitHub) revoke(userID int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, tokens := range []map[string]int64{g.access, g.refresh} {
		for token, id := range tokens {
			if id == userID {
				delete(tokens, token)
			}
		}
	}
}

// rejectAccess makes GitHub reject the access token, and nothing else.
func (g *grantGitHub) rejectAccess(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.access, token)
}

func (g *grantGitHub) set(change func(g *grantGitHub)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change(g)
}

func (g *grantGitHub) counts() (refreshes, repoChecks int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refreshes, len(g.repoLookups)
}

// repos is the RepoChecker that asks this GitHub.
func (g *grantGitHub) repos() faucet.TokenAuthorizer {
	return faucet.TokenAuthorizer{BaseURL: g.URL, Client: g.Client()}
}

// lookups returns the repositories that GitHub was asked about, in the
// order the lookups arrived.
func (g *grantGitHub) lookups() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.repoLookups)
}

func grantJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func grantBearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (g *grantGitHub) token(w http.ResponseWriter, r *http.Request) {
	presented := r.PostFormValue("refresh_token")
	time.Sleep(grantGitHubLatency)
	g.mu.Lock()
	g.refreshes++
	hook := g.onRefresh
	g.mu.Unlock()
	if hook != nil {
		hook()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	user, ok := g.refresh[presented]
	switch {
	case g.tokenStatus != 0:
		w.WriteHeader(g.tokenStatus)
	case !ok || r.PostFormValue("grant_type") != "refresh_token" || r.PostFormValue("client_secret") != "app-secret":
		grantJSON(w, map[string]any{"error": "bad_refresh_token"})
	default:
		delete(g.refresh, presented)
		access, refresh := g.issueLocked(user)
		grantJSON(w, map[string]any{
			"access_token": access, "expires_in": 28800, "refresh_token": refresh,
			"refresh_token_expires_in": 15897600, "token_type": "bearer",
		})
	}
}

func (g *grantGitHub) user(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id, ok := g.access[grantBearer(r)]
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if g.userAs != 0 {
		id = g.userAs
	}
	grantJSON(w, map[string]any{"id": id, "login": g.logins[id]})
}

func (g *grantGitHub) repo(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	g.mu.Lock()
	g.repoLookups = append(g.repoLookups, repo)
	hook := g.onRepo
	g.mu.Unlock()
	if hook != nil {
		hook(r)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.access[grantBearer(r)]
	switch {
	case g.repoStatus != 0:
		w.WriteHeader(g.repoStatus)
	case !ok:
		w.WriteHeader(http.StatusUnauthorized)
	case g.denied[strings.ToLower(repo)]:
		w.WriteHeader(http.StatusNotFound)
	default:
		name := strings.ToLower(repo)
		if renamed, ok := g.renamed[name]; ok {
			name = renamed
		}
		grantJSON(w, map[string]any{"id": grantRepoID(name), "full_name": name})
	}
}

// grantRepoID is the ID that the GitHub stub gives the repository with this
// name, owner/name: one ID per name, whatever its case.
func grantRepoID(repo string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(repo)))
	return int64(h.Sum32()) + 1
}

// grantHarness serves the token endpoint of a server with a browser login,
// over store and a GitHub stub, and records each token request.
type grantHarness struct {
	t      *testing.T
	store  oauth.Store
	clock  *quartz.Mock
	github *grantGitHub
	// repos asks github, for the server and for each UserAccess of the
	// harness.
	repos  *cancelingRepos
	login  *oauth.BrowserLogin
	server *oauth.Server
	srv    *httptest.Server

	mu       sync.Mutex
	received []grantRequest
}

// grantRequest is a token request as the server received it.
type grantRequest struct {
	authorization string
	form          url.Values
}

func newGrantHarness(t *testing.T, store oauth.Store) *grantHarness {
	t.Helper()
	h := &grantHarness{t: t, store: store, clock: quartz.NewMock(t), github: newGrantGitHub(t)}
	h.repos = &cancelingRepos{RepoChecker: h.github.repos()}
	h.login = testBrowserLogin(t, h.github.URL)
	server, err := oauth.NewServer(oauth.ServerDeps{
		Store: store, Clock: h.clock, Logger: slog.Make(), Issuer: issuer,
		Resources: []string{resource, issuer + "/mcp"}, Login: h.login, Repos: h.repos,
	})
	require.NoError(t, err)
	h.server = server
	token := server.TokenHandler()
	mux := http.NewServeMux()
	mux.Handle("POST "+oauth.TokenPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		h.mu.Lock()
		h.received = append(h.received, grantRequest{authorization: r.Header.Get("Authorization"), form: form})
		h.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		token.ServeHTTP(w, r)
	}))
	mux.Handle("POST "+oauth.RevokePath, server.RevokeHandler())
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *grantHarness) tokenRequests() []grantRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]grantRequest(nil), h.received...)
}

// grantLogin is a browser login that its user finished at GitHub: the
// request is approved, and the client has yet to redeem the code.
type grantLogin struct{ id, code, verifier string }

// grantSession is a session as its client holds it: the ID and the latest
// tokens.
type grantSession struct{ id, access, refresh string }

func grantSecret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(b)
}

// connection is the connection of the GitHub user with this ID, with a new
// pair of the stub's tokens that lasts eight hours from the clock's time.
func (h *grantHarness) connection(userID int64) oauth.Connection {
	h.t.Helper()
	now := h.clock.Now()
	c := oauth.Connection{
		ID: oauth.ConnectionID(oauth.ProviderGitHub, userID), Provider: oauth.ProviderGitHub, UpstreamUserID: userID,
		Login: fmt.Sprintf("user%d", userID), AccessExpiresAt: now.Add(8 * time.Hour),
		RefreshExpiresAt: now.Add(180 * 24 * time.Hour), UpdatedAt: now,
	}
	access, refresh := h.github.issue(userID, c.Login)
	require.NoError(h.t, oauth.GrantSeal(h.login.Keys, &c, access, refresh))
	return c
}

// approve stores the request with this ID and approves it for the GitHub
// user with this ID, whose connection gets a new pair of tokens, all at the
// clock's time.
func (h *grantHarness) approve(id string, userID int64) grantLogin {
	h.t.Helper()
	return h.approveFor(id, userID, resource)
}

// approveFor is approve for a login to the endpoint login.
func (h *grantHarness) approveFor(id string, userID int64, login string) grantLogin {
	h.t.Helper()
	ctx := context.Background()
	now := h.clock.Now()
	l := grantLogin{id: id, code: grantSecret(h.t), verifier: grantSecret(h.t)}
	challenge := sha256.Sum256([]byte(l.verifier))
	browser, state := oauth.HashToken("browser "+id), oauth.HashToken("github state "+id)
	require.NoError(h.t, h.store.ConsentAuthRequest(ctx, oauth.AuthRequest{
		ID: id, State: oauth.AuthRequestPending, ClientID: grantClientID, ClientName: "Example Agent",
		RedirectURI: grantRedirectURI, Resource: login, Scopes: bothScopes,
		CodeChallenge: base64.RawURLEncoding.EncodeToString(challenge[:]), BrowserHash: browser, GitHubStateHash: state,
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}, now))
	require.NoError(h.t, h.store.ApproveAuthRequest(ctx, oauth.Approval{
		RequestID: id, GitHubState: state, Connection: h.connection(userID), Code: oauth.HashToken(l.code),
		Now: now, ExpiresAt: now.Add(60 * time.Second),
	}, func(oauth.Connection) bool { return true }))
	return l
}

// post sends form to the token endpoint with authorization as the
// Authorization header, none when it is empty.
func (h *grantHarness) post(authorization string, form url.Values) tokenReply {
	h.t.Helper()
	reply, err := h.send(authorization, form)
	require.NoError(h.t, err)
	return reply
}

// send is post for a goroutine other than the test's.
func (h *grantHarness) send(authorization string, form url.Values) (tokenReply, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, h.srv.URL+oauth.TokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenReply{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		return tokenReply{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return tokenReply{}, err
	}
	reply := tokenReply{status: resp.StatusCode, header: resp.Header, rawBody: string(raw)}
	if err := json.Unmarshal(raw, &reply.body); err != nil {
		return tokenReply{}, fmt.Errorf("decode %q: %w", raw, err)
	}
	return reply, nil
}

// grantCodeForm is the token request of a public client that redeems l.
func grantCodeForm(l grantLogin) url.Values {
	return url.Values{
		"grant_type": {"authorization_code"}, "code": {l.code}, "code_verifier": {l.verifier},
		"redirect_uri": {grantRedirectURI}, "client_id": {grantClientID},
	}
}

// grantRefreshForm is the token request of a public client that presents
// refresh.
func grantRefreshForm(refresh string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {grantClientID}}
}

// grantTokens reads the tokens of a reply that must have succeeded.
func grantTokens(t *testing.T, id string, reply tokenReply) grantSession {
	t.Helper()
	require.Equal(t, http.StatusOK, reply.status, reply.rawBody)
	access, _ := reply.body["access_token"].(string)
	refresh, _ := reply.body["refresh_token"].(string)
	require.Regexp(t, `^fct_[A-Za-z0-9_-]{43}$`, access)
	require.Regexp(t, `^fcr_[A-Za-z0-9_-]{43}$`, refresh)
	return grantSession{id: id, access: access, refresh: refresh}
}

func (h *grantHarness) redeem(l grantLogin) grantSession {
	h.t.Helper()
	return grantTokens(h.t, l.id, h.post("", grantCodeForm(l)))
}

func (h *grantHarness) refresh(s grantSession) grantSession {
	h.t.Helper()
	return grantTokens(h.t, s.id, h.post("", grantRefreshForm(s.refresh)))
}

func (h *grantHarness) session(id string) oauth.Session {
	h.t.Helper()
	s, err := h.store.Session(context.Background(), id)
	require.NoError(h.t, err)
	return s
}

// refreshToken returns the stored refresh token of the session with this
// ID that refresh is.
func (h *grantHarness) refreshToken(id, refresh string) oauth.RefreshToken {
	h.t.Helper()
	session, token, err := h.store.SessionByRefreshToken(context.Background(), oauth.HashToken(refresh))
	require.NoError(h.t, err)
	require.Equal(h.t, id, session.ID)
	return token
}

func grantRequireRefused(t *testing.T, reply tokenReply, status int, code, name string) {
	t.Helper()
	require.Equal(t, status, reply.status, "%s: %s", name, reply.rawBody)
	require.Equal(t, code, reply.body["error"], "%s: %s", name, reply.rawBody)
	require.NotEmpty(t, reply.body["error_description"], name)
}

// grantBarrierStore can hold reads of a code or a refresh token back until
// a number of them have arrived, so that requests which race all read the
// row before any of them changes it.
type grantBarrierStore struct {
	oauth.Store
	mu      sync.Mutex
	waiting int
	release chan struct{}
}

func newGrantBarrierStore() *grantBarrierStore {
	return &grantBarrierStore{Store: newFakeStore()}
}

// hold makes the next n reads wait until all n have arrived.
func (s *grantBarrierStore) hold(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waiting, s.release = n, make(chan struct{})
}

func (s *grantBarrierStore) arrive() {
	s.mu.Lock()
	if s.waiting == 0 {
		s.mu.Unlock()
		return
	}
	s.waiting--
	release := s.release
	if s.waiting == 0 {
		close(release)
	}
	s.mu.Unlock()
	<-release
}

func (s *grantBarrierStore) AuthRequestByCode(ctx context.Context, code oauth.TokenHash) (oauth.AuthRequest, error) {
	r, err := s.Store.AuthRequestByCode(ctx, code)
	s.arrive()
	return r, err
}

func (s *grantBarrierStore) SessionByRefreshToken(ctx context.Context, refresh oauth.TokenHash) (oauth.Session, oauth.RefreshToken, error) {
	session, token, err := s.Store.SessionByRefreshToken(ctx, refresh)
	s.arrive()
	return session, token, err
}

// raceAll sends each form at once.
func (h *grantHarness) raceAll(forms ...url.Values) []tokenReply {
	h.t.Helper()
	start := make(chan struct{})
	replies := make([]tokenReply, len(forms))
	errs := make([]error, len(forms))
	var wg sync.WaitGroup
	for i, form := range forms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			replies[i], errs[i] = h.send("", form)
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		require.NoError(h.t, err)
	}
	return replies
}
