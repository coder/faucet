package oauth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/internal/githubapi"
)

// Made-up values that no error may hold.
const (
	stubClientID = "Iv23liStubClient0001"
	stubAppSec   = "stub-app-sec-29f1c0e7"
	stubCode     = "stub-code-77a1d3"
	stubVerifier = "stub-verifier-0b9e4c2a"
	stubAccess   = "ghu_stubAccess7c1f"
	stubRefresh  = "ghr_stubRefresh3e8a"
	callbackURI  = "https://faucet.example.com/oauth/github/callback"
)

var stubValues = []string{stubAppSec, stubCode, stubVerifier, stubAccess, stubRefresh}

func requireHoldsNoStubValue(t *testing.T, err error, name string) {
	t.Helper()
	for _, v := range stubValues {
		require.NotContains(t, err.Error(), v, name)
		require.NotContains(t, fmt.Sprintf("%+v", err), v, name)
	}
}

// recordedRequest is what GitHub received.
type recordedRequest struct {
	method, path string
	header       http.Header
	form         url.Values
}

// githubStub stands in for GitHub's web host and API, and records each
// request.
type githubStub struct {
	*httptest.Server
	requests chan recordedRequest
}

func newGitHubStub(t *testing.T, answer http.HandlerFunc) *githubStub {
	t.Helper()
	s := &githubStub{requests: make(chan recordedRequest, 8)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.requests <- recordedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), form: r.PostForm}
		answer(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *githubStub) app() GitHubApp {
	return GitHubApp{ClientID: stubClientID, ClientSecret: stubAppSec, WebURL: s.URL, APIURL: s.URL + "/api/v3", Client: s.Client()}
}

func (s *githubStub) received(t *testing.T) recordedRequest {
	t.Helper()
	select {
	case r := <-s.requests:
		return r
	default:
		require.FailNow(t, "GitHub received no request")
		return recordedRequest{}
	}
}

func answer(status int, header map[string]string, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

const grantedTokens = `{"access_token":"` + stubAccess + `","expires_in":28800,"refresh_token":"` + stubRefresh +
	`","refresh_token_expires_in":15897600,"scope":"","token_type":"bearer"}`

func TestTheAuthorizeURLAsksGitHubForACodeUnderPKCE(t *testing.T) {
	t.Parallel()

	for _, webURL := range []string{"https://github.com", "https://ghe.example.com/"} {
		app := GitHubApp{ClientID: stubClientID, ClientSecret: stubAppSec, WebURL: webURL}
		u, err := url.Parse(app.authorizeURL(callbackURI, "state-1", "challenge-1"))
		require.NoError(t, err)
		require.Equal(t, strings.TrimSuffix(webURL, "/")+"/login/oauth/authorize", u.Scheme+"://"+u.Host+u.Path)
		require.Equal(t, url.Values{
			"client_id":             {stubClientID},
			"redirect_uri":          {callbackURI},
			"state":                 {"state-1"},
			"code_challenge":        {"challenge-1"},
			"code_challenge_method": {"S256"},
		}, u.Query())
		require.NotContains(t, u.String(), stubAppSec)
	}
}

func requireTokenRequest(t *testing.T, got recordedRequest, form url.Values) {
	t.Helper()
	require.Equal(t, http.MethodPost, got.method)
	require.Equal(t, "/login/oauth/access_token", got.path)
	require.Equal(t, "application/json", got.header.Get("Accept"))
	require.Equal(t, "coder-faucet", got.header.Get("User-Agent"))
	require.Equal(t, "application/x-www-form-urlencoded", got.header.Get("Content-Type"))
	require.Empty(t, got.header.Get("Authorization"))
	require.Equal(t, form, got.form)
}

func TestExchangeSendsTheCodeAndItsVerifierWithTheAppsCredentials(t *testing.T) {
	t.Parallel()

	s := newGitHubStub(t, answer(http.StatusOK, nil, grantedTokens))
	tokens, err := s.app().exchange(context.Background(), stubCode, callbackURI, stubVerifier)
	require.NoError(t, err)
	requireTokenRequest(t, s.received(t), url.Values{
		"client_id":     {stubClientID},
		"client_secret": {stubAppSec},
		"code":          {stubCode},
		"redirect_uri":  {callbackURI},
		"code_verifier": {stubVerifier},
	})
	require.Equal(t, githubTokens{
		access: stubAccess, refresh: stubRefresh,
		accessLifetime: 8 * time.Hour, refreshLifetime: 15897600 * time.Second,
	}, tokens)
}

func TestRefreshSendsTheRefreshTokenGrantWithTheAppsCredentials(t *testing.T) {
	t.Parallel()

	s := newGitHubStub(t, answer(http.StatusOK, nil, grantedTokens))
	tokens, err := s.app().refresh(context.Background(), stubRefresh)
	require.NoError(t, err)
	requireTokenRequest(t, s.received(t), url.Values{
		"client_id":     {stubClientID},
		"client_secret": {stubAppSec},
		"grant_type":    {"refresh_token"},
		"refresh_token": {stubRefresh},
	})
	require.Equal(t, stubAccess, tokens.access)
	require.Equal(t, 8*time.Hour, tokens.accessLifetime)
}

func TestATokenWithoutExpiryMeansTheAppDoesNotExpireUserTokens(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"expiry off":         `{"access_token":"` + stubAccess + `","scope":"","token_type":"bearer"}`,
		"no refresh token":   `{"access_token":"` + stubAccess + `","expires_in":28800,"token_type":"bearer"}`,
		"no access lifetime": `{"access_token":"` + stubAccess + `","refresh_token":"` + stubRefresh + `","refresh_token_expires_in":15897600}`,
	} {
		s := newGitHubStub(t, answer(http.StatusOK, nil, body))
		_, err := s.app().exchange(context.Background(), stubCode, callbackURI, stubVerifier)
		require.ErrorIs(t, err, errTokensDoNotExpire, name)
		requireHoldsNoStubValue(t, err, name)
	}
}

