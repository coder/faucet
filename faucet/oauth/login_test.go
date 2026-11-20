package oauth_test

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/quartz"
)

// Made-up values of faucet's GitHub App and of the user who logs in.
const (
	loginAppClientID = "Iv1.loginapp"
	loginAppSecret   = "login-app-secret-5d1e0c"
	loginUserID      = 583231
	loginUserLogin   = "octo-user"
	loginForeign     = "https://attacker.example"
)

// loginGitHub stands in for GitHub. Its authorize page sends the browser
// straight back with a code, as GitHub does for a user who authorized the
// App before; its token endpoint trades a code for tokens once, under PKCE,
// and a refresh token for a new pair once; and /user names the user of an
// access token.
type loginGitHub struct {
	*httptest.Server
	mu sync.Mutex
	// authorizes counts the browsers that reached the authorize page,
	// trades the trades of a code and refreshes those of a refresh token.
	authorizes, trades, refreshes int
	// codes are the codes that the authorize page gave and the token
	// endpoint has not traded yet.
	codes map[string]loginGitHubCode
	// users are the access tokens that GitHub takes, and refresh the
	// refresh tokens.
	users, refresh map[string]bool
	// userStatus, when not zero, answers every GET /user.
	userStatus int
	// issued are the tokens of each trade, in order.
	issued []loginGitHubTokens
	// deny makes the user refuse at GitHub.
	deny bool
	// tokenAnswer, when set, is the token endpoint's answer to a good trade.
	tokenAnswer string
	n           int
}

type loginGitHubCode struct{ challenge, redirectURI string }

type loginGitHubTokens struct{ access, refresh string }

func newLoginGitHub(t *testing.T) *loginGitHub {
	t.Helper()
	g := &loginGitHub{codes: map[string]loginGitHubCode{}, users: map[string]bool{}, refresh: map[string]bool{}}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Close)
	return g
}

func (g *loginGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch r.URL.Path {
	case "/login/oauth/authorize":
		g.authorizes++
		q := r.URL.Query()
		if q.Get("client_id") != loginAppClientID || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
			http.Error(w, "bad authorize request", http.StatusBadRequest)
			return
		}
		back := url.Values{"state": {q.Get("state")}}
		if g.deny {
			back.Set("error", "access_denied")
		} else {
			g.n++
			code := fmt.Sprintf("gh-code-%d", g.n)
			g.codes[code] = loginGitHubCode{challenge: q.Get("code_challenge"), redirectURI: q.Get("redirect_uri")}
			back.Set("code", code)
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+back.Encode(), http.StatusFound)
	case "/login/oauth/access_token":
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") == "refresh_token" {
			g.refreshPair(w, r.PostForm)
			return
		}
		g.trades++
		code, ok := g.codes[r.PostForm.Get("code")]
		delete(g.codes, r.PostForm.Get("code"))
		w.Header().Set("Content-Type", "application/json")
		if !ok || r.PostForm.Get("client_id") != loginAppClientID || r.PostForm.Get("client_secret") != loginAppSecret ||
			r.PostForm.Get("redirect_uri") != code.redirectURI || loginS256(r.PostForm.Get("code_verifier")) != code.challenge {
			_, _ = io.WriteString(w, `{"error":"bad_verification_code"}`)
			return
		}
		if g.tokenAnswer != "" {
			_, _ = io.WriteString(w, g.tokenAnswer)
			return
		}
		g.answerPair(w)
	case "/user":
		if g.userStatus != 0 {
			w.WriteHeader(g.userStatus)
			return
		}
		if !g.users[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": loginUserID, "login": loginUserLogin})
	default:
		http.NotFound(w, r)
	}
}

// answerPair gives a new pair of tokens in the answer that GitHub documents
// for a trade and a refresh, at
// https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens.
func (g *loginGitHub) answerPair(w http.ResponseWriter) {
	g.n++
	tokens := loginGitHubTokens{access: fmt.Sprintf("ghu_access%d", g.n), refresh: fmt.Sprintf("ghr_refresh%d", g.n)}
	g.users[tokens.access], g.refresh[tokens.refresh] = true, true
	g.issued = append(g.issued, tokens)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": tokens.access, "expires_in": 28800, "refresh_token": tokens.refresh,
		"refresh_token_expires_in": 15897600, "token_type": "bearer", "scope": "",
	})
}

