package oauth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/quartz"
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
		Documents: faucet.NewHTTPClient(faucet.HTTPClientOptions{AllowInsecureLoopback: true}),
	}
}

func TestTheMetadataOfABrowserLoginNamesItsGrantsAndClientDocuments(t *testing.T) {
	t.Parallel()
	server, err := oauth.NewServer(oauth.ServerDeps{
		Store: newFakeStore(), Clock: quartz.NewMock(t), Logger: slog.Make(), Issuer: issuer,
		Resources: []string{resource}, Login: testBrowserLogin(t, "https://github.example.com"),
	})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	server.MetadataHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, oauth.MetadataPath, nil))
	var m map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))

	require.Equal(t, issuer+oauth.AuthorizePath, m["authorization_endpoint"])
	require.Equal(t, []any{"authorization_code", "client_credentials", "refresh_token"}, m["grant_types_supported"])
	require.Equal(t, []any{"code"}, m["response_types_supported"])
	require.Equal(t, []any{"client_secret_basic", "none"}, m["token_endpoint_auth_methods_supported"])
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

	require.Equal(t, []any{"client_credentials"}, m["grant_types_supported"])
	require.NotContains(t, m, "client_id_metadata_document_supported")
	require.NotContains(t, m, "authorization_response_iss_parameter_supported")
}

func TestNewServerRefusesABrowserLoginThatLacksAPart(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*oauth.BrowserLogin){
		"no client ID":       func(l *oauth.BrowserLogin) { l.GitHub.ClientID = "" },
		"no client secret":   func(l *oauth.BrowserLogin) { l.GitHub.ClientSecret = "" },
		"a plain http host":  func(l *oauth.BrowserLogin) { l.GitHub.WebURL = "http://github.example.com" },
		"no API URL":         func(l *oauth.BrowserLogin) { l.GitHub.APIURL = "" },
		"no token key":       func(l *oauth.BrowserLogin) { l.Keys = oauth.TokenKeys{} },
		"no document client": func(l *oauth.BrowserLogin) { l.Documents = nil },
	} {
		login := testBrowserLogin(t, "https://github.example.com")
		mutate(login)
		_, err := oauth.NewServer(oauth.ServerDeps{
			Store: newFakeStore(), Clock: quartz.NewMock(t), Logger: slog.Make(), Issuer: issuer, Resources: []string{resource}, Login: login,
		})
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), "app-secret", name)
	}
}

// A request stays a day past its expiry, so a code that is used again within
// that day still ends the session it made.
func TestPurgeDeletesTheAuthRequestsThatExpiredADayAgo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newFakeStore()
	clock := quartz.NewMock(t)
	server, err := oauth.NewServer(oauth.ServerDeps{Store: store, Clock: clock, Logger: slog.Make(), Issuer: issuer, Resources: []string{resource}})
	require.NoError(t, err)
	now := clock.Now()
	request := func(id string, expires time.Time) oauth.AuthRequest {
		return oauth.AuthRequest{
			ID: id, State: oauth.AuthRequestPending, ClientID: "https://client.example.com/doc.json", RedirectURI: "http://127.0.0.1/cb",
			Resource: resource, Scopes: bothScopes, CodeChallenge: "challenge", BrowserHash: oauth.HashToken("browser " + id),
			CreatedAt: expires.Add(-10 * time.Minute), ExpiresAt: expires,
		}
	}
	old := request("old", now.Add(-24*time.Hour-time.Second))
	recent := request("recent", now.Add(-23*time.Hour))
	require.NoError(t, store.InsertAuthRequest(ctx, old))
	require.NoError(t, store.InsertAuthRequest(ctx, recent))

	res, err := server.Purge(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, res.ExpiredAuthRequests)
	require.Error(t, store.InsertAuthRequest(ctx, recent), "the recent request is still there")
	require.NoError(t, store.InsertAuthRequest(ctx, old), "the old request is gone")
}
