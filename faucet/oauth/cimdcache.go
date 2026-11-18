package oauth

import (
	"context"
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
// a client that logs in again soon is not fetched again, and for a minute
// the failures of the fetches that did not pass. It keeps one copy for
// every user, so it reads the headers as a shared cache of RFC 9111 does.
// Callers that ask at once for a document that it does not hold share one
// fetch.
type documentCache struct {
	clock    quartz.Clock
	mu       sync.Mutex
	entries  map[string]cachedDocument
	failures map[string]failedFetch
	flights  map[string]*documentFlight
}

type cachedDocument struct {
	doc     clientDocument
	expires time.Time
}

type failedFetch struct {
	err     error
	expires time.Time
}

// documentFlight is a fetch that callers share. doc and err are set before
// done closes.
type documentFlight struct {
	done chan struct{}
	doc  clientDocument
	err  error
}

func newDocumentCache(clock quartz.Clock) *documentCache {
	return &documentCache{
		clock: clock, entries: map[string]cachedDocument{}, failures: map[string]failedFetch{}, flights: map[string]*documentFlight{},
	}
}

// fetch returns the document of clientID while it is fresh, and the failure
// of its last fetch for failedFetchAge after it. Otherwise it calls read,
// once for all the callers that ask at once, and keeps what read returns.
// A caller whose ctx ends stops waiting for another caller's read.
func (c *documentCache) fetch(ctx context.Context, clientID string, read func() (clientDocument, http.Header, error)) (clientDocument, error) {
	c.mu.Lock()
	now := c.clock.Now()
	if e, ok := c.entries[clientID]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.doc, nil
	}
	if e, ok := c.failures[clientID]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return clientDocument{}, e.err
	}
	if fl, ok := c.flights[clientID]; ok {
		c.mu.Unlock()
		select {
		case <-fl.done:
			return fl.doc, fl.err
		case <-ctx.Done():
			return clientDocument{}, ctx.Err()
		}
	}
	fl := &documentFlight{done: make(chan struct{})}
	c.flights[clientID] = fl
	c.mu.Unlock()

	doc, header, err := read()
	c.mu.Lock()
	delete(c.flights, clientID)
	if err != nil {
		c.keepFailureLocked(clientID, err)
	} else {
		c.keepLocked(clientID, doc, header)
	}
	c.mu.Unlock()
	fl.doc, fl.err = doc, err
	close(fl.done)
	return doc, err
}

// keepLocked keeps doc, which a response with header served just now, for
// as long as freshFor allows. A full cache drops the document that expires
// first. The caller holds c.mu.
func (c *documentCache) keepLocked(clientID string, doc clientDocument, header http.Header) {
	now := c.clock.Now()
	lifetime := freshFor(header, now)
	if lifetime <= 0 {
		return
	}
	keep(c.entries, clientID, cachedDocument{doc: doc, expires: now.Add(lifetime)}, maxCachedDocuments, now,
		func(e cachedDocument) time.Time { return e.expires })
}

// keepFailureLocked keeps err, the failure of a fetch just now, for
// failedFetchAge. The caller holds c.mu.
func (c *documentCache) keepFailureLocked(clientID string, err error) {
	now := c.clock.Now()
	keep(c.failures, clientID, failedFetch{err: err, expires: now.Add(failedFetchAge)}, maxFailedFetches, now,
		func(e failedFetch) time.Time { return e.expires })
}

// keep puts e in m under key, after it drops the entries that expired at
// now and, when m holds limit entries, the one that expires first.
func keep[E any](m map[string]E, key string, e E, limit int, now time.Time, expires func(E) time.Time) {
	maps.DeleteFunc(m, func(_ string, o E) bool { return !now.Before(expires(o)) })
	if _, ok := m[key]; !ok && len(m) >= limit {
		first := slices.MinFunc(slices.Collect(maps.Keys(m)), func(a, b string) int {
			return expires(m[a]).Compare(expires(m[b]))
		})
		delete(m, first)
	}
	m[key] = e
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
