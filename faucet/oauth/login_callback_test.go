package oauth_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet/oauth"
)

// loginBack starts a login of c in b, presses Continue, lets GitHub answer,
// and returns the consent and the callback URL that GitHub sends the
// browser back to, which the browser has not loaded yet.
func (b *loginBrowser) loginBack(c loginClient) (loginConsent, string) {
	b.t.Helper()
	consent := b.start(c)
	toGitHub := b.continueWith(consent)
	back := b.get(toGitHub.location(b.t).String())
	return consent, back.location(b.t).String()
}

// loginRequireEnded checks that a deletes both cookies of the login with
// this ID.
func loginRequireEnded(t *testing.T, a loginAnswer, id string) {
	t.Helper()
	for _, name := range []string{"__Host-faucet_" + id, "__Host-faucet_" + id + "_gh"} {
		c := loginSetCookie(t, a, name)
		require.NotNil(t, c, name)
		require.Negative(t, c.MaxAge, name)
		require.True(t, c.Secure, name)
		require.Equal(t, "/", c.Path, name)
	}
}

// loginRequireErrorPage checks that a is an error page of status and no
// redirect.
func loginRequireErrorPage(t *testing.T, a loginAnswer, status int) {
	t.Helper()
	require.Equal(t, status, a.status, a.body)
	require.Empty(t, a.header.Get("Location"))
	loginRequirePage(t, a)
}

func TestALoginEndsAtTheClientWithACodeAndTheUsersSealedConnection(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	client := h.newClient("agent", nil)
	b := h.newBrowser(t)
	consent, back := b.loginBack(client)
	answer := b.get(back)
	loginRequireHeaders(t, answer)
	code := h.loginRequireCode(t, answer, client)
	loginRequireEnded(t, answer, consent.id)

	now := h.clock.Now()
	stored := loginStoredRequest(t, h.store, consent.id)
	require.Equal(t, oauth.AuthRequestApproved, stored.State)
	require.Equal(t, oauth.HashToken(code), stored.CodeHash)
	require.Equal(t, "github:583231", stored.ConnectionID)
	require.Equal(t, now.Add(60*time.Second), stored.ExpiresAt, "the code lasts 60 seconds")

	connection, err := h.store.Connection(t.Context(), "github:583231")
	require.NoError(t, err)
	require.Equal(t, oauth.ProviderGitHub, connection.Provider)
	require.EqualValues(t, loginUserID, connection.UpstreamUserID)
	require.Equal(t, loginUserLogin, connection.Login)
	require.Equal(t, now.Add(28800*time.Second), connection.AccessExpiresAt)
	require.Equal(t, now.Add(15897600*time.Second), connection.RefreshExpiresAt)
	require.Equal(t, now, connection.UpdatedAt)
	gave := h.github.lastTokens()
	require.NotContains(t, string(connection.AccessToken), gave.access)
	require.NotContains(t, string(connection.RefreshToken), gave.refresh)
	access, refresh, err := oauth.LoginOpenTokens(h.keys, connection)
	require.NoError(t, err)
	require.Equal(t, gave.access, access)
	require.Equal(t, gave.refresh, refresh)
}

func TestTheUsersRefusalAtGitHubGoesToTheClientAndEndsTheLogin(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	h.github.set(func(g *loginGitHub) { g.deny = true })
	client := h.newClient("agent", nil)
	b := h.newBrowser(t)
	consent, back := b.loginBack(client)
	answer := b.get(back)
	require.Equal(t, http.StatusFound, answer.status, answer.body)
	loginRequireHeaders(t, answer)
	loginRequireEnded(t, answer, consent.id)

	to := answer.location(t)
	require.Equal(t, client.redirectURI, loginEndpoint(to))
	require.Equal(t, url.Values{"error": {"access_denied"}, "state": {"agent-state-/cb/agent"}, "iss": {h.issuer}}, to.Query())
	require.Equal(t, oauth.AuthRequestExpired, loginStoredRequest(t, h.store, consent.id).State)
	require.Zero(t, h.github.tradeCount())
}