// refreshPair trades a refresh token for a new pair. As GitHub documents,
// the refresh token and the access token of its pair stop working, and a
// refresh token that GitHub does not take is bad_refresh_token.
func (g *loginGitHub) refreshPair(w http.ResponseWriter, form url.Values) {
	g.refreshes++
	w.Header().Set("Content-Type", "application/json")
	presented := form.Get("refresh_token")
	if !g.refresh[presented] || form.Get("client_id") != loginAppClientID || form.Get("client_secret") != loginAppSecret {
		_, _ = io.WriteString(w, `{"error":"bad_refresh_token","error_description":"The refresh token passed is incorrect or expired."}`)
		return
	}
	delete(g.refresh, presented)
	for _, pair := range g.issued {
		if pair.refresh == presented {
			delete(g.users, pair.access)
		}
	}
	g.answerPair(w)
}

// revoke makes GitHub refuse every token that it gave, as when the user
// revokes the App.
func (g *loginGitHub) revoke() {
	g.mu.Lock()
	defer g.mu.Unlock()
	clear(g.users)
	clear(g.refresh)
}

func (g *loginGitHub) refreshCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refreshes
}

func (g *loginGitHub) authorizeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.authorizes
}

func (g *loginGitHub) tradeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.trades
}

func (g *loginGitHub) set(change func(g *loginGitHub)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change(g)
}

func (g *loginGitHub) lastTokens() loginGitHubTokens {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.issued) == 0 {
		return loginGitHubTokens{}
	}
	return g.issued[len(g.issued)-1]
}

func loginS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// loginDocuments serves the client ID metadata documents over TLS, and
// counts the requests for each path.
type loginDocuments struct {
	*httptest.Server
	mu   sync.Mutex
	docs map[string]map[string]any
	// headers are added to every answer for a path.
	headers map[string]http.Header
	fetches map[string]int
	// hold, when set, keeps each answer back until it is closed.
	hold chan struct{}
}

func newLoginDocuments(t *testing.T) *loginDocuments {
	t.Helper()
	d := &loginDocuments{docs: map[string]map[string]any{}, headers: map[string]http.Header{}, fetches: map[string]int{}}
	d.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		doc, ok := d.docs[r.URL.Path]
		maps.Copy(w.Header(), d.headers[r.URL.Path])
		d.fetches[r.URL.Path]++
		hold := d.hold
		d.mu.Unlock()
		if hold != nil {
			<-hold
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(d.Close)
	return d
}

// loginAgents is where the clients' redirect URIs point: it records the
// query of every request that reaches each path.
type loginAgents struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string][]string
}

func newLoginAgents(t *testing.T) *loginAgents {
	t.Helper()
	a := &loginAgents{hits: map[string][]string{}}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.hits[r.URL.Path] = append(a.hits[r.URL.Path], r.URL.RawQuery)
		_, _ = io.WriteString(w, "agent got it")
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *loginAgents) received(path string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.hits[path]...)
}

// loginLogs holds what the server logged.
type loginLogs struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *loginLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *loginLogs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// loginHarness serves faucet's browser login over TLS, with a stub GitHub,
// a TLS server of client documents that faucet trusts, and the server that
// the clients' redirect URIs point at.
type loginHarness struct {
	t         *testing.T
	issuer    string
	resource  string
	store     *fakeStore
	clock     *quartz.Mock
	github    *loginGitHub
	docs      *loginDocuments
	agents    *loginAgents
	logs      *loginLogs
	keys      oauth.TokenKeys
	transport http.RoundTripper
}

func newLoginHarness(t *testing.T) *loginHarness {
	t.Helper()
	h := &loginHarness{
		t: t, store: newFakeStore(), clock: quartz.NewMock(t), github: newLoginGitHub(t),
		docs: newLoginDocuments(t), agents: newLoginAgents(t), logs: &loginLogs{}, keys: testTokenKeys(t),
	}
	mux := http.NewServeMux()
	srv := httptest.NewUnstartedServer(mux)
	h.issuer = "https://" + srv.Listener.Addr().String()
	h.resource = h.issuer + "/github/mcp"
	pool := x509.NewCertPool()
	pool.AddCert(h.docs.Certificate())
	server, err := oauth.NewServer(oauth.ServerDeps{
		Store: h.store, Clock: h.clock, Logger: slog.Make(sloghuman.Sink(h.logs)).Leveled(slog.LevelDebug),
		Issuer: h.issuer, Resources: []string{h.resource},
		Login: &oauth.BrowserLogin{
			GitHub: oauth.GitHubApp{
				ClientID: loginAppClientID, ClientSecret: loginAppSecret, WebURL: h.github.URL, APIURL: h.github.URL,
				Client: &http.Client{Timeout: 5 * time.Second},
			},
			Keys:      h.keys,
			Documents: faucet.HTTPClientOptions{AllowInsecureLoopback: true, RootCAs: pool},
		},
		Repos: faucet.TokenAuthorizer{BaseURL: h.github.URL, Client: h.github.Client()},
	})
	require.NoError(t, err)
	mux.Handle(oauth.AuthorizePath, server.AuthorizeHandler())
	mux.Handle(oauth.GitHubCallbackPath, server.GitHubCallbackHandler())
	mux.Handle("POST "+oauth.TokenPath, server.TokenHandler())
	srv.StartTLS()
	t.Cleanup(srv.Close)
	h.transport = srv.Client().Transport
	return h
}

