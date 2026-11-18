package oauth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/quartz"
)

type countingTransport struct{ requests atomic.Int64 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.requests.Add(1)
	return nil, errors.New("this test has no network")
}

// A client ID on faucet's own host would have faucet fetch from itself, and
// such a document can name another one on faucet's host in turn.
func TestAClientIDOnFaucetsOwnHostIsRefusedWithoutAFetch(t *testing.T) {
	t.Parallel()
	documents := &countingTransport{}
	login := testBrowserLogin(t, "https://github.example.com")
	login.Documents = &http.Client{Transport: documents}
	server, err := oauth.NewServer(oauth.ServerDeps{
		Store: newFakeStore(), Clock: quartz.NewMock(t), Logger: slog.Make(), Issuer: issuer, Resources: []string{resource}, Login: login,
	})
	require.NoError(t, err)
	authorize := func(clientID string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		q := url.Values{"client_id": {clientID}}
		server.AuthorizeHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, oauth.AuthorizePath+"?"+q.Encode(), nil))
		return rec
	}

	for _, clientID := range []string{
		issuer + "/client.json",
		"https://FAUCET.Example.COM/client.json",
		"https://faucet.example.com:443/client.json",
		"https://faucet.example.com.:0443/oauth/authorize?client_id=x",
	} {
		page := authorize(clientID)
		require.Zero(t, documents.requests.Load(), "%s is not fetched", clientID)
		require.Equal(t, http.StatusBadRequest, page.Code, clientID)
		require.Contains(t, page.Body.String(), "client_id must not be on faucet&#39;s own host", clientID)
	}

	page := authorize("https://faucet.example.com:8443/client.json")
	require.EqualValues(t, 1, documents.requests.Load(), "another port is another host")
	require.Contains(t, page.Body.String(), "faucet could not fetch the client&#39;s document")
}