func TestARefusalForAClientWithoutStateCarriesNoState(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	h.github.set(func(g *loginGitHub) { g.deny = true })
	client := h.newClient("agent", nil)
	b := h.newBrowser(t)
	q := h.request(client)
	q.Del("state")
	toGitHub := b.post(loginFormOf(t, b.authorize(q)), []string{h.issuer})
	answer := b.get(b.get(toGitHub.location(t).String()).location(t).String())
	require.Equal(t, url.Values{"error": {"access_denied"}, "iss": {h.issuer}}, answer.location(t).Query())
}

func TestACallbackWithAStateThatNoLoginHoldsIsAnErrorPage(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	b := h.newBrowser(t)
	for _, query := range []string{"state=made-up&code=gh-code-1", "code=gh-code-1", "state=made-up&error=access_denied"} {
		loginRequireErrorPage(t, b.get(h.issuer+oauth.GitHubCallbackPath+"?"+query), http.StatusBadRequest)
	}
	require.Zero(t, h.github.tradeCount())
}

func TestACallbackInAnotherBrowserIsRefusedAndTheRightBrowserCanFinish(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	client := h.newClient("agent", nil)
	b := h.newBrowser(t)
	consent, back := b.loginBack(client)
	other := h.newBrowser(t)
	for name, cookies := range map[string][]*http.Cookie{
		"no GitHub cookie":   nil,
		"a wrong one":        {{Name: "__Host-faucet_" + consent.id + "_gh", Value: "made-up-value"}},
		"the browser cookie": {consent.cookie},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, back, nil)
		require.NoError(t, err)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		refused := other.do(req)
		loginRequireErrorPage(t, refused, http.StatusForbidden)
		require.Empty(t, refused.header.Values("Set-Cookie"), name)
	}
	require.Zero(t, h.github.tradeCount(), "faucet traded no code for another browser")
	require.Equal(t, oauth.AuthRequestPending, loginStoredRequest(t, h.store, consent.id).State)

	h.loginRequireCode(t, b.get(back), client)
}

func TestAFailedTradeAtGitHubIsAnErrorPageAndLeavesTheLoginOpen(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		answer string
		// says is in the page.
		says string
	}{
		"GitHub refuses the code": {answer: `{"error":"bad_verification_code"}`, says: "faucet could not get your login from GitHub"},
		"the App's tokens never expire": {
			answer: `{"access_token":"ghu_neverexpires","token_type":"bearer","scope":""}`, says: "tokens that never expire",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newLoginHarness(t)
			h.github.set(func(g *loginGitHub) { g.tokenAnswer = c.answer })
			client := h.newClient("agent", nil)
			b := h.newBrowser(t)
			consent, back := b.loginBack(client)
			answer := b.get(back)
			loginRequireErrorPage(t, answer, http.StatusBadGateway)
			require.Contains(t, answer.body, c.says)
			require.Empty(t, answer.header.Values("Set-Cookie"))
			stored := loginStoredRequest(t, h.store, consent.id)
			require.Equal(t, oauth.AuthRequestPending, stored.State)
			require.Equal(t, oauth.TokenHash{}, stored.CodeHash)
			require.Empty(t, h.agents.received(client.path))
		})
	}
}

func TestTheOperatorLearnsThatTheAppsTokensNeverExpire(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	h.github.set(func(g *loginGitHub) { g.tokenAnswer = `{"access_token":"ghu_neverexpires","token_type":"bearer"}` })
	b := h.newBrowser(t)
	_, back := b.loginBack(h.newClient("agent", nil))
	loginRequireErrorPage(t, b.get(back), http.StatusBadGateway)
	require.Contains(t, h.logs.String(), `turn on "Expire user authorization tokens" in the GitHub App's settings`)
}

func TestACallbackLoadedTwiceIsAnErrorPageTheSecondTimeAndTheCodeStillWorks(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	client := h.newClient("agent", nil)
	b := h.newBrowser(t)
	consent, back := b.loginBack(client)
	code := h.loginRequireCode(t, b.get(back), client)

	again := b.get(back)
	loginRequireErrorPage(t, again, http.StatusBadRequest)
	require.Contains(t, again.body, "This login is over")
	require.Equal(t, 1, h.github.tradeCount())
	stored := loginStoredRequest(t, h.store, consent.id)
	require.Equal(t, oauth.AuthRequestApproved, stored.State)
	require.Equal(t, oauth.HashToken(code), stored.CodeHash)
	require.True(t, stored.ExpiresAt.After(h.clock.Now()))
}

