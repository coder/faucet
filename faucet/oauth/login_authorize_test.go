package oauth_test

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
)

// loginStoredRequest returns the request with this ID as the store keeps it.
func loginStoredRequest(t *testing.T, s *fakeStore, id string) oauth.AuthRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.requests[id]
	require.True(t, ok, "the store holds the request")
	return cloneAuthRequest(r)
}

func loginHasRequest(s *fakeStore, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.requests[id]
	return ok
}

func loginRequestCount(s *fakeStore) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// loginRequirePage checks that a is an HTML page with the headers of every
// page of the browser login.
func loginRequirePage(t *testing.T, a loginAnswer) {
	t.Helper()
	require.Equal(t, "text/html; charset=utf-8", a.header.Get("Content-Type"))
	loginRequireHeaders(t, a)
}

// loginRequireHeaders checks the headers of every answer of the browser
// login.
func loginRequireHeaders(t *testing.T, a loginAnswer) {
	t.Helper()
	require.Equal(t, "no-store", a.header.Get("Cache-Control"))
	csp := loginCSP(t, a)
	require.Regexp(t, `^'sha256-[A-Za-z0-9+/]{43}='$`, strings.Join(csp["script-src"], " "), "one script may run, named by its hash")
	delete(csp, "script-src")
	require.Equal(t, map[string][]string{
		"default-src": {"'none'"}, "style-src": {"'unsafe-inline'"}, "frame-ancestors": {"'none'"}, "base-uri": {"'none'"},
	}, csp)
	require.Equal(t, "DENY", a.header.Get("X-Frame-Options"))
	require.Equal(t, "same-origin", a.header.Get("Referrer-Policy"))
	require.Equal(t, "nosniff", a.header.Get("X-Content-Type-Options"))
}

// loginCSP returns the directives of the Content-Security-Policy of a, each
// with its sources.
func loginCSP(t *testing.T, a loginAnswer) map[string][]string {
	t.Helper()
	csp := map[string][]string{}
	for _, directive := range strings.Split(a.header.Get("Content-Security-Policy"), ";") {
		fields := strings.Fields(directive)
		require.NotEmpty(t, fields, "the policy has an empty directive")
		require.NotContains(t, csp, fields[0], "the policy names a directive twice")
		csp[fields[0]] = fields[1:]
	}
	return csp
}

