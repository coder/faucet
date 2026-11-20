package oauth_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/coder/superblink-events/faucet/oauth"
)

// serveWith adds header to every answer for the document of c.
func (d *loginDocuments) serveWith(c loginClient, header http.Header) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.headers[strings.TrimPrefix(c.id, d.URL)] = header
}

// fetchCount is how many requests reached the document of c.
func (d *loginDocuments) fetchCount(c loginClient) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fetches[strings.TrimPrefix(c.id, d.URL)]
}

func TestAClientDocumentIsFetchedOnceInItsMaxAgeAndItsChangeShowsAfter(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	c := h.newClient("agent", nil)
	h.docs.serveWith(c, http.Header{"Cache-Control": {"max-age=60"}})
	b := h.newBrowser(t)
	require.Contains(t, b.start(c).page.body, "says it is <strong>Agent agent</strong>")

	h.newClient("agent", map[string]any{"client_name": "Agent renamed"})
	h.clock.Advance(59 * time.Second)
	page := b.start(c).page
	require.Equal(t, 1, h.docs.fetchCount(c))
	require.Contains(t, page.body, "says it is <strong>Agent agent</strong>")

	h.clock.Advance(time.Second)
	page = b.start(c).page
	require.Equal(t, 2, h.docs.fetchCount(c))
	require.Contains(t, page.body, "says it is <strong>Agent renamed</strong>")
}

// Faucet keeps one copy of a document for all its users, so it reads the
// headers as a shared cache does (RFC 9111), and keeps the document for
// MaxDocumentAge at most.
func TestTheHeadersOfAClientDocumentSetHowLongFaucetKeepsIt(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	cacheControl := func(lines ...string) http.Header { return http.Header{"Cache-Control": lines} }
	const date, aMinuteLater = "Mon, 02 Nov 2026 10:00:00 GMT", "Mon, 02 Nov 2026 10:01:00 GMT"
	for i, c := range []struct {
		name    string
		header  http.Header
		keptFor time.Duration
	}{
		{"no cache header", nil, 0},
		{"a max-age", cacheControl("max-age=60"), time.Minute},
		{"a max-age in capitals and quotes", cacheControl(`MAX-AGE="60"`), time.Minute},
		{"a max-age over the bound", cacheControl("max-age=86400"), oauth.MaxDocumentAge},
		{"a max-age of 0", cacheControl("max-age=0"), 0},
		{"a max-age that is no number", cacheControl("max-age=sixty"), 0},
		{"two max-ages", cacheControl("max-age=60, max-age=120"), 0},
		{"no-store", cacheControl("max-age=60, no-store"), 0},
		{"no-cache", cacheControl("no-cache, max-age=60"), 0},
		{"private, which a shared cache must not keep", cacheControl("private, max-age=60"), 0},
		{"an s-maxage, which a shared cache takes over max-age", cacheControl("max-age=60, s-maxage=10"), 10 * time.Second},
		{"other directives", cacheControl("public, max-age=60, must-revalidate"), time.Minute},
		{"two Cache-Control lines", cacheControl("public", "max-age=60"), time.Minute},
		{"an Age", http.Header{"Cache-Control": {"max-age=60"}, "Age": {"50"}}, 10 * time.Second},
		{"an Expires a minute after the Date", http.Header{"Date": {date}, "Expires": {aMinuteLater}}, time.Minute},
		{"an Expires that is no date", http.Header{"Expires": {"0"}}, 0},
		{"a max-age beside an Expires", http.Header{"Cache-Control": {"max-age=10"}, "Date": {date}, "Expires": {aMinuteLater}}, 10 * time.Second},
	} {
		client := h.newClient(fmt.Sprintf("case%d", i), nil)
		h.docs.serveWith(client, c.header)
		b := h.newBrowser(t)
		b.start(client)
		if c.keptFor > 0 {
			h.clock.Advance(c.keptFor - time.Second)
			b.start(client)
			require.Equal(t, 1, h.docs.fetchCount(client), "%s: kept until a second before %s", c.name, c.keptFor)
			h.clock.Advance(time.Second)
		}
		b.start(client)
		require.Equal(t, 2, h.docs.fetchCount(client), "%s: kept for %s only", c.name, c.keptFor)
	}
}