func TestAGitHubAnswerToAnEarlierContinueIsRefusedAndTheLaterOneWorks(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	client := h.newClient("agent", nil)
	b := h.newBrowser(t)
	consent := b.start(client)
	firstBack := b.get(b.continueWith(consent).location(t).String()).location(t).String()
	secondBack := b.get(b.continueWith(consent).location(t).String()).location(t).String()

	loginRequireErrorPage(t, b.get(firstBack), http.StatusBadRequest)
	h.loginRequireCode(t, b.get(secondBack), client)
}

func TestACallbackAfterTheLoginExpiredIsAnErrorPage(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	client := h.newClient("agent", nil)
	late := h.newBrowser(t)
	lateConsent, lateBack := late.loginBack(client)
	h.clock.Advance(10 * time.Minute)
	answer := late.get(lateBack)
	loginRequireErrorPage(t, answer, http.StatusBadRequest)
	require.Contains(t, answer.body, "This login is over")
	require.Zero(t, h.github.tradeCount())
	require.Equal(t, oauth.TokenHash{}, loginStoredRequest(t, h.store, lateConsent.id).CodeHash)

	b := h.newBrowser(t)
	_, back := b.loginBack(client)
	h.clock.Advance(10*time.Minute - time.Second)
	h.loginRequireCode(t, b.get(back), client)
}

func TestTheRedirectKeepsTheQueryOfTheRedirectURIAsItIsWritten(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	for name, suffix := range map[string]string{
		"a query":             "?tenant=a%20b&empty=&x=1",
		"a query ending in &": "?tenant=a+b&",
		"an empty query":      "?",
	} {
		uri := h.agents.URL + "/cb/query" + suffix
		client := h.newClient("query", map[string]any{"redirect_uris": []string{uri}})
		b := h.newBrowser(t)
		_, back := b.loginBack(client)
		location := b.get(back).header.Get("Location")
		rest, ok := strings.CutPrefix(location, uri)
		require.True(t, ok, "%s: %s", name, location)
		if !strings.HasSuffix(uri, "?") && !strings.HasSuffix(uri, "&") {
			rest, ok = strings.CutPrefix(rest, "&")
			require.True(t, ok, name)
		}
		added, err := url.ParseQuery(rest)
		require.NoError(t, err, name)
		require.NotEmpty(t, added.Get("code"), name)
		require.Equal(t, "agent-state-/cb/query", added.Get("state"), name)
		require.Equal(t, h.issuer, added.Get("iss"), name)
		require.Len(t, added, 3, name)
	}
}

func TestTheLogsOfALoginHoldNoSecret(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	client := h.newClient("agent", nil)
	b := h.newBrowser(t)
	consent := b.start(client)
	toGitHub := b.continueWith(consent)
	githubCookie := b.cookie("__Host-faucet_" + consent.id + "_gh")
	require.NotNil(t, githubCookie)
	back := b.get(toGitHub.location(t).String()).location(t)
	h.newBrowser(t).get(back.String())
	answer := b.get(back.String())
	code := h.loginRequireCode(t, answer, client)
	h.newBrowser(t).post(consent.form, []string{loginForeign}, consent.cookie)
	gave := h.github.lastTokens()

	logs := h.logs.String()
	require.Contains(t, logs, consent.id, "the log names the request")
	require.Contains(t, logs, h.docs.Listener.Addr().String(), "the log names the client's host")
	for name, secret := range map[string]string{
		"the browser cookie": consent.cookie.Value,
		"the form token":     consent.form.Get("token"),
		"the GitHub cookie":  githubCookie.Value,
		"GitHub's state":     back.Query().Get("state"),
		"GitHub's code":      back.Query().Get("code"),
		"the client's code":  code,
		"the client's state": "agent-state-/cb/agent",
		"the access token":   gave.access,
		"the refresh token":  gave.refresh,
		"the App's secret":   loginAppSecret,
	} {
		require.NotEmpty(t, secret, name)
		require.NotContains(t, logs, secret, name)
	}
}