// loginSetCookie returns the cookie named name that a sets, or nil.
func loginSetCookie(t *testing.T, a loginAnswer, name string) *http.Cookie {
	t.Helper()
	for _, line := range a.header.Values("Set-Cookie") {
		c, err := http.ParseSetCookie(line)
		require.NoError(t, err)
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestAnAuthorizationRequestThatFaucetDoesNotTakeIsAnErrorPageAndNoRedirect(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	client := h.newClient("agent", nil)
	impostor := h.newClient("impostor", map[string]any{"client_id": h.docs.URL + "/agent.json"})
	for name, c := range map[string]struct {
		edit func(q url.Values)
		// detail is in what the page tells the client's developer.
		detail string
	}{
		"a parameter that appears twice":              {func(q url.Values) { q.Add("state", "again") }, "each parameter must appear at most once"},
		"no client_id":                                {func(q url.Values) { q.Del("client_id") }, "client_id is required"},
		"a client_id over http":                       {func(q url.Values) { q.Set("client_id", strings.Replace(client.id, "https:", "http:", 1)) }, "must be an https URL"},
		"a client_id whose host is not ASCII":         {func(q url.Values) { q.Set("client_id", "https://vѕcode.dev/agent.json") }, "must be ASCII"},
		"a client_id where no document is":            {func(q url.Values) { q.Set("client_id", h.docs.URL+"/missing.json") }, "status 200"},
		"a document that names another client":        {func(q url.Values) { q.Set("client_id", impostor.id) }, "client_id must equal its URL"},
		"no redirect_uri":                             {func(q url.Values) { q.Del("redirect_uri") }, "redirect_uri is required"},
		"a redirect_uri that the document lacks":      {func(q url.Values) { q.Set("redirect_uri", h.agents.URL+"/cb/elsewhere") }, "redirect_uri must be one of"},
		"no response_type":                            {func(q url.Values) { q.Del("response_type") }, "response_type must be code"},
		"the response_type token":                     {func(q url.Values) { q.Set("response_type", "token") }, "response_type must be code"},
		"no code_challenge":                           {func(q url.Values) { q.Del("code_challenge") }, "code_challenge must be"},
		"a code_challenge of 42 characters":           {func(q url.Values) { q.Set("code_challenge", strings.Repeat("a", 42)) }, "code_challenge must be"},
		"a code_challenge of 43 other characters":     {func(q url.Values) { q.Set("code_challenge", strings.Repeat("+", 43)) }, "code_challenge must be"},
		"no code_challenge_method":                    {func(q url.Values) { q.Del("code_challenge_method") }, "code_challenge_method must be S256"},
		"the code_challenge_method plain":             {func(q url.Values) { q.Set("code_challenge_method", "plain") }, "code_challenge_method must be S256"},
		"no resource":                                 {func(q url.Values) { q.Del("resource") }, "resource must be an endpoint"},
		"an endpoint that this faucet does not serve": {func(q url.Values) { q.Set("resource", h.issuer+"/other/mcp") }, "resource must be an endpoint"},
		"only scopes that faucet does not know":       {func(q url.Values) { q.Set("scope", "repo admin:org") }, "scope must name at least one scope"},
		"a state of 2049 bytes":                       {func(q url.Values) { q.Set("state", strings.Repeat("s", 2049)) }, "state must be at most 2048 bytes"},
	} {
		q := h.request(client)
		c.edit(q)
		a := h.newBrowser(t).authorize(q)
		require.Equal(t, http.StatusBadRequest, a.status, name)
		require.Empty(t, a.header.Get("Location"), name)
		require.Empty(t, a.header.Values("Set-Cookie"), name)
		loginRequirePage(t, a)
		require.Contains(t, a.body, "Go back to your agent and start the login again", name)
		require.Contains(t, a.body, c.detail, name)
	}

	a := h.newBrowser(t).get(h.issuer + oauth.AuthorizePath + "?" + h.request(client).Encode() + "&junk=%zz")
	require.Equal(t, http.StatusBadRequest, a.status, "a query that does not parse")
	require.Empty(t, a.header.Get("Location"))

	require.Zero(t, loginRequestCount(h.store), "no refused request is stored")
	require.Zero(t, h.github.authorizeCount())
}

// The cause of a failed fetch can name an address or a resolver of faucet's
// own network, so only the log gets it.
func TestTheErrorPageOfADocumentThatCannotBeFetchedKeepsTheCauseToTheLog(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	c := h.newClient("unreachable", nil)
	q := h.request(c)
	q.Set("client_id", "https://127.0.0.1:1/unreachable.json")

	page := h.newBrowser(t).authorize(q)
	require.Equal(t, http.StatusBadRequest, page.status)
	require.Empty(t, page.header.Get("Location"))
	require.Contains(t, page.body, "faucet could not fetch the client&#39;s document")
	for _, cause := range []string{"connection refused", "dial tcp", "connect:"} {
		require.NotContains(t, page.body, cause)
	}
	require.Contains(t, h.logs.String(), "connection refused")
}

func TestTheEdgesOfAnAuthorizationRequestAreTaken(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	client := h.newClient("agent", nil)
	for name, c := range map[string]struct {
		edit   func(q url.Values)
		scopes []faucet.Scope
		state  string
	}{
		"a state of 2048 bytes": {
			edit: func(q url.Values) { q.Set("state", strings.Repeat("s", 2048)) }, scopes: bothScopes, state: strings.Repeat("s", 2048),
		},
		"no state":                            {edit: func(q url.Values) { q.Del("state") }, scopes: bothScopes},
		"no scope, which asks for all":        {edit: func(q url.Values) { q.Del("scope") }, scopes: bothScopes, state: "agent-state-/cb/agent"},
		"an unknown scope beside a known one": {edit: func(q url.Values) { q.Set("scope", "repo events:read") }, scopes: []faucet.Scope{faucet.ScopeEventsRead}, state: "agent-state-/cb/agent"},
	} {
		b := h.newBrowser(t)
		q := h.request(client)
		c.edit(q)
		page := b.authorize(q)
		require.Equal(t, http.StatusOK, page.status, name)
		consent := b.consentOf(page)
		require.Equal(t, http.StatusSeeOther, b.continueWith(consent).status, name)
		stored := loginStoredRequest(t, h.store, consent.id)
		require.Equal(t, c.scopes, stored.Scopes, name)
		require.Equal(t, c.state, stored.ClientState, name)
	}
}

func TestTheConsentPageSaysWhoAsksForWhatAndWhereTheBrowserReturns(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	client := h.newClient("agent", nil)
	b := h.newBrowser(t)
	page := b.authorize(h.request(client))
	require.Equal(t, http.StatusOK, page.status, page.body)
	loginRequirePage(t, page)

	host := h.docs.Listener.Addr().String()
	require.Contains(t, page.body, "<h1>"+host+"</h1>")
	require.Contains(t, page.body, "says it is <strong>Agent agent</strong>")
	require.Contains(t, page.body, "<code>"+h.resource+"</code>")
	require.Contains(t, page.body, "<code>events:read</code>: wait for and read the GitHub events")
	require.Contains(t, page.body, "<code>events:subscribe</code>: have faucet send those events")
	require.Contains(t, page.body, "returns to</dt>\n<dd><code>"+client.redirectURI+"</code>")
	require.Contains(t, page.body, "faucet keeps the GitHub token that it gets there for you, sealed, and gives this app only its own faucet token")
	require.Contains(t, page.body, `<form method="post" action="/oauth/authorize">`)

	consent := b.consentOf(page)
	cookie := loginSetCookie(t, page, "__Host-faucet_"+consent.id)
	require.NotNil(t, cookie)
	require.True(t, cookie.Secure)
	require.True(t, cookie.HttpOnly)
	require.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
	require.Equal(t, "/", cookie.Path)
	require.Empty(t, cookie.Domain)
	require.Equal(t, 600, cookie.MaxAge)

	require.Zero(t, loginRequestCount(h.store), "the page stores nothing")
	now := h.clock.Now()
	h.clock.Advance(time.Minute)
	require.Equal(t, http.StatusSeeOther, b.continueWith(consent).status)
	stored := loginStoredRequest(t, h.store, consent.id)
	require.NotEqual(t, oauth.TokenHash{}, stored.GitHubStateHash, "Continue stores the GitHub state")
	stored.GitHubStateHash = oauth.TokenHash{}
	require.Equal(t, oauth.AuthRequest{
		ID: consent.id, State: oauth.AuthRequestPending, ClientID: client.id, ClientName: "Agent agent",
		RedirectURI: client.redirectURI, ClientState: "agent-state-/cb/agent", Resource: h.resource, Scopes: bothScopes,
		CodeChallenge: loginS256(client.verifier()), BrowserHash: oauth.HashToken(cookie.Value),
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}, stored, "the request is the one of the page, sealed in its form")
}

var inlineScript = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

// GitHub asks nothing of a user who authorized faucet's App before, so a
// click on Continue is all that a login needs. A page of another site can
// open the consent page under a click that the user meant for that page,
// such as the second click of a double-click. Continue starts disabled, and
// the one script that the policy lets run turns it on.
func TestContinueStartsDisabledAndOnlyThePagesOwnScriptMayRun(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	page := h.newBrowser(t).authorize(h.request(h.newClient("agent", nil)))
	require.Equal(t, http.StatusOK, page.status, page.body)
	require.Equal(t, 1, strings.Count(page.body, "<button"))
	require.Contains(t, page.body, `<button type="submit" disabled>Continue with GitHub</button>`)
	require.Regexp(t, `(?s)<noscript>.*JavaScript.*</noscript>`, page.body, "a browser without JavaScript says why Continue stays off")

	scripts := inlineScript.FindAllStringSubmatch(page.body, -1)
	require.Len(t, scripts, 1, "the page has one inline script")
	require.Equal(t, 1, strings.Count(page.body, "<script"), "and no script of another form")
	sum := sha256.Sum256([]byte(scripts[0][1]))
	require.Equal(t, []string{"'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"}, loginCSP(t, page)["script-src"],
		"the policy lets that script run, and no other")
}

func TestTheConsentPageOfAClientWithoutANameSaysSo(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	client := h.newClient("nameless", map[string]any{"client_name": nil})
	page := h.newBrowser(t).authorize(h.request(client))
	require.Equal(t, http.StatusOK, page.status, page.body)
	require.Contains(t, page.body, "gives no name.")
	require.NotContains(t, page.body, "says it is")
}

func TestTheConsentPageEscapesWhatTheClientWrites(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	const hostile = `<img src=x onerror="alert(1)">`
	client := h.newClient("hostile", map[string]any{"client_name": hostile})
	q := h.request(client)
	q.Set("state", `"><script>alert(2)</script>`)
	page := h.newBrowser(t).authorize(q)
	require.Equal(t, http.StatusOK, page.status, page.body)
	require.NotContains(t, page.body, "<img")
	require.NotContains(t, page.body, "<script>alert(2)")
	require.Contains(t, page.body, "&lt;img src=x onerror=&#34;alert(1)&#34;&gt;")
}

func TestContinueSendsTheBrowserToGitHubWithACookieOfItsOwn(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	client := h.newClient("agent", nil)
	b := h.newBrowser(t)
	consent := b.start(client)
	answer := b.continueWith(consent)
	require.Equal(t, http.StatusSeeOther, answer.status, answer.body)
	loginRequireHeaders(t, answer)

	to := answer.location(t)
	require.Equal(t, h.github.URL+"/login/oauth/authorize", loginEndpoint(to))
	require.Equal(t, loginAppClientID, to.Query().Get("client_id"))
	require.Equal(t, h.issuer+oauth.GitHubCallbackPath, to.Query().Get("redirect_uri"))
	require.Equal(t, "S256", to.Query().Get("code_challenge_method"))
	require.Len(t, to.Query().Get("code_challenge"), 43)
	require.NotEmpty(t, to.Query().Get("state"))

	cookie := loginSetCookie(t, answer, "__Host-faucet_"+consent.id+"_gh")
	require.NotNil(t, cookie)
	require.True(t, cookie.Secure)
	require.True(t, cookie.HttpOnly)
	require.Equal(t, http.SameSiteLaxMode, cookie.SameSite, "the cookie comes along on GitHub's redirect back")
	require.Equal(t, "/", cookie.Path)
	require.Equal(t, 600, cookie.MaxAge)
	require.Nil(t, loginSetCookie(t, answer, consent.cookie.Name), "the browser keeps its cookie")

	again := b.continueWith(consent)
	require.Equal(t, http.StatusSeeOther, again.status, "Continue works again, as after the back button")
	require.NotEqual(t, to.Query().Get("state"), again.location(t).Query().Get("state"))
}

func TestAConsentFormThatIsNotASmallFormIsRefused(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	client := h.newClient("agent", nil)
	b := h.newBrowser(t)
	for name, send := range map[string]func(c loginConsent) loginAnswer{
		"a field twice": func(c loginConsent) loginAnswer {
			form := url.Values{"request": {c.id, c.id}, "login": c.form["login"], "token": c.form["token"]}
			return b.post(form, []string{h.issuer})
		},
		"a form over 16 KiB": func(c loginConsent) loginAnswer {
			form := url.Values{"request": {c.id}, "login": c.form["login"], "token": c.form["token"], "pad": {strings.Repeat("p", 16<<10)}}
			return b.post(form, []string{h.issuer})
		},
		"a multipart body": func(c loginConsent) loginAnswer {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.issuer+oauth.AuthorizePath, strings.NewReader("--x\r\n\r\n--x--\r\n"))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
			req.Header.Set("Origin", h.issuer)
			return b.do(req)
		},
	} {
		consent := b.start(client)
		refused := send(consent)
		require.Equal(t, http.StatusForbidden, refused.status, name)
		require.Empty(t, refused.header.Values("Set-Cookie"), name)
		require.Empty(t, refused.header.Get("Location"), name)
		loginRequirePage(t, refused)
	}
	require.Zero(t, loginRequestCount(h.store), "a refused form stores nothing")
	first, second := b.start(client), b.start(client)
	b.post(url.Values{"request": {first.id, second.id}, "login": first.form["login"], "token": first.form["token"]}, []string{h.issuer})
	for _, c := range []loginConsent{first, second} {
		require.Equal(t, http.StatusSeeOther, b.continueWith(c).status, "a form with a field twice leaves each login that it names to its own form")
	}
	require.Zero(t, h.github.authorizeCount())
}

func TestAConsentAfterTheRequestExpiredIsRefused(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	client := h.newClient("agent", nil)
	b := h.newBrowser(t)
	consent := b.start(client)
	h.clock.Advance(10 * time.Minute)
	refused := b.continueWith(consent)
	require.Equal(t, http.StatusForbidden, refused.status, refused.body)
	require.Empty(t, refused.header.Values("Set-Cookie"))

	fresh := b.start(client)
	h.clock.Advance(10*time.Minute - time.Second)
	require.Equal(t, http.StatusSeeOther, b.continueWith(fresh).status, "a second before its expiry the request still works")
}

func TestTheAuthorizationEndpointTakesOnlyGetAndPost(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, h.issuer+oauth.AuthorizePath, nil)
	require.NoError(t, err)
	a := h.newBrowser(t).do(req)
	require.Equal(t, http.StatusMethodNotAllowed, a.status)
	require.Equal(t, "GET, POST", a.header.Get("Allow"))
	loginRequirePage(t, a)
}
