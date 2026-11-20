package oauth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
)

// testTokenKeys is a key ring with one made-up key.
func testTokenKeys(t *testing.T) oauth.TokenKeys {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	keys, err := oauth.ParseTokenKeys("k1:" + base64.StdEncoding.EncodeToString(key))
	require.NoError(t, err)
	return keys
}

// testBrowserLogin is a browser login whose GitHub is at githubURL.
func testBrowserLogin(t *testing.T, githubURL string) *oauth.BrowserLogin {
	t.Helper()
	return &oauth.BrowserLogin{
		GitHub: oauth.GitHubApp{
			ClientID: "Iv1.faucet", ClientSecret: "app-secret", WebURL: githubURL, APIURL: githubURL,
			Client: &http.Client{Timeout: 5 * time.Second},
		},
		Keys:      testTokenKeys(t),
		Documents: faucet.HTTPClientOptions{AllowInsecureLoopback: true},
	}
}

func TestTheMetadataOfABrowserLoginNamesItsGrantsAndClientDocuments(t *testing.T) {
	t.Parallel()
	server, err := oauth.NewServer(oauth.ServerDeps{
		Store: newFakeStore(), Clock: quartz.NewMock(t), Logger: slog.Make(), Issuer: issuer,
		Resources: []string{resource}, Login: testBrowserLogin(t, "https://github.example.com"), Repos: noRepos,
	})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	server.MetadataHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, oauth.MetadataPath, nil))
	var m map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))

	require.Equal(t, issuer+oauth.AuthorizePath, m["authorization_endpoint"])
	require.Equal(t, []any{"authorization_code", "client_credentials", "refresh_token", "urn:ietf:params:oauth:grant-type:token-exchange"}, m["grant_types_supported"])
	require.Equal(t, []any{"code"}, m["response_types_supported"])
	require.Equal(t, []any{"client_secret_basic", "none"}, m["token_endpoint_auth_methods_supported"])
	require.Equal(t, issuer+"/oauth/revoke", m["revocation_endpoint"])
	require.Equal(t, []any{"client_secret_basic", "none"}, m["revocation_endpoint_auth_methods_supported"])
	require.Equal(t, []any{"S256"}, m["code_challenge_methods_supported"])
	require.Equal(t, true, m["client_id_metadata_document_supported"])
	require.Equal(t, true, m["authorization_response_iss_parameter_supported"])
}

func TestWithoutABrowserLoginTheMetadataNamesNoClientDocuments(t *testing.T) {
	t.Parallel()
	server, err := oauth.NewServer(oauth.ServerDeps{
		Store: newFakeStore(), Clock: quartz.NewMock(t), Logger: slog.Make(), Issuer: issuer, Resources: []string{resource},
	})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	server.MetadataHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, oauth.MetadataPath, nil))
	var m map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))

	require.Equal(t, []any{"client_credentials", "urn:ietf:params:oauth:grant-type:token-exchange"}, m["grant_types_supported"])
	require.NotContains(t, m, "client_id_metadata_document_supported")
	require.NotContains(t, m, "authorization_response_iss_parameter_supported")
}

func TestNewServerRefusesABrowserLoginThatLacksAPart(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*oauth.BrowserLogin){
		"no client ID":      func(l *oauth.BrowserLogin) { l.GitHub.ClientID = "" },
		"no client secret":  func(l *oauth.BrowserLogin) { l.GitHub.ClientSecret = "" },
		"a plain http host": func(l *oauth.BrowserLogin) { l.GitHub.WebURL = "http://github.example.com" },
		"no API URL":        func(l *oauth.BrowserLogin) { l.GitHub.APIURL = "" },
		"no token key":      func(l *oauth.BrowserLogin) { l.Keys = oauth.TokenKeys{} },
	} {
		login := testBrowserLogin(t, "https://github.example.com")
		mutate(login)
		_, err := oauth.NewServer(oauth.ServerDeps{
			Store: newFakeStore(), Clock: quartz.NewMock(t), Logger: slog.Make(), Issuer: issuer, Resources: []string{resource}, Login: login, Repos: noRepos,
		})
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), "app-secret", name)
	}
	_, err := oauth.NewServer(oauth.ServerDeps{
		Store: newFakeStore(), Clock: quartz.NewMock(t), Logger: slog.Make(), Issuer: issuer, Resources: []string{resource},
		Login: testBrowserLogin(t, "https://github.example.com"),
	})
	require.Error(t, err, "no repository checker")
}