// A client ID whose fetch failed, by an error answer or a document that
// fails a check, is fetched once a minute at most, whatever its headers
// say, so a bad client ID costs one fetch however often it comes. Each
// request in between gets the same page.
func TestAClientIDWhoseFetchFailedIsFetchedOnceAMinute(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	refused := h.newClient("refused", map[string]any{"redirect_uris": []string{}})
	missing := loginClient{id: h.docs.URL + "/missing.json", redirectURI: h.agents.URL + "/cb/missing", path: "/cb/missing"}
	for _, c := range []loginClient{refused, missing} {
		h.docs.serveWith(c, http.Header{"Cache-Control": {"max-age=600"}})
	}
	for _, c := range []loginClient{refused, missing} {
		var pages []string
		for range 3 {
			a := h.newBrowser(t).authorize(h.request(c))
			require.Equal(t, http.StatusBadRequest, a.status, c.id)
			pages = append(pages, a.body)
		}
		require.Equal(t, 1, h.docs.fetchCount(c), c.id)
		require.Equal(t, pages[0], pages[2], "%s: the same page each time", c.id)
	}
	h.clock.Advance(time.Minute)
	for _, c := range []loginClient{refused, missing} {
		h.newBrowser(t).authorize(h.request(c))
		require.Equal(t, 2, h.docs.fetchCount(c), "%s is fetched again after a minute", c.id)
	}
}

// Logins of one client that start at once, before faucet holds its
// document, share one fetch of it.
func TestLoginsOfOneClientThatStartAtOnceShareOneFetch(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	c := h.newClient("agent", nil)
	hold := make(chan struct{})
	h.docs.mu.Lock()
	h.docs.hold = hold
	h.docs.mu.Unlock()

	const logins = 8
	var wg sync.WaitGroup
	statuses := make([]int, logins)
	for i := range logins {
		b := h.newBrowser(t)
		wg.Go(func() { statuses[i] = b.authorize(h.request(c)).status })
	}
	require.Eventually(t, func() bool { return h.docs.fetchCount(c) > 0 }, 10*time.Second, time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	close(hold)
	wg.Wait()
	require.Equal(t, 1, h.docs.fetchCount(c))
	for i, status := range statuses {
		require.Equal(t, http.StatusOK, status, "login %d", i)
	}
}

func TestFaucetKeepsAtMostMaxCachedDocumentsAndDropsTheOneThatExpiresFirst(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	b := h.newBrowser(t)
	maxAge := fmt.Sprintf("max-age=%d", int(oauth.MaxDocumentAge/time.Second))
	clients := make([]loginClient, oauth.MaxCachedDocuments+1)
	for i := range clients {
		clients[i] = h.newClient(fmt.Sprintf("agent%d", i), nil)
		h.docs.serveWith(clients[i], http.Header{"Cache-Control": {maxAge}})
		b.start(clients[i])
		h.clock.Advance(time.Millisecond)
	}
	for _, c := range []loginClient{clients[len(clients)-1], clients[1]} {
		b.start(c)
		require.Equal(t, 1, h.docs.fetchCount(c), "%s is kept", c.id)
	}
	b.start(clients[0])
	require.Equal(t, 2, h.docs.fetchCount(clients[0]), "the first document made room for the last")
}

// Failed fetches are kept apart from documents, so that many bad client IDs
// push out no document.
func TestFailedFetchesPushOutNoDocument(t *testing.T) {
	t.Parallel()
	h := newLoginHarness(t)
	b := h.newBrowser(t)
	good := h.newClient("agent", nil)
	h.docs.serveWith(good, http.Header{"Cache-Control": {"max-age=60"}})
	b.start(good)
	for i := range oauth.MaxCachedDocuments + 1 {
		bad := loginClient{id: fmt.Sprintf("%s/missing%d.json", h.docs.URL, i), redirectURI: h.agents.URL + "/cb/missing", path: "/cb/missing"}
		require.Equal(t, http.StatusBadRequest, b.authorize(h.request(bad)).status)
	}
	b.start(good)
	require.Equal(t, 1, h.docs.fetchCount(good), "the document is kept")
}