func TestARefusedRefreshTokenIsErrBadRefreshToken(t *testing.T) {
	t.Parallel()

	s := newGitHubStub(t, answer(http.StatusOK, nil,
		`{"error":"bad_refresh_token","error_description":"The refresh token passed is incorrect or expired.","error_uri":"https://docs.github.com"}`))
	_, err := s.app().refresh(context.Background(), stubRefresh)
	require.ErrorIs(t, err, errBadRefreshToken)
	requireHoldsNoStubValue(t, err, "bad_refresh_token")
}

func TestOtherTokenAnswersAreErrorsThatHoldNoSecret(t *testing.T) {
	t.Parallel()

	withToken := func(field, value string) string {
		return strings.Replace(grantedTokens, `"`+field+`":"`+map[string]string{"access_token": stubAccess, "refresh_token": stubRefresh}[field]+`"`, `"`+field+`":`+value, 1)
	}
	withLifetime := func(field, value string) string {
		old := map[string]string{"expires_in": `"expires_in":28800`, "refresh_token_expires_in": `"refresh_token_expires_in":15897600`}[field]
		if value == "" {
			return strings.Replace(grantedTokens, ","+old, "", 1)
		}
		return strings.Replace(grantedTokens, old, `"`+field+`":`+value, 1)
	}
	cases := map[string]struct {
		status int
		body   string
		// code is GitHub's error code that the error names, if any.
		code string
	}{
		"a wrong code":                {status: 200, body: `{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`, code: "bad_verification_code"},
		"wrong credentials":           {status: 200, body: `{"error":"incorrect_client_credentials"}`, code: "incorrect_client_credentials"},
		"a code that is not safe":     {status: 200, body: `{"error":"Bad ` + stubCode + `"}`},
		"a code of 65 bytes":          {status: 200, body: `{"error":"` + strings.Repeat("a", 65) + `"}`},
		"status 500":                  {status: 500, body: grantedTokens},
		"status 401":                  {status: 401, body: `{"message":"` + stubAppSec + `"}`},
		"status 404":                  {status: 404, body: grantedTokens},
		"not JSON":                    {status: 200, body: "<html>" + stubAccess},
		"a form answer":               {status: 200, body: "access_token=" + stubAccess + "&refresh_token=" + stubRefresh},
		"a JSON array":                {status: 200, body: `["` + stubAccess + `"]`},
		"an access token of a number": {status: 200, body: withToken("access_token", "7")},
		"no access token":             {status: 200, body: withToken("access_token", `""`)},
		"a space in the access token": {status: 200, body: withToken("access_token", `"ghu_a b"`)},
		"a newline in it":             {status: 200, body: withToken("access_token", `"ghu_a\nb"`)},
		"a letter beyond ASCII":       {status: 200, body: withToken("access_token", `"ghu_é"`)},
		"an access token of 1025":     {status: 200, body: withToken("access_token", `"`+strings.Repeat("a", 1025)+`"`)},
		"a tab in the refresh token":  {status: 200, body: withToken("refresh_token", `"ghr_a\tb"`)},
		"a zero lifetime":             {status: 200, body: withLifetime("expires_in", "0")},
		"a negative lifetime":         {status: 200, body: withLifetime("expires_in", "-1")},
		"a lifetime past Duration":    {status: 200, body: withLifetime("expires_in", "9300000000")},
		"a lifetime in a string":      {status: 200, body: withLifetime("expires_in", `"28800"`)},
		"no refresh lifetime":         {status: 200, body: withLifetime("refresh_token_expires_in", "")},
		"a zero refresh lifetime":     {status: 200, body: withLifetime("refresh_token_expires_in", "0")},
		"65537 bytes":                 {status: 200, body: grantedTokens + strings.Repeat(" ", 65537-len(grantedTokens))},
	}
	for name, c := range cases {
		s := newGitHubStub(t, answer(c.status, nil, c.body))
		for call, do := range map[string]func() (githubTokens, error){
			"exchange": func() (githubTokens, error) {
				return s.app().exchange(context.Background(), stubCode, callbackURI, stubVerifier)
			},
			"refresh": func() (githubTokens, error) { return s.app().refresh(context.Background(), stubRefresh) },
		} {
			tokens, err := do()
			require.Error(t, err, "%s by %s", name, call)
			require.Equal(t, githubTokens{}, tokens, name)
			require.NotErrorIs(t, err, errBadRefreshToken, name)
			require.NotErrorIs(t, err, errTokensDoNotExpire, name)
			requireHoldsNoStubValue(t, err, name)
			if c.code != "" {
				require.Contains(t, err.Error(), c.code, name)
			}
			if c.status != 200 {
				require.Contains(t, err.Error(), fmt.Sprint(c.status), name)
			}
		}
	}
}

