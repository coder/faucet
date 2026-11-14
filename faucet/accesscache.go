package faucet

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/coder/quartz"
)

const (
	accessCacheTTL        = time.Minute
	maxAccessCacheEntries = 10000
)

// tokenChecker is what AccessCache wraps. TokenAuthorizer is one.
type tokenChecker interface {
	Authorizer
	Authenticate(ctx context.Context, token string) (Principal, error)
}

// AccessCache remembers the answers of a TokenAuthorizer for one minute, so
// that each events/poll and wait_for_github_events call does not spend two
// GitHub API calls of the caller's quota (GET /user and GET /repos/{owner}/{name}).
// Use it as the Authenticate step of your server and as Deps.Authorizer.
//
// Only a success is remembered. A denial, a rejected token, a rate limit and any
// other error ask GitHub again on the next call, so a new grant takes effect at
// once. A grant that GitHub withdraws keeps working for up to a minute, and an
// entry is never extended by use. The cache holds a SHA-256 of the token as its
// key and never the token. It keeps at most 10,000 entries and holds them per
// process.
//
// events/subscribe does not use the remembered answer: a renewal calls
// RepoIDUncached, because every renewal must check access again.
type AccessCache struct {
	inner tokenChecker
	clock quartz.Clock

	mu        sync.Mutex
	entries   map[accessKey]accessEntry
	nextSweep time.Time
}

type accessKey struct {
	token [sha256.Size]byte
	// repo is empty for the answer of Authenticate.
	repo string
}

type accessEntry struct {
	login   string
	repoID  int64
	expires time.Time
}

var _ Authorizer = (*AccessCache)(nil)

// NewAccessCache returns a cache in front of inner.
func NewAccessCache(inner tokenChecker, clock quartz.Clock) *AccessCache {
	return &AccessCache{inner: inner, clock: clock, entries: map[accessKey]accessEntry{}}
}

// Authenticate implements the authenticator of the server. A cached login is
// returned with the token of this call.
func (c *AccessCache) Authenticate(ctx context.Context, token string) (Principal, error) {
	key := accessKey{token: sha256.Sum256([]byte(token))}
	if e, ok := c.lookup(key); ok {
		return NewPrincipal(e.login, token)
	}
	p, err := c.inner.Authenticate(ctx, token)
	if err != nil {
		return Principal{}, err
	}
	c.store(key, accessEntry{login: p.Login})
	return p, nil
}

// RepoID implements Authorizer.
func (c *AccessCache) RepoID(ctx context.Context, p Principal, repo string) (int64, error) {
	if p.token == "" {
		return c.inner.RepoID(ctx, p, repo)
	}
	if e, ok := c.lookup(accessKey{token: sha256.Sum256([]byte(p.token)), repo: repo}); ok {
		return e.repoID, nil
	}
	return c.RepoIDUncached(ctx, p, repo)
}

// RepoIDUncached asks the wrapped authorizer and remembers a success.
func (c *AccessCache) RepoIDUncached(ctx context.Context, p Principal, repo string) (int64, error) {
	id, err := c.inner.RepoID(ctx, p, repo)
	if err == nil && p.token != "" {
		c.store(accessKey{token: sha256.Sum256([]byte(p.token)), repo: repo}, accessEntry{repoID: id})
	}
	return id, err
}

func (c *AccessCache) lookup(key accessKey) (accessEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return e, ok && e.expires.After(c.clock.Now())
}

// store drops the expired entries once a minute and whenever the cache is full,
// and when it is still full it keeps the answer to itself.
func (c *AccessCache) store(key accessKey, e accessEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	if !now.Before(c.nextSweep) || len(c.entries) >= maxAccessCacheEntries {
		for k, old := range c.entries {
			if !old.expires.After(now) {
				delete(c.entries, k)
			}
		}
		c.nextSweep = now.Add(accessCacheTTL)
	}
	if _, known := c.entries[key]; !known && len(c.entries) >= maxAccessCacheEntries {
		return
	}
	e.expires = now.Add(accessCacheTTL)
	c.entries[key] = e
}

// uncachedAuthorizer is implemented by an Authorizer that remembers answers.
type uncachedAuthorizer interface {
	RepoIDUncached(ctx context.Context, p Principal, repo string) (int64, error)
}
