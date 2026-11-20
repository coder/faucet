package oauth_test

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"cdr.dev/slog/v3"
	"cdr.dev/slog/v3/sloggers/sloghuman"
	"github.com/coder/quartz"
	"github.com/coder/superblink-events/faucet"
	"github.com/coder/superblink-events/faucet/oauth"
)

// fixedResolver answers each host name with its addresses.
type fixedResolver map[string][]netip.Addr

func (r fixedResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return r[host], nil
}

// A client chooses the URL of its document, so faucet builds the client that
// fetches it. No option that a host can set lets a fetch reach a private
// address or follow a redirect, and only the test hook AllowInsecureLoopback
// reaches loopback.
func TestNoOptionOfTheDocumentClientReachesAPrivateAddressOrFollowsARedirect(t *testing.T) {
	t.Parallel()
	var internal atomic.Int64
	docs := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved.json" {
			http.Redirect(w, r, "/internal.json", http.StatusFound)
			return
		}
		internal.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(docs.Close)
	pool := x509.NewCertPool()
	pool.AddCert(docs.Certificate())
	port := docs.Listener.Addr().(interface{ AddrPort() netip.AddrPort }).AddrPort().Port()
	resolver := fixedResolver{
		"loopback.example.com":    {netip.MustParseAddr("127.0.0.1")},
		"loopback6.example.com":   {netip.MustParseAddr("::1")},
		"private.example.com":     {netip.MustParseAddr("10.0.0.1")},
		"metadata.example.com":    {netip.MustParseAddr("169.254.169.254")},
		"cgnat.example.com":       {netip.MustParseAddr("100.64.0.1")},
		"unique.example.com":      {netip.MustParseAddr("fd00::1")},
		"unspecified.example.com": {netip.MustParseAddr("0.0.0.0")},
		"mixed.example.com":       {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")},
	}

	authorize := func(t *testing.T, opts faucet.HTTPClientOptions, clientID string) (*httptest.ResponseRecorder, string) {
		t.Helper()
		logs := &loginLogs{}
		login := testBrowserLogin(t, "https://github.example.com")
		login.Documents = opts
		server, err := oauth.NewServer(oauth.ServerDeps{
			Store: newFakeStore(), Clock: quartz.NewMock(t), Logger: slog.Make(sloghuman.Sink(logs)), Issuer: issuer,
			Resources: []string{resource}, Login: login, Repos: noRepos,
		})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		q := url.Values{"client_id": {clientID}}
		server.AuthorizeHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, oauth.AuthorizePath+"?"+q.Encode(), nil))
		return rec, logs.String()
	}

	for name, opts := range map[string]faucet.HTTPClientOptions{
		"the zero value":           {},
		"roots that trust it":      {RootCAs: pool},
		"a resolver":               {Resolver: resolver},
		"a resolver and the roots": {Resolver: resolver, RootCAs: pool},
		"the test hook":            {Resolver: resolver, RootCAs: pool, AllowInsecureLoopback: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hosts := []string{"[::1]", "10.0.0.1", "169.254.169.254", "100.64.0.1", "[fd00::1]", "0.0.0.0"}
			if opts.Resolver != nil {
				for host := range resolver {
					hosts = append(hosts, host)
				}
			}
			if !opts.AllowInsecureLoopback {
				hosts = append(hosts, "127.0.0.1")
			}
			for _, host := range hosts {
				if opts.AllowInsecureLoopback && (host == "[::1]" || strings.HasPrefix(host, "loopback")) {
					continue
				}
				page, logged := authorize(t, opts, "https://"+host+":"+strconv.Itoa(int(port))+"/internal.json")
				require.Equal(t, http.StatusBadRequest, page.Code, host)
				require.Contains(t, page.Body.String(), "faucet could not fetch the client&#39;s document", host)
				require.Contains(t, logged, faucet.ErrDestinationRefused.Error(), host)
			}
			page, _ := authorize(t, opts, docs.URL+"/moved.json")
			require.Equal(t, http.StatusBadRequest, page.Code)
			require.Zero(t, internal.Load(), "no fetch reached the loopback document, directly or through a redirect")
		})
	}
}

// The test hook reaches a document on loopback, which the end-to-end tests
// serve.
func TestTheTestHookOfTheDocumentClientReachesLoopback(t *testing.T) {
	t.Parallel()
	docs := newLoginDocuments(t)
	pool := x509.NewCertPool()
	pool.AddCert(docs.Certificate())
	clientID := docs.URL + "/client.json"
	docs.mu.Lock()
	docs.docs["/client.json"] = map[string]any{"client_id": clientID, "redirect_uris": []string{"https://app.example.com/cb"}}
	docs.mu.Unlock()
	login := testBrowserLogin(t, "https://github.example.com")
	login.Documents = faucet.HTTPClientOptions{AllowInsecureLoopback: true, RootCAs: pool}
	server, err := oauth.NewServer(oauth.ServerDeps{
		Store: newFakeStore(), Clock: quartz.NewMock(t), Logger: slog.Make(), Issuer: issuer, Resources: []string{resource}, Login: login, Repos: noRepos,
	})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	q := url.Values{"client_id": {clientID}}
	server.AuthorizeHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, oauth.AuthorizePath+"?"+q.Encode(), nil))
	docs.mu.Lock()
	defer docs.mu.Unlock()
	require.Equal(t, 1, docs.fetches["/client.json"])
	require.NotContains(t, rec.Body.String(), "could not fetch", "the document arrived, and only the query is refused")
}