// A document client that reaches loopback is for tests, so a server that has
// one says so in its log, and a server for production says nothing.
func TestNewServerWarnsOfADocumentClientThatReachesLoopback(t *testing.T) {
	t.Parallel()
	const warning = "the client that fetches client documents reaches loopback over http, which is for tests only"
	for loopback, want := range map[bool]bool{true: true, false: false} {
		logs := &loginLogs{}
		login := testBrowserLogin(t, "https://github.example.com")
		login.Documents.AllowInsecureLoopback = loopback
		_, err := oauth.NewServer(oauth.ServerDeps{
			Store: newFakeStore(), Clock: quartz.NewMock(t), Logger: slog.Make(sloghuman.Sink(logs)), Issuer: issuer,
			Resources: []string{resource}, Login: login, Repos: noRepos,
		})
		require.NoError(t, err)
		require.Equal(t, want, strings.Contains(logs.String(), "[warn]") && strings.Contains(logs.String(), warning), "AllowInsecureLoopback %v: %s", loopback, logs.String())
	}
}

// noRepos is the RepoChecker of a server whose test asks GitHub about no
// repository.
var noRepos = faucet.TokenAuthorizer{BaseURL: "https://api.github.example.com"}

// A session that ended, or went unused for 180 days, goes with its refresh
// tokens. A connection that no session and no request names goes too, with
// the GitHub tokens that faucet kept for its user.
func TestPurgeDeletesDeadSessionsAndTheConnectionsThatNoneNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t, newFakeStore())
	live := h.redeem(h.approve("login-live", 1))
	ended := h.redeem(h.approve("login-ended", 2))
	idle := h.redeem(h.approve("login-idle", 3))
	require.NoError(t, h.store.EndSession(ctx, ended.id, h.clock.Now()))

	h.clock.Advance(180*24*time.Hour - time.Minute)
	live = h.refresh(live)
	h.clock.Advance(time.Minute)
	_, err := h.server.Purge(ctx)
	require.NoError(t, err)

	for name, s := range map[string]grantSession{"an ended session": ended, "an idle session": idle} {
		_, err := h.store.Session(ctx, s.id)
		require.ErrorIs(t, err, oauth.ErrUnknownSession, name)
		grantRequireRefused(t, h.post("", grantRefreshForm(s.refresh)), http.StatusBadRequest, "invalid_grant", name)
	}
	for _, user := range []int64{2, 3} {
		_, err := h.store.Connection(ctx, oauth.ConnectionID(oauth.ProviderGitHub, user))
		require.ErrorIs(t, err, oauth.ErrUnknownConnection, "user %d", user)
	}
	h.refresh(live)
	_, err = h.store.Connection(ctx, oauth.ConnectionID(oauth.ProviderGitHub, 1))
	require.NoError(t, err, "the connection of the live session")
}

// A request without a code goes once it expires, since nothing can come of
// it. A request with a code stays a day past its expiry, so a code that is
// used again within that day still ends the session it made.
func TestPurgeDeletesARequestWithoutACodeOnceItExpiredAndOneWithACodeADayAfter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newGrantHarness(t, newFakeStore())
	h.redeem(h.approve("login-redeemed", 1))
	now := h.clock.Now()
	pending := func(id string, expires time.Time) oauth.AuthRequest {
		return oauth.AuthRequest{
			ID: id, State: oauth.AuthRequestPending, ClientID: grantClientID, RedirectURI: grantRedirectURI,
			Resource: resource, Scopes: bothScopes, CodeChallenge: "challenge", BrowserHash: oauth.HashToken("browser " + id),
			GitHubStateHash: oauth.HashToken("github state " + id), CreatedAt: now, ExpiresAt: expires,
		}
	}
	gone, given, live := pending("gone", now.Add(10*time.Minute)), pending("given-up", now.Add(10*time.Minute)), pending("live", now.Add(time.Hour))
	for _, r := range []oauth.AuthRequest{gone, given, live} {
		require.NoError(t, h.store.ConsentAuthRequest(ctx, r, now))
	}
	require.NoError(t, h.store.ExpireAuthRequest(ctx, given.ID))
	stored := func(r oauth.AuthRequest) bool {
		_, err := h.store.AuthRequestByGitHubState(ctx, r.GitHubStateHash)
		return err == nil
	}
	redeemed := func() bool {
		_, err := h.store.AuthRequestByGitHubState(ctx, oauth.HashToken("github state login-redeemed"))
		return err == nil
	}

	h.clock.Advance(10 * time.Minute)
	res, err := h.server.Purge(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, res.ExpiredAuthRequests)
	require.False(t, stored(gone), "a pending request goes at its expiry")
	require.False(t, stored(given), "an expired request goes at its expiry")
	require.True(t, stored(live), "a pending request stays until its expiry")
	require.True(t, redeemed(), "a request with a code stays")

	h.clock.Advance(24 * time.Hour)
	res, err = h.server.Purge(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, res.ExpiredAuthRequests)
	require.False(t, redeemed(), "a request with a code goes a day after its expiry")
}