func TestATokenAnswerOf64KiBIsRead(t *testing.T) {
	t.Parallel()

	s := newGitHubStub(t, answer(http.StatusOK, nil, grantedTokens+strings.Repeat(" ", 65536-len(grantedTokens))))
	tokens, err := s.app().exchange(context.Background(), stubCode, callbackURI, stubVerifier)
	require.NoError(t, err)
	require.Equal(t, stubRefresh, tokens.refresh)

	long := `"` + strings.Repeat("a", 1024) + `"`
	s = newGitHubStub(t, answer(http.StatusOK, nil, strings.Replace(grantedTokens, `"`+stubAccess+`"`, long, 1)))
	tokens, err = s.app().exchange(context.Background(), stubCode, callbackURI, stubVerifier)
	require.NoError(t, err, "a token of 1024 characters")
	require.Len(t, tokens.access, 1024)
}

func TestTheTokenEndpointIsNotFollowedThroughARedirect(t *testing.T) {
	t.Parallel()

	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		answer(http.StatusOK, nil, grantedTokens)(w, nil)
	}))
	t.Cleanup(other.Close)
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		s := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/login/oauth/access_token", status)
		})
		_, err := s.app().exchange(context.Background(), stubCode, callbackURI, stubVerifier)
		require.Error(t, err, status)
		requireHoldsNoStubValue(t, err, http.StatusText(status))
		_, err = s.app().refresh(context.Background(), stubRefresh)
		require.Error(t, err, status)
	}
	require.Zero(t, elsewhere.Load(), "the secret never reaches another host")
}

func TestUserReadsTheIDAndLoginThatTheTokenBelongsTo(t *testing.T) {
	t.Parallel()

	s := newGitHubStub(t, answer(http.StatusOK, nil, `{"id":583231,"login":"octocat","name":"The Octocat"}`))
	user, err := s.app().user(context.Background(), stubAccess)
	require.NoError(t, err)
	require.Equal(t, githubUser{id: 583231, login: "octocat"}, user)
	got := s.received(t)
	require.Equal(t, http.MethodGet, got.method)
	require.Equal(t, "/api/v3/user", got.path)
	require.Equal(t, "Bearer "+stubAccess, got.header.Get("Authorization"))
	require.Equal(t, "coder-faucet", got.header.Get("User-Agent"))
}

