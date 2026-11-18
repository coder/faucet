package oauth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"github.com/coder/faucet/faucet/oauth"
	"github.com/coder/quartz"
)

// roomyLimits are limits that only the tests of the limits reach.
var roomyLimits = oauth.AuthRequestLimits{PerClientHost: 1 << 20, Total: 1 << 20}

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

// loginRequireTooMany checks that a is the page of an authorization request
// that the limits of logins in progress refused, and that it started
// nothing.
func loginRequireTooMany(t *testing.T, a loginAnswer) {
	t.Helper()
	require.Equal(t, http.StatusTooManyRequests, a.status)
	loginRequirePage(t, a)
	require.Empty(t, a.header.Get("Location"))
	require.Empty(t, a.header.Values("Set-Cookie"))
	require.True(t, strings.Contains(a.body, "Try again in a few minutes."), "the page says when to try again")
}

func TestAClientHostWithTooManyLoginsInProgressGetsAPageThatSaysToTryLater(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	c := h.newClient("agent", nil)
	b := h.newBrowser(t)
	for range oauth.MaxPendingPerClientHost {
		b.start(c)
	}
	loginRequireTooMany(t, b.authorize(h.request(c)))
	require.Equal(t, oauth.MaxPendingPerClientHost, loginRequestCount(h.store))

	elsewhere := newLoginDocuments(t)
	other := loginClient{id: elsewhere.URL + "/agent.json", path: "/cb/elsewhere"}
	other.redirectURI = h.agents.URL + other.path
	elsewhere.docs["/agent.json"] = map[string]any{"client_id": other.id, "redirect_uris": []string{other.redirectURI}}
	b.start(other)

	h.clock.Advance(10 * time.Minute)
	b.start(c)
}

func TestWithTooManyLoginsInProgressNoClientStartsOne(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	c := h.newClient("agent", nil)
	b := h.newBrowser(t)
	held := loginStoredRequest(t, h.store, b.start(c).id)
	for i := range oauth.MaxPendingAuthRequests - 1 {
		r := held
		r.ID, r.ClientID = fmt.Sprintf("held-%d", i), fmt.Sprintf("https://agent%d.example.com/client.json", i)
		require.NoError(t, h.store.InsertAuthRequest(context.Background(), r, roomyLimits))
	}
	loginRequireTooMany(t, b.authorize(h.request(c)))
	require.Equal(t, oauth.MaxPendingAuthRequests, loginRequestCount(h.store))
}