// loginClient is a client of the browser login: the URL of its document,
// and its one redirect URI, which points at the agents server.
type loginClient struct {
	id          string
	redirectURI string
	// path is the path of the redirect URI on the agents server.
	path string
}

// newClient serves the document of a client named name, with members of the
// document added or replaced. A member set to nil is left out.
func (h *loginHarness) newClient(name string, members map[string]any) loginClient {
	c := loginClient{id: h.docs.URL + "/" + name + ".json", path: "/cb/" + name}
	c.redirectURI = h.agents.URL + c.path
	doc := map[string]any{"client_id": c.id, "client_name": "Agent " + name, "redirect_uris": []string{c.redirectURI}}
	for k, v := range members {
		if v == nil {
			delete(doc, k)
		} else {
			doc[k] = v
		}
	}
	if uris, ok := doc["redirect_uris"].([]string); ok && len(uris) > 0 {
		c.redirectURI = uris[0]
	}
	h.docs.mu.Lock()
	defer h.docs.mu.Unlock()
	h.docs.docs["/"+name+".json"] = doc
	return c
}

// verifier is the PKCE verifier of c's logins.
func (c loginClient) verifier() string {
	return "agent-verifier-0123456789abcdefghijklmnopqrstuvwxyz" + strings.ReplaceAll(c.path, "/", "-")
}

// request is the query of a valid authorization request of c.
func (h *loginHarness) request(c loginClient) url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {c.id},
		"redirect_uri":          {c.redirectURI},
		"code_challenge":        {loginS256(c.verifier())},
		"code_challenge_method": {"S256"},
		"resource":              {h.resource},
		"scope":                 {"events:read events:subscribe"},
		"state":                 {"agent-state-" + c.path},
	}
}

// loginAnswer is what a browser got for one request.
type loginAnswer struct {
	url    *url.URL
	status int
	header http.Header
	body   string
}

func (a loginAnswer) location(t *testing.T) *url.URL {
	t.Helper()
	require.Contains(t, []int{http.StatusFound, http.StatusSeeOther}, a.status, a.body)
	u, err := a.url.Parse(a.header.Get("Location"))
	require.NoError(t, err)
	return u
}

// loginEndpoint is u without its query.
func loginEndpoint(u *url.URL) string { return u.Scheme + "://" + u.Host + u.Path }

// loginBrowser is one browser, with its own cookie jar. It follows a
// redirect only when a test asks it to.
type loginBrowser struct {
	t      *testing.T
	h      *loginHarness
	jar    http.CookieJar
	client *http.Client
}