func TestUserTellsARejectedTokenFromTheRateLimitAndFromOtherAnswers(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		status int
		header map[string]string
		body   string
		want   error
	}{
		"401":                    {status: 401, body: `{"message":"Bad credentials"}`, want: faucet.ErrInvalidToken},
		"429":                    {status: 429, body: `{}`, want: githubapi.ErrRateLimited},
		"403 with no quota left": {status: 403, header: map[string]string{"X-RateLimit-Remaining": "0"}, body: `{}`, want: githubapi.ErrRateLimited},
		"403 secondary limit":    {status: 403, body: `{"message":"You have exceeded a secondary rate limit."}`, want: githubapi.ErrRateLimited},
		"403":                    {status: 403, body: `{"message":"Resource not accessible by integration"}`},
		"500":                    {status: 500, body: `{}`},
		"no ID":                  {status: 200, body: `{"login":"octocat"}`},
		"a negative ID":          {status: 200, body: `{"id":-1,"login":"octocat"}`},
		"no login":               {status: 200, body: `{"id":583231}`},
		"a login with a colon":   {status: 200, body: `{"id":583231,"login":"svc:ci"}`},
		"not JSON":               {status: 200, body: stubAccess},
	} {
		s := newGitHubStub(t, answer(c.status, c.header, c.body))
		user, err := s.app().user(context.Background(), stubAccess)
		require.Error(t, err, name)
		require.Equal(t, githubUser{}, user, name)
		requireHoldsNoStubValue(t, err, name)
		if c.want != nil {
			require.ErrorIs(t, err, c.want, name)
			continue
		}
		require.NotErrorIs(t, err, faucet.ErrInvalidToken, name)
		require.NotErrorIs(t, err, githubapi.ErrRateLimited, name)
		if c.status != 200 {
			require.Contains(t, err.Error(), fmt.Sprint(c.status), name)
		}
	}
}

func TestValidateNeedsTheAppsCredentialsAndURLsThatAreSafeForThem(t *testing.T) {
	t.Parallel()

	good := GitHubApp{ClientID: stubClientID, ClientSecret: stubAppSec, WebURL: "https://github.com", APIURL: "https://api.github.com"}
	require.NoError(t, good.Validate())
	local := GitHubApp{ClientID: stubClientID, ClientSecret: stubAppSec, WebURL: "http://127.0.0.1:8080", APIURL: "http://localhost:8080/api/v3"}
	require.NoError(t, local.Validate(), "http is for a loopback host")

	for name, change := range map[string]func(*GitHubApp){
		"no client ID":         func(g *GitHubApp) { g.ClientID = "" },
		"no client secret":     func(g *GitHubApp) { g.ClientSecret = "" },
		"no web URL":           func(g *GitHubApp) { g.WebURL = "" },
		"an http web URL":      func(g *GitHubApp) { g.WebURL = "http://github.com" },
		"a web URL with a key": func(g *GitHubApp) { g.WebURL = "https://user:" + stubAppSec + "@github.com" },
		"a web URL with query": func(g *GitHubApp) { g.WebURL = "https://github.com?x=" + stubAppSec },
		"a relative web URL":   func(g *GitHubApp) { g.WebURL = "github.com" },
		"no API URL":           func(g *GitHubApp) { g.APIURL = "" },
		"an http API URL":      func(g *GitHubApp) { g.APIURL = "http://api.github.com" },
		"an API URL with a key": func(g *GitHubApp) {
			g.APIURL = "https://user:" + stubAppSec + "@api.github.com"
		},
	} {
		app := good
		change(&app)
		err := app.Validate()
		require.Error(t, err, name)
		requireHoldsNoStubValue(t, err, name)
	}
}

func TestAGitHubAppNeverPrintsItsSecret(t *testing.T) {
	t.Parallel()

	app := GitHubApp{ClientID: stubClientID, ClientSecret: stubAppSec, WebURL: "https://github.com", APIURL: "https://api.github.com"}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%q"} {
		for _, value := range []any{app, &app, []GitHubApp{app}, struct{ App GitHubApp }{app}} {
			out := fmt.Sprintf(verb, value)
			require.NotContains(t, out, stubAppSec, "%s of %T", verb, value)
			require.NotContains(t, out, fmt.Sprintf("%x", stubAppSec), "%s of %T", verb, value)
		}
	}
	require.Contains(t, fmt.Sprint(app), stubClientID)
}
