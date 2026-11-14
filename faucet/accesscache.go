package faucet

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"

	"golang.org/x/xerrors"

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
	started := c.clock.Now()
	p, err := c.inner.Authenticate(ctx, token)
	if err != nil {
		return Principal{}, err
	}
	c.store(key, accessEntry{login: p.Login}, started)
	return p, nil
}

func (c *AccessCache) RepoID(ctx context.Context, p Principal, repo string) (int64, error) {
	if p.token == "" || repo == "" {
		return c.RepoIDUncached(ctx, p, repo)
	}
	if e, ok := c.lookup(accessKey{token: sha256.Sum256([]byte(p.token)), repo: repo}); ok {
		return e.repoID, nil
	}
	return c.RepoIDUncached(ctx, p, repo)
}

// RepoIDUncached asks the wrapped authorizer. It remembers a success, and a
// denial removes what was remembered, so a poll right after a renewal that
// lost access is refused.
func (c *AccessCache) RepoIDUncached(ctx context.Context, p Principal, repo string) (int64, error) {
	if repo == "" {
		return 0, xerrors.New("repo must be written owner/name")
	}
	started := c.clock.Now()
	id, err := c.inner.RepoID(ctx, p, repo)
	if p.token == "" {
		return id, err
	}
	key := accessKey{token: sha256.Sum256([]byte(p.token)), repo: repo}
	switch {
	case err == nil:
		c.store(key, accessEntry{repoID: id}, started)
	case xerrors.Is(err, ErrNoAccess):
		c.forget(key)
	case xerrors.Is(err, ErrInvalidToken):
		c.forget(key, accessKey{token: key.token})
	}
	return id, err
}

func (c *AccessCache) lookup(key accessKey) (accessEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return e, ok && e.expires.After(c.clock.Now())
}

func (c *AccessCache) forget(keys ...accessKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, key := range keys {
		delete(c.entries, key)
	}
}

// store counts the minute from started, the start of the lookup, so a slow
// GitHub answer does not stretch what it says. It drops the expired entries
// once a minute and whenever the cache is full, and when it is still full it
// keeps the answer to itself.
func (c *AccessCache) store(key accessKey, e accessEntry, started time.Time) {
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
	e.expires = started.Add(accessCacheTTL)
	c.entries[key] = e
}

type uncachedAuthorizer interface {
	RepoIDUncached(ctx context.Context, p Principal, repo string) (int64, error)
}