func (h *loginHarness) newBrowser(t *testing.T) *loginBrowser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &loginBrowser{t: t, h: h, jar: jar, client: &http.Client{
		Transport: h.transport, Jar: jar, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *loginBrowser) do(req *http.Request) loginAnswer {
	b.t.Helper()
	resp, err := b.client.Do(req)
	require.NoError(b.t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(b.t, err)
	return loginAnswer{url: req.URL, status: resp.StatusCode, header: resp.Header, body: string(body)}
}

func (b *loginBrowser) get(u string) loginAnswer {
	b.t.Helper()
	req, err := http.NewRequestWithContext(b.t.Context(), http.MethodGet, u, nil)
	require.NoError(b.t, err)
	return b.do(req)
}

// authorize opens the authorization endpoint with query q.
func (b *loginBrowser) authorize(q url.Values) loginAnswer {
	b.t.Helper()
	return b.get(b.h.issuer + oauth.AuthorizePath + "?" + q.Encode())
}

// post posts form to the authorization endpoint with these Origin headers,
// and with cookies on top of the jar's.
func (b *loginBrowser) post(form url.Values, origins []string, cookies ...*http.Cookie) loginAnswer {
	b.t.Helper()
	req, err := http.NewRequestWithContext(b.t.Context(), http.MethodPost, b.h.issuer+oauth.AuthorizePath, strings.NewReader(form.Encode()))
	require.NoError(b.t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, origin := range origins {
		req.Header.Add("Origin", origin)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return b.do(req)
}

// cookie returns the browser's cookie for faucet that is named name, or nil.
func (b *loginBrowser) cookie(name string) *http.Cookie {
	u, err := url.Parse(b.h.issuer)
	require.NoError(b.t, err)
	for _, c := range b.jar.Cookies(u) {
		if c.Name == name {
			return &http.Cookie{Name: c.Name, Value: c.Value}
		}
	}
	return nil
}

var consentField = regexp.MustCompile(`<input type="hidden" name="(request|login|token)" value="([^"]*)">`)

// loginConsent is the consent page of a login that a browser started: its
// form and the cookie that the page set.
type loginConsent struct {
	page   loginAnswer
	id     string
	form   url.Values
	cookie *http.Cookie
}

// start opens the authorization endpoint with a valid request of c and reads
// the consent page.
func (b *loginBrowser) start(c loginClient) loginConsent {
	b.t.Helper()
	page := b.authorize(b.h.request(c))
	require.Equal(b.t, http.StatusOK, page.status, page.body)
	return b.consentOf(page)
}

// loginFormOf reads the form of a consent page.
func loginFormOf(t *testing.T, page loginAnswer) url.Values {
	t.Helper()
	require.Equal(t, http.StatusOK, page.status, page.body)
	form := url.Values{}
	for _, m := range consentField.FindAllStringSubmatch(page.body, -1) {
		form.Add(m[1], m[2])
	}
	for _, name := range []string{"request", "login", "token"} {
		require.Len(t, form[name], 1, page.body)
	}
	return form
}

func (b *loginBrowser) consentOf(page loginAnswer) loginConsent {
	b.t.Helper()
	form := loginFormOf(b.t, page)
	id := form.Get("request")
	cookie := b.cookie("__Host-faucet_" + id)
	require.NotNil(b.t, cookie, "the consent page sets the cookie of its request")
	return loginConsent{page: page, id: id, form: form, cookie: cookie}
}

// continueWith presses Continue on the consent page: the browser posts the
// page's form with faucet's issuer as Origin.
func (b *loginBrowser) continueWith(c loginConsent) loginAnswer {
	b.t.Helper()
	return b.post(c.form, []string{b.h.issuer})
}

// loginRequireCode checks that a sends the browser to c's redirect URI with
// a code, the client's state and faucet's issuer, and returns the code.
func (h *loginHarness) loginRequireCode(t *testing.T, a loginAnswer, c loginClient) string {
	t.Helper()
	to := a.location(t)
	require.Equal(t, c.redirectURI, loginEndpoint(to))
	q := to.Query()
	require.NotEmpty(t, q.Get("code"))
	require.Equal(t, "agent-state-"+c.path, q.Get("state"))
	require.Equal(t, h.issuer, q.Get("iss"))
	require.Len(t, q, 3, "the redirect holds the code, the state and the issuer")
	return q.Get("code")
}

// R1: the attacker's server starts a login for the attacker's client, and a
// page of the attacker makes the victim's browser post the consent form. The
// victim authorized the App before, so GitHub would send the browser straight
// back, and the attacker's client would get a code for the victim. A
// refused post stores nothing.
func TestAConsentThatTheBrowserOfTheLoginDidNotSendIsRefusedAndStoresNothing(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	client := h.newClient("attacker", nil)
	ownForm := func(own, _ loginConsent) url.Values { return own.form }
	ownCookie := func(own, _ loginConsent) []*http.Cookie { return []*http.Cookie{own.cookie} }
	noCookie := func(_, _ loginConsent) []*http.Cookie { return nil }
	onlyID := func(own, _ loginConsent) url.Values { return url.Values{"request": {own.id}} }
	for _, c := range []struct {
		name    string
		origins []string
		form    func(own, other loginConsent) url.Values
		cookies func(own, other loginConsent) []*http.Cookie
	}{
		{name: "the victim's browser posts only the request id", origins: []string{loginForeign}, form: onlyID, cookies: noCookie},
		{name: "only the request id, with faucet's Origin", origins: []string{h.issuer}, form: onlyID, cookies: noCookie},
		{name: "the victim's browser posts the request id and the form token", origins: []string{h.issuer}, form: ownForm, cookies: noCookie},
		{name: "a foreign Origin", origins: []string{loginForeign}, form: ownForm, cookies: ownCookie},
		{name: "Origin null", origins: []string{"null"}, form: ownForm, cookies: ownCookie},
		{name: "no Origin", form: ownForm, cookies: ownCookie},
		{name: "a second Origin", origins: []string{h.issuer, loginForeign}, form: ownForm, cookies: ownCookie},
		{
			name: "the cookie of another request", origins: []string{h.issuer}, form: ownForm,
			cookies: func(_, other loginConsent) []*http.Cookie { return []*http.Cookie{other.cookie} },
		},
		{
			name: "another request's cookie value under this request's name, with its form token", origins: []string{h.issuer},
			form: func(own, other loginConsent) url.Values {
				return url.Values{"request": {own.id}, "token": {other.form.Get("token")}}
			},
			cookies: func(own, other loginConsent) []*http.Cookie {
				return []*http.Cookie{{Name: own.cookie.Name, Value: other.cookie.Value}}
			},
		},
		{
			name: "the right cookie and a wrong form token", origins: []string{h.issuer},
			form: func(own, other loginConsent) url.Values {
				return url.Values{"request": {own.id}, "login": {own.form.Get("login")}, "token": {other.form.Get("token")}}
			},
			cookies: ownCookie,
		},
		{
			name: "the sealed request of another login", origins: []string{h.issuer}, cookies: ownCookie,
			form: func(own, other loginConsent) url.Values {
				return url.Values{"request": {own.id}, "login": {other.form.Get("login")}, "token": {own.form.Get("token")}}
			},
		},
		{
			name: "a sealed request with a changed character", origins: []string{h.issuer}, cookies: ownCookie,
			form: func(own, _ loginConsent) url.Values {
				login := []byte(own.form.Get("login"))
				login[len(login)-2] ^= 'A' ^ 'B'
				return url.Values{"request": {own.id}, "login": {string(login)}, "token": {own.form.Get("token")}}
			},
		},
		{
			name: "no sealed request", origins: []string{h.issuer}, cookies: ownCookie,
			form: func(own, _ loginConsent) url.Values {
				return url.Values{"request": {own.id}, "token": {own.form.Get("token")}}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			attacker := h.newBrowser(t)
			own, other := attacker.start(client), attacker.start(client)

			forged := h.newBrowser(t).post(c.form(own, other), c.origins, c.cookies(own, other)...)
			require.Equal(t, http.StatusForbidden, forged.status, forged.body)
			require.Empty(t, forged.header.Values("Set-Cookie"))
			require.Empty(t, forged.header.Get("Location"))

			require.False(t, loginHasRequest(h.store, own.id), "the refused post stored nothing")
			require.Zero(t, h.github.authorizeCount(), "no browser reached GitHub")
			require.Empty(t, h.agents.received(client.path), "no code reached the attacker's client")

			untouched := attacker.continueWith(other)
			require.Equal(t, http.StatusSeeOther, untouched.status, "the browser's other login still goes to GitHub")
			require.Equal(t, h.github.URL+"/login/oauth/authorize", loginEndpoint(untouched.location(t)))
		})
	}
}

// R7: two logins in one browser, interleaved at every step, each reach
// their own client with their own code and state.
func TestTwoLoginsInOneBrowserEachReachTheirOwnClient(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	first, second := h.newClient("first", nil), h.newClient("second", nil)
	b := h.newBrowser(t)

	firstForm, secondForm := loginFormOf(t, b.authorize(h.request(first))), loginFormOf(t, b.authorize(h.request(second)))
	secondToGitHub := b.post(secondForm, []string{h.issuer})
	firstToGitHub := b.post(firstForm, []string{h.issuer})
	secondBack := b.get(secondToGitHub.location(t).String())
	firstBack := b.get(firstToGitHub.location(t).String())
	firstToClient := b.get(firstBack.location(t).String())
	secondToClient := b.get(secondBack.location(t).String())

	firstCode := h.loginRequireCode(t, firstToClient, first)
	secondCode := h.loginRequireCode(t, secondToClient, second)
	require.NotEqual(t, firstCode, secondCode)
	require.Equal(t, http.StatusOK, b.get(firstToClient.location(t).String()).status)
	require.Equal(t, []string{firstToClient.location(t).RawQuery}, h.agents.received(first.path))
	for id, code := range map[string]string{firstForm.Get("request"): firstCode, secondForm.Get("request"): secondCode} {
		stored := loginStoredRequest(t, h.store, id)
		require.Equal(t, oauth.AuthRequestApproved, stored.State)
		require.Equal(t, oauth.HashToken(code), stored.CodeHash)
	}
}
