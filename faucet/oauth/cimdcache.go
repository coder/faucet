package oauth

import (
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/quartz"
)

// documentCache keeps the client documents that passed every check, so that
// a client that logs in again soon is not fetched again. It keeps one copy
// for every user, so it reads the headers as a shared cache of RFC 9111
// does.
type documentCache struct {
	clock   quartz.Clock
	mu      sync.Mutex
	entries map[string]cachedDocument
}

type cachedDocument struct {
	doc     clientDocument
	expires time.Time
}

func newDocumentCache(clock quartz.Clock) *documentCache {
	return &documentCache{clock: clock, entries: map[string]cachedDocument{}}
}

// get returns the document of clientID while it is fresh.
func (c *documentCache) get(clientID string) (clientDocument, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[clientID]
	if !ok || !c.clock.Now().Before(e.expires) {
		return clientDocument{}, false
	}
	return e.doc, true
}

// put keeps doc, which a response with header served just now, for as long
// as freshFor allows. A full cache drops the document that expires first.
func (c *documentCache) put(clientID string, doc clientDocument, header http.Header) {
	now := c.clock.Now()
	lifetime := freshFor(header, now)
	if lifetime <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	maps.DeleteFunc(c.entries, func(_ string, e cachedDocument) bool { return !now.Before(e.expires) })
	if _, ok := c.entries[clientID]; !ok && len(c.entries) >= maxCachedDocuments {
		first := slices.MinFunc(slices.Collect(maps.Keys(c.entries)), func(a, b string) int {
			return c.entries[a].expires.Compare(c.entries[b].expires)
		})
		delete(c.entries, first)
	}
	c.entries[clientID] = cachedDocument{doc: doc, expires: now.Add(lifetime)}
}

// freshFor is how long a shared cache may keep a response with header that
// arrived at now (RFC 9111 section 4.2), and maxDocumentAge at most. It is
// zero or less when the cache must not keep the response, or when the
// header does not say.
func freshFor(header http.Header, now time.Time) time.Duration {
	directives := map[string]string{}
	for _, line := range header.Values("Cache-Control") {
		for _, d := range strings.Split(line, ",") {
			name, value, _ := strings.Cut(strings.TrimSpace(d), "=")
			name = strings.ToLower(name)
			if _, twice := directives[name]; twice {
				// RFC 9111 lets a cache take such a response as stale.
				return 0
			}
			if name != "" {
				directives[name] = strings.Trim(value, `"`)
			}
		}
	}
	for _, name := range []string{"no-store", "no-cache", "private"} {
		if _, ok := directives[name]; ok {
			return 0
		}
	}
	var lifetime time.Duration
	if value, ok := directives["s-maxage"]; ok {
		lifetime = deltaSeconds(value)
	} else if value, ok := directives["max-age"]; ok {
		lifetime = deltaSeconds(value)
	} else if expires, err := http.ParseTime(header.Get("Expires")); err == nil {
		date, err := http.ParseTime(header.Get("Date"))
		if err != nil {
			date = now
		}
		lifetime = expires.Sub(date)
	}
	return min(lifetime-deltaSeconds(header.Get("Age")), maxDocumentAge)
}

// deltaSeconds reads a delta-seconds value (RFC 9111 section 1.2.2): one or
// more digits. It is zero for a value that is not one.
func deltaSeconds(value string) time.Duration {
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return 0
	}
	// Digits that pass 32 bits parse as the largest 32-bit number, about the
	// 2^31 seconds that RFC 9111 takes them for.
	n, _ := strconv.ParseInt(value, 10, 32)
	return time.Duration(n) * time.Second
}
