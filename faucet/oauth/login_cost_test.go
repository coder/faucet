package oauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
)

// countingResolver counts the host names that the document client looks up,
// and finds none.
type countingResolver struct{ lookups atomic.Int64 }

func (c *countingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	c.lookups.Add(1)
	return nil, errors.New("this test has no network")
}

// A client ID on faucet's own host would have faucet fetch from itself, and
// such a document can name another one on faucet's host in turn.
func TestAClientIDOnFaucetsOwnHostIsRefusedWithoutAFetch(t *testing.T) {
	t.Parallel()
	documents := &countingResolver{}
	login := testBrowserLogin(t, "https://github.example.com")
	login.Documents = faucet.HTTPClientOptions{Resolver: documents}
	server, err := oauth.NewServer(oauth.ServerDeps{
		Store: newFakeStore(), Clock: quartz.NewMock(t), Logger: slog.Make(), Issuer: issuer, Resources: []string{resource}, Login: login, Repos: noRepos,
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
		require.Zero(t, documents.lookups.Load(), "%s is not fetched", clientID)
		require.Equal(t, http.StatusBadRequest, page.Code, clientID)
		require.Contains(t, page.Body.String(), "client_id must not be on faucet&#39;s own host", clientID)
	}

	page := authorize("https://faucet.example.com:8443/client.json")
	require.EqualValues(t, 1, documents.lookups.Load(), "another port is another host")
	require.Contains(t, page.Body.String(), "faucet could not fetch the client&#39;s document")
}

// Showing the consent page stores nothing: the login waits in the page's
// form, sealed, until its user presses Continue. So the GETs that anyone
// can send grow no table and use up no limit that real users need.
func TestTheConsentPageStoresNothingUntilContinue(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	b := h.newBrowser(t)
	client := h.newClient("agent", nil)
	var last loginConsent
	for range 150 {
		last = b.start(client)
	}
	require.Zero(t, loginRequestCount(h.store), "no request is stored before Continue")

	a := b.continueWith(last)
	require.Equal(t, http.StatusSeeOther, a.status, a.body)
	require.Equal(t, 1, loginRequestCount(h.store), "Continue stores the request")
}
